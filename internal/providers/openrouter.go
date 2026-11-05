package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Foncifelo/sidecar-proxy/pkg/openai"
)

// maxPeek bounds how much of a response ParseServed reads before giving up.
const maxPeek = 1 << 20

// OpenRouter is the generic adapter plus provider pinning.
type OpenRouter struct{}

// BuildRequest implements Provider. It pins ep.Provider with
// provider.order and allow_fallbacks=false, replacing any agent value.
func (OpenRouter) BuildRequest(ctx context.Context, ep Endpoint, body []byte) (*http.Request, error) {
	if ep.Provider == "" {
		return nil, fmt.Errorf("%w: openrouter endpoint %q has no provider to pin", ErrInvalidEndpoint, ep.ID)
	}
	req, err := buildRequest(ctx, ep, body, pinProvider)
	if err != nil {
		return nil, err
	}
	// Attribution headers come only from the YAML, never from the agent (ADR 0008).
	if ep.HTTPReferer != "" {
		req.Header.Set("HTTP-Referer", ep.HTTPReferer)
	}
	if ep.XTitle != "" {
		req.Header.Set("X-Title", ep.XTitle)
	}
	return req, nil
}

func pinProvider(ep Endpoint, body []byte) ([]byte, error) {
	pin := struct {
		Order          []string `json:"order"`
		AllowFallbacks bool     `json:"allow_fallbacks"`
	}{Order: []string{ep.Provider}}
	raw, err := json.Marshal(pin)
	if err != nil {
		return nil, fmt.Errorf("providers: encode provider pin: %w", err)
	}
	out, err := openai.SetMember(body, "provider", raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidBody, err)
	}
	return out, nil
}

// ParseServed implements Provider. It reads the "provider" field from the
// JSON body or from the first SSE data event, then restores resp.Body so the
// proxy still copies every byte. It returns "" when nothing is reported.
func (OpenRouter) ParseServed(resp *http.Response) string {
	if resp == nil || resp.Body == nil || resp.Body == http.NoBody {
		return ""
	}
	var peeked []byte
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		peeked = peekFirstEvent(resp.Body)
	} else {
		peeked, _ = io.ReadAll(io.LimitReader(resp.Body, maxPeek)) // a read error leaves a partial peek; the rest stays in Body
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peeked), resp.Body), resp.Body}
	if strings.HasPrefix(string(bytes.TrimSpace(peeked)), "{") {
		return servedFrom(peeked)
	}
	return servedFromSSE(peeked)
}

// peekFirstEvent reads lines until the first data line with a JSON payload
// is complete, or maxPeek bytes. It never reads past that line.
func peekFirstEvent(r io.Reader) []byte {
	br := bufio.NewReaderSize(io.LimitReader(r, maxPeek), 4096)
	var buf bytes.Buffer
	for {
		line, err := br.ReadBytes('\n')
		buf.Write(line)
		if strings.HasPrefix(string(line), "data:") && bytes.Contains(line, []byte("{")) {
			break
		}
		if err != nil {
			break
		}
	}
	// Anything bufio read ahead must be handed back too.
	rest, _ := br.Peek(br.Buffered())
	buf.Write(rest)
	return buf.Bytes()
}

func servedFromSSE(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		data, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "data:")
		if !ok {
			continue
		}
		if p := servedFrom([]byte(strings.TrimSpace(data))); p != "" {
			return p
		}
	}
	return ""
}

func servedFrom(b []byte) string {
	var v struct {
		Provider string `json:"provider"`
	}
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	return v.Provider
}

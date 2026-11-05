package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Foncifelo/sidecar-proxy/pkg/openai"
)

const chatPath = "/chat/completions"

// OpenAICompatible is the generic adapter for any OpenAI-compatible endpoint.
type OpenAICompatible struct{}

// BuildRequest implements Provider.
func (OpenAICompatible) BuildRequest(ctx context.Context, ep Endpoint, body []byte) (*http.Request, error) {
	return buildRequest(ctx, ep, body, nil)
}

// ParseServed implements Provider. Direct channels do not report a served
// provider: the endpoint's provider is the one that served.
func (OpenAICompatible) ParseServed(*http.Response) string { return "" }

// bodyHook lets an override rewrite the body. It must preserve every byte it
// does not own (docs/contrato-proxy.md, guarantee 1).
type bodyHook func(ep Endpoint, body []byte) ([]byte, error)

func buildRequest(ctx context.Context, ep Endpoint, body []byte, hook bodyHook) (*http.Request, error) {
	target, err := chatURL(ep.BaseURL)
	if err != nil {
		return nil, err
	}
	if ep.APIKey == "" {
		return nil, fmt.Errorf("%w: endpoint %q has no api key", ErrInvalidEndpoint, ep.ID)
	}
	out, err := rewriteBody(ep, body, hook)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(out))
	if err != nil {
		return nil, fmt.Errorf("providers: new request for endpoint %q: %w", ep.ID, err)
	}
	header, scheme := ep.AuthHeader, ep.AuthScheme
	if header == "" {
		header = "Authorization"
		if scheme == "" {
			scheme = "Bearer"
		}
	}
	value := ep.APIKey.Reveal()
	if scheme != "" {
		value = scheme + " " + value
	}
	req.Header.Set(header, value)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	return req, nil
}

func chatURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%w: base_url must be an absolute http(s) URL", ErrInvalidEndpoint)
	}
	return strings.TrimRight(u.String(), "/") + chatPath, nil
}

// rewriteBody applies the only body changes the proxy contract allows: model
// (when asked), stream_options.include_usage (when streaming) and the hook.
// Every other byte is preserved; with nothing to rewrite body is returned as is.
func rewriteBody(ep Endpoint, body []byte, hook bodyHook) ([]byte, error) {
	stream, err := streamFlag(body)
	if err != nil {
		return nil, err
	}
	out := body
	if ep.RewriteModel {
		if ep.ProviderModelName == "" {
			return nil, fmt.Errorf("%w: endpoint %q has no provider_model_name", ErrInvalidEndpoint, ep.ID)
		}
		if out, err = openai.SetModel(out, ep.ProviderModelName); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidBody, err)
		}
	}
	if stream {
		if out, err = openai.SetIncludeUsage(out, true); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidBody, err)
		}
	}
	if hook != nil {
		if out, err = hook(ep, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// streamFlag checks that body is a JSON object and reads its "stream" member.
func streamFlag(body []byte) (bool, error) {
	if t := bytes.TrimSpace(body); len(t) == 0 || t[0] != '{' {
		return false, fmt.Errorf("%w: not a JSON object", ErrInvalidBody)
	}
	var probe struct {
		Stream json.RawMessage `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false, fmt.Errorf("%w: not a JSON object", ErrInvalidBody)
	}
	var stream bool
	if len(probe.Stream) > 0 {
		if err := json.Unmarshal(probe.Stream, &stream); err != nil {
			return false, fmt.Errorf("%w: stream must be a boolean", ErrInvalidBody)
		}
	}
	return stream, nil
}

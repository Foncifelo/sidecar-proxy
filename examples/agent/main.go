// Command agent is a minimal example agent: it sends one streamed chat
// completion to the sidecar using only net/http and copies the raw SSE bytes
// it receives to stdout. The X-Sidecar-* response headers go to stderr.
//
//	go run ./examples/agent -url http://127.0.0.1:8787 -model my/model -prompt "hi"
//
// The agent's own key (AGENT_API_KEY) is sent as Authorization; the sidecar
// never forwards it to a provider (CONTRIBUTING.md rule 3).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"
)

const (
	defaultURL     = "http://127.0.0.1:8787"
	defaultModel   = "example/model"
	requestTimeout = 5 * time.Minute
	dialTimeout    = 5 * time.Second
	headerTimeout  = 2 * time.Minute
	sidecarPrefix  = "X-Sidecar-"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "agent:", err)
		os.Exit(1)
	}
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Stream   bool      `json:"stream"`
	Messages []message `json:"messages"`
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("url", defaultURL, "sidecar base URL")
	model := fs.String("model", defaultModel, "model to request")
	prompt := fs.String("prompt", "Say hello.", "user message")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("parse flags: %w", err)
	}

	body, err := json.Marshal(chatRequest{
		Model:    *model,
		Stream:   true,
		Messages: []message{{Role: "user", Content: *prompt}},
	})
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	url := strings.TrimRight(*base, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if key := os.Getenv("AGENT_API_KEY"); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := newClient().Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	writeSidecarHeaders(stderr, resp.StatusCode, resp.Header)
	if _, err := io.Copy(stdout, resp.Body); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sidecar answered %d", resp.StatusCode)
	}
	return nil
}

// newClient has explicit timeouts and its own transport (no http.DefaultClient).
// There is no global Timeout because the response is a stream.
func newClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
		ResponseHeaderTimeout: headerTimeout,
		DisableCompression:    true, // keep the bytes exactly as the sidecar sent them
	}}
}

func writeSidecarHeaders(w io.Writer, status int, h http.Header) {
	_, _ = fmt.Fprintf(w, "status: %d\n", status)
	keys := make([]string, 0, len(h))
	for k := range h {
		if strings.HasPrefix(k, sidecarPrefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(w, "%s: %s\n", k, h.Get(k))
	}
}

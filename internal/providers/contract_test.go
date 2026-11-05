package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/sse"
)

// upstream serves a fixture and records the request it received.
func upstream(t *testing.T, status int, fixture string, got *http.Request) *httptest.Server {
	t.Helper()
	var payload []byte
	if fixture != "" {
		var err error
		payload, err = os.ReadFile(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = *r.Clone(r.Context())
		got.Body = io.NopCloser(bytes.NewReader(b))
		ct := "application/json"
		if strings.HasSuffix(fixture, ".sse") {
			ct = "text/event-stream"
		}
		w.Header().Set("Content-Type", ct)
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		w.WriteHeader(status)
		if payload == nil {
			payload = []byte(`{"error":{"message":"fictitious upstream error"}}`)
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func streamFacts(t *testing.T, b []byte) (toolDeltas int, hasUsage bool) {
	t.Helper()
	r := sse.NewReader(bytes.NewReader(b))
	for {
		ev, err := r.Next()
		if err != nil {
			return toolDeltas, hasUsage
		}
		if !ev.HasData || ev.IsDone() {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					ToolCalls []json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &c); err != nil {
			t.Fatalf("bad chunk %q: %v", ev.Data, err)
		}
		for _, ch := range c.Choices {
			toolDeltas += len(ch.Delta.ToolCalls)
		}
		hasUsage = hasUsage || (len(c.Usage) > 0 && string(c.Usage) != "null")
	}
}

func TestContract(t *testing.T) {
	tests := []struct {
		name        string
		channel     Channel
		status      int
		fixture     string
		stream      bool
		wantServed  string
		wantTools   bool
		wantUsage   bool
		wantPinning bool
	}{
		{name: "together no-stream", channel: ChannelDirect, status: 200, fixture: "together/chat.json"},
		{name: "together stream tool_calls", channel: ChannelDirect, status: 200, fixture: "together/chat.stream.sse", stream: true, wantTools: true, wantUsage: true},
		{name: "together stream usage absent", channel: ChannelDirect, status: 200, fixture: "together/chat.nousage.stream.sse", stream: true},
		{name: "together 429", channel: ChannelDirect, status: 429},
		{name: "together 5xx", channel: ChannelDirect, status: 502},
		{name: "openrouter no-stream", channel: ChannelOpenRouter, status: 200, fixture: "openrouter/chat.json", wantServed: "FictProviderA", wantPinning: true},
		{name: "openrouter stream tool_calls", channel: ChannelOpenRouter, status: 200, fixture: "openrouter/chat.stream.sse", stream: true, wantServed: "FictProviderA", wantTools: true, wantUsage: true, wantPinning: true},
		{name: "openrouter 429", channel: ChannelOpenRouter, status: 429, wantPinning: true},
		{name: "openrouter 5xx", channel: ChannelOpenRouter, status: 503, wantPinning: true},
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got http.Request
			srv := upstream(t, tt.status, tt.fixture, &got)
			ep := testEndpoint(tt.channel)
			ep.BaseURL = srv.URL + "/v1"
			ep.RewriteModel = true
			body := `{"model":"agent-model","messages":[{"role":"user","content":"x"}],"stream":` + map[bool]string{true: "true", false: "false"}[tt.stream] + `}`
			p := For(tt.channel)
			req, err := p.BuildRequest(context.Background(), ep, []byte(body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			if served := p.ParseServed(resp); served != tt.wantServed {
				t.Errorf("served = %q, want %q", served, tt.wantServed)
			}
			out, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if tt.fixture != "" {
				want, _ := os.ReadFile(filepath.Join("testdata", tt.fixture))
				if !bytes.Equal(out, want) {
					t.Error("response body altered by adapter")
				}
			} else if !bytes.Contains(out, []byte("fictitious upstream error")) {
				t.Errorf("error body lost: %q", out)
			}
			if got.URL.Path != "/v1/chat/completions" || got.Header.Get("Authorization") != "Bearer "+yamlKey {
				t.Errorf("upstream saw %s with wrong auth", got.URL.Path)
			}
			if tt.stream {
				tools, usage := streamFacts(t, out)
				if (tools > 0) != tt.wantTools || usage != tt.wantUsage {
					t.Errorf("tool deltas = %d, usage = %v; want tools %v usage %v", tools, usage, tt.wantTools, tt.wantUsage)
				}
			}
			sent := decodeBody(t, &got)
			if sent["model"] != "vendor/model-fp8" {
				t.Errorf("model sent = %v", sent["model"])
			}
			if _, pinned := sent["provider"]; pinned != tt.wantPinning {
				t.Errorf("provider pin present = %v, want %v", pinned, tt.wantPinning)
			}
		})
	}
}

func TestOnlyAllowlistedInboundHeadersReachProvider(t *testing.T) {
	inbound := http.Header{
		"Authorization":       {"Bearer agent-secret-fictitious"},
		"Cookie":              {"session=fict"},
		"X-Api-Key":           {"agent-key-fict"},
		"Openai-Organization": {"org-fict"},
		"X-Custom":            {"v"},
		"Accept":              {"text/event-stream"},
		"User-Agent":          {"agent/1.0"},
		"Content-Type":        {"text/plain"},
	}
	adapterSet := []string{"Authorization", "Content-Type", "Accept-Encoding"}
	for _, ch := range []Channel{ChannelDirect, ChannelOpenRouter} {
		req, err := For(ch).BuildRequest(context.Background(), testEndpoint(ch), []byte(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		CopyAllowedHeaders(req, inbound)
		for k, v := range req.Header {
			if !slices.Contains(AllowedInboundHeaders(), k) && !slices.Contains(adapterSet, k) {
				t.Errorf("%s: header %q reached the provider", ch, k)
			}
			for _, s := range v {
				if strings.Contains(s, "agent-secret") {
					t.Errorf("%s: agent Authorization leaked in %q", ch, k)
				}
			}
		}
		if req.Header.Get("Authorization") != "Bearer "+yamlKey || req.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: adapter headers overridden by agent", ch)
		}
		if req.Header.Get("Accept") != "text/event-stream" || req.Header.Get("User-Agent") != "agent/1.0" {
			t.Errorf("%s: allowlisted headers not copied", ch)
		}
	}
}

// TestAttributionHeadersOnlyFromYAML: HTTP-Referer and X-Title reach the
// provider only on the openrouter channel and only with the YAML values;
// the agent's never pass (ADR 0008).
func TestAttributionHeadersOnlyFromYAML(t *testing.T) {
	inbound := http.Header{
		"Http-Referer": {"https://agent.example/fictitious"},
		"X-Title":      {"agent-title-fictitious"},
	}
	tests := []struct {
		name             string
		ch               Channel
		referer, title   string
		wantRef, wantTit string
	}{
		{"openrouter with yaml", ChannelOpenRouter, "https://yaml.example/app", "yaml-app", "https://yaml.example/app", "yaml-app"},
		{"openrouter without yaml", ChannelOpenRouter, "", "", "", ""},
		{"direct ignores yaml fields", ChannelDirect, "https://yaml.example/app", "yaml-app", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got http.Request
			srv := upstream(t, http.StatusOK, "openrouter/chat.json", &got)
			ep := testEndpoint(tt.ch)
			ep.BaseURL, ep.HTTPReferer, ep.XTitle = srv.URL, tt.referer, tt.title
			req, err := For(tt.ch).BuildRequest(context.Background(), ep, []byte(`{"model":"m"}`))
			if err != nil {
				t.Fatal(err)
			}
			CopyAllowedHeaders(req, inbound)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if r, ti := got.Header.Get("HTTP-Referer"), got.Header.Get("X-Title"); r != tt.wantRef || ti != tt.wantTit {
				t.Errorf("HTTP-Referer=%q X-Title=%q, want %q %q", r, ti, tt.wantRef, tt.wantTit)
			}
		})
	}
}

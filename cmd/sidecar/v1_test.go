package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/policy"
	"github.com/Foncifelo/sidecar-proxy/internal/proxy"
)

// The upstream below is a local fake; its key and payloads are fictitious.
func TestNewV1Handler(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer up.Close()

	tests := []struct {
		name     string
		upstream bool
		listen   string
		auth     string
		want     int
	}{
		{"no upstream answers 501", false, "127.0.0.1:8787", "", http.StatusNotImplemented},
		{"loopback needs no token", true, "127.0.0.1:8787", "", http.StatusOK},
		{"non-loopback without token", true, "0.0.0.0:8787", "", http.StatusUnauthorized},
		{"non-loopback with token", true, "0.0.0.0:8787", "Bearer tok", http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{
				Mode: config.ModeShadow, Listen: tc.listen, InboundToken: "tok",
				Limits:       config.Limits{MaxBodyMB: 1, MaxInflight: 4},
				ControlPlane: config.ControlPlane{QueueDir: t.TempDir()},
			}
			if tc.upstream {
				cfg.Upstream = config.Upstream{Provider: "fictitious", BaseURL: up.URL + "/v1", APIKey: "sk-fictitious"}
			}
			h, closeFn, err := newV1Handler(cfg, slog.New(slog.DiscardHandler), nil)
			if err != nil {
				t.Fatalf("newV1Handler: %v", err)
			}
			defer closeFn(context.Background())
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if !tc.upstream {
				// The 501 carries the X-Sidecar-* headers too (contract, guarantee 3).
				for _, k := range []string{proxy.HeaderRequestID, proxy.HeaderMode, proxy.HeaderProvider, proxy.HeaderEndpoint, proxy.HeaderDecision, proxy.HeaderFallback} {
					if rec.Header().Get(k) == "" {
						t.Errorf("501 misses header %s", k)
					}
				}
				if got := rec.Header().Get(proxy.HeaderDecision); got != proxy.ReasonNotConfigured {
					t.Errorf("decision = %q", got)
				}
			}
		})
	}
}

// Route mode is accepted and serves through the plan; without a policy
// the plan is the YAML upstream alone. Payloads are fictitious.
func TestNewV1HandlerAcceptsRoute(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"fict/served","choices":[]}`)
	}))
	defer up.Close()
	cfg := config.Config{
		Mode: config.ModeRoute, Listen: "127.0.0.1:8787",
		Limits:       config.Limits{MaxBodyMB: 1, MaxInflight: 4, FirstByteTimeout: config.NewDuration(time.Second)},
		Upstream:     config.Upstream{Provider: "fictitious", BaseURL: up.URL + "/v1", APIKey: "k"},
		ControlPlane: config.ControlPlane{QueueDir: t.TempDir()},
	}
	h, closeFn, err := newV1Handler(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("route mode rejected: %v", err)
	}
	defer closeFn(context.Background())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"fict/agent","messages":[]}`)))
	if rec.Code != http.StatusOK || rec.Header().Get(proxy.HeaderMode) != "route" ||
		rec.Header().Get(proxy.HeaderDecision) != "no_policy" || !strings.Contains(rec.Body.String(), `"model":"fict/agent"`) {
		t.Errorf("status=%d mode=%q decision=%q body=%s", rec.Code, rec.Header().Get(proxy.HeaderMode),
			rec.Header().Get(proxy.HeaderDecision), rec.Body.String())
	}
}

// NewV1Handler passes limits.stream_idle_timeout to the proxy,
// so a stalled upstream is cut with event: error. Payloads are fictitious.
func TestNewV1HandlerCutsStalledStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\n")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer up.Close()
	cfg := config.Config{
		Mode: config.ModePassthrough, Listen: "127.0.0.1:8787",
		Limits: config.Limits{
			MaxBodyMB: 1, MaxInflight: 4,
			StreamWriteTimeout: config.NewDuration(time.Second),
			StreamIdleTimeout:  config.NewDuration(200 * time.Millisecond),
		},
		Upstream: config.Upstream{Provider: "fictitious", BaseURL: up.URL + "/v1", APIKey: "sk-fictitious"},
	}
	h, closeFn, err := newV1Handler(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("newV1Handler: %v", err)
	}
	defer closeFn(context.Background())
	srv := httptest.NewServer(newAPIServer("", h, cfg.Limits).Handler)
	defer srv.Close()

	start := time.Now()
	resp, err := srv.Client().Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("stalled stream took %s", elapsed)
	}
	if !strings.Contains(string(body), "event: error\n") {
		t.Errorf("no event: error in %q", body)
	}
}

// fictPolicyDoc is a fictitious policy: model, endpoints, providers and
// prices are invented. The cheap candidate beats the current endpoint; it
// declares a context length because one that is unknown (0) never passes
// the context filter.
const fictPolicyDoc = `{"schema_version":1,"version":"p_fict_shadow","computed_at":"2026-10-29T00:00:00Z","flags":{},"models":{"fict-model":{
"aliases":{"fictitious":["m"]},"ref_usage":{"tokens_in":1000,"tokens_out":100},"route_enabled":true,
"current":[{"provider":"fictitious","raw_model":"m","endpoint_id":"fictitious/m@fp8/direct","quantization":"fp8","price":{"price_id":"1","input_per_m":1000000,"output_per_m":2000000}}],
"candidates":[{"endpoint_id":"fict-cheap/m@fp8/direct","provider":"fict-cheap","channel":"direct","provider_model_name":"m","quantization":"fp8",
"caps":{"n":1},"context_length":131072,"price":{"price_id":"2","input_per_m":100000,"output_per_m":200000},"stats":{"uptime_1d":0.999,"source":"probe"}}]}}}`

// newV1Handler hands the refreshed (here, cached) policy to the proxy,
// so the shadow decision shows in X-Sidecar-Decision and X-Sidecar-Endpoint.
func TestNewV1HandlerUsesCachedPolicy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer up.Close()
	ctl := httptest.NewServer(http.NotFoundHandler())
	ctlURL := ctl.URL
	ctl.Close() // control plane down: only the disk cache is available

	cfg := policyConfig(t, ctlURL)
	cfg.Listen = "127.0.0.1:8787"
	cfg.Limits = config.Limits{MaxBodyMB: 1, MaxInflight: 4}
	cfg.Upstream = config.Upstream{Provider: "fictitious", BaseURL: up.URL + "/v1", APIKey: "sk-fictitious"}
	cfg.Routing = config.Routing{AllowedChannels: []string{"direct"}, AllowedProviders: []string{"fict-cheap"}}
	if err := policy.SaveCache(policy.CachePath(cfg.ControlPlane.QueueDir), policy.Cached{
		ETag: `"1:p_fict_shadow"`, ValidatedAt: time.Now(), Document: []byte(fictPolicyDoc),
	}); err != nil {
		t.Fatal(err)
	}
	h, closeFn, err := newV1Handler(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("newV1Handler: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // control plane is down: close without waiting for delivery
	defer closeFn(ctx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)))
	if got := rec.Header().Get(proxy.HeaderDecision); got != "cheaper_endpoint" {
		t.Errorf("decision = %q, want cheaper_endpoint", got)
	}
	if got := rec.Header().Get(proxy.HeaderEndpoint); got != "fictitious/m@fp8/direct" {
		t.Errorf("endpoint = %q", got)
	}
}

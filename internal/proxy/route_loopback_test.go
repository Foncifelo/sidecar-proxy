package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/policy"
	"github.com/Foncifelo/sidecar-proxy/internal/transport"
)

// Every provider, endpoint, URL, key and price in this file is fictitious.
const secondEP = "fict-second/fict-model@fp8/standard/global/direct"

// loopbackYAML is a route-mode YAML with two direct providers on plain http
// loopback, as a local fake provider serves them (ADR 0011).
func loopbackYAML(upURL, firstURL, secondURL string) string {
	return "mode: route\n" +
		"upstream:\n  provider: fictitious-provider\n  base_url: " + upURL + "/v1\n  api_key: " + upstreamKey + "\n" +
		"control_plane:\n  url: http://127.0.0.1:1\n  api_key: sk_test_fictitious\n" +
		"routing:\n  allowed_channels: [direct]\n  allowed_providers: [fict-cheap, fict-second]\n" +
		"providers:\n" +
		"  fict-cheap:\n    api_key: fict-cheap-key\n    base_url: " + firstURL + "/v1\n" +
		"  fict-second:\n    api_key: fict-second-key\n    base_url: " + secondURL + "/v1\n"
}

func loadLoopbackConfig(t *testing.T, yamlText string, env map[string]string) (config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fict.yaml")
	if err := os.WriteFile(path, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }
	return config.Load(config.Options{Path: path, PathExplicit: true, LookupEnv: lookup})
}

// twoCandidatePolicy adds fict-second, dearer than fict-cheap and cheaper
// than the current endpoint, so the plan is [fict-cheap, fict-second, upstream].
func twoCandidatePolicy() policy.Policy {
	p := fictPolicy()
	m := p.Models[fictModel]
	second := m.Candidates[0]
	second.EndpointID, second.Provider, second.ProviderModelName = secondEP, "fict-second", "fict/second"
	second.Price = policy.Price{PriceID: "201", InputPerM: 700_000, OutputPerM: 1_400_000}
	m.Candidates = append(m.Candidates, second)
	p.Models = map[string]policy.Model{fictModel: m}
	return p
}

// TestRouteHTTPLoopbackFallback: with SIDECAR_ENV=dev, two fake providers on
// http://127.0.0.1 are routable; the first answers 429 and the request falls
// back to the second (ADR 0011).
func TestRouteHTTPLoopbackFallback(t *testing.T) {
	var firstHits, secondHits, upHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHits.Add(1)
		jsonServe(http.StatusTooManyRequests, `{"error":{}}`)(w, r)
	}))
	t.Cleanup(first.Close)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		jsonServe(http.StatusOK, provBody)(w, r)
	}))
	t.Cleanup(second.Close)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upHits.Add(1)
		jsonServe(http.StatusOK, provBody)(w, r)
	}))
	t.Cleanup(up.Close)
	for _, s := range []*httptest.Server{first, second} {
		if !strings.HasPrefix(s.URL, "http://127.0.0.1:") {
			t.Fatalf("fake provider not on http loopback: %s", s.URL)
		}
	}
	yamlText := loopbackYAML(up.URL, first.URL, second.URL)

	if _, err := loadLoopbackConfig(t, yamlText, map[string]string{}); err == nil {
		t.Fatal("without SIDECAR_ENV the http loopback providers must be rejected at load")
	}
	cfg, err := loadLoopbackConfig(t, yamlText, map[string]string{config.EnvVarSidecarEnv: "dev"})
	if err != nil {
		t.Fatalf("load with SIDECAR_ENV=dev: %v", err)
	}

	set, err := transport.New(transport.Config{Providers: map[string]transport.ProviderConfig{
		"fictitious-provider": {}, "fict-cheap": {}, "fict-second": {},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rt := &Route{Config: cfg, Transports: set, FirstByteTimeout: time.Second, NonStreamFirstByteTimeout: time.Second}
	a := activePolicyFor(t, twoCandidatePolicy(), cfg.Routing)
	h := newHarness(t, up.URL, func(o *Options) {
		o.Mode = config.ModeRoute
		o.Route = rt
		withPolicy(a)(o)
	})
	env := routeEnv{h: h, set: set}

	resp, body := env.post(t, routeReq, nil)
	ev := env.h.sink.wait(t)
	if resp.StatusCode != http.StatusOK || body != wantBody {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	hd := resp.Header
	if hd.Get(HeaderEndpoint) != secondEP || hd.Get(HeaderFallback) != "1" ||
		hd.Get(HeaderProvider) != "fict-second" || hd.Get(HeaderDecision) != string(policy.PlanRouted) {
		t.Errorf("headers endpoint=%q fallback=%q provider=%q decision=%q", hd.Get(HeaderEndpoint),
			hd.Get(HeaderFallback), hd.Get(HeaderProvider), hd.Get(HeaderDecision))
	}
	if got := abortReasons(ev.Attempts); strings.Join(got, ",") != abortRateLimited+"," {
		t.Errorf("abort reasons %q, want [rate_limited, \"\"]", got)
	}
	if ev.EndpointBest != candEP || ev.EndpointUsed != secondEP {
		t.Errorf("event best=%q used=%q", ev.EndpointBest, ev.EndpointUsed)
	}
	if firstHits.Load() != 1 || secondHits.Load() != 1 || upHits.Load() != 0 {
		t.Errorf("hits first=%d second=%d upstream=%d, want 1,1,0", firstHits.Load(), secondHits.Load(), upHits.Load())
	}
}

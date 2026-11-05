//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"go/version"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
)

// Everything below is fictitious: the OpenRouter stand-in, providers,
// endpoints, models, keys, prices (microdollars per million tokens) and
// latencies.
const (
	orKey       = "or-key-fictitious"
	orReferer   = "https://yaml.example/app"
	orTitle     = "yaml-app"
	orPath      = "/api/v1/chat/completions"
	provA       = "fictitious-or-a"
	provB       = "fictitious-or-b"
	epA         = "ep-fictitious-or-a"
	epB         = "ep-fictitious-or-b"
	modelA      = "fictitious/or-model-a"
	modelB      = "fictitious/or-model-b"
	servedModel = "fictitious/served-by-or" // what the fake OpenRouter answers in "model"

	agentReferer = "https://agent.example/must-not-pass"
	agentHeader  = "X-Agent-Private"

	routeFirstByteTimeout = "300ms"
	routeTTFTSlack        = "100ms"
	stallFor              = 2 * time.Second // longer than first_byte_timeout and p95 + slack

	// Both candidates go through the openrouter channel and are cheaper than
	// the current endpoint; epA is the cheapest, so the plan is
	// [epA, epB, upstream]. client_ttft_p95_ms + ttft_p95_slack = 300 ms.
	routePolicyDoc = `{
  "schema_version": 1,
  "version": "` + policyVersion + `",
  "computed_at": "2026-10-29T00:00:00Z",
  "flags": {},
  "models": {
    "` + fakeCanonical + `": {
      "aliases": {"` + fakeProvider + `": ["` + fakeModel + `"]},
      "client_ttft_p95_ms": 200,
      "client_tps_p50": 40,
      "ref_usage": {"tokens_in": 1000, "tokens_out": 200, "tokens_reasoning": 0, "tokens_cached_read": 0, "tokens_cached_write": 0},
      "route_enabled": true,
      "current": [{
        "provider": "` + fakeProvider + `", "raw_model": "` + fakeModel + `", "endpoint_id": "` + epCurrent + `",
        "quantization": "fp8",
        "price": {"price_id": "` + priceIDCurrent + `", "input_per_m": 1000000, "output_per_m": 2000000}
      }],
      "candidates": [` + routeCandA + `, ` + routeCandB + `]
    }
  }
}`
	routeCandA = `{
        "endpoint_id": "` + epA + `", "provider": "` + provA + `", "channel": "openrouter",
        "provider_model_name": "` + modelA + `", "quantization": "fp8",
        "caps": {"tools": true, "json_schema": true, "vision": false, "logprobs": false, "n": 1},
        "context_length": 131072,
        "price": {"price_id": "201", "input_per_m": 300000, "output_per_m": 600000},
        "stats": {"uptime_1d": 0.999, "p90_ttft_ms": 50, "tps_p50": 90, "source": "probe"}
      }`
	routeCandB = `{
        "endpoint_id": "` + epB + `", "provider": "` + provB + `", "channel": "openrouter",
        "provider_model_name": "` + modelB + `", "quantization": "fp8",
        "caps": {"tools": true, "json_schema": true, "vision": false, "logprobs": false, "n": 1},
        "context_length": 131072,
        "price": {"price_id": "202", "input_per_m": 500000, "output_per_m": 1000000},
        "stats": {"uptime_1d": 0.999, "p90_ttft_ms": 50, "tps_p50": 90, "source": "probe"}
      }`
)

// Chaos scenarios of the first attempt (epA). The agent selects one through
// User-Agent, one of the three agent headers that reach the provider.
const (
	chaos429      = "chaos-429"
	chaos503      = "chaos-503"
	chaosStall    = "chaos-stall"     // no headers before first_byte_timeout
	chaosSlowData = "chaos-slow-data" // headers at once, first data after p95 + slack
	chaosCut      = "chaos-cut"       // first chunk, then the connection drops
)

func orChunk(content, finish string) string {
	delta := `{}`
	if content != "" {
		delta = `{"content":"` + content + `"}`
	}
	fin := `null`
	if finish != "" {
		fin = `"` + finish + `"`
	}
	return `data: {"id":"or1","object":"chat.completion.chunk","model":"` + servedModel +
		`","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + fin + `}]}` + "\n\n"
}

// orJSON is the fake OpenRouter's answer without stream.
const orJSON = `{"id":"or1", "object":"chat.completion","model":"` + servedModel + `","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":2,"total_tokens":14}}`

// orChunks is the fake OpenRouter's stream, usage chunk included.
func orChunks() []string {
	return []string{
		orChunk("Hel", ""), orChunk("lo", ""), orChunk("", "stop"),
		`data: {"id":"or1","object":"chat.completion.chunk","model":"` + servedModel + `","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":2,"total_tokens":14}}` + "\n\n",
		chunkDone,
	}
}

// orSeen is what the fake OpenRouter observed in one request.
type orSeen struct {
	model, auth, referer, title, userAgent, acceptEncode string
	order                                                []string
	allowFallbacks                                       *bool
	includeUsage                                         bool
	agentLeak                                            bool
}

type fakeOpenRouter struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []orSeen
}

func (f *fakeOpenRouter) requests() []orSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]orSeen(nil), f.seen...)
}

// newFakeOpenRouter is an HTTPS stand-in for OpenRouter: the pinned provider
// epA misbehaves as the User-Agent says, epB always answers well.
func newFakeOpenRouter(t *testing.T) *fakeOpenRouter {
	t.Helper()
	f := &fakeOpenRouter{}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOpenRouter) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != orPath {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Provider struct {
			Order          []string `json:"order"`
			AllowFallbacks *bool    `json:"allow_fallbacks"`
		} `json:"provider"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	_ = json.Unmarshal(body, &req)
	s := orSeen{
		model: req.Model, auth: r.Header.Get("Authorization"),
		referer: r.Header.Get("HTTP-Referer"), title: r.Header.Get("X-Title"),
		userAgent: r.Header.Get("User-Agent"), acceptEncode: r.Header.Get("Accept-Encoding"),
		order: req.Provider.Order, allowFallbacks: req.Provider.AllowFallbacks,
		includeUsage: req.StreamOptions.IncludeUsage,
		agentLeak: headerHas(r.Header, agentKey) || r.Header.Get(agentHeader) != "" ||
			strings.Contains(string(body), agentKey),
	}
	f.mu.Lock()
	f.seen = append(f.seen, s)
	f.mu.Unlock()

	if slices.Equal(req.Provider.Order, []string{provA}) && misbehave(w, r, s.userAgent) {
		return
	}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, orJSON)
		return
	}
	streamOK(w, orChunks())
}

// misbehave applies the chaos scenario ua to epA; false means answer well.
func misbehave(w http.ResponseWriter, r *http.Request, ua string) bool {
	fl, _ := w.(http.Flusher)
	switch ua {
	case chaos429:
		w.Header().Set("Retry-After", "1")
		http.Error(w, `{"error":{"message":"fictitious rate limit"}}`, http.StatusTooManyRequests)
	case chaos503:
		http.Error(w, `{"error":{"message":"fictitious outage"}}`, http.StatusServiceUnavailable)
	case chaosStall:
		sleepCtx(r.Context(), stallFor)
	case chaosSlowData:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		sleepCtx(r.Context(), stallFor)
		_, _ = io.WriteString(w, orChunk("Hel", ""))
	case chaosCut:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, orChunk("Hel", ""))
		fl.Flush()
		time.Sleep(50 * time.Millisecond)
		panic(http.ErrAbortHandler) // drops the connection mid-stream
	default:
		return false
	}
	return true
}

func streamOK(w http.ResponseWriter, chunks []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for _, c := range chunks {
		_, _ = io.WriteString(w, c)
		fl.Flush()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// writeRouteConfig writes a route sidecar.yaml whose only channel is the fake
// OpenRouter, with HTTP-Referer and X-Title from the YAML.
func writeRouteConfig(t *testing.T, dir, listen, metrics, upstreamURL, orURL, controlURL string) string {
	t.Helper()
	yaml := fmt.Sprintf(`mode: route
listen: %s
metrics_listen: %s
upstream:
  provider: %s
  base_url: %s/v1
  api_key: %s
control_plane:
  url: %s
  api_key: %s
  policy_refresh: %s
  queue_dir: %s
  queue_max_mb: 16
limits:
  max_body_mb: 1
  max_inflight: 8
  first_byte_timeout: %s
  ttft_p95_slack: %s
routing:
  allowed_channels: [openrouter]
  allowed_providers: [%s, %s]
providers:
  openrouter:
    api_key: %s
    base_url: %s/api/v1
    http_referer: %s
    x_title: %s
`, listen, metrics, fakeProvider, upstreamURL, upstreamKey, controlURL, controlKey, policyRefreshIv,
		filepath.Join(dir, "queue"), routeFirstByteTimeout, routeTTFTSlack, provA, provB, orKey, orURL, orReferer, orTitle)
	path := filepath.Join(dir, "sidecar.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// trustEnv makes the sidecar trust the fake OpenRouter's test certificate.
// GODEBUG lets SSL_CERT_FILE override the platform verifier on macOS too.
func trustEnv(t *testing.T, dir string, srv *httptest.Server) []string {
	t.Helper()
	path := filepath.Join(dir, "or-ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	return []string{"SSL_CERT_FILE=" + path, "GODEBUG=x509sslcertoverrideplatform=1"}
}

type agentResp struct {
	status int
	header http.Header
	body   string
}

// routeRequest sends what a real agent would, including headers that must
// never reach a provider: its own Authorization, HTTP-Referer, X-Title and a
// private header.
func routeRequest(t *testing.T, listen, userAgent string, stream bool) agentResp {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"fictitious prompt"}]}`, fakeModel, stream)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+listen+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+agentKey)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("HTTP-Referer", agentReferer)
	req.Header.Set("X-Title", "agent-title")
	req.Header.Set(agentHeader, agentKey)
	client := &http.Client{Timeout: clientTO, Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return agentResp{status: resp.StatusCode, header: resp.Header, body: string(got)}
}

// asAgentModel is s with the provider's top-level model replaced by the
// agent's (guarantee 2); nothing else changes.
func asAgentModel(s string) string {
	return strings.ReplaceAll(s, `"model":"`+servedModel+`"`, `"model":"`+fakeModel+`"`)
}

type wantAttempt struct {
	endpoint, provider string
	status             int
	abort              string
}

type routeCase struct {
	name       string
	userAgent  string
	stream     bool
	wantBody   string // exact bytes; empty means check wantSubstr
	wantSubstr []string
	wantEP     string
	wantProv   string
	wantFB     string
	wantOrders [][]string
	attempts   []wantAttempt
	truncated  bool
}

func routeCases() []routeCase {
	// The agent did not ask for usage, so the usage chunk is stripped (guarantee 6).
	c := orChunks()
	okStream := asAgentModel(c[0] + c[1] + c[2] + c[4])
	viaB := func(abort string, status int) []wantAttempt {
		return []wantAttempt{
			{epA, provA, status, abort},
			{epB, provB, http.StatusOK, ""},
		}
	}
	both := [][]string{{provA}, {provB}}
	return []routeCase{
		{name: "429 falls back to the second endpoint", userAgent: chaos429, stream: true, wantBody: okStream,
			wantEP: epB, wantProv: provB, wantFB: "1", wantOrders: both, attempts: viaB("rate_limited", http.StatusTooManyRequests)},
		{name: "429 without stream", userAgent: chaos429, wantBody: asAgentModel(orJSON),
			wantEP: epB, wantProv: provB, wantFB: "1", wantOrders: both, attempts: viaB("rate_limited", http.StatusTooManyRequests)},
		{name: "5xx falls back", userAgent: chaos503, stream: true, wantBody: okStream,
			wantEP: epB, wantProv: provB, wantFB: "1", wantOrders: both, attempts: viaB("http_error", http.StatusServiceUnavailable)},
		{name: "no headers before first_byte_timeout", userAgent: chaosStall, stream: true, wantBody: okStream,
			wantEP: epB, wantProv: provB, wantFB: "1", wantOrders: both, attempts: viaB("first_byte_timeout", 0)},
		{name: "first token after p95 plus slack", userAgent: chaosSlowData, stream: true, wantBody: okStream,
			wantEP: epB, wantProv: provB, wantFB: "1", wantOrders: both, attempts: viaB("ttft_over_p95", http.StatusOK)},
		{name: "cut after the first byte never falls back", userAgent: chaosCut, stream: true,
			wantSubstr: []string{asAgentModel(c[0]), "event: error\n", `"upstream_stream_error"`},
			wantEP:     epA, wantProv: provA, wantFB: "0", wantOrders: [][]string{{provA}},
			attempts: []wantAttempt{{epA, provA, http.StatusOK, ""}}, truncated: true},
	}
}

// TestEndToEndRouteOpenRouter runs the sidecar binary in route mode against
// a fake OpenRouter (HTTPS) and a fake control plane. It covers guarantees 1,
// 2 and 4 of docs/contrato-proxy.md: what reaches the provider, the agent's
// model in the JSON and in every chunk, and fallback only before the first
// byte with every attempt in the event.
func TestEndToEndRouteOpenRouter(t *testing.T) {
	// Before Go 1.27 macOS ignores SSL_CERT_FILE (no x509sslcertoverrideplatform),
	// so the sidecar could not trust the fake OpenRouter's test certificate.
	if v := runtime.Version(); runtime.GOOS == "darwin" && version.IsValid(v) && version.Compare(v, "go1.27") < 0 {
		t.Skip("macOS needs Go >= 1.27 for SSL_CERT_FILE (x509sslcertoverrideplatform); got " + v)
	}
	bin := filepath.Join(t.TempDir(), "sidecar")
	buildBinary(t, "./cmd/sidecar", bin)
	for _, tc := range routeCases() {
		t.Run(tc.name, func(t *testing.T) {
			runRouteCase(t, bin, tc)
		})
	}
}

func runRouteCase(t *testing.T, bin string, tc routeCase) {
	tmp := t.TempDir()
	upstream, upstreamRequests := newFakeUpstream(t)
	or := newFakeOpenRouter(t)
	control, envelopes, served := newFakePolicyControlDoc(t, routePolicyDoc)
	listen, metrics := freeAddr(t), freeAddr(t)
	cfg := writeRouteConfig(t, tmp, listen, metrics, upstream.URL, or.srv.URL, control.URL)
	p := startSidecar(t, bin, cfg, listen, trustEnv(t, tmp, or.srv)...)

	// A 304 carries the ETag of the first 200, so the policy is active.
	select {
	case <-served.got304:
	case <-time.After(policyTimeout):
		t.Fatalf("policy not active within %s\n%s", policyTimeout, p.stderr)
	}

	got := routeRequest(t, listen, tc.userAgent, tc.stream)
	checkAgentResponse(t, tc, got)
	checkOpenRouterRequests(t, tc, or.requests())
	if n := len(upstreamRequests()); n != 0 {
		t.Errorf("upstream got %d requests, want 0: a candidate answered", n)
	}

	p.stop(t)
	for _, secret := range []string{controlKey, upstreamKey, agentKey, orKey} {
		if strings.Contains(p.stderr.String(), secret) {
			t.Error("sidecar log contains a key")
		}
	}
	checkRouteEvent(t, tc, waitEvent(t, envelopes, got.header.Get("X-Sidecar-Request-Id")))
}

func checkAgentResponse(t *testing.T, tc routeCase, got agentResp) {
	t.Helper()
	if got.status != http.StatusOK {
		t.Errorf("status %d, want 200; body %q", got.status, got.body)
	}
	for k, v := range map[string]string{
		"X-Sidecar-Mode": "route", "X-Sidecar-Endpoint": tc.wantEP,
		"X-Sidecar-Provider": tc.wantProv, "X-Sidecar-Fallback": tc.wantFB,
	} {
		if g := got.header.Get(k); g != v {
			t.Errorf("%s = %q, want %q", k, g, v)
		}
	}
	if tc.wantBody != "" && got.body != tc.wantBody {
		t.Errorf("agent body:\n got: %q\nwant: %q", got.body, tc.wantBody)
	}
	for _, s := range tc.wantSubstr {
		if !strings.Contains(got.body, s) {
			t.Errorf("agent body %q lacks %q", got.body, s)
		}
	}
	if strings.Contains(got.body, servedModel) {
		t.Errorf("agent body carries the provider's model %q", servedModel)
	}
}

func checkOpenRouterRequests(t *testing.T, tc routeCase, seen []orSeen) {
	t.Helper()
	if len(seen) != len(tc.wantOrders) {
		t.Fatalf("fake OpenRouter got %d requests, want %d", len(seen), len(tc.wantOrders))
	}
	wantModel := map[string]string{provA: modelA, provB: modelB}
	for i, s := range seen {
		if !slices.Equal(s.order, tc.wantOrders[i]) || s.allowFallbacks == nil || *s.allowFallbacks {
			t.Errorf("request %d: provider.order=%v allow_fallbacks=%v, want %v and false",
				i, s.order, s.allowFallbacks, tc.wantOrders[i])
		}
		if len(s.order) == 1 && s.model != wantModel[s.order[0]] {
			t.Errorf("request %d: model %q, want %q", i, s.model, wantModel[s.order[0]])
		}
		if s.auth != "Bearer "+orKey || s.agentLeak {
			t.Errorf("request %d: Authorization is not the YAML key or an agent secret leaked (leak=%v)", i, s.agentLeak)
		}
		if s.referer != orReferer || s.title != orTitle {
			t.Errorf("request %d: HTTP-Referer=%q X-Title=%q, want the YAML values", i, s.referer, s.title)
		}
		if s.userAgent != tc.userAgent || s.acceptEncode != "identity" || s.includeUsage != tc.stream {
			t.Errorf("request %d: User-Agent=%q Accept-Encoding=%q include_usage=%v",
				i, s.userAgent, s.acceptEncode, s.includeUsage)
		}
	}
}

func checkRouteEvent(t *testing.T, tc routeCase, ev events.Event) {
	t.Helper()
	if ev.Mode != events.Mode("route") || ev.EndpointUsed != tc.wantEP || ev.PolicyVersion != policyVersion {
		t.Errorf("event: mode=%q endpoint_used=%q policy_version=%q", ev.Mode, ev.EndpointUsed, ev.PolicyVersion)
	}
	if len(ev.Attempts) != len(tc.attempts) {
		t.Fatalf("event has %d attempts, want %d: %+v", len(ev.Attempts), len(tc.attempts), ev.Attempts)
	}
	var sum events.Micros
	for i, a := range ev.Attempts {
		w := tc.attempts[i]
		if a.EndpointID != w.endpoint || a.ProviderName != w.provider || a.Channel != "openrouter" ||
			a.Status != w.status || a.AbortReason != w.abort {
			t.Errorf("attempt %d = %+v, want %+v on channel openrouter", i, a, w)
		}
		sum += a.CostEst
	}
	if ev.CostActual != sum {
		t.Errorf("cost_actual = %d, want the sum of attempts %d", ev.CostActual, sum)
	}
	// Only ttft_over_p95 estimates the cost of an aborted attempt today.
	if tc.attempts[0].abort == "ttft_over_p95" && ev.Attempts[0].CostEst <= 0 {
		t.Errorf("ttft_over_p95 attempt cost_est = %d, want > 0", ev.Attempts[0].CostEst)
	}
	if tc.truncated != (ev.StreamState == events.StreamState("truncated")) {
		t.Errorf("stream_state = %q, truncated want %v", ev.StreamState, tc.truncated)
	}
}

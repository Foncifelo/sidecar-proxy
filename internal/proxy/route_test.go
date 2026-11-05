package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/policy"
	"github.com/Foncifelo/sidecar-proxy/internal/transport"
)

// Every provider, endpoint, URL, key, price and served-provider name in this
// file is fictitious.
const (
	routeReq = `{"model":"` + agentModel + `","messages":[]}`
	// provBody is a non-streamed answer as a provider would send it, with its
	// own model name, a nested model and odd spacing that must survive.
	provBody = `{"id":"x", "model":"fict/model","nested":{"model":"keep"},"choices":[{"index":0,"message":{"role":"assistant","content":"fictitious"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`
	wantBody = `{"id":"x", "model":"` + agentModel + `","nested":{"model":"keep"},"choices":[{"index":0,"message":{"role":"assistant","content":"fictitious"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`
	// candCost is 1000 in × 0.5 $/M + 500 out × 1 $/M; upCost uses 1 and 2 $/M.
	candCost = 1000
	upCost   = 2000
)

type routeEnv struct {
	h   harness
	set *transport.Set
}

// newRouteEnv runs a route-mode handler whose plan is [candidate, upstream].
// opts tune the handler's Options after route mode is set.
func newRouteEnv(t *testing.T, cand, up http.Handler, mutPolicy func(*policy.Policy), tune func(*transport.Config, *Route), opts ...func(*Options)) routeEnv {
	t.Helper()
	candSrv := httptest.NewTLSServer(cand)
	t.Cleanup(candSrv.Close)
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)

	p := fictPolicy()
	if mutPolicy != nil {
		mutPolicy(&p)
	}
	tc := transport.Config{Providers: map[string]transport.ProviderConfig{
		"fictitious-provider": {}, "fict-cheap": {}, config.ProviderOpenRouter: {},
	}}
	rt := &Route{Config: config.Config{
		Mode:     config.ModeRoute,
		Upstream: config.Upstream{Provider: "fictitious-provider", BaseURL: upSrv.URL + "/v1", APIKey: upstreamKey},
		Routing:  config.Routing{AllowedChannels: []string{"direct", "openrouter"}, AllowedProviders: []string{"fict-cheap"}},
		Providers: map[string]config.ProviderConfig{
			"fict-cheap": {APIKey: "fict-cheap-key", BaseURL: candSrv.URL + "/v1"},
			config.ProviderOpenRouter: {
				APIKey: "fict-or-key", BaseURL: candSrv.URL + "/v1",
				HTTPReferer: "https://yaml.example/app", XTitle: "yaml-app",
			},
		},
	}, FirstByteTimeout: time.Second}
	if tune != nil {
		tune(&tc, rt)
	}
	set, err := transport.New(tc)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg := candSrv.Client().Transport.(*http.Transport).TLSClientConfig
	for _, name := range []string{"fict-cheap", config.ProviderOpenRouter} {
		tr, _ := set.Transport(name)
		tr.TLSClientConfig = tlsCfg.Clone()
	}
	rt.Transports = set
	a := activePolicy(t, p)
	h := newHarness(t, upSrv.URL, func(o *Options) {
		o.Mode = config.ModeRoute
		o.Route = rt
		withPolicy(a)(o)
		for _, opt := range opts {
			opt(o)
		}
	})
	return routeEnv{h: h, set: set}
}

// reply is the part of the agent-side response the route tests inspect.
type reply struct {
	StatusCode int
	Header     http.Header
}

func (e routeEnv) post(t *testing.T, body string, hdr http.Header) (reply, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.h.srv.URL+chatPath, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	resp, err := e.h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return reply{StatusCode: resp.StatusCode, Header: resp.Header}, string(b)
}

// dropConn closes the connection without an HTTP answer (network error).
func dropConn(w http.ResponseWriter, _ *http.Request) {
	c, _, err := http.NewResponseController(w).Hijack()
	if err == nil {
		_ = c.Close()
	}
}

// slow sends no headers until the caller gives up. Reading the body lets the
// server notice the closed connection and cancel r.Context().
func slow(_ http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

func abortReasons(as []events.Attempt) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.AbortReason
	}
	return out
}

func TestRouteChaos(t *testing.T) {
	ok := jsonServe(http.StatusOK, provBody)
	tests := []struct {
		name         string
		cand, up     http.HandlerFunc
		fbt          time.Duration
		wantStatus   int
		wantEndpoint string
		wantFallback string
		wantAborts   []string
		wantBody     string
		wantCost     events.Micros
	}{
		{"candidate ok", ok, ok, time.Second, 200, candEP, "0", []string{""}, wantBody, candCost},
		{"429 then ok", jsonServe(429, `{"error":{}}`), ok, time.Second, 200, curEndpoint, "1", []string{abortRateLimited, ""}, wantBody, upCost},
		{"5xx then ok", jsonServe(503, `{"error":{}}`), ok, time.Second, 200, curEndpoint, "1", []string{abortHTTPError, ""}, wantBody, upCost},
		{"network error then ok", dropConn, ok, time.Second, 200, curEndpoint, "1", []string{abortHTTPError, ""}, wantBody, upCost},
		{"timeout then ok", slow, ok, 50 * time.Millisecond, 200, curEndpoint, "1", []string{abortFirstByteTimeout, ""}, wantBody, upCost + routeReqTimeoutCost},
		{
			"all fail: last error to the agent", jsonServe(429, `{"error":{}}`),
			jsonServe(502, `{"error":{"type":"fict"},"model":"fict/up"}`), time.Second,
			502, curEndpoint, "1", []string{abortRateLimited, ""}, `{"error":{"type":"fict"},"model":"` + agentModel + `"}`, 0,
		},
		{"no answer at all", dropConn, dropConn, time.Second, 502, curEndpoint, "1", []string{abortHTTPError, abortHTTPError}, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// routeReq has no stream: its timeout is NonStreamFirstByteTimeout.
			env := newRouteEnv(t, tt.cand, tt.up, nil, func(_ *transport.Config, r *Route) { r.NonStreamFirstByteTimeout = tt.fbt })
			resp, body := env.post(t, routeReq, nil)
			ev := env.h.sink.wait(t)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			hd := resp.Header
			if hd.Get(HeaderEndpoint) != tt.wantEndpoint || hd.Get(HeaderFallback) != tt.wantFallback ||
				hd.Get(HeaderMode) != "route" || hd.Get(HeaderDecision) != string(policy.PlanRouted) {
				t.Errorf("headers endpoint=%q fallback=%q mode=%q decision=%q", hd.Get(HeaderEndpoint),
					hd.Get(HeaderFallback), hd.Get(HeaderMode), hd.Get(HeaderDecision))
			}
			wantProv := "fictitious-provider"
			if tt.wantEndpoint == candEP {
				wantProv = "fict-cheap"
			}
			if hd.Get(HeaderProvider) != wantProv {
				t.Errorf("provider header %q, want %q", hd.Get(HeaderProvider), wantProv)
			}
			if tt.wantBody != "" && body != tt.wantBody {
				t.Errorf("body\n got %s\nwant %s", body, tt.wantBody)
			}
			if got := abortReasons(ev.Attempts); strings.Join(got, ",") != strings.Join(tt.wantAborts, ",") {
				t.Errorf("abort reasons %q, want %q", got, tt.wantAborts)
			}
			var sum events.Micros
			for _, a := range ev.Attempts {
				sum += a.CostEst
			}
			if ev.CostActual != sum || ev.CostActual != tt.wantCost {
				t.Errorf("cost_actual %d, attempts sum %d, want %d", ev.CostActual, sum, tt.wantCost)
			}
			if ev.Mode != events.ModeRoute || !ev.Billable || ev.EndpointBest != candEP || ev.Status != tt.wantStatus {
				t.Errorf("event mode=%q billable=%v best=%q status=%d", ev.Mode, ev.Billable, ev.EndpointBest, ev.Status)
			}
		})
	}
}

func TestRouteOpenRouterPinningAndHeaders(t *testing.T) {
	var gotRef, gotTitle, gotAuth string
	var gotBody []byte
	cand := func(w http.ResponseWriter, r *http.Request) {
		gotRef, gotTitle, gotAuth = r.Header.Get("HTTP-Referer"), r.Header.Get("X-Title"), r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		jsonServe(200, `{"id":"x","model":"fict/model","provider":"FictServed","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":500}}`)(w, r)
	}
	env := newRouteEnv(t, http.HandlerFunc(cand), jsonServe(200, provBody), func(p *policy.Policy) {
		m := p.Models[fictModel]
		m.Candidates[0].Channel = policy.ChannelOpenRouter
		p.Models[fictModel] = m
	}, nil)
	resp, body := env.post(t, routeReq, http.Header{
		"Http-Referer": {"https://agent.example/fict"}, "X-Title": {"agent"}, "Authorization": {"Bearer agent-secret"},
	})
	ev := env.h.sink.wait(t)
	if resp.StatusCode != 200 || !strings.Contains(body, `"model":"`+agentModel+`"`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if gotRef != "https://yaml.example/app" || gotTitle != "yaml-app" || gotAuth != "Bearer fict-or-key" {
		t.Errorf("referer=%q title=%q auth=%q", gotRef, gotTitle, gotAuth)
	}
	if !strings.Contains(string(gotBody), `"provider":{"order":["fict-cheap"],"allow_fallbacks":false}`) ||
		!strings.Contains(string(gotBody), `"model":"fict/model"`) {
		t.Errorf("upstream body %s", gotBody)
	}
	if ev.ProviderServed != "FictServed" || ev.EndpointUsed != candEP || ev.Attempts[0].Channel != "openrouter" {
		t.Errorf("provider_served=%q endpoint_used=%q attempts=%+v", ev.ProviderServed, ev.EndpointUsed, ev.Attempts)
	}
}

// TestRouteOpenRouterUpstreamBodyIntact is guarantee 1 of the proxy
// contract: with upstream.provider openrouter, the upstream attempt is the
// agent's path and its body arrives byte for byte, including the agent's own
// provider preferences (no pin, no model rewrite). "deepinfra" is only an
// opaque value the agent sends; nothing here calls or prices it.
func TestRouteOpenRouterUpstreamBodyIntact(t *testing.T) {
	const agentBody = `{"model":"` + agentModel + `", "messages":[],"provider":{"order":["deepinfra"]}}`
	var gotBody []byte
	up := func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		jsonServe(200, provBody)(w, r)
	}
	var candCalled bool
	cand := func(w http.ResponseWriter, r *http.Request) {
		candCalled = true
		jsonServe(500, `{}`)(w, r)
	}
	env := newRouteEnv(t, http.HandlerFunc(cand), http.HandlerFunc(up), nil, func(_ *transport.Config, r *Route) {
		r.Config.Upstream.Provider = config.ProviderOpenRouter
	})
	resp, _ := env.post(t, agentBody, nil)
	ev := env.h.sink.wait(t)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if string(gotBody) != agentBody {
		t.Errorf("upstream body\n got %s\nwant %s", gotBody, agentBody)
	}
	if candCalled || len(ev.Attempts) != 1 || ev.Attempts[0].ProviderName != config.ProviderOpenRouter {
		t.Errorf("candidate called=%v attempts=%+v", candCalled, ev.Attempts)
	}
}

// stallJSON sends headers and the start of a JSON body, then goes silent
// until the request is cancelled (or a long while passes).
func stallJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"id":"x",`)
	_ = http.NewResponseController(w).Flush()
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

// A provider that sends headers and then stalls mid-body is cut at
// StreamIdleTimeout: the request does not hang, and the attempt's bulkhead
// slot is released.
func TestRouteProviderStallsMidBody(t *testing.T) {
	const idle = 100 * time.Millisecond
	const bound = 3 * time.Second // well under stallJSON's 10s
	tests := []struct {
		name       string
		cand, up   http.HandlerFunc
		wantStatus int
		wantAborts []string
		wantError  string
	}{
		{"candidate stalls, upstream answers", stallJSON, jsonServe(200, provBody), 200, []string{abortHTTPError, ""}, ""},
		{"last attempt stalls", jsonServe(429, `{"error":{}}`), stallJSON, 200, []string{abortRateLimited, ""}, "upstream_body_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newRouteEnv(t, tt.cand, tt.up, nil, func(c *transport.Config, _ *Route) {
				c.Endpoints = map[string]transport.EndpointConfig{candEP: {MaxInFlight: 1}}
			}, func(o *Options) { o.StreamIdleTimeout = idle })
			start := time.Now()
			resp, _ := env.post(t, routeReq, nil)
			ev := env.h.sink.wait(t)
			if elapsed := time.Since(start); elapsed > bound {
				t.Fatalf("request took %s, want about %s", elapsed, idle)
			}
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if got := abortReasons(ev.Attempts); strings.Join(got, ",") != strings.Join(tt.wantAborts, ",") {
				t.Errorf("abort reasons %q, want %q", got, tt.wantAborts)
			}
			if ev.ErrorType != tt.wantError {
				t.Errorf("error_type %q, want %q", ev.ErrorType, tt.wantError)
			}
			ctx, cancel := context.WithTimeout(t.Context(), bound)
			defer cancel()
			release, err := env.set.Endpoint(candEP).Bulkhead.Acquire(ctx)
			if err != nil {
				t.Fatalf("candidate bulkhead slot still held: %v", err)
			}
			release()
		})
	}
}

// Vision and the estimated context (body bytes / 4 + requested output) are
// requirements: a candidate that lacks them is filtered out and the request
// stays on the upstream (docs/politica.md, "Filtro en el sidecar").
func TestRouteFiltersVisionAndContext(t *testing.T) {
	const (
		textReq  = `{"model":"` + agentModel + `","messages":[{"role":"user","content":"fictitious"}]}`
		imageReq = `{"model":"` + agentModel + `","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://img.example/fict.png"}}]}]}`
	)
	bigOut := `{"model":"` + agentModel + `","max_tokens":200000,"messages":[]}`
	bigCompletion := `{"model":"` + agentModel + `","max_tokens":10,"max_completion_tokens":131072,"messages":[]}`
	tests := []struct {
		name     string
		body     string
		vision   bool
		ctxLen   int64
		wantEP   string
		wantWhy  policy.PlanReason
		wantCand bool
	}{
		{"text request", textReq, false, 131072, candEP, policy.PlanRouted, true},
		{"image, candidate without vision", imageReq, false, 131072, curEndpoint, policy.PlanNoCandidate, false},
		{"image, candidate with vision", imageReq, true, 131072, candEP, policy.PlanRouted, true},
		{"max_tokens over the context", bigOut, false, 131072, curEndpoint, policy.PlanNoCandidate, false},
		{"max_completion_tokens counts over max_tokens", bigCompletion, false, 131072, curEndpoint, policy.PlanNoCandidate, false},
		{"body alone over the context", textReq, false, int64(len(textReq))/4 - 1, curEndpoint, policy.PlanNoCandidate, false},
		{"unknown context length", textReq, false, 0, curEndpoint, policy.PlanNoCandidate, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var candCalled bool
			cand := func(w http.ResponseWriter, r *http.Request) {
				candCalled = true
				jsonServe(200, provBody)(w, r)
			}
			env := newRouteEnv(t, http.HandlerFunc(cand), jsonServe(200, provBody), func(p *policy.Policy) {
				c := &p.Models[fictModel].Candidates[0]
				c.Caps.Tools, c.Caps.Vision, c.ContextLength = false, tt.vision, tt.ctxLen
			}, nil)
			resp, _ := env.post(t, tt.body, nil)
			_ = env.h.sink.wait(t)
			if resp.StatusCode != 200 || resp.Header.Get(HeaderEndpoint) != tt.wantEP ||
				resp.Header.Get(HeaderDecision) != string(tt.wantWhy) || candCalled != tt.wantCand {
				t.Errorf("status=%d endpoint=%q decision=%q candidate called=%v; want %q %q %v", resp.StatusCode,
					resp.Header.Get(HeaderEndpoint), resp.Header.Get(HeaderDecision), candCalled, tt.wantEP, tt.wantWhy, tt.wantCand)
			}
		})
	}
}

func TestRouteBreakerAndBulkhead(t *testing.T) {
	t.Run("open breaker leaves the candidate out", func(t *testing.T) {
		env := newRouteEnv(t, jsonServe(500, `{}`), jsonServe(200, provBody), nil, func(c *transport.Config, _ *Route) {
			c.EndpointDefaults.FailureThreshold = 1
		})
		_, _ = env.post(t, routeReq, nil)
		first := env.h.sink.wait(t)
		if len(first.Attempts) != 2 {
			t.Fatalf("first request attempts %+v", first.Attempts)
		}
		resp, _ := env.post(t, routeReq, nil)
		ev := env.h.sink.wait(t)
		if resp.Header.Get(HeaderFallback) != "0" || len(ev.Attempts) != 1 || ev.Attempts[0].EndpointID != curEndpoint {
			t.Errorf("fallback=%q attempts=%+v", resp.Header.Get(HeaderFallback), ev.Attempts)
		}
		if resp.Header.Get(HeaderDecision) != string(policy.PlanNoCandidate) {
			t.Errorf("decision %q", resp.Header.Get(HeaderDecision))
		}
	})
	t.Run("full bulkhead falls back at first_byte_timeout", func(t *testing.T) {
		env := newRouteEnv(t, jsonServe(200, provBody), jsonServe(200, provBody), nil, func(c *transport.Config, r *Route) {
			c.Endpoints = map[string]transport.EndpointConfig{candEP: {MaxInFlight: 1}}
			r.NonStreamFirstByteTimeout = 50 * time.Millisecond // routeReq has no stream
		})
		release, err := env.set.Endpoint(candEP).Bulkhead.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		resp, _ := env.post(t, routeReq, nil)
		ev := env.h.sink.wait(t)
		if resp.Header.Get(HeaderEndpoint) != curEndpoint || abortReasons(ev.Attempts)[0] != abortFirstByteTimeout {
			t.Errorf("endpoint=%q attempts=%+v", resp.Header.Get(HeaderEndpoint), ev.Attempts)
		}
		// The request never left the sidecar: nothing the provider can bill.
		if ev.Attempts[0].CostEst != 0 {
			t.Errorf("bulkhead-held attempt cost_est = %d, want 0", ev.Attempts[0].CostEst)
		}
	})
}

// TestRouteNonChatNotRouted: streamed chat is routed too
// (route_stream_test.go); every other route still goes to the YAML upstream
// unchanged.
func TestRouteNonChatNotRouted(t *testing.T) {
	var gotModel bool
	up := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotModel = r.URL.Path == "/v1/embeddings" && strings.Contains(string(b), `"model":"`+agentModel+`"`)
		jsonServe(http.StatusOK, `{"model":"fict/model","data":[]}`)(w, r)
	}
	env := newRouteEnv(t, jsonServe(200, provBody), http.HandlerFunc(up), nil, nil)
	req, err := http.NewRequest(http.MethodPost, env.h.srv.URL+"/v1/embeddings", strings.NewReader(`{"model":"`+agentModel+`","input":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := env.h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	ev := env.h.sink.wait(t)
	if !gotModel || resp.Header.Get(HeaderDecision) != ReasonNotRouted || resp.Header.Get(HeaderEndpoint) != UpstreamEndpointID {
		t.Errorf("model kept=%v decision=%q endpoint=%q", gotModel, resp.Header.Get(HeaderDecision), resp.Header.Get(HeaderEndpoint))
	}
	if !strings.Contains(string(b), `"model":"fict/model"`) || ev.Mode != events.ModeRoute || len(ev.Attempts) != 1 {
		t.Errorf("body %q mode=%q attempts=%d", b, ev.Mode, len(ev.Attempts))
	}
}

func TestRewriteModel(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"top-level only", provBody, wantBody},
		{"no model is not added", `{"id":"x","nested":{"model":"keep"}}`, `{"id":"x","nested":{"model":"keep"}}`},
		{"not an object", `[{"model":"x"}]`, `[{"model":"x"}]`},
		{"invalid json", `{"model":`, `{"model":`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(rewriteModel([]byte(tt.in), agentModel)); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

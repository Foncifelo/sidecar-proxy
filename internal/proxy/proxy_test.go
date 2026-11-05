package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
)

// All payloads below are fictitious: model names, ids and token counts are
// made up for the tests and do not come from any real provider.

const (
	upstreamKey = "sk-upstream-fictitious"
	agentKey    = "sk-agent-must-not-leak"
	agentModel  = "fictitious/model-x"
)

const (
	chunkTool1 = `data: {"id":"c1","object":"chat.completion.chunk","model":"fictitious/model-x","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}` + "\n\n"
	chunkTool2 = `data: {"id":"c1","object":"chat.completion.chunk","model":"fictitious/model-x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Lima\"}"}}]},"finish_reason":null}]}` + "\n\n"
	chunkStop  = `data: {"id":"c1","object":"chat.completion.chunk","model":"fictitious/model-x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"
	chunkUsage = `data: {"id":"c1","object":"chat.completion.chunk","model":"fictitious/model-x","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30,"prompt_tokens_details":{"cached_tokens":5},"completion_tokens_details":{"reasoning_tokens":4}}}` + "\n\n"
	chunkDone  = "data: [DONE]\n\n"
	keepalive  = ": keepalive\n\n"
)

// fullStream is what a well-behaved upstream sends, with a comment block.
var fullStream = []string{keepalive, chunkTool1, keepalive, chunkTool2, chunkStop, chunkUsage, chunkDone}

// received is what the fake upstream saw.
type received struct {
	Method, Path, Query string
	Body                []byte
	Header              http.Header
}

type fakeUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []received
}

func (f *fakeUpstream) last(t *testing.T) received {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("upstream got no request")
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// newUpstream records every request and then runs serve.
func newUpstream(t testing.TB, serve http.HandlerFunc) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, received{r.Method, r.URL.Path, r.URL.RawQuery, b, r.Header.Clone()})
		f.mu.Unlock()
		serve(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

// sseServe writes blocks with a flush after each; cut aborts the connection
// after the given number of blocks (0 means no cut).
func sseServe(blocks []string, cut int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "99999") // must never reach the agent
		w.Header().Del("Content-Length")
		rc := http.NewResponseController(w)
		for i, b := range blocks {
			if cut > 0 && i == cut {
				panic(http.ErrAbortHandler)
			}
			_, _ = io.WriteString(w, b)
			_ = rc.Flush()
		}
	}
}

func jsonServe(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

type memSink struct{ ch chan events.Event }

func (m *memSink) Append(ev events.Event) error { m.ch <- ev; return nil }

func (m *memSink) wait(t *testing.T) events.Event {
	t.Helper()
	select {
	case ev := <-m.ch:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event emitted")
		return events.Event{}
	}
}

func (m *memSink) none(t *testing.T) {
	t.Helper()
	select {
	case ev := <-m.ch:
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

type harness struct {
	srv  *httptest.Server
	sink *memSink
}

func newHarness(t testing.TB, upstreamURL string, mutate func(*Options)) harness {
	t.Helper()
	return newHarnessServer(t, upstreamURL, mutate, nil)
}

// newHarnessServer is newHarness with a hook to tune the agent-facing
// http.Server (timeouts) before it starts.
func newHarnessServer(t testing.TB, upstreamURL string, mutate func(*Options), tune func(*http.Server)) harness {
	t.Helper()
	sink := &memSink{ch: make(chan events.Event, 16)}
	o := Options{
		Mode:         config.ModeShadow,
		Upstream:     Upstream{Provider: "fictitious-provider", BaseURL: upstreamURL + "/v1", APIKey: upstreamKey},
		Client:       &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 5 * time.Second}},
		Sink:         sink,
		MaxBodyBytes: 1 << 20,
		MaxInflight:  8,
		Version:      "test",
	}
	if mutate != nil {
		mutate(&o)
	}
	h, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewUnstartedServer(h)
	if tune != nil {
		tune(srv.Config)
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return harness{srv: srv, sink: sink}
}

// result is the part of the agent-side response the tests inspect.
type result struct {
	StatusCode       int
	Header           http.Header
	TransferEncoding []string
	ContentLength    int64
}

func (hs harness) do(t *testing.T, method, path, body string, hdr map[string]string) (result, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, hs.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+agentKey)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := hs.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return result{resp.StatusCode, resp.Header, resp.TransferEncoding, resp.ContentLength}, b
}

func chatBody(stream, includeUsage bool) string {
	b := `{"model":"` + agentModel + `","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather"}}]`
	if stream {
		b += `,"stream":true`
	}
	if includeUsage {
		b += `,"stream_options":{"include_usage":true}`
	}
	return b + "}"
}

// rawChatBody has spacing, a newline, keys out of alphabetical order and <&>
// in the prompt: the upstream must receive exactly these bytes (guarantee 1).
const rawChatBody = `{ "stream":false, "tools":[] ,` + "\n" +
	` "messages":[{"role":"user","content":"a <&> b  c"}],"model":"` + agentModel + `" }`

// streamBody is a streamed chat body in the same spirit as rawChatBody.
func streamBody(includeUsage bool) string {
	b := `{"stream" : true, "tools":[{"type":"function","function":{"name":"get_weather"}}],` + "\n" +
		` "messages":[{"role":"user","content":"a <&> b  c"}], "model":"` + agentModel + `"`
	if includeUsage {
		b += `, "stream_options": {"include_usage":true}`
	}
	return b + "}"
}

func assertSidecarHeaders(t *testing.T, resp result, mode, decision string) {
	t.Helper()
	for _, k := range []string{HeaderRequestID, HeaderMode, HeaderProvider, HeaderEndpoint, HeaderDecision, HeaderFallback} {
		if resp.Header.Get(k) == "" {
			t.Errorf("missing header %s", k)
		}
	}
	if len(resp.Header.Get(HeaderRequestID)) != 26 {
		t.Errorf("request id %q is not a ULID", resp.Header.Get(HeaderRequestID))
	}
	if got := resp.Header.Get(HeaderMode); got != mode {
		t.Errorf("mode header = %q, want %q", got, mode)
	}
	if got := resp.Header.Get(HeaderDecision); got != decision {
		t.Errorf("decision header = %q, want %q", got, decision)
	}
	if resp.Header.Get(HeaderFallback) != "0" || resp.Header.Get(HeaderProvider) != "fictitious-provider" ||
		resp.Header.Get(HeaderEndpoint) != UpstreamEndpointID {
		t.Errorf("provider/endpoint/fallback headers = %q/%q/%q", resp.Header.Get(HeaderProvider),
			resp.Header.Get(HeaderEndpoint), resp.Header.Get(HeaderFallback))
	}
}

// Buffered body with limit, generic adapter, model untouched,
// upstream key, never the agent's Authorization.
func TestForwardsBodyAndAuth(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantHit    bool
	}{
		{"chat forwarded", chatBody(false, false), http.StatusOK, true},
		{"chat bytes preserved", rawChatBody, http.StatusOK, true},
		{"body over limit", `{"model":"x","messages":"` + strings.Repeat("a", 400) + `"}`, http.StatusRequestEntityTooLarge, false},
		{"non-json body passes raw", `not json`, http.StatusOK, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t, jsonServe(http.StatusOK, `{"choices":[]}`))
			hs := newHarness(t, up.URL, func(o *Options) { o.MaxBodyBytes = 256 })
			resp, _ := hs.do(t, http.MethodPost, chatPath, tc.body, nil)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			ev := hs.sink.wait(t)
			if !tc.wantHit {
				if up.count() != 0 || ev.Status != http.StatusRequestEntityTooLarge || len(ev.Attempts) != 0 {
					t.Fatalf("over-limit body reached upstream or bad event: hits=%d ev=%+v", up.count(), ev)
				}
				return
			}
			got := up.last(t)
			if got.Path != "/v1/chat/completions" {
				t.Errorf("path = %q", got.Path)
			}
			if a := got.Header.Get("Authorization"); a != "Bearer "+upstreamKey {
				t.Errorf("upstream Authorization = %q, want the YAML key", a)
			}
			for _, vs := range got.Header {
				for _, v := range vs {
					if strings.Contains(v, agentKey) {
						t.Fatal("agent Authorization reached the upstream")
					}
				}
			}
			// Shadow without stream: nothing may change, byte for byte.
			if string(got.Body) != tc.body {
				t.Errorf("upstream body changed:\n got %q\nwant %q", got.Body, tc.body)
			}
		})
	}
}

// SSE copy with sse.Tee, usage chunk stripped unless asked,
// ttft and total measured, usage read, always chunked; plus the cut.
func TestStream(t *testing.T) {
	tests := []struct {
		name         string
		includeUsage bool
		cut          int
		wantBody     string
		wantState    events.StreamState
	}{
		{
			name:      "usage stripped when not requested",
			wantBody:  keepalive + chunkTool1 + keepalive + chunkTool2 + chunkStop + chunkDone,
			wantState: events.StreamComplete,
		},
		{
			name:         "usage kept when requested",
			includeUsage: true,
			wantBody:     strings.Join(fullStream, ""),
			wantState:    events.StreamComplete,
		},
		{
			name:      "cut mid-stream",
			cut:       3,
			wantBody:  keepalive + chunkTool1 + keepalive,
			wantState: events.StreamTruncated,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t, sseServe(fullStream, tc.cut))
			hs := newHarness(t, up.URL, nil)
			sent := streamBody(tc.includeUsage)
			resp, body := hs.do(t, http.MethodPost, chatPath, sent, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			if len(resp.TransferEncoding) == 0 || resp.TransferEncoding[0] != "chunked" || resp.ContentLength != -1 {
				t.Errorf("response not chunked: te=%v cl=%d", resp.TransferEncoding, resp.ContentLength)
			}
			id := resp.Header.Get(HeaderRequestID)
			want := tc.wantBody
			if tc.wantState == events.StreamTruncated {
				want += "event: error\ndata: {\"error\":{\"type\":\"upstream_stream_error\",\"request_id\":\"" + id + "\"}}\n\n"
			}
			if string(body) != want {
				t.Errorf("agent body:\n got %q\nwant %q", body, want)
			}
			// The only rewrite allowed: include_usage forced to true. When the
			// agent already asked for it, the bytes are unchanged.
			wantSent := sent
			if !tc.includeUsage {
				wantSent = strings.TrimSuffix(sent, "}") + `,"stream_options":{"include_usage":true}}`
			}
			if got := string(up.last(t).Body); got != wantSent {
				t.Errorf("upstream body:\n got %q\nwant %q", got, wantSent)
			}
			ev := hs.sink.wait(t)
			if ev.StreamState != tc.wantState || ev.ID != id {
				t.Errorf("event state=%q id=%q, want %q %q", ev.StreamState, ev.ID, tc.wantState, id)
			}
			if ev.TotalMs < ev.TTFTMs || ev.TTFTMs < 0 {
				t.Errorf("ttft=%d total=%d", ev.TTFTMs, ev.TotalMs)
			}
			if tc.cut > 0 {
				if ev.UsageSource != events.UsageNone || ev.ErrorType != "upstream_stream_error" {
					t.Errorf("cut event usage_source=%q error=%q", ev.UsageSource, ev.ErrorType)
				}
				return
			}
			// Disjoint buckets: In = 20-5, Out = 10-4.
			if ev.UsageSource != events.UsageProvider || ev.TokensIn != 15 || ev.TokensCachedRead != 5 ||
				ev.TokensOut != 6 || ev.TokensReasoning != 4 || ev.FinishReason != "tool_calls" {
				t.Errorf("usage not read: %+v", ev)
			}
		})
	}
}

// The six X-Sidecar-* headers on every answer, errors included.
func TestSidecarHeaders(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	tests := []struct {
		name       string
		upstream   string // "" means a working upstream
		token      string
		send       string
		body       string
		wantStatus int
		decision   string
	}{
		{"ok", "", "", "", chatBody(false, false), http.StatusOK, ReasonNoPolicy},
		{"upstream http error", "500", "", "", chatBody(false, false), http.StatusInternalServerError, ReasonNoPolicy},
		{"unauthorized", "", "tok", "", chatBody(false, false), http.StatusUnauthorized, ReasonUnauthorized},
		{"too large", "", "", "", strings.Repeat("x", 300), http.StatusRequestEntityTooLarge, ReasonBodyTooLarge},
		{"upstream down", closedURL, "", "", chatBody(false, false), http.StatusBadGateway, ReasonUpstreamErr},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status := http.StatusOK
			if tc.upstream == "500" {
				status = http.StatusInternalServerError
			}
			up := newUpstream(t, jsonServe(status, `{"error":{"message":"fictitious"}}`))
			url := up.URL
			if strings.HasPrefix(tc.upstream, "http") {
				url = tc.upstream
			}
			hs := newHarness(t, url, func(o *Options) {
				o.MaxBodyBytes = 256
				o.InboundToken = "tok"
				if tc.token == "" {
					o.InboundToken = ""
				}
			})
			resp, _ := hs.do(t, http.MethodPost, chatPath, tc.body, nil)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			assertSidecarHeaders(t, resp, "shadow", tc.decision)
			if tc.decision == ReasonUnauthorized {
				hs.sink.none(t) // rejected before any request state exists
				return
			}
			// decision_reason must match the header the agent saw.
			if ev := hs.sink.wait(t); ev.DecisionReason != resp.Header.Get(HeaderDecision) {
				t.Errorf("event decision_reason = %q, header = %q", ev.DecisionReason, resp.Header.Get(HeaderDecision))
			}
		})
	}
}

// The shadow event.
func TestEventShadow(t *testing.T) {
	up := newUpstream(t, jsonServe(http.StatusOK,
		`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"fictitious"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	hs := newHarness(t, up.URL, nil)
	resp, _ := hs.do(t, http.MethodPost, chatPath, chatBody(false, false), nil)
	ev := hs.sink.wait(t)
	checks := []struct {
		name string
		ok   bool
	}{
		{"id matches header", ev.ID == resp.Header.Get(HeaderRequestID)},
		{"mode shadow", ev.Mode == events.ModeShadow},
		{"kind chat", ev.Kind == events.KindChat},
		{"provider_served", ev.ProviderServed == "fictitious-provider"},
		{"endpoint_used", ev.EndpointUsed == UpstreamEndpointID},
		{"raw_model", ev.RawModel == agentModel},
		{"requirements", len(ev.Requirements) == 1 && ev.Requirements[0] == "tools"},
		{"usage", ev.TokensIn == 7 && ev.TokensOut == 3 && ev.UsageSource == events.UsageProvider},
		{"stream_state none", ev.StreamState == events.StreamNone && !ev.Stream},
		{"one attempt", len(ev.Attempts) == 1 && ev.Attempts[0].Status == http.StatusOK && ev.Attempts[0].EndpointID == UpstreamEndpointID},
		{"not billable", !ev.Billable},
		{"no cost", ev.CostActual == 0 && ev.CostBest == 0},
		{"decision no_policy", ev.DecisionReason == ReasonNoPolicy},
		{"finish", ev.FinishReason == "stop"},
		{"status", ev.Status == http.StatusOK},
		{"version", ev.SidecarVersion == "test"},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s: event %+v", c.name, ev)
		}
	}
}

// Other routes pass intact with their endpoint_kind.
func TestPassthroughRoutes(t *testing.T) {
	tests := []struct {
		method, path, query, body string
		kind                      events.EndpointKind
		wantIn                    int64
	}{
		{http.MethodPost, "/v1/responses", "", `{"model":"m","input":"hi"}`, events.KindResponses, 0},
		{http.MethodPost, "/v1/embeddings", "", `{"model":"m",  "input":["a"]}`, events.KindEmbeddings, 9},
		{http.MethodGet, "/v1/models", "page=2", "", events.KindPassthrough, 0},
		{http.MethodDelete, "/v1/files/abc", "x=1&y=2", "", events.KindPassthrough, 0},
		{http.MethodGet, "/v1/chat/completions", "", "", events.KindPassthrough, 0},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			up := newUpstream(t, jsonServe(http.StatusOK, `{"object":"list","usage":{"prompt_tokens":9,"total_tokens":9}}`))
			hs := newHarness(t, up.URL, nil)
			path := tc.path
			if tc.query != "" {
				path += "?" + tc.query
			}
			resp, body := hs.do(t, tc.method, path, tc.body, nil)
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"object":"list"`) {
				t.Fatalf("status=%d body=%q", resp.StatusCode, body)
			}
			got := up.last(t)
			if got.Method != tc.method || got.Path != tc.path || got.Query != tc.query || string(got.Body) != tc.body {
				t.Errorf("upstream saw %s %s?%s %q", got.Method, got.Path, got.Query, got.Body)
			}
			if got.Header.Get("Authorization") != "Bearer "+upstreamKey {
				t.Errorf("upstream auth = %q", got.Header.Get("Authorization"))
			}
			ev := hs.sink.wait(t)
			if ev.Kind != tc.kind || ev.TokensIn != tc.wantIn {
				t.Errorf("event kind=%q tokens_in=%d, want %q %d", ev.Kind, ev.TokensIn, tc.kind, tc.wantIn)
			}
			assertSidecarHeaders(t, resp, "shadow", ReasonNoPolicy)
		})
	}
}

// Inbound token.
func TestInboundToken(t *testing.T) {
	tests := []struct {
		name   string
		auth   string
		status int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong", "Bearer nope", http.StatusUnauthorized},
		{"right", "Bearer s3cret", http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t, jsonServe(http.StatusOK, `{}`))
			hs := newHarness(t, up.URL, func(o *Options) { o.InboundToken = "s3cret" })
			resp, _ := hs.do(t, http.MethodPost, chatPath, chatBody(false, false), map[string]string{"Authorization": tc.auth})
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if tc.status == http.StatusUnauthorized {
				if up.count() != 0 {
					t.Error("unauthorized request reached upstream")
				}
				hs.sink.none(t)
				return
			}
			if up.last(t).Header.Get("Authorization") != "Bearer "+upstreamKey {
				t.Error("inbound token forwarded instead of upstream key")
			}
		})
	}
}

// In-flight limit answers 503 with Retry-After.
func TestInflightLimit(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	hs := newHarness(t, up.URL, func(o *Options) { o.MaxInflight = 1 })
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest(http.MethodPost, hs.srv.URL+chatPath, strings.NewReader(chatBody(false, false)))
		if resp, err := hs.srv.Client().Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	resp, _ := hs.do(t, http.MethodPost, chatPath, chatBody(false, false), nil)
	close(release)
	<-done
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status=%d retry-after=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	assertSidecarHeaders(t, resp, "shadow", ReasonOverloaded)
	// Two events (the held request and the rejected one), in either order.
	for range 2 {
		if ev := hs.sink.wait(t); ev.Status == http.StatusServiceUnavailable {
			if ev.DecisionReason != ReasonOverloaded {
				t.Errorf("overloaded event decision_reason = %q", ev.DecisionReason)
			}
			return
		}
	}
	t.Error("no event for the rejected request")
}

func TestPassthroughModeDoesNotMeasure(t *testing.T) {
	up := newUpstream(t, sseServe(fullStream, 0))
	hs := newHarness(t, up.URL, func(o *Options) { o.Mode = config.ModePassthrough; o.Sink = nil })
	resp, body := hs.do(t, http.MethodPost, chatPath, chatBody(true, true), nil)
	if string(body) != strings.Join(fullStream, "") {
		t.Errorf("passthrough altered the stream: %q", body)
	}
	assertSidecarHeaders(t, resp, "passthrough", ReasonPassthrough)
	hs.sink.none(t)
}

func TestNewValidates(t *testing.T) {
	base := Options{
		Mode: config.ModeShadow, Upstream: Upstream{Provider: "p", BaseURL: "http://127.0.0.1:1/v1"},
		Client: &http.Client{}, Sink: &memSink{}, MaxBodyBytes: 1, MaxInflight: 1,
	}
	tests := []struct {
		name   string
		mutate func(*Options)
		ok     bool
	}{
		{"valid", func(*Options) {}, true},
		{"route not yet", func(o *Options) { o.Mode = config.ModeRoute }, false},
		{"unknown mode", func(o *Options) { o.Mode = "x" }, false},
		{"shadow without sink", func(o *Options) { o.Sink = nil }, false},
		{"no client", func(o *Options) { o.Client = nil }, false},
		{"no upstream", func(o *Options) { o.Upstream = Upstream{} }, false},
		{"no limits", func(o *Options) { o.MaxInflight = 0 }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := base
			tc.mutate(&o)
			if _, err := New(o); (err == nil) != tc.ok {
				t.Fatalf("New err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// The agent's query string reaches the upstream on chat
// too, as on every other route.
func TestQueryStringKept(t *testing.T) {
	tests := []struct {
		name, method, path, query, body string
	}{
		{"chat", http.MethodPost, "/v1/chat/completions", "api-version=2024-01-01", chatBody(false, false)},
		{"chat stream", http.MethodPost, "/v1/chat/completions", "a=1&b=2", chatBody(true, false)},
		{"chat not json", http.MethodPost, "/v1/chat/completions", "x=1", `not json`},
		{"chat no query", http.MethodPost, "/v1/chat/completions", "", chatBody(false, false)},
		{"embeddings", http.MethodPost, "/v1/embeddings", "x=%20y", `{"model":"m","input":["a"]}`},
		{"models", http.MethodGet, "/v1/models", "page=2", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t, jsonServe(http.StatusOK, `{"object":"list"}`))
			hs := newHarness(t, up.URL, nil)
			path := tc.path
			if tc.query != "" {
				path += "?" + tc.query
			}
			resp, _ := hs.do(t, tc.method, path, tc.body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			got := up.last(t)
			if got.Path != tc.path || got.Query != tc.query {
				t.Errorf("upstream saw %s?%s, want %s?%s", got.Path, got.Query, tc.path, tc.query)
			}
			if got.Header.Get("Authorization") != "Bearer "+upstreamKey {
				t.Errorf("upstream auth = %q", got.Header.Get("Authorization"))
			}
			hs.sink.wait(t)
		})
	}
}

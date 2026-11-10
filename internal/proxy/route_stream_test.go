package proxy

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/policy"
	"github.com/Foncifelo/sidecar-proxy/internal/pricing"
	"github.com/Foncifelo/sidecar-proxy/internal/transport"
	"github.com/Foncifelo/sidecar-proxy/pkg/openai"
)

// Every provider, endpoint, price and latency here is fictitious. The
// policy's client_ttft_p95 is 300 ms and ttft_p95_slack 100 ms, so a first
// event later than 400 ms after an attempt starts is late.
const (
	streamReq   = `{"model":"` + agentModel + `","stream":true,"messages":[]}`
	clientP95Ms = 300
	ttftSlack   = 100 * time.Millisecond
	lateDelay   = 1500 * time.Millisecond
	candChunk   = "data: {\"model\":\"fict/model\",\"choices\":[{\"delta\":{\"content\":\"cand\"}}]}\n\n"
	// candOut is candChunk as the agent gets it: its own model (ADR 0008).
	candOut   = "data: {\"model\":\"" + agentModel + "\",\"choices\":[{\"delta\":{\"content\":\"cand\"}}]}\n\n"
	upChunk   = "data: {\"model\":\"" + agentModel + "\",\"choices\":[{\"delta\":{\"content\":\"up\"}}]}\n\n"
	doneChunk = "data: [DONE]\n\n"
	pingChunk = ": ping\n\n"
)

// step is one write of a fictitious SSE provider: wait, then send block.
// An empty block with cut set aborts the connection mid-stream.
type step struct {
	wait  time.Duration
	block string
	cut   bool
}

// sseSteps sends headers at once (after headerWait) and then each step.
func sseSteps(headerWait time.Duration, steps ...step) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sleepCtx(r, headerWait) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		_ = rc.Flush()
		for _, s := range steps {
			if !sleepCtx(r, s.wait) {
				return
			}
			if s.cut {
				panic(http.ErrAbortHandler)
			}
			_, _ = w.Write([]byte(s.block))
			_ = rc.Flush()
		}
	}
}

func sleepCtx(r *http.Request, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-r.Context().Done():
		return false
	case <-time.After(d):
		return true
	}
}

func newStreamEnv(t *testing.T, cand, up http.Handler, p95Ms int64) routeEnv {
	t.Helper()
	return newRouteEnv(t, cand, up, func(p *policy.Policy) {
		m := p.Models[fictModel]
		m.ClientTTFTP95Ms = p95Ms
		p.Models[fictModel] = m
	}, func(_ *transport.Config, r *Route) {
		r.Config.Limits.TTFTP95Slack = config.NewDuration(ttftSlack)
	})
}

var upStream = sseSteps(0, step{block: upChunk}, step{block: doneChunk})

// The first SSE event is held until the attempt is kept; past
// client_ttft_p95 + ttft_p95_slack the attempt is aborted (ttft_over_p95)
// and the next one serves without a byte of the first reaching the agent.
func TestRouteStreamTTFTFallback(t *testing.T) {
	tests := []struct {
		name         string
		cand, up     http.HandlerFunc
		p95Ms        int64
		wantEndpoint string
		wantAborts   []string
		wantBody     string
	}{
		{"first event on time", sseSteps(0, step{block: candChunk}, step{block: doneChunk}), upStream, clientP95Ms,
			candEP, []string{""}, candOut + doneChunk},
		{"first event late", sseSteps(0, step{wait: lateDelay, block: candChunk}, step{block: doneChunk}), upStream, clientP95Ms,
			curEndpoint, []string{abortTTFTOverP95, ""}, upChunk + doneChunk},
		{"keepalive on time, data late", sseSteps(0, step{block: pingChunk}, step{wait: lateDelay, block: candChunk}), upStream, clientP95Ms,
			curEndpoint, []string{abortTTFTOverP95, ""}, upChunk + doneChunk},
		{"headers already late", sseSteps(600*time.Millisecond, step{block: candChunk}), upStream, clientP95Ms,
			curEndpoint, []string{abortTTFTOverP95, ""}, upChunk + doneChunk},
		{"no client p95 means no limit", sseSteps(0, step{wait: 600 * time.Millisecond, block: candChunk}, step{block: doneChunk}), upStream, 0,
			candEP, []string{""}, candOut + doneChunk},
		{"cut before the first event", sseSteps(0, step{block: pingChunk}, step{cut: true}), upStream, clientP95Ms,
			curEndpoint, []string{abortHTTPError, ""}, upChunk + doneChunk},
		{"last attempt late is delivered", jsonServe(429, `{"error":{}}`), sseSteps(0, step{wait: 600 * time.Millisecond, block: upChunk}, step{block: doneChunk}), clientP95Ms,
			curEndpoint, []string{abortRateLimited, ""}, upChunk + doneChunk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newStreamEnv(t, tt.cand, tt.up, tt.p95Ms)
			resp, body := env.post(t, streamReq, nil)
			ev := env.h.sink.wait(t)
			if resp.StatusCode != http.StatusOK || body != tt.wantBody {
				t.Fatalf("status %d body\n got %q\nwant %q", resp.StatusCode, body, tt.wantBody)
			}
			wantFallback := "0"
			if tt.wantEndpoint != candEP {
				wantFallback = "1"
			}
			if resp.Header.Get(HeaderEndpoint) != tt.wantEndpoint || resp.Header.Get(HeaderFallback) != wantFallback ||
				resp.Header.Get(HeaderDecision) != string(policy.PlanRouted) {
				t.Errorf("headers endpoint=%q fallback=%q decision=%q", resp.Header.Get(HeaderEndpoint),
					resp.Header.Get(HeaderFallback), resp.Header.Get(HeaderDecision))
			}
			if got := abortReasons(ev.Attempts); strings.Join(got, ",") != strings.Join(tt.wantAborts, ",") {
				t.Errorf("abort reasons %q, want %q", got, tt.wantAborts)
			}
			if ev.StreamState != events.StreamComplete || ev.EndpointUsed != tt.wantEndpoint || !ev.Stream {
				t.Errorf("event state=%q used=%q stream=%v", ev.StreamState, ev.EndpointUsed, ev.Stream)
			}
		})
	}
}

// After the first byte a provider failure is an SSE error,
// stream_state truncated, and no retry even though the upstream remains.
func TestRouteStreamCutAfterFirstByte(t *testing.T) {
	upCalled := make(chan struct{}, 1)
	up := func(w http.ResponseWriter, r *http.Request) {
		upCalled <- struct{}{}
		upStream(w, r)
	}
	env := newStreamEnv(t, sseSteps(0, step{block: candChunk}, step{cut: true}), http.HandlerFunc(up), clientP95Ms)
	resp, body := env.post(t, streamReq, nil)
	ev := env.h.sink.wait(t)
	id := resp.Header.Get(HeaderRequestID)
	want := candOut + "event: error\ndata: {\"error\":{\"type\":\"upstream_stream_error\",\"request_id\":\"" + id + "\"}}\n\n"
	if resp.StatusCode != http.StatusOK || body != want {
		t.Fatalf("status %d body\n got %q\nwant %q", resp.StatusCode, body, want)
	}
	select {
	case <-upCalled:
		t.Error("upstream was called after the first byte")
	default:
	}
	if ev.StreamState != events.StreamTruncated || ev.ErrorType != streamErrorType || len(ev.Attempts) != 1 ||
		ev.Attempts[0].EndpointID != candEP || ev.Attempts[0].AbortReason != "" || resp.Header.Get(HeaderFallback) != "0" {
		t.Errorf("event state=%q error=%q attempts=%+v", ev.StreamState, ev.ErrorType, ev.Attempts)
	}
}

// A streaming provider that goes silent longer than StreamIdleTimeout is
// cut like any other failure: before the first data event a candidate falls
// back to the upstream; after it, or on the last attempt (nowhere to fall
// back to, so its held bytes go out), the agent gets an SSE error and the
// event is truncated, with no retry. Coverage of the contract: loopback does
// not reproduce the clean TLS EOF that TestIdleReaderEOFAfterCancel pins.
func TestRouteStreamStallsIdle(t *testing.T) {
	// Above TLS setup and first-event latency on a loaded CI runner.
	const idle = 300 * time.Millisecond
	stall := step{wait: 10 * time.Second, block: doneChunk} // cut by the idle timeout long before
	sseError := func(id string) string {
		return "event: error\ndata: {\"error\":{\"type\":\"upstream_stream_error\",\"request_id\":\"" + id + "\"}}\n\n"
	}
	tests := []struct {
		name         string
		cand, up     http.HandlerFunc
		wantEndpoint string
		wantAborts   []string
		wantState    events.StreamState
		wantError    string
		wantBody     func(id string) string
	}{
		{"stall before the first event", sseSteps(0, step{block: pingChunk}, stall), upStream, curEndpoint,
			[]string{abortHTTPError, ""}, events.StreamComplete, "",
			func(string) string { return upChunk + doneChunk }},
		{"stall after the first event", sseSteps(0, step{block: candChunk}, stall), upStream, candEP,
			[]string{""}, events.StreamTruncated, streamErrorType,
			func(id string) string { return candOut + sseError(id) }},
		{"last attempt stalls before the first event", jsonServe(429, `{"error":{}}`), sseSteps(0, step{block: pingChunk}, stall), curEndpoint,
			[]string{abortRateLimited, ""}, events.StreamTruncated, streamErrorType,
			func(id string) string { return pingChunk + sseError(id) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// No client p95: only the idle timeout can cut an attempt.
			env := newRouteEnv(t, tt.cand, tt.up, nil, nil, func(o *Options) { o.StreamIdleTimeout = idle })
			start := time.Now()
			resp, body := env.post(t, streamReq, nil)
			ev := env.h.sink.wait(t)
			if elapsed := time.Since(start); elapsed > 5*time.Second { // the stall lasts 10 s
				t.Fatalf("request took %s, want about %s", elapsed, idle)
			}
			if want := tt.wantBody(resp.Header.Get(HeaderRequestID)); resp.StatusCode != http.StatusOK || body != want {
				t.Fatalf("status %d body\n got %q\nwant %q", resp.StatusCode, body, want)
			}
			wantFallback := "0"
			if tt.wantEndpoint != candEP {
				wantFallback = "1"
			}
			if resp.Header.Get(HeaderEndpoint) != tt.wantEndpoint || resp.Header.Get(HeaderFallback) != wantFallback {
				t.Errorf("headers endpoint=%q fallback=%q", resp.Header.Get(HeaderEndpoint), resp.Header.Get(HeaderFallback))
			}
			if got := abortReasons(ev.Attempts); strings.Join(got, ",") != strings.Join(tt.wantAborts, ",") {
				t.Errorf("abort reasons %q, want %q", got, tt.wantAborts)
			}
			if ev.StreamState != tt.wantState || ev.ErrorType != tt.wantError {
				t.Errorf("event state=%q error=%q, want %q %q", ev.StreamState, ev.ErrorType, tt.wantState, tt.wantError)
			}
		})
	}
}

// The attempt aborted for ttft_over_p95 carries cost_est from
// the estimated prompt and its tokens_out_est, and it adds to cost_actual.
func TestRouteStreamAbortedAttemptCost(t *testing.T) {
	prompt := strings.Repeat("x", 40000) // fictitious padding: about 10k prompt tokens
	req := `{"model":"` + agentModel + `","stream":true,"messages":[{"role":"user","content":"` + prompt + `"}]}`
	usageChunk := `data: {"model":"` + agentModel + `","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}` + "\n\n"
	up := sseSteps(0, step{block: upChunk}, step{block: usageChunk}, step{block: doneChunk})
	env := newStreamEnv(t, sseSteps(0, step{wait: lateDelay, block: candChunk}), up, clientP95Ms)
	_, _ = env.post(t, req, nil)
	ev := env.h.sink.wait(t)
	if len(ev.Attempts) != 2 || ev.Attempts[0].AbortReason != abortTTFTOverP95 {
		t.Fatalf("attempts %+v", ev.Attempts)
	}
	aborted := ev.Attempts[0]
	candPrice := pricing.Price{PriceID: candPriceID, InputPerM: 500_000, OutputPerM: 1_000_000}
	wantAborted, err := pricing.Cost(candPrice, openai.Usage{In: int64(len(req)) / 4, Out: aborted.TokensOutEst})
	if err != nil {
		t.Fatal(err)
	}
	if aborted.TokensOutEst != 0 || aborted.CostEst != wantAborted || wantAborted <= 0 {
		t.Errorf("aborted tokens_out_est=%d cost_est=%d, want 0 and %d (> 0)", aborted.TokensOutEst, aborted.CostEst, wantAborted)
	}
	if ev.Attempts[1].CostEst != upCost {
		t.Errorf("served cost_est %d, want %d", ev.Attempts[1].CostEst, upCost)
	}
	if ev.CostActual != wantAborted+upCost || ev.PriceIDActual != curPriceID {
		t.Errorf("cost_actual %d price_id %q, want %d and %q", ev.CostActual, ev.PriceIDActual, wantAborted+upCost, curPriceID)
	}
}

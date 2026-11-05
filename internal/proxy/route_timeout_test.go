package proxy

import (
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/pricing"
	"github.com/Foncifelo/sidecar-proxy/internal/transport"
	"github.com/Foncifelo/sidecar-proxy/pkg/openai"
)

// Every provider, endpoint, price and context length here is fictitious
// (fictPolicy: candEP at 0.5 $/M in, 1 $/M out, context_length 131072).
const (
	// routeReqTimeoutCost is the conservative cost_est of candEP abandoned
	// at the first byte timeout for routeReq (45 bytes, no max_tokens):
	// 11 prompt tokens × 0.5 $/M + (131072 − 11) output tokens × 1 $/M,
	// rounded up by pricing.Cost.
	routeReqTimeoutCost = 131067
	maxTokensReq        = `{"model":"` + agentModel + `","max_tokens":100,"messages":[]}`
	// maxTokensTimeoutCost: 15 prompt tokens × 0.5 $/M + 100 out × 1 $/M.
	maxTokensTimeoutCost = 108
	headerDelay          = 300 * time.Millisecond
	shortTimeout         = 50 * time.Millisecond
)

// delayed waits d before letting next answer.
func delayed(d time.Duration, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sleepCtx(r, d) {
			next(w, r)
		}
	}
}

// TestRouteNonStreamFirstByteTimeout: without stream, route mode waits
// limits.nonstream_first_byte_timeout for headers instead of
// first_byte_timeout; with stream first_byte_timeout still rules.
func TestRouteNonStreamFirstByteTimeout(t *testing.T) {
	jsonOK := jsonServe(http.StatusOK, provBody)
	sseOK := sseSteps(0, step{block: upChunk}, step{block: doneChunk})
	tests := []struct {
		name         string
		body         string
		cand, up     http.HandlerFunc
		tune         func(*Route)
		wantEndpoint string
		wantAborts   []string
	}{
		{
			"no stream ignores first_byte_timeout", routeReq, delayed(headerDelay, jsonOK), jsonOK,
			func(r *Route) { r.FirstByteTimeout, r.NonStreamFirstByteTimeout = shortTimeout, 5*time.Second },
			candEP, []string{""},
		},
		{
			"no stream falls back at nonstream_first_byte_timeout", routeReq, delayed(headerDelay, jsonOK), jsonOK,
			func(r *Route) { r.FirstByteTimeout, r.NonStreamFirstByteTimeout = 5*time.Second, shortTimeout },
			curEndpoint, []string{abortFirstByteTimeout, ""},
		},
		{
			"no stream reads the timeout from the YAML limits", routeReq, delayed(headerDelay, jsonOK), jsonOK,
			func(r *Route) {
				r.FirstByteTimeout = 5 * time.Second
				r.Config.Limits.NonStreamFirstByteTimeout = config.NewDuration(shortTimeout)
			},
			curEndpoint, []string{abortFirstByteTimeout, ""},
		},
		{
			"stream still uses first_byte_timeout", streamReq, sseSteps(headerDelay, step{block: candChunk}, step{block: doneChunk}), sseOK,
			func(r *Route) { r.FirstByteTimeout, r.NonStreamFirstByteTimeout = shortTimeout, 5*time.Second },
			curEndpoint, []string{abortFirstByteTimeout, ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newRouteEnv(t, tt.cand, tt.up, nil, func(_ *transport.Config, r *Route) { tt.tune(r) })
			resp, _ := env.post(t, tt.body, nil)
			ev := env.h.sink.wait(t)
			if resp.StatusCode != http.StatusOK || resp.Header.Get(HeaderEndpoint) != tt.wantEndpoint {
				t.Errorf("status %d endpoint %q, want 200 %q", resp.StatusCode, resp.Header.Get(HeaderEndpoint), tt.wantEndpoint)
			}
			if got := abortReasons(ev.Attempts); strings.Join(got, ",") != strings.Join(tt.wantAborts, ",") {
				t.Errorf("abort reasons %q, want %q", got, tt.wantAborts)
			}
		})
	}
}

func TestRouteFirstByteTimeoutDefault(t *testing.T) {
	tests := []struct {
		name   string
		rt     Route
		stream bool
		want   time.Duration
	}{
		{"stream", Route{FirstByteTimeout: time.Second, NonStreamFirstByteTimeout: time.Minute}, true, time.Second},
		{"no stream, route field", Route{FirstByteTimeout: time.Second, NonStreamFirstByteTimeout: time.Minute}, false, time.Minute},
		{"no stream, YAML limits", Route{Config: config.Config{Limits: config.Limits{NonStreamFirstByteTimeout: config.NewDuration(7 * time.Second)}}}, false, 7 * time.Second},
		{"no stream, nothing set: 30s default", Route{FirstByteTimeout: time.Second}, false, 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rt.firstByteTimeout(tt.stream); got != tt.want {
				t.Errorf("firstByteTimeout(%v) = %v, want %v", tt.stream, got, tt.want)
			}
		})
	}
}

// TestRouteTimeoutAttemptCost: an attempt abandoned at the first byte
// timeout after its request was written carries a conservative cost_est,
// summed into cost_actual; one that never left the sidecar carries 0.
func TestRouteTimeoutAttemptCost(t *testing.T) {
	ok := jsonServe(http.StatusOK, provBody)
	tests := []struct {
		name        string
		body        string
		wantCostEst events.Micros
		wantOutEst  int64
	}{
		{"max_tokens bounds the output", maxTokensReq, maxTokensTimeoutCost, 100},
		{"no max_tokens: rest of the endpoint's context", routeReq, routeReqTimeoutCost, 131072 - 11},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newRouteEnv(t, http.HandlerFunc(slow), ok, nil, func(_ *transport.Config, r *Route) { r.NonStreamFirstByteTimeout = shortTimeout })
			env.post(t, tt.body, nil)
			ev := env.h.sink.wait(t)
			if len(ev.Attempts) != 2 || ev.Attempts[0].AbortReason != abortFirstByteTimeout {
				t.Fatalf("attempts %+v", ev.Attempts)
			}
			a := ev.Attempts[0]
			if a.CostEst != tt.wantCostEst || a.TokensOutEst != tt.wantOutEst {
				t.Errorf("timed out attempt cost_est %d tokens_out_est %d, want %d %d", a.CostEst, a.TokensOutEst, tt.wantCostEst, tt.wantOutEst)
			}
			if want := tt.wantCostEst + upCost; ev.CostActual != want || ev.CostActual != a.CostEst+ev.Attempts[1].CostEst {
				t.Errorf("cost_actual %d, want %d (sum of attempts)", ev.CostActual, want)
			}
		})
	}
}

// Fictitious prices: hugePrice (10^7 USD per million output tokens) puts
// 100 output tokens above the ceiling; overflowPrice makes pricing.Cost
// overflow int64.
var (
	hugePrice     = pricing.Price{PriceID: "fict-huge", InputPerM: 1_000_000, OutputPerM: 10_000_000_000_000}
	overflowPrice = pricing.Price{PriceID: "fict-overflow", InputPerM: 1_000_000, OutputPerM: math.MaxInt64}
)

func TestTimeoutCost(t *testing.T) {
	price := pricing.Price{PriceID: "fict-price", InputPerM: 1_000_000, OutputPerM: 2_000_000}
	tests := []struct {
		name     string
		info     openai.RequestInfo
		prompt   int64
		contexts map[string]int64
		prices   map[string]pricing.Price
		wantCost events.Micros
		wantOut  int64
	}{
		{"max_tokens", openai.RequestInfo{MaxOutputTokens: 100}, 10, map[string]int64{"ep": 1000}, map[string]pricing.Price{"ep": price}, 210, 100},
		{"n multiplies the output", openai.RequestInfo{MaxOutputTokens: 100, N: 3}, 10, nil, map[string]pricing.Price{"ep": price}, 610, 300},
		{"context bound", openai.RequestInfo{}, 10, map[string]int64{"ep": 1000}, map[string]pricing.Price{"ep": price}, 1990, 990},
		{"prompt over the context: prompt only", openai.RequestInfo{}, 2000, map[string]int64{"ep": 1000}, map[string]pricing.Price{"ep": price}, 2000, 0},
		{"no bound: prompt only", openai.RequestInfo{}, 10, nil, map[string]pricing.Price{"ep": price}, 10, 0},
		{"no price: zero", openai.RequestInfo{MaxOutputTokens: 100}, 10, nil, nil, 0, 0},
		{"max_tokens over the context: context per choice", openai.RequestInfo{MaxOutputTokens: 5000, N: 2}, 10, map[string]int64{"ep": 1000}, map[string]pricing.Price{"ep": price}, 4010, 2000},
		{"max int64 times n: context per choice", openai.RequestInfo{MaxOutputTokens: math.MaxInt64, N: 2}, 10, map[string]int64{"ep": 1000}, map[string]pricing.Price{"ep": price}, 4010, 2000},
		{"max int64 times n without context: maxAssumedOutputTokens per choice", openai.RequestInfo{MaxOutputTokens: math.MaxInt64, N: 2}, 10, nil, map[string]pricing.Price{"ep": price}, 10 + 2*2*maxAssumedOutputTokens, 2 * maxAssumedOutputTokens},
		{"half max int64 times 3 without context: maxAssumedOutputTokens per choice", openai.RequestInfo{MaxOutputTokens: math.MaxInt64 / 2, N: 3}, 10, nil, map[string]pricing.Price{"ep": price}, 10 + 2*3*maxAssumedOutputTokens, 3 * maxAssumedOutputTokens},
		{"n capped at maxAssumedChoices", openai.RequestInfo{MaxOutputTokens: 100, N: 1000}, 10, nil, map[string]pricing.Price{"ep": price}, 10 + 2*100*maxAssumedChoices, 100 * maxAssumedChoices},
		{"cost above the ceiling: maxAttemptCostMicros", openai.RequestInfo{MaxOutputTokens: 100}, 10, nil, map[string]pricing.Price{"ep": hugePrice}, events.MaxAttemptCostMicros, 100},
		{"pricing overflow: maxAttemptCostMicros", openai.RequestInfo{MaxOutputTokens: 1 << 40}, 10, map[string]int64{"ep": 1 << 40}, map[string]pricing.Price{"ep": overflowPrice}, events.MaxAttemptCostMicros, 1 << 40},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &requestState{info: tt.info, promptTokensEst: tt.prompt, contexts: tt.contexts, prices: tt.prices}
			cost, out := s.timeoutCost("ep")
			if cost != tt.wantCost || out != tt.wantOut {
				t.Errorf("timeoutCost = %d, %d; want %d, %d", cost, out, tt.wantCost, tt.wantOut)
			}
		})
	}
}

// TestTimeoutCostMonotonic checks that a larger agent limit or n never
// yields a smaller cost_est, overflow included, and that cost_est never
// exceeds events.MaxAttemptCostMicros.
func TestTimeoutCostMonotonic(t *testing.T) {
	normal := pricing.Price{PriceID: "fict-price", InputPerM: 1_000_000, OutputPerM: 2_000_000}
	limits := []int64{1, 1000, maxAssumedOutputTokens, math.MaxInt64 / 1_000_000, math.MaxInt64 / 4, math.MaxInt64 / 2, math.MaxInt64}
	contexts := []map[string]int64{nil, {"ep": 131072}, {"ep": 1 << 40}}
	for _, price := range []pricing.Price{normal, hugePrice, overflowPrice} {
		for _, ctx := range contexts {
			for _, n := range []int{1, 2, 7, maxAssumedChoices, 1 << 20} {
				var prev events.Micros
				for _, l := range limits {
					s := &requestState{info: openai.RequestInfo{MaxOutputTokens: l, N: n}, promptTokensEst: 10, contexts: ctx, prices: map[string]pricing.Price{"ep": price}}
					cost, _ := s.timeoutCost("ep")
					if cost <= 0 || cost < prev || cost > events.MaxAttemptCostMicros {
						t.Errorf("%s ctx=%v n=%d max_tokens=%d: cost_est %d, previous %d; want in (0, %d] and non-decreasing",
							price.PriceID, ctx, n, l, cost, prev, events.MaxAttemptCostMicros)
					}
					prev = cost
				}
			}
		}
	}
}

func TestAttemptCost(t *testing.T) {
	tests := []struct {
		name  string
		price pricing.Price
		usage openai.Usage
		want  events.Micros
	}{
		{"below the ceiling", pricing.Price{PriceID: "fict-price", InputPerM: 1_000_000, OutputPerM: 2_000_000}, openai.Usage{In: 10, Out: 100}, 210},
		{"above the ceiling: maxAttemptCostMicros", hugePrice, openai.Usage{In: 10, Out: 100}, events.MaxAttemptCostMicros},
		{"overflow: maxAttemptCostMicros", overflowPrice, openai.Usage{In: 10, Out: 1 << 40}, events.MaxAttemptCostMicros},
		{"other pricing error: zero", pricing.Price{PriceID: "fict-negative", PerRequest: -1}, openai.Usage{In: 10}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := attemptCost(tt.price, tt.usage); got != tt.want {
				t.Errorf("attemptCost = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestApplyRouteCostsCapped: cost_actual is the sum of the attempts, capped
// at events.MaxAttemptCostMicros without wrapping.
func TestApplyRouteCostsCapped(t *testing.T) {
	tests := []struct {
		name  string
		costs []events.Micros
		want  events.Micros
	}{
		{"plain sum", []events.Micros{100, 200}, 300},
		{"sum reaching the ceiling", []events.Micros{events.MaxAttemptCostMicros - 500, 500}, events.MaxAttemptCostMicros},
		{"two attempts over the ceiling", []events.Micros{60_000_000, 60_000_000}, events.MaxAttemptCostMicros},
		{"capped attempt plus more", []events.Micros{events.MaxAttemptCostMicros, 500}, events.MaxAttemptCostMicros},
		{"max int64 attempt does not wrap", []events.Micros{500, math.MaxInt64, 500}, events.MaxAttemptCostMicros},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &requestState{}
			for _, c := range tt.costs {
				s.attempts = append(s.attempts, events.Attempt{CostEst: c})
			}
			var ev events.Event
			s.applyRouteCosts(&ev)
			if ev.CostActual != tt.want || ev.CostBest != tt.want {
				t.Errorf("cost_actual %d, cost_best %d; want both %d", ev.CostActual, ev.CostBest, tt.want)
			}
		})
	}
}

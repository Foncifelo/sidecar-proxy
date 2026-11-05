package proxy

import (
	"bufio"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/metrics"
	"github.com/Foncifelo/sidecar-proxy/internal/transport"
)

// scrapeMetrics returns every sample of m keyed by "name{labels}".
func scrapeMetrics(t *testing.T, m *metrics.Sidecar) (map[string]float64, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	out := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("bad sample %q: %v", line, err)
		}
		out[line[:i]] = v
	}
	return out, rec.Body.String()
}

func withMetrics(m *metrics.Sidecar) func(*Options) {
	return func(o *Options) { o.Metrics = m }
}

// Guarantee 8: the added latency is exposed in sidecar_overhead_seconds and
// its p95 stays under 50 ms on loopback.
func TestOverheadMetric(t *testing.T) {
	const n = 100
	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{"no policy", nil},
		{"with policy", withPolicy(activePolicy(t, fictPolicy()))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := metrics.New()
			up := newUpstream(t, sseServe(fullStream, 0))
			hs := newHarness(t, up.URL, func(o *Options) {
				o.Sink = discardSink{}
				o.Metrics = m
				if tt.mutate != nil {
					tt.mutate(o)
				}
			})
			timeRequests(t, hs.srv.Client(), hs.srv.URL, n)
			got, _ := scrapeMetrics(t, m)
			if c := got["sidecar_overhead_seconds_count"]; c != n {
				t.Fatalf("overhead count = %v, want %d", c, n)
			}
			if c := got[`sidecar_requests_total{kind="chat"}`]; c != n {
				t.Errorf("chat requests = %v, want %d", c, n)
			}
			within := got[`sidecar_overhead_seconds_bucket{le="0.05"}`]
			if need := math.Ceil(n * 0.95); within < need {
				t.Errorf("only %v of %d observations within 50 ms, want >= %v", within, n, need)
			}
		})
	}
}

var labelPair = regexp.MustCompile(`(\w+)="([^"]*)"`)

// Neither the agent's model nor any content reaches a label.
func TestMetricsNoAgentContent(t *testing.T) {
	const sentinel = "SENTINEL-7f3a"
	body := `{"model":"` + sentinel + `/model","messages":[{"role":"user","content":"` + sentinel + `"}]}`
	ok := jsonServe(http.StatusOK, provBody)
	tests := []struct {
		name string
		mode config.Mode
		path string
	}{
		{"shadow chat", config.ModeShadow, chatPath},
		{"passthrough unknown path", config.ModePassthrough, "/v1/" + sentinel},
		{"route chat", config.ModeRoute, chatPath},
		{"route other path", config.ModeRoute, "/v1/embeddings?q=" + sentinel},
	}
	allowed := map[string][]string{
		"kind":   {"chat", "responses", "embeddings", "passthrough", "other"},
		"reason": {"none", "first_byte_timeout", "http_error", "rate_limited", "ttft_over_p95", "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := metrics.New()
			var srv *httptest.Server
			if tt.mode == config.ModeRoute {
				srv = newRouteEnv(t, ok, ok, nil, nil, withMetrics(m)).h.srv
			} else {
				up := newUpstream(t, ok)
				srv = newHarness(t, up.URL, func(o *Options) {
					o.Mode, o.Sink, o.Metrics = tt.mode, discardSink{}, m
				}).srv
			}
			req, err := http.NewRequest(http.MethodPost, srv.URL+tt.path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Fict-Header", sentinel)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			_, raw := scrapeMetrics(t, m)
			if strings.Contains(raw, sentinel) {
				t.Fatalf("/metrics leaks agent content:\n%s", raw)
			}
			for _, p := range labelPair.FindAllStringSubmatch(raw, -1) {
				if p[1] == "le" {
					continue
				}
				if !slices.Contains(allowed[p[1]], p[2]) {
					t.Errorf("label %s=%q outside its closed list", p[1], p[2])
				}
			}
		})
	}
}

// Route counters: one attempt per try by abort reason, one fallback per
// request served after more than one attempt.
func TestRouteMetrics(t *testing.T) {
	ok := jsonServe(http.StatusOK, provBody)
	tests := []struct {
		name         string
		cand, up     http.HandlerFunc
		fbt          time.Duration
		wantFallback float64
		wantAttempts map[string]float64
	}{
		{"candidate ok", ok, ok, time.Second, 0, map[string]float64{"none": 1}},
		{"429 then ok", jsonServe(429, `{"error":{}}`), ok, time.Second, 1, map[string]float64{"rate_limited": 1, "none": 1}},
		{"timeout then ok", slow, ok, 50 * time.Millisecond, 1, map[string]float64{"first_byte_timeout": 1, "none": 1}},
		{"no answer at all", dropConn, dropConn, time.Second, 1, map[string]float64{"http_error": 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := metrics.New()
			env := newRouteEnv(t, tt.cand, tt.up, nil, func(_ *transport.Config, r *Route) { r.NonStreamFirstByteTimeout = tt.fbt }, withMetrics(m))
			env.post(t, routeReq, nil)
			env.h.sink.wait(t) // counters are updated when the event is emitted
			got, _ := scrapeMetrics(t, m)
			if f := got["sidecar_fallbacks_total"]; f != tt.wantFallback {
				t.Errorf("fallbacks = %v, want %v", f, tt.wantFallback)
			}
			for _, r := range []string{"none", "first_byte_timeout", "http_error", "rate_limited", "ttft_over_p95", "other"} {
				if a := got[`sidecar_attempts_total{reason="`+r+`"}`]; a != tt.wantAttempts[r] {
					t.Errorf("attempts{%s} = %v, want %v", r, a, tt.wantAttempts[r])
				}
			}
			if c := got["sidecar_overhead_seconds_count"]; c != 1 {
				t.Errorf("overhead count = %v, want 1", c)
			}
		})
	}
}

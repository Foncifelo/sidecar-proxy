package proxy

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
)

const (
	overheadSamples = 200
	maxOverheadP95  = 50 * time.Millisecond
)

// timeRequests sends n streamed chat requests to base and returns their durations.
func timeRequests(tb testing.TB, client *http.Client, base string, n int) []time.Duration {
	tb.Helper()
	body := chatBody(true, false)
	out := make([]time.Duration, 0, n)
	for range n {
		start := time.Now()
		req, err := http.NewRequest(http.MethodPost, base+chatPath, strings.NewReader(body))
		if err != nil {
			tb.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			tb.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		out = append(out, time.Since(start))
	}
	return out
}

func p95(d []time.Duration) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[len(s)*95/100]
}

// overheadP95 compares p95 latency through the proxy against the upstream
// called directly, both on loopback.
func overheadP95(tb testing.TB, n int, mutate func(*Options)) time.Duration {
	tb.Helper()
	up := newUpstream(tb, sseServe(fullStream, 0))
	hs := newHarness(tb, up.URL, func(o *Options) {
		o.Sink = discardSink{}
		if mutate != nil {
			mutate(o)
		}
	})
	direct := timeRequests(tb, up.Client(), up.URL+"/v1", n)
	proxied := timeRequests(tb, hs.srv.Client(), hs.srv.URL, n)
	return p95(proxied) - p95(direct)
}

type discardSink struct{}

func (discardSink) Append(_ events.Event) error { return nil }

// Under 50 ms p95 of added latency locally.
// The shadow decision with an active policy stays within the budget.
func TestOverheadP95(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{"no policy", nil},
		{"with policy", withPolicy(activePolicy(t, fictPolicy()))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if d := overheadP95(t, overheadSamples, tt.mutate); d >= maxOverheadP95 {
				t.Fatalf("p95 overhead = %v, want < %v", d, maxOverheadP95)
			}
		})
	}
}

func BenchmarkProxyStream(b *testing.B) {
	up := newUpstream(b, sseServe(fullStream, 0))
	hs := newHarness(b, up.URL, func(o *Options) { o.Sink = discardSink{} })
	var samples []time.Duration
	for b.Loop() {
		samples = append(samples, timeRequests(b, hs.srv.Client(), hs.srv.URL, 1)...)
	}
	b.ReportMetric(float64(p95(samples).Microseconds())/1000, "p95-ms")
	b.ReportMetric(float64(overheadP95(b, overheadSamples, nil).Microseconds())/1000, "p95-overhead-ms")
}

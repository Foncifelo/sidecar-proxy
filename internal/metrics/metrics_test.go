package metrics

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func scrape(t *testing.T, m *Sidecar) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("content type %q", ct)
	}
	return rec.Body.String()
}

func TestExposition(t *testing.T) {
	tests := []struct {
		name    string
		src     *Sources
		record  func(m *Sidecar)
		want    []string
		notWant []string
	}{
		{
			name:   "empty",
			record: func(*Sidecar) {},
			want: []string{
				"# TYPE sidecar_overhead_seconds histogram\n",
				`sidecar_overhead_seconds_bucket{le="+Inf"} 0` + "\n",
				"sidecar_overhead_seconds_sum 0\nsidecar_overhead_seconds_count 0\n",
				"# TYPE sidecar_requests_total counter\n",
				`sidecar_requests_total{kind="chat"} 0` + "\n",
				"sidecar_fallbacks_total 0\n",
				`sidecar_attempts_total{reason="none"} 0` + "\n",
				"# TYPE sidecar_queue_depth gauge\nsidecar_queue_depth 0\n",
				"sidecar_dropped_events_total 0\n",
				"# TYPE sidecar_queue_disk_errors_total counter\nsidecar_queue_disk_errors_total 0\n",
				"# TYPE sidecar_policy_age_seconds gauge\n",
			},
			notWant: []string{"\nsidecar_policy_age_seconds "},
		},
		{
			name: "recorded",
			src: &Sources{
				QueueDepth:      func() int { return 7 },
				DroppedEvents:   func() uint64 { return 3 },
				QueueDiskErrors: func() uint64 { return 5 },
				PolicyAge:       func() (time.Duration, bool) { return 42 * time.Second, true },
			},
			record: func(m *Sidecar) {
				m.ObserveOverhead(2 * time.Millisecond)
				m.ObserveOverhead(40 * time.Millisecond)
				m.ObserveOverhead(3 * time.Second)
				m.Request("chat")
				m.Request("chat")
				m.Request("embeddings")
				m.Fallback()
				m.Attempt("")
				m.Attempt("rate_limited")
			},
			want: []string{
				`sidecar_overhead_seconds_bucket{le="0.001"} 0` + "\n",
				`sidecar_overhead_seconds_bucket{le="0.0025"} 1` + "\n",
				`sidecar_overhead_seconds_bucket{le="0.05"} 2` + "\n",
				`sidecar_overhead_seconds_bucket{le="1"} 2` + "\n",
				`sidecar_overhead_seconds_bucket{le="+Inf"} 3` + "\n",
				"sidecar_overhead_seconds_sum 3.042\nsidecar_overhead_seconds_count 3\n",
				`sidecar_requests_total{kind="chat"} 2` + "\n",
				`sidecar_requests_total{kind="embeddings"} 1` + "\n",
				"sidecar_fallbacks_total 1\n",
				`sidecar_attempts_total{reason="none"} 1` + "\n",
				`sidecar_attempts_total{reason="rate_limited"} 1` + "\n",
				"sidecar_queue_depth 7\n",
				"sidecar_dropped_events_total 3\n",
				"sidecar_queue_disk_errors_total 5\n",
				"sidecar_policy_age_seconds 42\n",
			},
		},
		{
			name:    "no policy omits the age sample",
			src:     &Sources{PolicyAge: func() (time.Duration, bool) { return 0, false }},
			record:  func(*Sidecar) {},
			notWant: []string{"\nsidecar_policy_age_seconds "},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New()
			if tt.src != nil {
				m.SetSources(*tt.src)
			}
			tt.record(m)
			out := scrape(t, m)
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in\n%s", w, out)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(out, w) {
					t.Errorf("unexpected %q in\n%s", w, out)
				}
			}
		})
	}
}

// labelRe matches every label pair in the exposition.
var labelRe = regexp.MustCompile(`\{(\w+)="([^"]*)"\}`)

// Label values come from closed lists; anything else is "other".
func TestLabelsBounded(t *testing.T) {
	tests := []struct {
		name   string
		record func(m *Sidecar)
		label  string
	}{
		{"agent model as kind", func(m *Sidecar) { m.Request("secret-agent-model") }, `sidecar_requests_total{kind="other"} 1`},
		{"content as reason", func(m *Sidecar) { m.Attempt(`PROMPT "sentinel"`) }, `sidecar_attempts_total{reason="other"} 1`},
	}
	allowed := map[string][]string{
		"kind":   append(kindLabels(), labelOther),
		"reason": append(reasonLabels(), labelOther),
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New()
			tt.record(m)
			out := scrape(t, m)
			if !strings.Contains(out, tt.label) {
				t.Errorf("missing %q", tt.label)
			}
			for _, bad := range []string{"secret-agent-model", "PROMPT", "sentinel"} {
				if strings.Contains(out, bad) {
					t.Errorf("exposition leaks %q", bad)
				}
			}
			for _, mm := range labelRe.FindAllStringSubmatch(out, -1) {
				if mm[1] == "le" {
					continue
				}
				if !slices.Contains(allowed[mm[1]], mm[2]) {
					t.Errorf("label %s=%q outside its closed list", mm[1], mm[2])
				}
			}
		})
	}
}

func TestNilSafe(t *testing.T) {
	var m *Sidecar
	m.SetSources(Sources{})
	m.ObserveOverhead(time.Second)
	m.Request("chat")
	m.Fallback()
	m.Attempt("")
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("nil scrape: %d %q", rec.Code, rec.Body.String())
	}
}

func TestConcurrent(t *testing.T) {
	m := New()
	const workers, each = 8, 100
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range each {
				m.ObserveOverhead(time.Millisecond)
				m.Request("chat")
				m.Attempt("http_error")
				_ = scrape(t, m)
			}
		})
	}
	wg.Wait()
	out := scrape(t, m)
	for _, w := range []string{
		"sidecar_overhead_seconds_count 800\n",
		`sidecar_requests_total{kind="chat"} 800`,
		`sidecar_attempts_total{reason="http_error"} 800`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q", w)
		}
	}
}

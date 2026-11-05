// Package metrics exposes the sidecar's /metrics in the Prometheus text format
// (version 0.0.4), written with the standard library only.
//
// Every label takes its value from a closed list defined here; anything else
// is counted as "other". No label ever carries the agent's model, a path, an
// endpoint, a provider or any request content (CONTRIBUTING.md rule 1).
package metrics

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// labelOther is the value any label takes outside its closed list.
const labelOther = "other"

// kindLabels are the endpoint kinds of internal/events.
func kindLabels() []string { return []string{"chat", "responses", "embeddings", "passthrough"} }

// reasonLabels are the attempt abort reasons of docs/evento.md; "none" is the
// attempt that answered.
func reasonLabels() []string {
	return []string{"none", "first_byte_timeout", "http_error", "rate_limited", "ttft_over_p95"}
}

// overheadBuckets are the upper bounds, in seconds, of sidecar_overhead_seconds.
// 0.05 is the limit of guarantee 8 in docs/contrato-proxy.md.
func overheadBuckets() []float64 {
	return []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1}
}

// Sources feed the gauges read at scrape time. Any nil func reads as zero
// (PolicyAge as absent).
type Sources struct {
	QueueDepth      func() int
	DroppedEvents   func() uint64
	QueueDiskErrors func() uint64
	// PolicyAge reports the age of the active policy; false without one.
	PolicyAge func() (time.Duration, bool)
}

// Sidecar holds the sidecar's series. A nil *Sidecar records nothing.
type Sidecar struct {
	sources   atomic.Pointer[Sources]
	overhead  histogram
	requests  labeled
	fallbacks atomic.Uint64
	attempts  labeled
}

// New returns an empty set of series.
func New() *Sidecar {
	return &Sidecar{
		overhead: newHistogram(overheadBuckets()),
		requests: newLabeled(kindLabels()),
		attempts: newLabeled(reasonLabels()),
	}
}

// SetSources sets the gauges' sources. Safe to call while serving.
func (m *Sidecar) SetSources(s Sources) {
	if m != nil {
		m.sources.Store(&s)
	}
}

// ObserveOverhead records the time the sidecar added before the upstream call.
func (m *Sidecar) ObserveOverhead(d time.Duration) {
	if m != nil {
		m.overhead.observe(d)
	}
}

// Request counts one request of the given endpoint kind.
func (m *Sidecar) Request(kind string) {
	if m != nil {
		m.requests.inc(kind)
	}
}

// Fallback counts one routed request served after more than one attempt.
func (m *Sidecar) Fallback() {
	if m != nil {
		m.fallbacks.Add(1)
	}
}

// Attempt counts one routed attempt by its abort reason; "" is the attempt
// that answered.
func (m *Sidecar) Attempt(abortReason string) {
	if m == nil {
		return
	}
	if abortReason == "" {
		abortReason = "none"
	}
	m.attempts.inc(abortReason)
}

// ServeHTTP writes every series in the text format.
func (m *Sidecar) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	var buf bytes.Buffer
	m.write(&buf)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func (m *Sidecar) write(w io.Writer) {
	if m == nil {
		return
	}
	var src Sources
	if s := m.sources.Load(); s != nil {
		src = *s
	}
	m.overhead.write(w, "sidecar_overhead_seconds", "Time the sidecar adds before the first upstream call.")
	m.requests.write(w, "sidecar_requests_total", "Requests received, by endpoint kind.", "kind")
	header(w, "sidecar_fallbacks_total", "Routed requests served after more than one attempt.", "counter")
	_, _ = fmt.Fprintf(w, "sidecar_fallbacks_total %d\n", m.fallbacks.Load())
	m.attempts.write(w, "sidecar_attempts_total", "Routed attempts, by abort reason (none = answered).", "reason")

	var depth int
	if src.QueueDepth != nil {
		depth = src.QueueDepth()
	}
	header(w, "sidecar_queue_depth", "Events waiting in the local queue.", "gauge")
	_, _ = fmt.Fprintf(w, "sidecar_queue_depth %d\n", depth)

	var dropped uint64
	if src.DroppedEvents != nil {
		dropped = src.DroppedEvents()
	}
	header(w, "sidecar_dropped_events_total", "Events dropped because the local queue was full.", "counter")
	_, _ = fmt.Fprintf(w, "sidecar_dropped_events_total %d\n", dropped)

	var diskErrors uint64
	if src.QueueDiskErrors != nil {
		diskErrors = src.QueueDiskErrors()
	}
	header(w, "sidecar_queue_disk_errors_total", "Failed disk operations of the local event queue.", "counter")
	_, _ = fmt.Fprintf(w, "sidecar_queue_disk_errors_total %d\n", diskErrors)

	header(w, "sidecar_policy_age_seconds", "Seconds since the control plane last confirmed the active policy.", "gauge")
	if src.PolicyAge != nil {
		if age, ok := src.PolicyAge(); ok {
			_, _ = fmt.Fprintf(w, "sidecar_policy_age_seconds %s\n", formatFloat(age.Seconds()))
		}
	}
}

func header(w io.Writer, name, help, typ string) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func formatFloat(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// labeled is a counter with one label from a closed list plus "other".
// The map is built once in newLabeled and only read afterwards.
type labeled struct {
	order  []string
	counts map[string]*atomic.Uint64
}

func newLabeled(values []string) labeled {
	l := labeled{order: append(values, labelOther), counts: make(map[string]*atomic.Uint64, len(values)+1)}
	for _, v := range l.order {
		l.counts[v] = new(atomic.Uint64)
	}
	return l
}

func (l labeled) inc(value string) {
	c, ok := l.counts[value]
	if !ok {
		c = l.counts[labelOther]
	}
	c.Add(1)
}

func (l labeled) write(w io.Writer, name, help, label string) {
	header(w, name, help, "counter")
	for _, v := range l.order {
		_, _ = fmt.Fprintf(w, "%s{%s=%q} %d\n", name, label, v, l.counts[v].Load())
	}
}

// histogram has fixed buckets; counts are per bucket and summed on write, so
// _count always equals the +Inf bucket.
type histogram struct {
	bounds []float64
	counts []atomic.Uint64 // len(bounds)+1; the last one is +Inf
	sumNs  atomic.Int64
}

func newHistogram(bounds []float64) histogram {
	return histogram{bounds: bounds, counts: make([]atomic.Uint64, len(bounds)+1)}
}

func (h *histogram) observe(d time.Duration) {
	d = max(d, 0)
	s := d.Seconds()
	i := len(h.bounds)
	for j, b := range h.bounds {
		if s <= b {
			i = j
			break
		}
	}
	h.counts[i].Add(1)
	h.sumNs.Add(int64(d))
}

func (h *histogram) write(w io.Writer, name, help string) {
	header(w, name, help, "histogram")
	var cum uint64
	for i := range h.counts {
		cum += h.counts[i].Load()
		le := math.Inf(1)
		if i < len(h.bounds) {
			le = h.bounds[i]
		}
		_, _ = fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", name, formatFloat(le), cum)
	}
	sum := time.Duration(h.sumNs.Load()).Seconds()
	_, _ = fmt.Fprintf(w, "%s_sum %s\n%s_count %d\n", name, formatFloat(sum), name, cum)
}

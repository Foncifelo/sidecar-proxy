package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/logging"
)

const testKey = logging.Secret("sk_test_fictitious_0123456789")

// reply is one scripted answer of the fake control plane.
type reply struct {
	code       int
	body       string // default for 202: {"accepted_n": <items>}
	retryAfter string
}

type recorded struct {
	auth string
	ct   string
	raw  map[string]any
	env  Envelope
}

// fakeControl is an httptest control plane that answers from a script; the
// last reply repeats once the script runs out.
type fakeControl struct {
	t      *testing.T
	mu     sync.Mutex
	script []reply
	reqs   []recorded
}

func (f *fakeControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/ingest" {
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read body: %v", err)
	}
	rec := recorded{auth: r.Header.Get("Authorization"), ct: r.Header.Get("Content-Type")}
	if err := json.Unmarshal(b, &rec.raw); err != nil {
		f.t.Errorf("body is not JSON: %v", err)
	}
	if err := json.Unmarshal(b, &rec.env); err != nil {
		f.t.Errorf("body is not an envelope: %v", err)
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, rec)
	rp := f.script[min(len(f.reqs), len(f.script))-1]
	f.mu.Unlock()
	if rp.retryAfter != "" {
		w.Header().Set("Retry-After", rp.retryAfter)
	}
	w.WriteHeader(rp.code)
	if rp.code == http.StatusAccepted && rp.body == "" {
		rp.body = `{"accepted_n":` + itoa(len(rec.env.Items)) + `}`
	}
	_, _ = io.WriteString(w, rp.body)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func (f *fakeControl) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

type harness struct {
	fake   *fakeControl
	client *Client
	logs   *syncBuffer
	mu     sync.Mutex
	sleeps []time.Duration
}

func (h *harness) slept() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.sleeps...)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.b.Write(p)
	return n, err //nolint:wrapcheck // bytes.Buffer never fails
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newHarness(t *testing.T, script ...reply) *harness {
	t.Helper()
	h := &harness{fake: &fakeControl{t: t, script: script}, logs: &syncBuffer{}}
	srv := httptest.NewServer(h.fake)
	t.Cleanup(srv.Close)
	c, err := New(Config{
		BaseURL:        srv.URL + "/",
		APIKey:         testKey,
		SidecarVersion: "0.1.0-test",
		HTTPClient:     &http.Client{Timeout: 5 * time.Second},
		MaxAttempts:    3,
		Logger:         logging.New(h.logs, logging.Options{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(ctx context.Context, d time.Duration) error {
		h.mu.Lock()
		h.sleeps = append(h.sleeps, d)
		h.mu.Unlock()
		return ctx.Err()
	}
	c.jitter = func(limit time.Duration) time.Duration { return limit }
	h.client = c
	return h
}

func batch(id string, n int) events.Batch {
	b := events.Batch{ID: id, Events: make([]events.Event, n)}
	for i := range n {
		b.Events[i] = events.Event{ID: id + "-ev" + itoa(i), Mode: events.ModeShadow}
	}
	return b
}

func TestNewValidates(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"ok", Config{BaseURL: "https://control.example", APIKey: testKey}, true},
		{"no url", Config{APIKey: testKey}, false},
		{"relative url", Config{BaseURL: "control.example", APIKey: testKey}, false},
		{"bad scheme", Config{BaseURL: "ftp://control.example", APIKey: testKey}, false},
		{"no key", Config{BaseURL: "https://control.example"}, false},
		{"not sk_", Config{BaseURL: "https://control.example", APIKey: "pk_123"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.cfg)
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok=%v", err, tt.ok)
			}
			if err != nil && strings.Contains(err.Error(), testKey.Reveal()) {
				t.Fatal("error leaks the key")
			}
			if tt.ok && (c.http == nil || c.http.Timeout <= 0) {
				t.Fatal("default HTTP client must have an explicit timeout")
			}
		})
	}
}

func TestSendEnvelopeAndAuth(t *testing.T) {
	h := newHarness(t, reply{code: http.StatusAccepted})
	if err := h.client.Send(context.Background(), batch("01JBATCH", 2)); err != nil {
		t.Fatal(err)
	}
	reqs := h.fake.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	r := reqs[0]
	if r.auth != "Bearer "+testKey.Reveal() {
		t.Errorf("Authorization = %q", r.auth)
	}
	if r.ct != "application/json" {
		t.Errorf("Content-Type = %q", r.ct)
	}
	for _, k := range []string{"schema_version", "sidecar_version", "batch_id", "items"} {
		if _, ok := r.raw[k]; !ok {
			t.Errorf("envelope lacks %q", k)
		}
	}
	if len(r.raw) != 4 {
		t.Errorf("envelope has extra keys: %v", r.raw)
	}
	e := r.env
	if e.SchemaVersion != events.SchemaVersion || e.SidecarVersion != "0.1.0-test" || e.BatchID != "01JBATCH" {
		t.Errorf("envelope header = %+v", e)
	}
	if len(e.Items) != 2 || e.Items[0].Type != ItemRequest || e.Items[0].Event == nil || e.Items[1].Event.ID != "01JBATCH-ev1" {
		t.Errorf("items = %+v", e.Items)
	}
	if strings.Contains(h.logs.String(), testKey.Reveal()) {
		t.Error("key leaked to logs")
	}
}

// TestSendStatusCodes covers every status code the control plane can answer.
func TestSendStatusCodes(t *testing.T) {
	tests := []struct {
		name     string
		script   []reply
		wantErr  error // nil means success (batch acked)
		wantCode int   // StatusError code expected, 0 if none
		wantReqs int
	}{
		{"202 coherent", []reply{{code: 202}}, nil, 0, 1},
		{"202 partial accepted_n", []reply{{code: 202, body: `{"accepted_n":1}`}}, nil, 0, 1},
		{"202 accepted_n above items", []reply{{code: 202, body: `{"accepted_n":5}`}}, ErrIncoherentAck, 0, 1},
		{"202 negative accepted_n", []reply{{code: 202, body: `{"accepted_n":-1}`}}, ErrIncoherentAck, 0, 1},
		{"202 missing accepted_n", []reply{{code: 202, body: `{}`}}, ErrIncoherentAck, 0, 1},
		{"202 not JSON", []reply{{code: 202, body: `ok`}}, ErrIncoherentAck, 0, 1},
		{"200 is not an ack", []reply{{code: 200, body: `{"accepted_n":2}`}}, nil, 200, 1},
		{"400 no retry", []reply{{code: 400}}, nil, 400, 1},
		{"401 no retry", []reply{{code: 401}}, nil, 401, 1},
		{"403 no retry", []reply{{code: 403}}, nil, 403, 1},
		{"408 retried", []reply{{code: 408}, {code: 202}}, nil, 0, 2},
		{"429 retried", []reply{{code: 429}, {code: 202}}, nil, 0, 2},
		{"500 retried", []reply{{code: 500}, {code: 202}}, nil, 0, 2},
		{"502 retried", []reply{{code: 502}, {code: 202}}, nil, 0, 2},
		{"503 retried", []reply{{code: 503}, {code: 503}, {code: 202}}, nil, 0, 3},
		{"504 retried", []reply{{code: 504}, {code: 202}}, nil, 0, 2},
		{"5xx exhausts attempts", []reply{{code: 500}}, nil, 500, 3},
		{"426 stops", []reply{{code: 426}}, ErrUpgradeRequired, 0, 1},
		{"409 rejected, no retry", []reply{{code: 409}}, events.ErrRejected, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.script...)
			err := h.client.Send(context.Background(), batch("01JB", 2))
			var se *StatusError
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.wantCode != 0:
				if !errors.As(err, &se) || se.Code != tt.wantCode {
					t.Fatalf("err = %v, want status %d", err, tt.wantCode)
				}
			case err != nil:
				t.Fatalf("err = %v, want nil", err)
			}
			if got := len(h.fake.requests()); got != tt.wantReqs {
				t.Fatalf("requests = %d, want %d", got, tt.wantReqs)
			}
		})
	}
}

func TestBackoffExponentialWithJitter(t *testing.T) {
	h := newHarness(t, reply{code: 503})
	h.client.cfg.MaxAttempts = 6
	h.client.cfg.BaseBackoff = 100 * time.Millisecond
	h.client.cfg.MaxBackoff = time.Second
	_ = h.client.Send(context.Background(), batch("01JB", 1))
	want := []time.Duration{100, 200, 400, 800, 1000}
	got := h.slept()
	if len(got) != len(want) {
		t.Fatalf("sleeps = %v", got)
	}
	for i := range want {
		if got[i] != want[i]*time.Millisecond {
			t.Errorf("sleep[%d] = %v, want %v", i, got[i], want[i]*time.Millisecond)
		}
	}

	for _, limit := range []time.Duration{0, time.Nanosecond, time.Second} {
		for range 100 {
			if j := fullJitter(limit); j < 0 || (limit > 0 && j >= limit) {
				t.Fatalf("fullJitter(%v) = %v out of range", limit, j)
			}
		}
	}
	if got := backoffLimit(time.Second, time.Minute, 200); got != time.Minute {
		t.Errorf("backoffLimit overflow guard = %v", got)
	}
}

func TestRetryAfterOn429(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		retryAfter string
		want       time.Duration
	}{
		{"seconds", "7", 7 * time.Second},
		{"http date", now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second},
		{"capped", "86400", DefaultMaxRetryAfter},
		{"absent uses backoff", "", DefaultBaseBackoff},
		{"invalid uses backoff", "soon", DefaultBaseBackoff},
		{"past date uses backoff", now.Add(-time.Minute).Format(http.TimeFormat), DefaultBaseBackoff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, reply{code: 429, retryAfter: tt.retryAfter}, reply{code: 202})
			h.client.now = func() time.Time { return now }
			if err := h.client.Send(context.Background(), batch("01JB", 1)); err != nil {
				t.Fatal(err)
			}
			if got := h.slept(); len(got) != 1 || got[0] != tt.want {
				t.Fatalf("sleeps = %v, want [%v]", got, tt.want)
			}
		})
	}
}

func TestRetryAfterIgnoredOn503(t *testing.T) {
	h := newHarness(t, reply{code: 503, retryAfter: "120"}, reply{code: 202})
	if err := h.client.Send(context.Background(), batch("01JB", 1)); err != nil {
		t.Fatal(err)
	}
	if got := h.slept(); len(got) != 1 || got[0] != DefaultBaseBackoff {
		t.Fatalf("sleeps = %v", got)
	}
}

func Test413SplitsBatch(t *testing.T) {
	// The server rejects any envelope with more than 2 items.
	var mu sync.Mutex
	var ids []string
	var sizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e Envelope
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		ids = append(ids, e.BatchID)
		sizes = append(sizes, len(e.Items))
		mu.Unlock()
		if len(e.Items) > 2 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"accepted_n":`+itoa(len(e.Items))+`}`)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, APIKey: testKey, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(context.Background(), batch("B", 5)); err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"B", "B.0", "B.1", "B.1.0", "B.1.1"}
	wantSizes := []int{5, 2, 3, 1, 2}
	if strings.Join(ids, ",") != strings.Join(wantIDs, ",") {
		t.Errorf("batch ids = %v, want %v", ids, wantIDs)
	}
	for i := range wantSizes {
		if i >= len(sizes) || sizes[i] != wantSizes[i] {
			t.Fatalf("sizes = %v, want %v", sizes, wantSizes)
		}
	}
}

func Test413SingleItemDropped(t *testing.T) {
	h := newHarness(t, reply{code: 413})
	if err := h.client.Send(context.Background(), batch("B", 1)); err != nil {
		t.Fatalf("a lone oversized item must not block the queue: %v", err)
	}
	if !strings.Contains(h.logs.String(), "too large") {
		t.Error("drop not logged")
	}
}

func Test413HalfFailsReturnsError(t *testing.T) {
	h := newHarness(t, reply{code: 413}, reply{code: 400})
	err := h.client.Send(context.Background(), batch("B", 4))
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 400 {
		t.Fatalf("err = %v", err)
	}
	if got := len(h.fake.requests()); got != 2 {
		t.Fatalf("second half must not be sent after the first fails; requests = %d", got)
	}
}

func Test426StopsSending(t *testing.T) {
	h := newHarness(t, reply{code: 426})
	ctx := context.Background()
	if err := h.client.Send(ctx, batch("B", 1)); !errors.Is(err, ErrUpgradeRequired) {
		t.Fatalf("err = %v", err)
	}
	if !h.client.Stopped() {
		t.Fatal("client not stopped")
	}
	for range 3 {
		if err := h.client.Send(ctx, batch("C", 1)); !errors.Is(err, ErrUpgradeRequired) {
			t.Fatalf("err = %v", err)
		}
	}
	h.client.sendHeartbeat(ctx)
	if got := len(h.fake.requests()); got != 1 {
		t.Fatalf("requests after 426 = %d, want 1", got)
	}
	if n := strings.Count(h.logs.String(), "426"); n != 1 {
		t.Fatalf("426 logged %d times, want once", n)
	}
}

func TestNetworkErrorRetried(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // connection refused
	c, err := New(Config{BaseURL: url, APIKey: testKey, MaxAttempts: 2, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Send(context.Background(), batch("B", 1))
	if err == nil || !strings.Contains(err.Error(), "2 attempts") {
		t.Fatalf("err = %v", err)
	}
}

// TestTransportErrorHidesURL: the sink logs Send errors, so a transport error
// must not carry the control plane URL (user:pass@ or ?token=).
func TestTransportErrorHidesURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := strings.Replace(srv.URL, "http://", "http://user:s3cret@", 1) + "/?token=t0ken"
	srv.Close()
	c, err := New(Config{BaseURL: u, APIKey: testKey, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Send(context.Background(), batch("B", 1))
	if err == nil {
		t.Fatal("want transport error")
	}
	for _, leak := range []string{"user", "s3cret", "t0ken"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error leaks %q: %v", leak, err)
		}
	}
}

func TestContextCancelledDuringBackoff(t *testing.T) {
	h := newHarness(t, reply{code: 503})
	h.client.sleep = sleepCtx
	h.client.cfg.BaseBackoff = time.Hour
	h.client.cfg.MaxBackoff = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := h.client.Send(ctx, batch("B", 1))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Send did not honour ctx")
	}
}

package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
)

func openQueue(t *testing.T) *events.Queue {
	t.Helper()
	q, err := events.OpenQueue(events.QueueConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestBatcherAcksOnlyOn202 wires Client.Send as the events.Sink.
func TestBatcherAcksOnlyOn202(t *testing.T) {
	tests := []struct {
		name      string
		script    []reply
		wantDepth int
	}{
		{"202 acks", []reply{{code: 202}}, 0},
		{"500 keeps events", []reply{{code: 500}}, 3},
		{"400 keeps events", []reply{{code: 400}}, 3},
		{"202 incoherent keeps events", []reply{{code: 202, body: `{"accepted_n":9}`}}, 3},
		{"426 keeps events", []reply{{code: 426}}, 3},
		{"409 sets the batch aside", []reply{{code: 409}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.script...)
			q := openQueue(t)
			for i := range 3 {
				if err := q.Append(events.Event{ID: "ev" + itoa(i)}); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := events.StartBatcher(ctx, q, h.client.Send, events.BatcherConfig{MaxEvents: 10, Interval: 10 * time.Millisecond})
			waitFor(t, "a delivery", func() bool { return len(h.fake.requests()) > 0 })
			if tt.wantDepth == 0 {
				waitFor(t, "ack", func() bool { return q.Depth() == 0 })
			}
			cancel()
			closeCtx, cc := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cc()
			_ = b.Close(closeCtx)
			if got := q.Depth(); got != tt.wantDepth {
				t.Fatalf("depth = %d, want %d", got, tt.wantDepth)
			}
		})
	}
}

// TestNeverBlocksRequestPath: with the control plane hung, Append (the only
// call on the request path) still returns immediately.
func TestNeverBlocksRequestPath(t *testing.T) {
	release := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	c, err := New(Config{BaseURL: srv.URL, APIKey: testKey, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	q := openQueue(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := events.StartBatcher(ctx, q, c.Send, events.BatcherConfig{MaxEvents: 1, Interval: 10 * time.Millisecond})
	if err := q.Append(events.Event{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the hung request", func() bool { return hits.Load() > 0 })
	start := time.Now()
	for i := range 100 {
		if err := q.Append(events.Event{ID: "ev" + itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Append blocked for %v while ingest was hung", d)
	}
	cancel()
	closeCtx, cc := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cc()
	_ = b.Close(closeCtx)
}

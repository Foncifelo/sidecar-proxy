package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// recordingSink records delivered batches; it fails the first `fail` calls.
type recordingSink struct {
	mu      sync.Mutex
	fail    int
	calls   int
	batches []Batch
	got     chan struct{}
}

func newSink(fail int) *recordingSink {
	return &recordingSink{fail: fail, got: make(chan struct{}, 1000)}
}

func (s *recordingSink) deliver(_ context.Context, b Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.batches = append(s.batches, b)
	s.got <- struct{}{}
	if s.calls <= s.fail {
		return errors.New("ingest unavailable")
	}
	return nil
}

func (s *recordingSink) snapshot() []Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Batch(nil), s.batches...)
}

func waitCalls(t *testing.T, s *recordingSink, n int, within time.Duration) {
	t.Helper()
	timeout := time.After(within)
	for i := 0; i < n; i++ {
		select {
		case <-s.got:
		case <-timeout:
			t.Fatalf("timed out waiting for %d sink calls (got %d)", n, i)
		}
	}
}

func TestBatcherDelivery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		appended  int
		cfg       BatcherConfig
		wantSizes []int
		within    time.Duration
	}{
		// Interval is an hour: only the size trigger can fire.
		{"full batch of 100 goes out immediately", 250, BatcherConfig{Interval: time.Hour}, []int{100, 100}, 2 * time.Second},
		// Fewer than MaxEvents: only the interval can fire.
		{"partial batch on interval", 7, BatcherConfig{Interval: 20 * time.Millisecond}, []int{7}, 2 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := openTestQueue(t, t.TempDir(), QueueConfig{})
			s := newSink(0)
			appendN(t, q, 0, tc.appended) // before start, so a tick cannot split the input
			b := StartBatcher(context.Background(), q, s.deliver, tc.cfg)
			t.Cleanup(func() { _ = b.Close(context.Background()) })
			signal(q.notify) // what Append does once the batcher is running
			waitCalls(t, s, len(tc.wantSizes), tc.within)
			got := s.snapshot()
			for i, want := range tc.wantSizes {
				if len(got[i].Events) != want {
					t.Fatalf("batch %d has %d events, want %d", i, len(got[i].Events), want)
				}
			}
		})
	}
}

func TestBatcherRetriesWithSameID(t *testing.T) {
	t.Parallel()
	q := openTestQueue(t, t.TempDir(), QueueConfig{})
	s := newSink(2)
	b := StartBatcher(context.Background(), q, s.deliver, BatcherConfig{Interval: 10 * time.Millisecond})
	appendN(t, q, 0, 3)
	waitCalls(t, s, 3, 2*time.Second)
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := s.snapshot()
	if got[0].ID != got[1].ID || got[1].ID != got[2].ID {
		t.Fatalf("retries must reuse batch_id: %q %q %q", got[0].ID, got[1].ID, got[2].ID)
	}
	if q.Depth() != 0 {
		t.Fatalf("depth %d after successful retry", q.Depth())
	}
}

func TestBatcherCloseDrains(t *testing.T) {
	t.Parallel()
	q := openTestQueue(t, t.TempDir(), QueueConfig{})
	s := newSink(0)
	b := StartBatcher(context.Background(), q, s.deliver, BatcherConfig{Interval: time.Hour})
	appendN(t, q, 0, 130)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	total := 0
	for _, bt := range s.snapshot() {
		total += len(bt.Events)
	}
	if total != 130 || q.Depth() != 0 {
		t.Fatalf("delivered %d, depth %d; want 130 and 0", total, q.Depth())
	}
}

func TestBatcherCloseDeadline(t *testing.T) {
	t.Parallel()
	q := openTestQueue(t, t.TempDir(), QueueConfig{})
	s := newSink(1 << 30) // always fails
	b := StartBatcher(context.Background(), q, s.deliver, BatcherConfig{Interval: time.Hour})
	appendN(t, q, 0, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := b.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close with failing sink: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close ignored its deadline: %v", d)
	}
	if q.Depth() != 5 {
		t.Fatalf("undelivered events must stay queued, depth %d", q.Depth())
	}
}

func TestBatcherCloseExpiredContext(t *testing.T) {
	t.Parallel()
	q := openTestQueue(t, t.TempDir(), QueueConfig{})
	s := newSink(0)
	b := StartBatcher(context.Background(), q, s.deliver, BatcherConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	appendN(t, q, 0, 1)
	if err := b.Close(ctx); err == nil && q.Depth() != 0 {
		t.Fatal("Close with cancelled ctx left events without reporting an error")
	}
}

func TestBatcherStopsOnParentContext(t *testing.T) {
	t.Parallel()
	q := openTestQueue(t, t.TempDir(), QueueConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	b := StartBatcher(ctx, q, newSink(0).deliver, BatcherConfig{})
	cancel()
	select {
	case <-b.done:
	case <-time.After(2 * time.Second):
		t.Fatal("batcher did not stop when its context was cancelled")
	}
}

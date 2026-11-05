package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// setAckBlocked makes every write of the ack cursor fail, as a full or
// read-only disk would: writeFileAtomic cannot open a directory as its temp
// file. It works for root too, unlike a chmod. Safe off the test goroutine.
func setAckBlocked(dir string, blocked bool) error {
	path := filepath.Join(dir, cursorFile+".tmp")
	if blocked {
		return os.Mkdir(path, dirPerm)
	}
	return os.Remove(path)
}

// blockAck is setAckBlocked(dir, true) for the test goroutine.
func blockAck(t *testing.T, dir string) {
	t.Helper()
	if err := setAckBlocked(dir, true); err != nil {
		t.Fatal(err)
	}
}

// With the real clock, a persistent disk error must not make the batcher
// redeliver at its tick rate: after the first failed ack it waits at least
// half of diskBackoffBase.
func TestBatcherDiskErrorRealClock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q := openTestQueue(t, dir, QueueConfig{})
	appendN(t, q, 0, 1)
	blockAck(t, dir)
	s := newSink(0)
	b := StartBatcher(context.Background(), q, s.deliver, BatcherConfig{Interval: 5 * time.Millisecond})
	time.Sleep(300 * time.Millisecond)
	b.cancel()
	<-b.done
	if n := len(s.snapshot()); n < 1 || n > 2 {
		t.Fatalf("%d deliveries in 300ms with a failing disk; want 1 or 2", n)
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeClock records each backoff wait. The first `fire` waits end at once;
// later ones never end, which parks the loop until it is stopped.
type fakeClock struct {
	mu    sync.Mutex
	fire  int
	waits []time.Duration
	hit   chan struct{}
}

func newFakeClock(fire int) *fakeClock {
	return &fakeClock{fire: fire, hit: make(chan struct{}, 64)}
}

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, d)
	c.hit <- struct{}{}
	if len(c.waits) > c.fire {
		return nil
	}
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

func (c *fakeClock) recorded() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.waits...)
}

func noJitter(limit time.Duration) time.Duration { return limit }

// scriptedSink accepts every batch and, before the batcher acks call i, makes
// the ack cursor fail if failAck[i] (the last entry holds afterwards). It
// runs on the batcher goroutine, so it reports with t.Error, never t.Fatal.
func scriptedSink(t *testing.T, dir string, failAck []bool) Sink {
	blocked := false
	calls := 0
	return func(context.Context, Batch) error {
		want := failAck[min(calls, len(failAck)-1)]
		calls++
		if want != blocked {
			if err := setAckBlocked(dir, want); err != nil {
				t.Error(err)
				return nil
			}
			blocked = want
		}
		return nil
	}
}

func TestBatcherDiskBackoff(t *testing.T) {
	t.Parallel()
	s := time.Second
	tests := []struct {
		name      string
		events    int
		failAck   []bool
		wantWaits []time.Duration
		wantWarn  int
		wantInfo  int
	}{
		{
			name:      "persistent error grows to the cap",
			events:    1,
			failAck:   []bool{true},
			wantWaits: []time.Duration{1 * s, 2 * s, 4 * s, 8 * s, 16 * s, 32 * s, 60 * s, 60 * s, 60 * s},
			wantWarn:  1,
		},
		{
			name:      "successful ack resets the backoff",
			events:    2,
			failAck:   []bool{true, true, true, false, true},
			wantWaits: []time.Duration{1 * s, 2 * s, 4 * s, 1 * s},
			wantWarn:  2,
			wantInfo:  1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			q := openTestQueue(t, dir, QueueConfig{})
			appendN(t, q, 0, tc.events)
			clock := newFakeClock(len(tc.wantWaits) - 1)
			logs := &syncBuffer{}
			cfg := BatcherConfig{MaxEvents: 1, Interval: time.Millisecond, Logger: slog.New(slog.NewTextHandler(logs, nil))}
			b := startBatcher(context.Background(), q, scriptedSink(t, dir, tc.failAck), cfg,
				batcherSeams{after: clock.after, jitter: noJitter})
			for range tc.wantWaits {
				select {
				case <-clock.hit:
				case <-time.After(5 * time.Second):
					t.Fatalf("waits so far %v, want %v", clock.recorded(), tc.wantWaits)
				}
			}
			b.cancel()
			<-b.done

			if got := clock.recorded(); !slices.Equal(got, tc.wantWaits) {
				t.Errorf("waits %v, want %v", got, tc.wantWaits)
			}
			if got := q.DiskErrors(); got != uint64(len(tc.wantWaits)) {
				t.Errorf("DiskErrors %d, want %d", got, len(tc.wantWaits))
			}
			out := logs.String()
			if got := strings.Count(out, "level=WARN"); got != tc.wantWarn {
				t.Errorf("%d warnings, want %d:\n%s", got, tc.wantWarn, out)
			}
			if got := strings.Count(out, "level=INFO"); got != tc.wantInfo {
				t.Errorf("%d info lines, want %d:\n%s", got, tc.wantInfo, out)
			}
			if strings.Contains(out, dir) || strings.Contains(out, "ev-0000") {
				t.Errorf("log carries a path or event data:\n%s", out)
			}
		})
	}
}

// A batcher waiting out a disk backoff stops at once on cancel or Close.
func TestBatcherDiskBackoffStops(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		stop func(b *Batcher)
	}{
		{"context cancelled", func(b *Batcher) { b.cancel() }},
		{"Close", func(b *Batcher) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_ = b.Close(ctx) // the drain fails too: the disk is still broken
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			q := openTestQueue(t, dir, QueueConfig{})
			appendN(t, q, 0, 1)
			blockAck(t, dir)
			clock := newFakeClock(0) // the first wait never ends
			b := startBatcher(context.Background(), q, newSink(0).deliver, BatcherConfig{Interval: time.Millisecond},
				batcherSeams{after: clock.after, jitter: noJitter})
			select {
			case <-clock.hit:
			case <-time.After(5 * time.Second):
				t.Fatal("batcher never backed off")
			}
			start := time.Now()
			tc.stop(b)
			select {
			case <-b.done:
			case <-time.After(2 * time.Second):
				t.Fatal("batcher did not stop while backing off")
			}
			if d := time.Since(start); d > time.Second {
				t.Errorf("stopping took %v", d)
			}
			if got := clock.recorded(); !slices.Equal(got, []time.Duration{diskBackoffBase}) {
				t.Errorf("waits %v, want [%v]", got, diskBackoffBase)
			}
		})
	}
}

func TestBackoffLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, time.Second}, {1, 2 * time.Second}, {5, 32 * time.Second}, {6, 60 * time.Second}, {1000, 60 * time.Second},
	}
	for _, tc := range tests {
		if got := backoffLimit(diskBackoffBase, diskBackoffMax, tc.attempt); got != tc.want {
			t.Errorf("backoffLimit(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestEqualJitter(t *testing.T) {
	t.Parallel()
	for _, limit := range []time.Duration{time.Second, diskBackoffMax} {
		for range 1000 {
			if d := equalJitter(limit); d < limit/2 || d >= limit {
				t.Fatalf("equalJitter(%v) = %v, want in [%v, %v)", limit, d, limit/2, limit)
			}
		}
	}
	if d := equalJitter(1); d != 1 {
		t.Errorf("equalJitter(1ns) = %v", d)
	}
}

func TestDiskCause(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"path error", fmt.Errorf("events: ack cursor: %w", &fs.PathError{Op: "open", Path: "/secret/dir/ack.json.tmp", Err: syscall.ENOSPC}), "open: " + syscall.ENOSPC.Error()},
		{"link error", &os.LinkError{Op: "rename", Old: "/secret/a", New: "/secret/b", Err: syscall.EACCES}, "rename: " + syscall.EACCES.Error()},
		{"sentinel", fmt.Errorf("append: %w", ErrQueueFull), ErrQueueFull.Error()},
		{"paused", ErrAppendPaused, ErrAppendPaused.Error()},
		{"unknown", errors.New("sink: token sk-fictitious at /secret/x"), unknownCause},
	}
	for _, tc := range tests {
		if got := DiskCause(tc.err); got != tc.want {
			t.Errorf("%s: DiskCause = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// DiskErrors also counts a failed append, which the proxy path drops and
// counts in DroppedEvents as before.
func TestQueueDiskErrorsCount(t *testing.T) {
	t.Parallel()
	q := openTestQueue(t, t.TempDir(), QueueConfig{})
	q.mu.Lock()
	_ = q.f.Close() // the next write fails with "file already closed"
	q.mu.Unlock()
	if err := q.Append(testEvent(0)); err == nil {
		t.Fatal("Append on a closed segment succeeded")
	}
	if got := q.DroppedEvents(); got != 1 {
		t.Errorf("DroppedEvents %d, want 1", got)
	}
	if got := q.DiskErrors(); got != 1 {
		t.Errorf("DiskErrors after append %d, want 1", got)
	}
}

// A batch the receiver rejects for good, with rejected/ unwritable, stays
// outstanding and the batcher backs off as for a failed ack.
func TestBatcherRejectDiskBackoff(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q := openTestQueue(t, dir, QueueConfig{})
	appendN(t, q, 0, 1)
	// A file where rejected/ should be: park cannot create the directory.
	if err := os.WriteFile(filepath.Join(dir, rejectedDir), nil, filePerm); err != nil {
		t.Fatal(err)
	}
	reject := func(context.Context, Batch) error { return fmt.Errorf("conflict: %w", ErrRejected) }
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	clock := newFakeClock(len(want) - 1)
	b := startBatcher(context.Background(), q, reject, BatcherConfig{Interval: time.Millisecond},
		batcherSeams{after: clock.after, jitter: noJitter})
	for range want {
		select {
		case <-clock.hit:
		case <-time.After(5 * time.Second):
			t.Fatalf("waits so far %v, want %v", clock.recorded(), want)
		}
	}
	b.cancel()
	<-b.done
	if got := clock.recorded(); !slices.Equal(got, want) {
		t.Errorf("waits %v, want %v", got, want)
	}
	if q.Depth() != 1 || !q.hasPending() {
		t.Errorf("depth %d, pending %v; want the batch still outstanding", q.Depth(), q.hasPending())
	}
	if got := q.RejectedBatches(); got != 0 {
		t.Errorf("RejectedBatches %d, want 0", got)
	}
}

// A failure to remove pending.json after the cursor was written releases the
// batch anyway: it is counted and logged once, but the batcher does not back
// off and keeps delivering.
func TestBatcherCleanupErrorNoBackoff(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q := openTestQueue(t, dir, QueueConfig{})
	// A non-empty directory where pending.json goes: writing the marker and
	// removing it both fail, the ack cursor does not.
	pendingDir := filepath.Join(dir, pendingFile)
	if err := os.MkdirAll(filepath.Join(pendingDir, "x"), dirPerm); err != nil {
		t.Fatal(err)
	}
	const n = 3
	appendN(t, q, 0, n)
	clock := newFakeClock(0)
	logs := &syncBuffer{}
	s := newSink(0)
	cfg := BatcherConfig{MaxEvents: 1, Interval: time.Hour, Logger: slog.New(slog.NewTextHandler(logs, nil))}
	b := startBatcher(context.Background(), q, s.deliver, cfg, batcherSeams{after: clock.after, jitter: noJitter})
	signal(q.notify)
	waitCalls(t, s, n, 5*time.Second)
	waitFor(t, func() bool { return q.Depth() == 0 })

	if err := os.RemoveAll(pendingDir); err != nil {
		t.Fatal(err)
	}
	appendN(t, q, n, 1)
	waitCalls(t, s, 1, 5*time.Second)
	waitFor(t, func() bool { return strings.Contains(logs.String(), "cleanup recovered") })
	b.cancel()
	<-b.done

	if got := clock.recorded(); len(got) != 0 {
		t.Errorf("backed off %v on cleanup errors", got)
	}
	// Per failing batch: the pending marker write and its removal.
	if got := q.DiskErrors(); got != 2*n {
		t.Errorf("DiskErrors %d, want %d", got, 2*n)
	}
	out := logs.String()
	if got := strings.Count(out, "level=WARN"); got != 1 {
		t.Errorf("%d warnings, want 1:\n%s", got, out)
	}
	if got := strings.Count(out, "level=INFO"); got != 1 {
		t.Errorf("%d info lines, want 1:\n%s", got, out)
	}
	if strings.Contains(out, dir) {
		t.Errorf("log carries a path:\n%s", out)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 5s")
		}
		time.Sleep(time.Millisecond)
	}
}

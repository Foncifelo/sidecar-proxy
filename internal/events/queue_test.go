package events

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func testEvent(i int) Event {
	return Event{ID: fmt.Sprintf("ev-%04d", i), Mode: ModeShadow, RawModel: "fictitious-model", Attempts: []Attempt{}}
}

func openTestQueue(t *testing.T, dir string, cfg QueueConfig) *Queue {
	t.Helper()
	cfg.Dir = dir
	q, err := OpenQueue(cfg)
	if err != nil {
		t.Fatalf("OpenQueue: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func appendN(t *testing.T, q *Queue, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		if err := q.Append(testEvent(i)); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
}

func ids(evs []Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

func segFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "seg-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestIDGenMonotonic(t *testing.T) {
	t.Parallel()
	base := time.UnixMilli(1_700_000_000_000)
	tests := []struct {
		name  string
		times []time.Time
	}{
		{"same millisecond", []time.Time{base, base, base, base}},
		{"increasing", []time.Time{base, base.Add(time.Millisecond), base.Add(time.Second)}},
		{"clock goes backwards", []time.Time{base, base.Add(-time.Second), base.Add(-time.Hour), base}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewIDGen(nil)
			prev := ""
			for _, ts := range tc.times {
				id, err := g.New(ts)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ulid.ParseStrict(id); err != nil {
					t.Fatalf("not a ULID %q: %v", id, err)
				}
				if id <= prev {
					t.Fatalf("not monotonic: %q after %q", id, prev)
				}
				prev = id
			}
		})
	}
}

func TestIDGenConcurrent(t *testing.T) {
	t.Parallel()
	g := NewIDGen(nil)
	const workers, per = 8, 200
	var mu sync.Mutex
	seen := make(map[string]bool, workers*per)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				id, err := g.New(time.Now())
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				seen[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != workers*per {
		t.Fatalf("got %d unique ids, want %d", len(seen), workers*per)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

func TestIDGenEntropyError(t *testing.T) {
	t.Parallel()
	if _, err := NewIDGen(failingReader{}).New(time.Now()); err == nil {
		t.Fatal("want error from failing entropy")
	}
}

func TestQueueNextAck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		appended  int
		max       int
		wantSizes []int
	}{
		{"empty", 0, 100, nil},
		{"one partial batch", 3, 100, []int{3}},
		{"exact batch", 5, 5, []int{5}},
		{"several batches", 7, 3, []int{3, 3, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := openTestQueue(t, t.TempDir(), QueueConfig{})
			appendN(t, q, 0, tc.appended)
			var got []int
			next := 0
			for {
				b, ok := q.Next(tc.max)
				if !ok {
					break
				}
				again, _ := q.Next(tc.max)
				if again.ID != b.ID {
					t.Fatalf("outstanding batch must keep its id: %q vs %q", again.ID, b.ID)
				}
				for _, e := range b.Events {
					if e.ID != testEvent(next).ID {
						t.Fatalf("order: got %s want %s", e.ID, testEvent(next).ID)
					}
					next++
				}
				got = append(got, len(b.Events))
				if err := q.Ack(b.ID); err != nil {
					t.Fatal(err)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.wantSizes) {
				t.Fatalf("batch sizes %v, want %v", got, tc.wantSizes)
			}
			if q.Depth() != 0 {
				t.Fatalf("depth %d after acking all", q.Depth())
			}
		})
	}
}

func TestQueueAckErrors(t *testing.T) {
	t.Parallel()
	q := openTestQueue(t, t.TempDir(), QueueConfig{})
	if err := q.Ack("nope"); !errors.Is(err, ErrUnknownBatch) {
		t.Fatalf("ack without batch: %v", err)
	}
	appendN(t, q, 0, 1)
	b, _ := q.Next(10)
	if err := q.Ack(b.ID + "x"); !errors.Is(err, ErrUnknownBatch) {
		t.Fatalf("ack wrong id: %v", err)
	}
	if err := q.Ack(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := q.Ack(b.ID); !errors.Is(err, ErrUnknownBatch) {
		t.Fatalf("double ack: %v", err)
	}
	if _, ok := q.Next(0); ok {
		t.Fatal("Next(0) must return nothing")
	}
}

func TestQueueFullDrops(t *testing.T) {
	t.Parallel()
	line, _ := encodedSize(testEvent(0))
	q := openTestQueue(t, t.TempDir(), QueueConfig{MaxBytes: 3 * line})
	appendN(t, q, 0, 3)
	for i := range 2 {
		start := time.Now()
		if err := q.Append(testEvent(3 + i)); !errors.Is(err, ErrQueueFull) {
			t.Fatalf("want ErrQueueFull, got %v", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("Append blocked for %v on a full queue", d)
		}
	}
	if got := q.DroppedEvents(); got != 2 {
		t.Fatalf("DroppedEvents = %d, want 2", got)
	}
	b, _ := q.Next(1)
	if err := q.Ack(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := q.Append(testEvent(9)); err != nil {
		t.Fatalf("space freed by ack, Append: %v", err)
	}
}

func encodedSize(ev Event) (int64, error) {
	b, err := jsonLine(ev)
	return int64(len(b)), err
}

func TestQueueRotatesAndDeletesAckedSegments(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	line, _ := encodedSize(testEvent(0))
	q := openTestQueue(t, dir, QueueConfig{SegmentBytes: 2 * line})
	appendN(t, q, 0, 6)
	if n := len(segFiles(t, dir)); n != 3 {
		t.Fatalf("segments = %d, want 3", n)
	}
	b, _ := q.Next(4)
	if err := q.Ack(b.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(segFiles(t, dir)); n != 1 {
		t.Fatalf("segments after ack = %d, want 1", n)
	}
}

func TestQueueSyncTriggers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  QueueConfig
		n    int
	}{
		{"by count", QueueConfig{SyncEvery: 2, SyncInterval: time.Hour}, 2},
		{"by interval", QueueConfig{SyncEvery: 1000, SyncInterval: 5 * time.Millisecond}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := openTestQueue(t, t.TempDir(), tc.cfg)
			appendN(t, q, 0, tc.n)
			deadline := time.Now().Add(2 * time.Second)
			for {
				q.mu.Lock()
				u := q.unsynced
				q.mu.Unlock()
				if u == 0 {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("still %d unsynced events", u)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestQueueClosed(t *testing.T) {
	t.Parallel()
	q := openTestQueue(t, t.TempDir(), QueueConfig{})
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := q.Append(testEvent(0)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Append after Close: %v", err)
	}
	if _, err := OpenQueue(QueueConfig{}); err == nil {
		t.Fatal("OpenQueue without dir must fail")
	}
}

func TestQueueRecovery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		appended    int
		ackFirst    int    // events acknowledged before the crash
		pendingOnly int    // events taken by an unacked batch before the crash
		tail        string // garbage written to the newest segment
		wantIDs     int    // events redelivered after reopening
		wantCorrupt uint64
	}{
		{"clean", 5, 0, 0, "", 5, 0},
		{"acked are not redelivered", 5, 3, 0, "", 2, 0},
		{"all acked", 4, 4, 0, "", 0, 0},
		{"unacked batch is redelivered", 5, 0, 3, "", 5, 0},
		{"torn last line dropped and counted", 4, 1, 0, `{"id":"ev-trunc","mo`, 3, 1},
		{"invalid json line counted", 2, 0, 0, "not json\n", 2, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			q, err := OpenQueue(QueueConfig{Dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			appendN(t, q, 0, tc.appended)
			if tc.ackFirst > 0 {
				b, _ := q.Next(tc.ackFirst)
				if err := q.Ack(b.ID); err != nil {
					t.Fatal(err)
				}
			}
			var pendingID string
			if tc.pendingOnly > 0 {
				b, _ := q.Next(tc.pendingOnly)
				pendingID = b.ID
			}
			if err := q.Close(); err != nil {
				t.Fatal(err)
			}
			if tc.tail != "" {
				segs := segFiles(t, dir)
				sort.Strings(segs)
				f, err := os.OpenFile(segs[len(segs)-1], os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString(tc.tail); err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
			}

			q2 := openTestQueue(t, dir, QueueConfig{})
			if got := q2.CorruptLines(); got != tc.wantCorrupt {
				t.Fatalf("CorruptLines = %d, want %d", got, tc.wantCorrupt)
			}
			if q2.Depth() != tc.wantIDs {
				t.Fatalf("Depth = %d, want %d", q2.Depth(), tc.wantIDs)
			}
			appendN(t, q2, 100, 1) // new writes go after recovered ones
			// An unacked batch comes back first with its original ID and events
			// (same batch_id after a restart); the rest follows in order.
			var got []string
			batches := 0
			for {
				b, ok := q2.Next(1000)
				if !ok {
					break
				}
				if batches == 0 && pendingID != "" && (b.ID != pendingID || len(b.Events) != tc.pendingOnly) {
					t.Fatalf("first batch after restart = %s with %d events, want %s with %d",
						b.ID, len(b.Events), pendingID, tc.pendingOnly)
				}
				batches++
				got = append(got, ids(b.Events)...)
				if err := q2.Ack(b.ID); err != nil {
					t.Fatal(err)
				}
			}
			if batches == 0 {
				t.Fatal("expected a batch")
			}
			want := []string{}
			for i := tc.ackFirst; i < tc.appended; i++ {
				want = append(want, testEvent(i).ID)
			}
			want = append(want, testEvent(100).ID)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("redelivered %v, want %v", got, want)
			}
			if err := q2.Close(); err != nil {
				t.Fatal(err)
			}
			q3 := openTestQueue(t, dir, QueueConfig{})
			if q3.Depth() != 0 {
				t.Fatalf("after full ack and restart depth = %d", q3.Depth())
			}
			if n := len(segFiles(t, dir)); n != 1 {
				t.Fatalf("only the fresh segment should remain, got %d", n)
			}
		})
	}
}

func TestQueueRecoveryCorruptCursor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, cursorFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenQueue(QueueConfig{Dir: dir}); err == nil {
		t.Fatal("want error for corrupt cursor")
	}
}

func TestQueueConcurrentAppend(t *testing.T) {
	t.Parallel()
	q := openTestQueue(t, t.TempDir(), QueueConfig{SyncEvery: 3, SegmentBytes: 512})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			appendN(t, q, w*1000, 50)
		}()
	}
	delivered := 0
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for ctx.Err() == nil {
			if b, ok := q.Next(7); ok {
				if err := q.Ack(b.ID); err == nil {
					delivered += len(b.Events)
				}
			}
		}
	}()
	wg.Wait()
	cancel()
	<-readerDone
	if got := delivered + q.Depth(); got != 200 {
		t.Fatalf("delivered %d + depth %d != 200 appended", delivered, q.Depth())
	}
}

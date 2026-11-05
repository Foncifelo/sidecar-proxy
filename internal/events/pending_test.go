package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestQueuePendingSurvivesRestart: the batch outstanding at shutdown or crash
// comes back with the same batch_id, so the control plane deduplicates it in
// ingest_batches whatever ts it rewrote.
func TestQueuePendingSurvivesRestart(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		ack       bool   // ack the batch before the restart
		marker    string // replaces pending.json before reopening; "" keeps it
		wantSame  bool   // first batch after restart has the original ID
		wantFirst int    // events in the first batch after restart
	}{
		{"unacked keeps id and events", false, "", true, 3},
		{"acked leaves nothing pending", true, "", false, 2},
		{"corrupt marker falls back to a new id", false, "{", false, 5},
		{"marker past the surviving events is trimmed", false, "huge-offset", true, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			q, err := OpenQueue(QueueConfig{Dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			appendN(t, q, 0, 5)
			b, ok := q.Next(3)
			if !ok {
				t.Fatal("expected a batch")
			}
			if tc.ack {
				if err := q.Ack(b.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := q.Close(); err != nil {
				t.Fatal(err)
			}
			switch tc.marker {
			case "":
			case "huge-offset":
				data, _ := json.Marshal(pendingMarker{BatchID: b.ID, Segment: 1, Offset: 1 << 40})
				writeTestFile(t, filepath.Join(dir, pendingFile), string(data))
			default:
				writeTestFile(t, filepath.Join(dir, pendingFile), tc.marker)
			}

			q2 := openTestQueue(t, dir, QueueConfig{})
			got, ok := q2.Next(1000)
			if !ok {
				t.Fatal("expected a batch after restart")
			}
			if (got.ID == b.ID) != tc.wantSame || len(got.Events) != tc.wantFirst {
				t.Fatalf("after restart: id same=%v events=%d, want same=%v events=%d",
					got.ID == b.ID, len(got.Events), tc.wantSame, tc.wantFirst)
			}
			if err := q2.Ack(got.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, pendingFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("pending.json left after ack: %v", err)
			}
		})
	}
}

func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestQueueReject(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q := openTestQueue(t, dir, QueueConfig{MaxRejected: 2})
	appendN(t, q, 0, 4)
	rejected := make([]string, 0, 3)
	for range 3 {
		b, ok := q.Next(1)
		if !ok {
			t.Fatal("expected a batch")
		}
		if err := q.Reject(b.ID); err != nil {
			t.Fatalf("Reject: %v", err)
		}
		rejected = append(rejected, b.ID)
	}
	if err := q.Reject("unknown"); !errors.Is(err, ErrUnknownBatch) {
		t.Fatalf("Reject unknown = %v", err)
	}
	if q.Depth() != 1 || q.RejectedBatches() != 2 || q.RejectedDropped() != 1 {
		t.Fatalf("depth=%d rejected=%d dropped=%d, want 1 2 1", q.Depth(), q.RejectedBatches(), q.RejectedDropped())
	}
	for i, id := range rejected {
		path := filepath.Join(dir, rejectedDir, id+".json")
		data, err := os.ReadFile(path) //nolint:gosec // test temp dir
		if i == 2 {
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("batch beyond the limit was written: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var rb rejectedBatch
		if err := json.Unmarshal(data, &rb); err != nil || rb.BatchID != id || len(rb.Events) != 1 ||
			rb.Events[0].ID != testEvent(i).ID {
			t.Fatalf("rejected file %d = %+v (%v)", i, rb, err)
		}
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != filePerm {
			t.Fatalf("rejected file mode: %v %v", fi, err)
		}
	}
	// Rejected batches are not redelivered after a restart.
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	q2 := openTestQueue(t, dir, QueueConfig{})
	if q2.Depth() != 1 {
		t.Fatalf("depth after restart = %d, want 1", q2.Depth())
	}
}

// TestBatcherRejectedDoesNotBlock: a sink error wrapping ErrRejected sets the
// batch aside and the following batches still go out.
func TestBatcherRejectedDoesNotBlock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q := openTestQueue(t, dir, QueueConfig{})
	appendN(t, q, 0, 3)
	var delivered []string
	calls := 0
	sink := func(_ context.Context, b Batch) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("control plane answered 409: %w", ErrRejected)
		}
		delivered = append(delivered, ids(b.Events)...)
		return nil
	}
	b := StartBatcher(context.Background(), q, sink, BatcherConfig{MaxEvents: 1, Interval: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if q.Depth() != 0 || q.RejectedBatches() != 1 {
		t.Fatalf("depth=%d rejected=%d", q.Depth(), q.RejectedBatches())
	}
	if fmt.Sprint(delivered) != fmt.Sprint([]string{testEvent(1).ID, testEvent(2).ID}) {
		t.Fatalf("delivered %v", delivered)
	}
}

package events

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// errNoSpace is what a write to a full disk returns.
func errNoSpace(f *os.File) error {
	return &fs.PathError{Op: "write", Path: f.Name(), Err: syscall.ENOSPC}
}

// Fake writes for a full disk: nothing written, half the line written, or
// half the line written and the file closed so it cannot be truncated.
func writeNothing(f *os.File, _ []byte) (int, error) { return 0, errNoSpace(f) }

func writeHalf(f *os.File, b []byte) (int, error) {
	n, err := f.Write(b[:len(b)/2])
	if err != nil {
		return n, err
	}
	return n, errNoSpace(f)
}

func writeHalfAndClose(f *os.File, b []byte) (int, error) {
	n, _ := writeHalf(f, b)
	_ = f.Close()
	return n, errNoSpace(f)
}

// writeNothingEIO is a broken file: nothing written, and not for lack of space.
func writeNothingEIO(f *os.File, _ []byte) (int, error) {
	return 0, &fs.PathError{Op: "write", Path: f.Name(), Err: syscall.EIO}
}

// manualClock is read only under q.mu, from Append.
type manualClock struct{ t time.Time }

func (c *manualClock) now() time.Time { return c.t }

func setWrite(q *Queue, w func(*os.File, []byte) (int, error)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.write = w
}

func segmentFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "seg-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// appendStepped appends events from..from+n, moving the clock by step before
// each one, and returns how many failed.
func appendStepped(q *Queue, c *manualClock, from, n int, step time.Duration) int {
	failed := 0
	for i := from; i < from+n; i++ {
		q.mu.Lock()
		c.t = c.t.Add(step)
		q.mu.Unlock()
		if q.Append(testEvent(i)) != nil {
			failed++
		}
	}
	return failed
}

// A full disk must not create a segment per request nor retry the write per
// request: Append keeps an intact segment, pauses appendDiskPause from the
// second failure in a row and loses none of the events written before.
func TestAppendDiskFailure(t *testing.T) {
	t.Parallel()
	const (
		failing = 100
		step    = appendDiskPause / 10 // 10 appends per pause, 10 s in all
		// Disk attempts: the first two failures in a row, then one per pause
		// (at 1.2 s, 2.2 s … 9.2 s).
		attempts = pauseAfterFailures + failing/10 - 1
	)
	tests := []struct {
		name         string
		segmentBytes int64 // 0: default, no rotation
		write        func(*os.File, []byte) (int, error)
		block        bool // a directory where the next segment goes: rotation fails
		// closesFile: the fake write closes the segment, whose fsync then
		// fails too, so DiskErrors and Close are not checked.
		closesFile  bool
		wantSegs    int // segment files on disk at the end
		wantCorrupt uint64
	}{
		{name: "nothing written keeps the segment", write: writeNothing, wantSegs: 1},
		{name: "partial line is truncated", write: writeHalf, wantSegs: 1},
		// Two before, one failed rotation kept, one after recovery; before
		// the fix every failing append created a segment.
		{name: "full segment rotates once", segmentBytes: 1, write: writeNothing, wantSegs: 4},
		{name: "blocked rotation retries once per pause", segmentBytes: 1, block: true, wantSegs: 4},
		// One torn segment per attempt (never more), plus the one written
		// after recovery; recovery drops each torn line.
		{name: "torn line that cannot be truncated rotates once per attempt", write: writeHalfAndClose, closesFile: true, wantSegs: attempts + 1, wantCorrupt: attempts},
		// Not a full disk: the segment is rotated after every third failed
		// write (attempts 4, 7 and 10), the last one is written on recovery.
		{name: "broken segment rotates every third failure", write: writeNothingEIO, wantSegs: 1 + (attempts-1)/rotateAfterWriteFailures},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			q := openTestQueue(t, dir, QueueConfig{SegmentBytes: tc.segmentBytes})
			clock := &manualClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
			q.mu.Lock()
			q.now = clock.now
			q.mu.Unlock()

			appendN(t, q, 0, 2)
			blocked := filepath.Join(dir, segmentName(3))
			if tc.block {
				if err := os.Mkdir(blocked, dirPerm); err != nil {
					t.Fatal(err)
				}
			} else {
				setWrite(q, tc.write)
			}
			if got := appendStepped(q, clock, 2, failing, step); got != failing {
				t.Fatalf("%d of %d appends failed on a full disk", got, failing)
			}
			if got, want := q.DroppedEvents(), uint64(failing); got != want {
				t.Errorf("DroppedEvents %d, want %d", got, want)
			}
			if !tc.closesFile {
				if got, want := q.DiskErrors(), uint64(attempts); got != want {
					t.Errorf("DiskErrors %d, want %d", got, want)
				}
			}

			// The disk recovers.
			if tc.block {
				if err := os.Remove(blocked); err != nil {
					t.Fatal(err)
				}
			}
			setWrite(q, (*os.File).Write)
			if got := appendStepped(q, clock, 2+failing, 1, appendDiskPause); got != 0 {
				t.Fatal("append after recovery failed")
			}
			appendN(t, q, 3+failing, 1)
			want := []string{"ev-0000", "ev-0001", "ev-0102", "ev-0103"}
			if b, ok := q.Next(10); !ok || !slices.Equal(ids(b.Events), want) {
				t.Errorf("queued %v, want %v", ids(b.Events), want)
			}
			if got := len(segmentFiles(t, dir)); got != tc.wantSegs {
				t.Errorf("%d segment files, want %d", got, tc.wantSegs)
			}

			// What reached the disk is still a valid queue.
			if err := q.Close(); err != nil && !tc.closesFile {
				t.Errorf("Close: %v", err)
			}
			r := openTestQueue(t, dir, QueueConfig{})
			if b, ok := r.Next(10); !ok || !slices.Equal(ids(b.Events), want) {
				t.Errorf("recovered %v, want %v", ids(b.Events), want)
			}
			if got := r.CorruptLines(); got != tc.wantCorrupt {
				t.Errorf("CorruptLines %d, want %d", got, tc.wantCorrupt)
			}
		})
	}
}

// A single failure is retried by the next append; from the second failure
// in a row Append does not touch the disk until the pause ends.
func TestAppendPause(t *testing.T) {
	t.Parallel()
	ok := (*os.File).Write
	tests := []struct {
		name    string
		writes  []func(*os.File, []byte) (int, error) // one per append
		advance []time.Duration                       // clock move before each append
		want    []error                               // nil: stored; else errors.Is target
	}{
		{
			name:   "transient failure is not paused",
			writes: []func(*os.File, []byte) (int, error){writeNothing, ok, writeNothing, ok},
			want:   []error{syscall.ENOSPC, nil, syscall.ENOSPC, nil},
		},
		{
			name:   "second failure in a row pauses",
			writes: []func(*os.File, []byte) (int, error){writeNothing, writeNothing, ok, ok},
			want:   []error{syscall.ENOSPC, syscall.ENOSPC, ErrAppendPaused, ErrAppendPaused},
		},
		{
			name:    "pause ends and success resets the streak",
			writes:  []func(*os.File, []byte) (int, error){writeNothing, writeNothing, ok, writeNothing, ok},
			advance: []time.Duration{0, 0, appendDiskPause, 0, 0},
			want:    []error{syscall.ENOSPC, syscall.ENOSPC, nil, syscall.ENOSPC, nil},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := openTestQueue(t, t.TempDir(), QueueConfig{})
			clock := &manualClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
			q.mu.Lock()
			q.now = clock.now
			q.mu.Unlock()
			for i, w := range tc.writes {
				setWrite(q, w)
				if i < len(tc.advance) {
					q.mu.Lock()
					clock.t = clock.t.Add(tc.advance[i])
					q.mu.Unlock()
				}
				err := q.Append(testEvent(i))
				switch {
				case tc.want[i] == nil && err != nil:
					t.Fatalf("append %d: %v, want stored", i, err)
				case tc.want[i] != nil && !errors.Is(err, tc.want[i]):
					t.Fatalf("append %d: %v, want %v", i, err, tc.want[i])
				}
			}
		})
	}
}

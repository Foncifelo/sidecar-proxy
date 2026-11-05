package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Queue defaults.
const (
	DefaultMaxBytes     int64 = 64 << 20
	DefaultSegmentBytes int64 = 4 << 20
	DefaultSyncEvery          = 64
	DefaultSyncInterval       = 200 * time.Millisecond
	DefaultMaxRejected        = 16
)

// Append's handling of a failing disk. From the pauseAfterFailures-th
// consecutive failed segment write or rotation on, Append drops new events
// without touching the disk for appendDiskPause: a full disk then costs one
// failed write per pause, not one per request, and never one new segment file
// per request. A single transient failure is not followed by a pause.
//
// A segment whose write fails rotateAfterWriteFailures times in a row without
// writing a byte is rotated, unless the error is "no space" or "quota
// exceeded": a new file would not help there, but it can for a broken file
// descriptor or an I/O error.
const (
	appendDiskPause          = time.Second
	pauseAfterFailures       = 2
	rotateAfterWriteFailures = 3
)

const (
	dirPerm  = 0o700
	filePerm = 0o600
)

// Queue errors.
var (
	ErrQueueFull    = errors.New("events: queue full")
	ErrUnknownBatch = errors.New("events: unknown batch id")
	ErrClosed       = errors.New("events: queue closed")
	// ErrAppendPaused: Append dropped the event without touching the disk
	// because segment writes or rotations kept failing until less than
	// appendDiskPause ago.
	ErrAppendPaused = errors.New("events: append paused after a disk error")
	// ErrRejected, wrapped by a Sink error, means the control plane rejected
	// the batch for good (e.g. 409): the batcher sets it aside with
	// Queue.Reject instead of redelivering it forever.
	ErrRejected = errors.New("events: batch rejected by the receiver")

	// errReleaseCleanup wraps a failure to remove pending.json or acked
	// segments after the ack cursor was written: the batch is released anyway
	// and the leftover files are ignored or removed later.
	errReleaseCleanup = errors.New("events: cleanup after release failed")
)

// QueueConfig configures a Queue. Zero values take the defaults above.
type QueueConfig struct {
	Dir          string        // directory owned by the queue
	MaxBytes     int64         // cap on unacknowledged data; beyond it Append drops
	SegmentBytes int64         // a segment rotates when the next line would exceed this
	SyncEvery    int           // fsync after this many appended events
	SyncInterval time.Duration // and at least this often while there are unsynced writes
	IDs          *IDGen        // batch_id generator; nil means NewIDGen(nil)
	MaxRejected  int           // batches kept in rejected/; beyond it they are discarded and counted
}

// Batch is a group of events delivered together. Its ID is a ULID used as the
// ingest batch_id, so a redelivered batch keeps the same ID (idempotent ingest).
type Batch struct {
	ID     string
	Events []Event
}

type record struct {
	ev   Event
	seg  uint64
	end  int64 // offset just past the line's newline
	size int64
}

type pendingBatch struct {
	batch Batch
	n     int
}

// Queue is a local, durable, append-only event queue made of JSON Lines
// segments. Append never waits for fsync and never blocks on a full queue:
// it drops and counts instead. Only one batch is outstanding at a time.
type Queue struct {
	cfg QueueConfig

	mu       sync.Mutex
	closed   bool
	seg      uint64
	f        *os.File
	segSize  int64
	segs     []uint64 // segment numbers present on disk, ascending
	recs     []record // unacknowledged events, oldest first
	used     int64
	pending  *pendingBatch
	unsynced int
	retired  []*os.File
	syncErr  error
	// pausedUntil: after repeated failed segment writes or rotations, Append
	// drops without touching the disk until this time.
	pausedUntil time.Time
	failStreak  int // consecutive failed segment writes or rotations
	zeroWrites  int // consecutive failed writes to q.f that wrote no byte
	// now and write are seams for tests; production uses time.Now and
	// (*os.File).Write.
	now   func() time.Time
	write func(f *os.File, b []byte) (int, error)

	ackMu sync.Mutex

	dropped         atomic.Uint64
	corrupt         atomic.Uint64
	rejected        atomic.Uint64
	rejectedDropped atomic.Uint64
	diskErrors      atomic.Uint64

	syncCh chan struct{}
	notify chan struct{}
	stop   chan struct{}
	done   chan struct{}
}

// OpenQueue opens (or creates) the queue in cfg.Dir, recovers unacknowledged
// events and starts a fresh segment for new writes.
func OpenQueue(cfg QueueConfig) (*Queue, error) {
	if cfg.Dir == "" {
		return nil, errors.New("events: queue dir is required")
	}
	cfg = withQueueDefaults(cfg)
	if err := os.MkdirAll(cfg.Dir, dirPerm); err != nil {
		return nil, fmt.Errorf("events: create queue dir: %w", err)
	}
	q := &Queue{
		cfg:    cfg,
		syncCh: make(chan struct{}, 1),
		notify: make(chan struct{}, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		now:    time.Now,
		write:  (*os.File).Write,
	}
	if err := q.recover(); err != nil {
		return nil, err
	}
	if err := q.openSegment(); err != nil {
		return nil, err
	}
	go q.syncLoop()
	return q, nil
}

func withQueueDefaults(cfg QueueConfig) QueueConfig {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.SegmentBytes <= 0 {
		cfg.SegmentBytes = DefaultSegmentBytes
	}
	if cfg.SyncEvery <= 0 {
		cfg.SyncEvery = DefaultSyncEvery
	}
	if cfg.SyncInterval <= 0 {
		cfg.SyncInterval = DefaultSyncInterval
	}
	if cfg.IDs == nil {
		cfg.IDs = NewIDGen(nil)
	}
	if cfg.MaxRejected <= 0 {
		cfg.MaxRejected = DefaultMaxRejected
	}
	return cfg
}

func segmentName(seq uint64) string { return fmt.Sprintf("seg-%020d.jsonl", seq) }

// openSegment starts segment q.seg+1. Caller holds q.mu or has exclusive access.
func (q *Queue) openSegment() error {
	next := q.seg + 1
	f, err := os.OpenFile(filepath.Join(q.cfg.Dir, segmentName(next)), os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("events: open segment: %w", err)
	}
	if q.f != nil {
		q.retired = append(q.retired, q.f) // fsynced and closed by the sync loop
	}
	q.f, q.seg, q.segSize, q.zeroWrites = f, next, 0, 0
	q.segs = append(q.segs, next)
	return nil
}

// Append stores ev at the end of the queue. It returns ErrQueueFull (and
// increments DroppedEvents) when the queue is at MaxBytes. It never waits for fsync.
//
// A failed write keeps the current segment when no byte of the line reached
// it, or when the partial line can be truncated away; only a segment left
// with a partial line, or one that keeps failing with an error other than "no
// space" (rotateAfterWriteFailures), is rotated. From the second consecutive
// failure on, Append pauses for appendDiskPause (ErrAppendPaused), so a full
// disk neither creates a segment per request nor retries the write on every
// request.
func (q *Queue) Append(ev Event) error {
	line, err := jsonLine(ev)
	if err != nil {
		q.dropped.Add(1)
		return err
	}
	size := int64(len(line))

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		q.dropped.Add(1)
		return ErrClosed
	}
	if q.used+size > q.cfg.MaxBytes {
		q.dropped.Add(1)
		return ErrQueueFull
	}
	if !q.pausedUntil.IsZero() && q.now().Before(q.pausedUntil) {
		q.dropped.Add(1)
		return ErrAppendPaused
	}
	if q.segSize > 0 && q.segSize+size > q.cfg.SegmentBytes {
		if err := q.openSegment(); err != nil {
			return q.appendFailed(err)
		}
	}
	n, err := q.write(q.f, line)
	if err != nil {
		q.keepOrRetire(n, err)
		return q.appendFailed(fmt.Errorf("events: write segment: %w", err))
	}
	q.pausedUntil, q.failStreak, q.zeroWrites = time.Time{}, 0, 0
	q.segSize += int64(n)
	q.recs = append(q.recs, record{ev: ev, seg: q.seg, end: q.segSize, size: size})
	q.used += size
	q.unsynced++
	if q.unsynced >= q.cfg.SyncEvery {
		signal(q.syncCh)
	}
	signal(q.notify)
	return nil
}

// keepOrRetire handles a write that failed with werr after n bytes. With
// n == 0 the segment is intact and kept, unless it failed
// rotateAfterWriteFailures times in a row with an error other than "no
// space". Otherwise the partial line is truncated away; if that fails too,
// the segment must never be written again (a later line would be glued to the
// torn one). A segment that must not be kept is marked full and rotates on
// the next attempt. Caller holds q.mu.
func (q *Queue) keepOrRetire(n int, werr error) {
	if n <= 0 {
		q.zeroWrites++
		if q.zeroWrites >= rotateAfterWriteFailures && !noSpace(werr) {
			q.segSize = q.cfg.SegmentBytes
		}
		return
	}
	q.zeroWrites = 0
	if err := q.f.Truncate(q.segSize); err != nil {
		q.segSize = q.cfg.SegmentBytes
	}
}

// noSpace reports whether err means the disk or the quota is full.
func noSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// appendFailed counts a failed segment write or rotation and, from the
// pauseAfterFailures-th in a row, pauses Append. Caller holds q.mu.
func (q *Queue) appendFailed(err error) error {
	q.dropped.Add(1)
	q.diskErrors.Add(1)
	q.failStreak++
	if q.failStreak >= pauseAfterFailures {
		q.pausedUntil = q.now().Add(appendDiskPause)
	}
	return err
}

func jsonLine(ev Event) ([]byte, error) {
	line, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("events: encode event: %w", err)
	}
	return append(line, '\n'), nil
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Next returns the outstanding batch if there is one (same ID), otherwise a new
// batch of up to max events. ok is false when there is nothing to deliver.
//
// A new batch is persisted (pending.json) before it is returned, so after a
// restart the same events go out again under the same batch_id and the
// control plane's ingest_batches deduplicates them, whatever ts it rewrote.
// The file is written without holding q.mu: Append never waits for the disk.
func (q *Queue) Next(maxEvents int) (Batch, bool) {
	q.ackMu.Lock()
	defer q.ackMu.Unlock()

	q.mu.Lock()
	if q.pending != nil {
		b := q.pending.batch
		q.mu.Unlock()
		return b, true
	}
	if len(q.recs) == 0 || maxEvents <= 0 {
		q.mu.Unlock()
		return Batch{}, false
	}
	n := min(maxEvents, len(q.recs))
	id, err := q.cfg.IDs.New(time.Now())
	if err != nil {
		q.mu.Unlock()
		return Batch{}, false
	}
	evs := make([]Event, n)
	for i := range n {
		evs[i] = q.recs[i].ev
	}
	last := q.recs[n-1]
	q.mu.Unlock()

	// Only Next, Ack and Reject touch pending and the head of recs, and ackMu
	// serialises them, so the snapshot above stays valid.
	perr := writePending(q.cfg.Dir, pendingMarker{BatchID: id, Segment: last.seg, Offset: last.end})

	q.mu.Lock()
	defer q.mu.Unlock()
	if perr != nil {
		// Still idempotent within this process; reported by Close.
		q.noteDiskErr(perr)
	}
	q.pending = &pendingBatch{batch: Batch{ID: id, Events: evs}, n: n}
	return q.pending.batch, true
}

// Ack confirms the outstanding batch. The new position is persisted before the
// events are released, so acknowledged events are never redelivered after a crash.
// If only removing pending.json or acked segments fails, the batch is still
// released and the error wraps errReleaseCleanup.
func (q *Queue) Ack(batchID string) error {
	return q.release(batchID, false)
}

// Reject sets the outstanding batch aside in rejected/<batch_id>.json and then
// releases it like Ack, so a batch the receiver will never accept does not
// block the queue. Beyond MaxRejected files the batch is discarded and counted
// in RejectedDropped.
func (q *Queue) Reject(batchID string) error {
	return q.release(batchID, true)
}

func (q *Queue) release(batchID string, reject bool) error {
	err := q.releaseFiles(batchID, reject)
	if err != nil && !errors.Is(err, ErrUnknownBatch) {
		q.diskErrors.Add(1)
	}
	return err
}

func (q *Queue) releaseFiles(batchID string, reject bool) error {
	q.ackMu.Lock()
	defer q.ackMu.Unlock()

	q.mu.Lock()
	if q.pending == nil || q.pending.batch.ID != batchID {
		q.mu.Unlock()
		return ErrUnknownBatch
	}
	batch := q.pending.batch
	last := q.recs[q.pending.n-1]
	q.mu.Unlock()

	// Only Next, Ack and Reject mutate pending and the head of recs, and ackMu
	// serialises them, so the snapshot above stays valid while files are written.
	if reject {
		if err := q.park(batch); err != nil {
			return err
		}
	}
	if err := writeCursor(q.cfg.Dir, ackCursor{Segment: last.seg, Offset: last.end, BatchID: batchID}); err != nil {
		return err
	}
	// A stale pending.json is covered by the cursor and ignored on recovery.
	var errs []error
	if err := os.Remove(filepath.Join(q.cfg.Dir, pendingFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("events: remove pending batch: %w", err))
	}

	q.mu.Lock()
	n := q.pending.n
	for _, r := range q.recs[:n] {
		q.used -= r.size
	}
	q.recs = append([]record(nil), q.recs[n:]...)
	q.pending = nil
	keepFrom := q.seg
	if len(q.recs) > 0 {
		keepFrom = q.recs[0].seg
	}
	var drop []uint64
	i := 0
	for i < len(q.segs) && q.segs[i] < keepFrom {
		drop = append(drop, q.segs[i])
		i++
	}
	q.segs = append([]uint64(nil), q.segs[i:]...)
	q.mu.Unlock()

	for _, s := range drop {
		if err := os.Remove(filepath.Join(q.cfg.Dir, segmentName(s))); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("events: remove acked segment: %w", err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", errReleaseCleanup, err)
	}
	return nil
}

// Depth is the number of unacknowledged events, including the outstanding batch.
func (q *Queue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.recs)
}

func (q *Queue) hasPending() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pending != nil
}

// DroppedEvents counts events rejected by Append since the queue was opened.
func (q *Queue) DroppedEvents() uint64 { return q.dropped.Load() }

// RejectedBatches counts batches set aside in rejected/ since the queue was opened.
func (q *Queue) RejectedBatches() uint64 { return q.rejected.Load() }

// RejectedDropped counts rejected batches discarded because rejected/ was full.
func (q *Queue) RejectedDropped() uint64 { return q.rejectedDropped.Load() }

// DiskErrors counts failed disk operations since the queue was opened:
// segment write, rotation and fsync, pending marker, ack cursor, rejected/
// file and segment removal.
func (q *Queue) DiskErrors() uint64 { return q.diskErrors.Load() }

// CorruptLines counts lines discarded during recovery (e.g. a line truncated by a crash).
func (q *Queue) CorruptLines() uint64 { return q.corrupt.Load() }

func (q *Queue) syncLoop() {
	defer close(q.done)
	t := time.NewTicker(q.cfg.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-q.stop:
			return
		case <-t.C:
		case <-q.syncCh:
		}
		q.syncOnce()
	}
}

// syncOnce fsyncs without holding q.mu, so Append is never blocked by the disk.
func (q *Queue) syncOnce() {
	q.mu.Lock()
	f, retired := q.f, q.retired
	dirty := q.unsynced > 0 || len(retired) > 0
	q.retired, q.unsynced = nil, 0
	q.mu.Unlock()
	if !dirty {
		return
	}
	errs := make([]error, 0, len(retired)+1)
	for _, r := range retired {
		errs = append(errs, syncClose(r))
	}
	if len(retired) > 0 {
		errs = append(errs, syncDir(q.cfg.Dir))
	}
	if err := f.Sync(); err != nil {
		errs = append(errs, fmt.Errorf("events: fsync segment: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		q.mu.Lock()
		q.noteDiskErr(err)
		q.mu.Unlock()
	}
}

// noteDiskErr counts err and keeps the first one for Close; a disk that keeps
// failing must not grow syncErr without bound. Caller holds q.mu.
func (q *Queue) noteDiskErr(err error) {
	q.diskErrors.Add(1)
	if q.syncErr == nil {
		q.syncErr = err
	}
}

func syncClose(f *os.File) error {
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("events: fsync segment: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("events: close segment: %w", err)
	}
	return nil
}

// Close stops the sync loop, fsyncs and closes all segments. Appends after
// Close fail with ErrClosed. It returns the first fsync or pending marker
// error seen while running, if any, and those of the final fsync.
func (q *Queue) Close() error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil
	}
	q.closed = true
	q.mu.Unlock()
	close(q.stop)
	<-q.done

	q.mu.Lock()
	defer q.mu.Unlock()
	errs := []error{q.syncErr}
	for _, r := range q.retired {
		errs = append(errs, syncClose(r))
	}
	q.retired = nil
	errs = append(errs, syncClose(q.f), syncDir(q.cfg.Dir))
	return errors.Join(errs...)
}

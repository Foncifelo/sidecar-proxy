package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Batcher defaults.
const (
	DefaultBatchMaxEvents = 100
	DefaultBatchInterval  = 10 * time.Second
	drainRetryDelay       = 100 * time.Millisecond
)

// Sink delivers one batch, typically POST /v1/ingest. A nil error means the
// batch is acknowledged and its events are released from the queue. On error
// the batch stays outstanding and is redelivered with the same ID, unless the
// error wraps ErrRejected: then the batch is set aside (Queue.Reject) and the
// queue moves on. The Sink is responsible for reporting its own failures.
type Sink func(ctx context.Context, b Batch) error

// BatcherConfig configures a Batcher. Zero values take the defaults.
type BatcherConfig struct {
	MaxEvents int           // deliver as soon as this many events are queued
	Interval  time.Duration // deliver whatever is queued at least this often
	Logger    *slog.Logger  // queue disk failures and recoveries; nil discards
}

// Batcher drains a Queue into a Sink in batches of up to MaxEvents events or
// every Interval, whichever comes first. After a failed Ack or Reject (a disk
// error) it waits with exponential backoff before trying again.
type Batcher struct {
	q      *Queue
	sink   Sink
	cfg    BatcherConfig
	log    *slog.Logger
	cancel context.CancelFunc
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once

	// Disk failure state. Only the loop goroutine touches it, and Close's
	// drain once the loop is done.
	diskFailures   int
	diskSince      time.Time
	cleanupFailing bool

	// Test seams; never nil.
	after  func(d time.Duration) <-chan time.Time
	jitter func(limit time.Duration) time.Duration
	now    func() time.Time
}

// batcherSeams replaces the clock and the jitter in tests.
type batcherSeams struct {
	after  func(d time.Duration) <-chan time.Time
	jitter func(limit time.Duration) time.Duration
}

// StartBatcher starts delivering from q to sink until ctx is cancelled or Close is called.
func StartBatcher(ctx context.Context, q *Queue, sink Sink, cfg BatcherConfig) *Batcher {
	return startBatcher(ctx, q, sink, cfg, batcherSeams{})
}

func startBatcher(ctx context.Context, q *Queue, sink Sink, cfg BatcherConfig, seams batcherSeams) *Batcher {
	if cfg.MaxEvents <= 0 {
		cfg.MaxEvents = DefaultBatchMaxEvents
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultBatchInterval
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if seams.after == nil {
		seams.after = time.After
	}
	if seams.jitter == nil {
		seams.jitter = equalJitter
	}
	runCtx, cancel := context.WithCancel(ctx)
	b := &Batcher{
		q: q, sink: sink, cfg: cfg, log: cfg.Logger, cancel: cancel,
		stop: make(chan struct{}), done: make(chan struct{}),
		after: seams.after, jitter: seams.jitter, now: time.Now,
	}
	go b.loop(runCtx)
	return b
}

func (b *Batcher) loop(ctx context.Context) {
	defer close(b.done)
	t := time.NewTicker(b.cfg.Interval)
	defer t.Stop()
	for {
		var err error
		select {
		case <-b.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			err = b.flush(ctx, true)
		case <-b.q.notify:
			err = b.flush(ctx, false)
		}
		if errors.Is(err, errQueueDisk) && !b.backOff(ctx) {
			return
		}
	}
}

// backOff waits after a disk failure; false means the batcher is stopping.
// Ticks and notifications that arrive meanwhile collapse into one each.
func (b *Batcher) backOff(ctx context.Context) bool {
	d := b.jitter(backoffLimit(diskBackoffBase, diskBackoffMax, b.diskFailures-1))
	select {
	case <-b.stop:
		return false
	case <-ctx.Done():
		return false
	case <-b.after(d):
		return true
	}
}

// flush delivers batches. With all=false only full batches go out and an
// outstanding (previously failed) batch waits for the next tick. It returns
// the error that stopped it, nil when there was nothing left to deliver.
func (b *Batcher) flush(ctx context.Context, all bool) error {
	for {
		if !all && (b.q.hasPending() || b.q.Depth() < b.cfg.MaxEvents) {
			return nil
		}
		err := b.deliverOne(ctx)
		if errors.As(err, new(nothingToDeliverError)) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type nothingToDeliverError struct{}

func (nothingToDeliverError) Error() string { return "events: nothing to deliver" }

func (b *Batcher) deliverOne(ctx context.Context) error {
	batch, ok := b.q.Next(b.cfg.MaxEvents)
	if !ok {
		return nothingToDeliverError{}
	}
	if err := b.sink(ctx, batch); err != nil {
		if errors.Is(err, ErrRejected) {
			return b.settled(ctx, "reject", b.q.Reject(batch.ID))
		}
		return fmt.Errorf("events: deliver batch: %w", err)
	}
	return b.settled(ctx, "ack", b.q.Ack(batch.ID))
}

// settled tracks the outcome of an Ack or Reject: one warning when the disk
// starts failing, one info line when it recovers, nothing per retry. Logs
// carry neither events nor file paths. A failed cleanup after the cursor was
// written (errReleaseCleanup) released the batch: it is logged the same way
// but does not back off.
func (b *Batcher) settled(ctx context.Context, op string, err error) error {
	if errors.Is(err, ErrUnknownBatch) {
		return err
	}
	if err != nil && !errors.Is(err, errReleaseCleanup) {
		if b.diskFailures == 0 {
			b.diskSince = b.now()
			b.log.WarnContext(ctx, "event queue disk operation failed; retrying with backoff, events stay queued",
				"op", op, "cause", DiskCause(err), "depth", b.q.Depth())
		}
		b.diskFailures++
		return fmt.Errorf("%w: %w", errQueueDisk, err)
	}
	if b.diskFailures > 0 {
		b.log.InfoContext(ctx, "event queue disk recovered",
			"failures", b.diskFailures, "down_for", b.now().Sub(b.diskSince).Round(time.Millisecond).String())
		b.diskFailures = 0
	}
	b.noteCleanup(ctx, err)
	return nil
}

// noteCleanup logs once when removing released files starts failing and once
// when it works again.
func (b *Batcher) noteCleanup(ctx context.Context, err error) {
	failed := err != nil
	switch {
	case failed && !b.cleanupFailing:
		b.log.WarnContext(ctx, "event queue cleanup failed; released files stay on disk until a later release",
			"cause", DiskCause(err))
	case !failed && b.cleanupFailing:
		b.log.InfoContext(ctx, "event queue cleanup recovered")
	}
	b.cleanupFailing = failed
}

// Close stops the background loop and drains the queue into the sink until it
// is empty or ctx expires. It returns ctx's error if events were left behind;
// they stay on disk and are delivered after the next start.
func (b *Batcher) Close(ctx context.Context) error {
	defer b.cancel()
	b.once.Do(func() { close(b.stop) })
	select {
	case <-b.done:
	case <-ctx.Done():
		b.cancel()
		<-b.done
		return fmt.Errorf("events: batcher close: %w", ctx.Err())
	}
	for b.q.Depth() > 0 {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("events: drain: %w", err)
		}
		if err := b.deliverOne(ctx); err != nil {
			select {
			case <-ctx.Done():
				return fmt.Errorf("events: drain: %w (last error: %w)", ctx.Err(), err)
			case <-time.After(drainRetryDelay):
			}
		}
	}
	return nil
}

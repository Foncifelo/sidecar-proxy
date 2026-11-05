package proxy

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
)

// dropWarnInterval is the shortest gap between two "dropping events"
// warnings, so a sink that flaps between failing and working does not flood
// the log either.
const dropWarnInterval = time.Minute

// Why an event was dropped, in the reason attribute of the drop log lines.
const (
	dropReasonDisk   = "disk"   // the queue could not write to disk
	dropReasonFull   = "full"   // the queue reached its size cap
	dropReasonClosed = "closed" // the queue is shut down
)

// dropLog logs dropped events by episode instead of per request: one warning
// when Sink.Append starts failing and one info line, with the number dropped,
// when it works again. Both carry the episode's reason and its cause, taken
// from the first error of the episode that is not events.ErrAppendPaused.
//
// A failure episode that starts less than dropWarnInterval after the last
// warning is not warned at once; its drops are reported by the next warning
// (dropped_unlogged), or by a warning of the same episode once the interval
// has passed. So a short episode inside that interval, with no later warning,
// shows up in no log line at all: only in events.Queue.DroppedEvents
// (sidecar_dropped_events_total).
//
// Drops because the queue is closed (shutdown) never open an episode; they
// are logged at debug level only.
//
// Lines carry no request ids, events or file paths (events.DiskCause).
type dropLog struct {
	failing atomic.Bool // fast path: a working sink never takes mu

	mu       sync.Mutex
	dropped  uint64    // drops in the current episode
	since    time.Time // start of the current episode
	reason   string    // of the current episode
	cause    string    // of the current episode; "" until a non-paused error
	warned   bool      // the current episode has been warned
	lastWarn time.Time
	unlogged uint64 // drops of earlier episodes no line has reported
}

// dropReport is the line to log after a state change, if any.
type dropReport struct {
	warn     bool
	recover  bool
	closed   bool
	reason   string
	cause    string
	dropped  uint64
	unlogged uint64
	downFor  time.Duration
}

func dropReason(err error) string {
	switch {
	case errors.Is(err, events.ErrClosed):
		return dropReasonClosed
	case errors.Is(err, events.ErrQueueFull):
		return dropReasonFull
	default:
		return dropReasonDisk
	}
}

// failed records one dropped event at now.
func (d *dropLog) failed(now time.Time, err error) dropReport {
	reason := dropReason(err)
	if reason == dropReasonClosed {
		return dropReport{closed: true, reason: reason}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.failing.Load() {
		d.failing.Store(true)
		d.dropped, d.since, d.reason, d.cause, d.warned = 0, now, reason, "", false
	}
	if d.cause == "" && !errors.Is(err, events.ErrAppendPaused) {
		d.cause = events.DiskCause(err)
	}
	d.dropped++
	if d.warned || (!d.lastWarn.IsZero() && now.Sub(d.lastWarn) < dropWarnInterval) {
		return dropReport{}
	}
	d.warned, d.lastWarn = true, now
	cause := d.cause
	if cause == "" { // only paused appends so far
		cause = events.DiskCause(err)
	}
	r := dropReport{warn: true, reason: d.reason, cause: cause, unlogged: d.unlogged}
	d.unlogged = 0
	return r
}

// succeeded records one stored event at now and ends a failure episode.
func (d *dropLog) succeeded(now time.Time) dropReport {
	if !d.failing.Load() {
		return dropReport{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.failing.Load() {
		return dropReport{}
	}
	d.failing.Store(false)
	if !d.warned {
		d.unlogged += d.dropped
		return dropReport{}
	}
	return dropReport{recover: true, reason: d.reason, dropped: d.dropped, downFor: now.Sub(d.since)}
}

// log writes r, outside dropLog's lock.
func (r dropReport) log(l *slog.Logger) {
	switch {
	case r.closed:
		l.Debug("event dropped: queue closed", "reason", r.reason)
	case r.warn:
		args := []any{"reason", r.reason, "cause", r.cause}
		if r.unlogged > 0 {
			args = append(args, "dropped_unlogged", r.unlogged)
		}
		msg := "event queue append failed; dropping events until it recovers"
		if r.reason == dropReasonFull {
			msg = "event queue full; dropping events until delivered batches free space"
		}
		l.Warn(msg, args...)
	case r.recover:
		l.Info("event queue append recovered", "reason", r.reason,
			"dropped", r.dropped, "down_for", r.downFor.Round(time.Millisecond).String())
	}
}

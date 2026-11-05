package events

import (
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"time"
)

// Pacing of the batcher after a failed queue disk operation (ack cursor,
// rejected/ file, segment removal): exponential from diskBackoffBase, capped
// at diskBackoffMax, reset by the next successful ack.
const (
	diskBackoffBase = 1 * time.Second
	diskBackoffMax  = 60 * time.Second
)

// errQueueDisk marks a failed Ack or Reject, as opposed to a Sink failure.
var errQueueDisk = errors.New("events: queue disk operation failed")

// backoffLimit is min(maxD, base·2^attempt) without overflow. Same rule as
// internal/ingest, which cannot be imported here (it imports this package).
func backoffLimit(base, maxD time.Duration, attempt int) time.Duration {
	d := base
	for range attempt {
		if d >= maxD/2 {
			return maxD
		}
		d *= 2
	}
	return min(d, maxD)
}

// equalJitter returns a delay in [limit/2, limit). Unlike full jitter it never
// comes close to zero, so a failing disk is never retried at once.
func equalJitter(limit time.Duration) time.Duration {
	half := limit / 2
	if half <= 0 {
		return limit
	}
	return half + rand.N(half) //nolint:gosec // retry jitter does not need cryptographic randomness
}

// unknownCause is what DiskCause says of an error it cannot vouch for.
const unknownCause = "unclassified error"

// DiskCause is err without file paths or other data, for logs: the operation
// and the system error of the first *fs.PathError or *os.LinkError in err's
// chain, else the text of one of this package's sentinel errors, else
// unknownCause. It never returns the text of an arbitrary error, which could
// come from any Sink and carry anything.
func DiskCause(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Op + ": " + le.Err.Error()
	}
	for _, s := range []error{ErrQueueFull, ErrClosed, ErrAppendPaused, ErrUnknownBatch, ErrRejected, errReleaseCleanup} {
		if errors.Is(err, s) {
			return s.Error()
		}
	}
	return unknownCause
}

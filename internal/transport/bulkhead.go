package transport

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// Bulkhead is a counting semaphore that caps concurrent requests to one
// endpoint.
type Bulkhead struct {
	slots    chan struct{}
	inFlight atomic.Int64
}

// NewBulkhead returns a bulkhead with capacity slots. capacity must be > 0.
func NewBulkhead(capacity int) *Bulkhead {
	if capacity <= 0 {
		capacity = 1
	}
	return &Bulkhead{slots: make(chan struct{}, capacity)}
}

// Acquire blocks until a slot is free or ctx is done. On success it returns
// a release function that is safe to call more than once.
func (b *Bulkhead) Acquire(ctx context.Context) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("transport: bulkhead acquire: %w", err)
	}
	select {
	case b.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("transport: bulkhead acquire: %w", ctx.Err())
	}
	b.inFlight.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			b.inFlight.Add(-1)
			<-b.slots
		})
	}, nil
}

// InFlight reports how many slots are currently held.
func (b *Bulkhead) InFlight() int64 { return b.inFlight.Load() }

// Capacity reports the maximum number of concurrent holders.
func (b *Bulkhead) Capacity() int { return cap(b.slots) }

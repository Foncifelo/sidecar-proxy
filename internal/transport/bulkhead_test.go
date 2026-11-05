package transport

import (
	"context"
	"errors"
	"testing"
)

func TestBulkheadAcquire(t *testing.T) {
	tests := []struct {
		name       string
		capacity   int
		held       int
		cancelCtx  bool
		wantErr    error
		wantFlight int64
	}{
		{name: "free slot", capacity: 2, held: 0, wantFlight: 1},
		{name: "last free slot", capacity: 2, held: 1, wantFlight: 2},
		{name: "full and context cancelled", capacity: 1, held: 1, cancelCtx: true, wantErr: context.Canceled, wantFlight: 1},
		{name: "free but context already cancelled", capacity: 2, held: 0, cancelCtx: true, wantErr: context.Canceled, wantFlight: 0},
		{name: "non-positive capacity becomes one", capacity: 0, held: 0, wantFlight: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBulkhead(tt.capacity)
			for range tt.held {
				if _, err := b.Acquire(context.Background()); err != nil {
					t.Fatalf("pre-acquire: %v", err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancelCtx {
				cancel()
			}
			release, err := b.Acquire(ctx)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Acquire() err = %v, want %v", err, tt.wantErr)
			}
			if got := b.InFlight(); got != tt.wantFlight {
				t.Fatalf("InFlight() = %d, want %d", got, tt.wantFlight)
			}
			if err == nil {
				release()
				release() // idempotent
				if got := b.InFlight(); got != tt.wantFlight-1 {
					t.Fatalf("after release InFlight() = %d, want %d", got, tt.wantFlight-1)
				}
			}
		})
	}
}

func TestBulkheadAcquireUnblocksOnCancel(t *testing.T) {
	b := NewBulkhead(1)
	if _, err := b.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.Acquire(ctx)
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked Acquire() = %v, want context.Canceled", err)
	}
	if b.Capacity() != 1 {
		t.Fatalf("Capacity() = %d, want 1", b.Capacity())
	}
}

func TestBulkheadReleaseWakesWaiter(t *testing.T) {
	b := NewBulkhead(1)
	release, err := b.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		r, err := b.Acquire(context.Background())
		if err == nil {
			r()
		}
		done <- err
	}()
	release()
	if err := <-done; err != nil {
		t.Fatalf("waiter Acquire() = %v", err)
	}
}

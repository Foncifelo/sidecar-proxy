package transport

import (
	"errors"
	"sync"
	"time"
)

// ErrBreakerOpen is returned when an endpoint's circuit breaker rejects a call.
var ErrBreakerOpen = errors.New("transport: circuit breaker open")

// Clock abstracts time so breakers can be tested without sleeping.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// State is the state of a circuit breaker.
type State int

// Breaker states.
const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Token identifies one call admitted by Breaker.Allow. Outcomes reported with
// a token from an earlier generation (the breaker changed state since the call
// was admitted) are discarded, so slow or stale calls cannot close, re-open or
// free the probe slot of a breaker they no longer belong to.
type Token struct {
	gen   uint64
	probe bool
}

// Breaker is a consecutive-failure circuit breaker. After threshold
// consecutive failures it opens; after cooldown it lets one probe through
// (half-open); the probe's outcome closes or re-opens it.
//
// Every successful Allow must be followed by exactly one of Success, Failure
// or Ignore with the returned token.
type Breaker struct {
	threshold int
	cooldown  time.Duration
	clock     Clock

	mu       sync.Mutex
	state    State
	gen      uint64
	failures int
	openedAt time.Time
	probing  bool
}

// NewBreaker builds a breaker. A nil clock means the system clock.
func NewBreaker(threshold int, cooldown time.Duration, clock Clock) *Breaker {
	if threshold <= 0 {
		threshold = 1
	}
	if clock == nil {
		clock = systemClock{}
	}
	return &Breaker{threshold: threshold, cooldown: cooldown, clock: clock}
}

// State returns the current state, applying the open -> half-open transition
// if the cooldown has elapsed.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advance()
	return b.state
}

// Allow reports whether a call may proceed. It returns ErrBreakerOpen when the
// breaker is open or a half-open probe is already in flight. On success the
// returned token must be passed to exactly one of Success, Failure or Ignore.
func (b *Breaker) Allow() (Token, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advance()
	switch b.state {
	case StateClosed:
		return Token{gen: b.gen}, nil
	case StateHalfOpen:
		if b.probing {
			return Token{}, ErrBreakerOpen
		}
		b.probing = true
		return Token{gen: b.gen, probe: true}, nil
	case StateOpen:
		return Token{}, ErrBreakerOpen
	default:
		return Token{}, ErrBreakerOpen
	}
}

// Success records a healthy outcome for the call identified by tok.
func (b *Breaker) Success(tok Token) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advance()
	if !b.current(tok) {
		return
	}
	if tok.probe {
		b.transition(StateClosed)
		return
	}
	b.failures = 0
}

// Failure records an unhealthy outcome for the call identified by tok.
func (b *Breaker) Failure(tok Token) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advance()
	if !b.current(tok) {
		return
	}
	if tok.probe {
		b.trip()
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.trip()
	}
}

// Ignore releases an allowed call without counting it, e.g. when the caller
// cancelled the request itself. Only the current probe frees the half-open
// slot.
func (b *Breaker) Ignore(tok Token) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advance()
	if b.current(tok) && tok.probe {
		b.probing = false
	}
}

// current reports whether tok belongs to the present generation and matches
// its kind (probe tokens only in half-open, plain tokens only in closed).
// It must be called with mu held.
func (b *Breaker) current(tok Token) bool {
	if tok.gen != b.gen {
		return false
	}
	if tok.probe {
		return b.state == StateHalfOpen
	}
	return b.state == StateClosed
}

// trip must be called with mu held.
func (b *Breaker) trip() {
	b.transition(StateOpen)
	b.openedAt = b.clock.Now()
}

// transition moves to state and starts a new generation, invalidating every
// outstanding token. It must be called with mu held.
func (b *Breaker) transition(state State) {
	b.state = state
	b.gen++
	b.failures = 0
	b.probing = false
}

// advance must be called with mu held.
func (b *Breaker) advance() {
	if b.state == StateOpen && !b.clock.Now().Before(b.openedAt.Add(b.cooldown)) {
		b.transition(StateHalfOpen)
	}
}

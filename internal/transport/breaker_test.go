package transport

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// step is one action applied to a breaker in a scripted scenario.
//
// "allow" stores the admitted token under tok. "fail", "ok" and "ignore"
// report the outcome of the token named tok; with an empty tok they admit a
// fresh call first and report on it immediately.
type step struct {
	do        string // "fail", "ok", "ignore", "advance", "allow", "deny"
	tok       string
	advance   time.Duration
	wantState State
}

func TestBreakerTransitions(t *testing.T) {
	const cooldown = 10 * time.Second
	tests := []struct {
		name      string
		threshold int
		steps     []step
	}{
		{
			name:      "stays closed below threshold",
			threshold: 3,
			steps: []step{
				{do: "fail", wantState: StateClosed},
				{do: "fail", wantState: StateClosed},
				{do: "allow", wantState: StateClosed},
			},
		},
		{
			name:      "success resets consecutive count",
			threshold: 2,
			steps: []step{
				{do: "fail", wantState: StateClosed},
				{do: "ok", wantState: StateClosed},
				{do: "fail", wantState: StateClosed},
				{do: "fail", wantState: StateOpen},
			},
		},
		{
			name:      "opens at threshold and denies until cooldown",
			threshold: 2,
			steps: []step{
				{do: "fail", wantState: StateClosed},
				{do: "fail", wantState: StateOpen},
				{do: "deny", wantState: StateOpen},
				{do: "advance", advance: cooldown - time.Nanosecond, wantState: StateOpen},
				{do: "deny", wantState: StateOpen},
				{do: "advance", advance: time.Nanosecond, wantState: StateHalfOpen},
			},
		},
		{
			name:      "half-open admits a single probe and success closes",
			threshold: 1,
			steps: []step{
				{do: "fail", wantState: StateOpen},
				{do: "advance", advance: cooldown, wantState: StateHalfOpen},
				{do: "allow", tok: "probe", wantState: StateHalfOpen},
				{do: "deny", wantState: StateHalfOpen},
				{do: "ok", tok: "probe", wantState: StateClosed},
				{do: "allow", wantState: StateClosed},
			},
		},
		{
			name:      "half-open failure reopens with a fresh cooldown",
			threshold: 1,
			steps: []step{
				{do: "fail", wantState: StateOpen},
				{do: "advance", advance: cooldown, wantState: StateHalfOpen},
				{do: "allow", tok: "probe", wantState: StateHalfOpen},
				{do: "fail", tok: "probe", wantState: StateOpen},
				{do: "advance", advance: cooldown / 2, wantState: StateOpen},
				{do: "deny", wantState: StateOpen},
				{do: "advance", advance: cooldown / 2, wantState: StateHalfOpen},
			},
		},
		{
			name:      "ignored probe frees the half-open slot",
			threshold: 1,
			steps: []step{
				{do: "fail", wantState: StateOpen},
				{do: "advance", advance: cooldown, wantState: StateHalfOpen},
				{do: "allow", tok: "probe", wantState: StateHalfOpen},
				{do: "ignore", tok: "probe", wantState: StateHalfOpen},
				{do: "allow", wantState: StateHalfOpen},
			},
		},
		{
			name:      "stale success while open keeps it open",
			threshold: 2,
			steps: []step{
				{do: "allow", tok: "slow", wantState: StateClosed},
				{do: "fail", wantState: StateClosed},
				{do: "fail", wantState: StateOpen},
				{do: "ok", tok: "slow", wantState: StateOpen},
				{do: "deny", wantState: StateOpen},
			},
		},
		{
			name:      "stale success in half-open does not close",
			threshold: 1,
			steps: []step{
				{do: "allow", tok: "slow", wantState: StateClosed},
				{do: "fail", wantState: StateOpen},
				{do: "advance", advance: cooldown, wantState: StateHalfOpen},
				{do: "allow", tok: "probe", wantState: StateHalfOpen},
				{do: "ok", tok: "slow", wantState: StateHalfOpen},
				{do: "deny", wantState: StateHalfOpen},
			},
		},
		{
			name:      "stale ignore in half-open does not admit a second probe",
			threshold: 1,
			steps: []step{
				{do: "allow", tok: "old", wantState: StateClosed},
				{do: "fail", wantState: StateOpen},
				{do: "advance", advance: cooldown, wantState: StateHalfOpen},
				{do: "allow", tok: "probe", wantState: StateHalfOpen},
				{do: "ignore", tok: "old", wantState: StateHalfOpen},
				{do: "deny", wantState: StateHalfOpen},
			},
		},
		{
			name:      "stale failure in half-open does not reopen",
			threshold: 1,
			steps: []step{
				{do: "allow", tok: "old", wantState: StateClosed},
				{do: "fail", wantState: StateOpen},
				{do: "advance", advance: cooldown, wantState: StateHalfOpen},
				{do: "allow", tok: "probe", wantState: StateHalfOpen},
				{do: "fail", tok: "old", wantState: StateHalfOpen},
				{do: "ok", tok: "probe", wantState: StateClosed},
			},
		},
		{
			name:      "stale failure from before a reopen does not count after close",
			threshold: 2,
			steps: []step{
				{do: "allow", tok: "old", wantState: StateClosed},
				{do: "fail", wantState: StateClosed},
				{do: "fail", wantState: StateOpen},
				{do: "advance", advance: cooldown, wantState: StateHalfOpen},
				{do: "ok", wantState: StateClosed},
				{do: "fail", wantState: StateClosed},
				{do: "fail", tok: "old", wantState: StateClosed},
				{do: "allow", wantState: StateClosed},
			},
		},
		{
			name:      "probe token reused after close is discarded",
			threshold: 1,
			steps: []step{
				{do: "fail", wantState: StateOpen},
				{do: "advance", advance: cooldown, wantState: StateHalfOpen},
				{do: "allow", tok: "probe", wantState: StateHalfOpen},
				{do: "ok", tok: "probe", wantState: StateClosed},
				{do: "fail", tok: "probe", wantState: StateClosed},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newFakeClock()
			b := NewBreaker(tt.threshold, cooldown, clock)
			tokens := map[string]Token{}
			for i, s := range tt.steps {
				switch s.do {
				case "fail", "ok", "ignore":
					tok := stepToken(t, i, b, tokens, s.tok)
					report(b, s.do, tok)
				case "advance":
					clock.Advance(s.advance)
				case "allow":
					tok, err := b.Allow()
					if err != nil {
						t.Fatalf("step %d: Allow() = %v, want nil", i, err)
					}
					if s.tok != "" {
						tokens[s.tok] = tok
					}
				case "deny":
					if _, err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
						t.Fatalf("step %d: Allow() = %v, want ErrBreakerOpen", i, err)
					}
				default:
					t.Fatalf("unknown step %q", s.do)
				}
				if got := b.State(); got != s.wantState {
					t.Fatalf("step %d (%s): state = %v, want %v", i, s.do, got, s.wantState)
				}
			}
		})
	}
}

// stepToken returns the stored token name, or admits a fresh call when name
// is empty.
func stepToken(t *testing.T, i int, b *Breaker, tokens map[string]Token, name string) Token {
	t.Helper()
	if name != "" {
		tok, ok := tokens[name]
		if !ok {
			t.Fatalf("step %d: unknown token %q", i, name)
		}
		return tok
	}
	tok, err := b.Allow()
	if err != nil {
		t.Fatalf("step %d: Allow() = %v, want nil", i, err)
	}
	return tok
}

func report(b *Breaker, outcome string, tok Token) {
	switch outcome {
	case "fail":
		b.Failure(tok)
	case "ok":
		b.Success(tok)
	case "ignore":
		b.Ignore(tok)
	}
}

func TestStateString(t *testing.T) {
	tests := map[State]string{
		StateClosed:   "closed",
		StateOpen:     "open",
		StateHalfOpen: "half-open",
		State(99):     "unknown",
	}
	for s, want := range tests {
		if got := s.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", int(s), got, want)
		}
	}
}

func TestNewBreakerDefaults(t *testing.T) {
	b := NewBreaker(0, time.Second, nil)
	tok, err := b.Allow()
	if err != nil {
		t.Fatal(err)
	}
	b.Failure(tok)
	if got := b.State(); got != StateOpen {
		t.Fatalf("threshold 0 should behave as 1, state = %v", got)
	}
}

package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
)

// fictitiousQueuePath stands for the queue directory; it must never reach a log.
const fictitiousQueuePath = "/fictitious/secret/queue/seg-00000000000000000001.jsonl"

// errDisk is what events.Queue returns for a full disk: the path is inside.
var errDisk = fmt.Errorf("events: write segment: %w",
	&fs.PathError{Op: "write", Path: fictitiousQueuePath, Err: syscall.ENOSPC})

// errNotOurs is an error from an unknown sink; its text must not be logged.
var errNotOurs = errors.New("fictitious sink failure at " + fictitiousQueuePath)

// scriptSink returns err from every Append.
type scriptSink struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (s *scriptSink) Append(events.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.err
}

func (s *scriptSink) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// stepClock is a clock tests move by hand.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// dropStep is one phase of a test: n concurrent emits with the sink
// returning err (nil: stored), after moving the clock by advance.
type dropStep struct {
	advance time.Duration
	err     error
	n       int
}

func TestEmitDropLog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		steps        []dropStep
		wantWarn     int
		wantInfo     int
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:     "healthy sink logs nothing",
			steps:    []dropStep{{n: 50}},
			wantWarn: 0,
		},
		{
			name:     "persistent failure warns once",
			steps:    []dropStep{{err: errDisk, n: 200}, {advance: time.Hour, err: errDisk, n: 200}},
			wantWarn: 1,
		},
		{
			name:         "recovery logs once with the count",
			steps:        []dropStep{{n: 5}, {err: errDisk, n: 100}, {advance: 3 * time.Second, n: 100}},
			wantWarn:     1,
			wantInfo:     1,
			wantContains: []string{"reason=disk", "dropped=100", "down_for=3s", "cause=\"write: " + syscall.ENOSPC.Error() + "\""},
		},
		{
			name: "flapping inside the interval is not warned again",
			steps: []dropStep{
				{err: errDisk, n: 10}, {n: 1},
				{advance: time.Second, err: errDisk, n: 10}, {n: 1},
				{advance: time.Second, err: errDisk, n: 10}, {n: 1},
			},
			wantWarn: 1,
			wantInfo: 1,
		},
		{
			name: "silent episode is reported by the next warning",
			steps: []dropStep{
				{err: errDisk, n: 10}, {n: 1},
				{advance: time.Second, err: errDisk, n: 7}, {n: 1},
				{advance: dropWarnInterval, err: errDisk, n: 3}, {n: 1},
			},
			wantWarn:     2,
			wantInfo:     2,
			wantContains: []string{"dropped_unlogged=7"},
		},
		{
			name: "long silent episode is warned once the interval passes",
			steps: []dropStep{
				{err: errDisk, n: 1}, {n: 1},
				{advance: time.Second, err: errDisk, n: 5},
				{advance: dropWarnInterval, err: events.ErrAppendPaused, n: 5},
				{advance: dropWarnInterval, err: events.ErrAppendPaused, n: 5},
			},
			wantWarn:     2,
			wantInfo:     1,
			wantContains: []string{"reason=disk"},
			// Its warning comes during paused appends but names the real cause.
			wantAbsent: []string{events.ErrAppendPaused.Error()},
		},
		{
			name: "warning carries the episode's first real cause",
			steps: []dropStep{
				{err: errDisk, n: 1},
				{err: events.ErrAppendPaused, n: 20},
				{n: 1},
				{advance: dropWarnInterval, err: events.ErrAppendPaused, n: 20},
				{err: errDisk, n: 1},
			},
			wantWarn: 2,
			wantInfo: 1,
			// The second episode never saw a real error before its warning.
			wantContains: []string{"cause=\"write: " + syscall.ENOSPC.Error() + "\"", "cause=\"" + events.ErrAppendPaused.Error() + "\""},
		},
		{
			name:         "full queue says so",
			steps:        []dropStep{{err: fmt.Errorf("sink: %w", events.ErrQueueFull), n: 30}, {n: 1}},
			wantWarn:     1,
			wantInfo:     1,
			wantContains: []string{"event queue full", "reason=full"},
			wantAbsent:   []string{"append failed"},
		},
		{
			name:  "closed queue opens no episode",
			steps: []dropStep{{err: events.ErrClosed, n: 30}, {n: 1}},
		},
		{
			name:         "unknown sink error text is not logged",
			steps:        []dropStep{{err: errNotOurs, n: 3}},
			wantWarn:     1,
			wantContains: []string{"reason=disk", "cause=\"unclassified error\""},
			wantAbsent:   []string{"fictitious sink failure"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sink := &scriptSink{}
			clock := &stepClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
			logs := &lockedBuffer{}
			h, err := New(Options{
				Mode:         config.ModeShadow,
				Upstream:     Upstream{Provider: "fictitious", BaseURL: "http://127.0.0.1:1/v1"},
				Client:       &http.Client{},
				Sink:         sink,
				MaxBodyBytes: 1,
				MaxInflight:  1,
				Logger:       slog.New(slog.NewTextHandler(logs, nil)),
				Now:          clock.now,
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, chatPath, nil)
			total := 0
			for _, st := range tc.steps {
				clock.advance(st.advance)
				sink.setErr(st.err)
				var wg sync.WaitGroup
				for range st.n {
					wg.Go(func() { h.emit(newRequestState("fictitious-id", clock.now(), req)) })
				}
				wg.Wait()
				total += st.n
			}

			if sink.calls != total {
				t.Fatalf("sink saw %d appends, want %d", sink.calls, total)
			}
			out := logs.String()
			if got := strings.Count(out, "level=WARN"); got != tc.wantWarn {
				t.Errorf("%d warnings, want %d:\n%s", got, tc.wantWarn, out)
			}
			if got := strings.Count(out, "level=INFO"); got != tc.wantInfo {
				t.Errorf("%d info lines, want %d:\n%s", got, tc.wantInfo, out)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("log lacks %q:\n%s", want, out)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(out, absent) {
					t.Errorf("log carries %q:\n%s", absent, out)
				}
			}
			for _, leak := range []string{"/fictitious", "secret", "seg-", "fictitious-id"} {
				if strings.Contains(out, leak) {
					t.Errorf("log carries %q:\n%s", leak, out)
				}
			}
		})
	}
}

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/providers"
)

// stallServe sends the given blocks and then hangs without closing the body,
// like an upstream that stops mid-stream.
func stallServe(blocks ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		for _, b := range blocks {
			_, _ = io.WriteString(w, b)
			_ = rc.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}
}

// floodServe sends 64 KiB SSE comment blocks until the request is cancelled.
func floodServe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	rc := http.NewResponseController(w)
	block := ": " + strings.Repeat("x", 64<<10) + "\n\n"
	for r.Context().Err() == nil {
		if _, err := io.WriteString(w, block); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

// An upstream that stops mid-stream past the chunk idle
// timeout is cut with event: error and the event is truncated, without
// hanging the request.
func TestStreamUpstreamStallsMidStream(t *testing.T) {
	const idle = 200 * time.Millisecond
	tests := []struct {
		name     string
		blocks   []string
		wantBody string
	}{
		{"after a full block", []string{chunkTool1}, chunkTool1},
		{"after a keepalive", []string{keepalive, chunkTool1, keepalive}, keepalive + chunkTool1 + keepalive},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t, stallServe(tc.blocks...))
			hs := newHarness(t, up.URL, func(o *Options) { o.StreamIdleTimeout = idle })
			start := time.Now()
			resp, body := hs.do(t, http.MethodPost, chatPath, streamBody(true), nil)
			if elapsed := time.Since(start); elapsed > 15*idle {
				t.Errorf("request took %s, want about %s", elapsed, idle)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			id := resp.Header.Get(HeaderRequestID)
			want := tc.wantBody + "event: error\ndata: {\"error\":{\"type\":\"upstream_stream_error\",\"request_id\":\"" + id + "\"}}\n\n"
			if string(body) != want {
				t.Errorf("agent body:\n got %q\nwant %q", body, want)
			}
			ev := hs.sink.wait(t)
			if ev.StreamState != events.StreamTruncated || ev.ErrorType != streamErrorType {
				t.Errorf("event state=%q error=%q, want truncated %s", ev.StreamState, ev.ErrorType, streamErrorType)
			}
		})
	}
}

// An agent that never reads makes the per-chunk write
// deadline expire; the handler ends instead of blocking forever.
func TestSlowAgentWriteDeadline(t *testing.T) {
	up := newUpstream(t, floodServe)
	hs := newHarness(t, up.URL, func(o *Options) { o.WriteTimeout = 200 * time.Millisecond })

	conn, err := net.Dial("tcp", hs.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4 << 10)
	}
	body := streamBody(true)
	reqText := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: sidecar\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		chatPath, len(body), body)
	if _, err := io.WriteString(conn, reqText); err != nil {
		t.Fatal(err)
	}
	// Never read from conn.
	ev := hs.sink.wait(t)
	if ev.StreamState != events.StreamAborted || ev.ErrorType != "client_gone" {
		t.Errorf("event state=%q error=%q, want aborted client_gone", ev.StreamState, ev.ErrorType)
	}
}

// The server's ReadTimeout bounds reading the request only: a stream that
// lasts longer must complete.
func TestReadTimeoutDoesNotCutLongStream(t *testing.T) {
	const readTimeout = 100 * time.Millisecond
	slow := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		for _, b := range fullStream {
			_, _ = io.WriteString(w, b)
			_ = rc.Flush()
			time.Sleep(readTimeout)
		}
	}
	up := newUpstream(t, slow)
	hs := newHarnessServer(t, up.URL, nil, func(s *http.Server) { s.ReadTimeout = readTimeout })
	_, body := hs.do(t, http.MethodPost, chatPath, streamBody(true), nil)
	if string(body) != strings.Join(fullStream, "") {
		t.Errorf("agent body:\n got %q\nwant %q", body, strings.Join(fullStream, ""))
	}
	if ev := hs.sink.wait(t); ev.StreamState != events.StreamComplete {
		t.Errorf("event state=%q error=%q, want complete", ev.StreamState, ev.ErrorType)
	}
}

// blockingReader returns one byte per Read after each delay.
type blockingReader struct{ delays []time.Duration }

func (b *blockingReader) Read(p []byte) (int, error) {
	if len(b.delays) == 0 {
		return 0, io.EOF
	}
	time.Sleep(b.delays[0])
	b.delays = b.delays[1:]
	p[0] = 'x'
	return 1, nil
}

func TestIdleReader(t *testing.T) {
	t.Parallel()
	const timeout = 250 * time.Millisecond // wide margins: the reads below must not race it under load
	tests := []struct {
		name       string
		timeout    time.Duration
		delays     []time.Duration
		wantCancel bool
	}{
		{"reads in time", timeout, []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}, false},
		{"total over timeout but each read in time", timeout, slices.Repeat([]time.Duration{20 * time.Millisecond}, 15), false},
		{"one slow read", timeout, []time.Duration{10 * time.Millisecond, 3 * timeout}, true},
		{"disabled", 0, []time.Duration{200 * time.Millisecond}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			r, stop := newIdleReader(&blockingReader{delays: tc.delays}, tc.timeout, cancel)
			_, err := io.Copy(io.Discard, r)
			if tc.wantCancel && !errors.Is(err, errUpstreamIdle) {
				t.Errorf("copy error %v, want %v", err, errUpstreamIdle)
			}
			if !tc.wantCancel && err != nil {
				t.Errorf("copy error %v, want nil", err)
			}
			stop()
			time.Sleep(2 * timeout) // a stopped timer must not fire late
			cancelled := errors.Is(context.Cause(ctx), errUpstreamIdle)
			if cancelled != tc.wantCancel {
				t.Errorf("cancelled = %v, want %v (cause %v)", cancelled, tc.wantCancel, context.Cause(ctx))
			}
		})
	}
}

// endsOnCancelReader blocks until ctx is cancelled and then ends cleanly, as
// a TLS provider can after our cancel: close_notify, last chunk, io.EOF.
// Every later Read is a clean EOF too.
type endsOnCancelReader struct {
	ctx   context.Context // the idle reader's cancel is what unblocks Read
	data  string
	ended bool
}

func (e *endsOnCancelReader) Read(p []byte) (int, error) {
	if e.ended {
		return 0, io.EOF
	}
	select {
	case <-e.ctx.Done():
	case <-time.After(10 * time.Second):
		return 0, errors.New("never cancelled")
	}
	e.ended = true
	return copy(p, e.data), io.EOF
}

// A clean EOF that only came because the idle timer cancelled the request is
// still a truncation: the reader must report errUpstreamIdle, or a cut body
// would be delivered as complete with no fallback.
func TestIdleReaderEOFAfterCancel(t *testing.T) {
	t.Parallel()
	for _, data := range []string{"", `"choices":[]}`} {
		t.Run(fmt.Sprintf("data %q", data), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			r, stop := newIdleReader(&endsOnCancelReader{ctx: ctx, data: data}, 20*time.Millisecond, cancel)
			defer stop()
			got, err := io.ReadAll(r)
			if !errors.Is(err, errUpstreamIdle) {
				t.Errorf("read error %v, want %v", err, errUpstreamIdle)
			}
			if string(got) != data {
				t.Errorf("read %q, want %q", got, data)
			}
			// The error sticks: a caller that dropped it must not read on
			// into a clean EOF.
			if n, err := r.Read(make([]byte, 8)); n != 0 || !errors.Is(err, errUpstreamIdle) {
				t.Errorf("read after the cut = %d, %v; want 0, %v", n, err, errUpstreamIdle)
			}
		})
	}
}

// The OpenRouter adapter peeks the body for the served provider and drops
// read errors (internal/providers/openrouter.go). Reading on through the
// restored body must still surface the cut, for a JSON body and an SSE one.
func TestIdleReaderThroughOpenRouterPeek(t *testing.T) {
	t.Parallel()
	tests := []struct{ contentType, data string }{
		{"application/json", `{"id":"x",`},
		{"text/event-stream", ": ping\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.contentType, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			idle, stop := newIdleReader(&endsOnCancelReader{ctx: ctx, data: tt.data}, 20*time.Millisecond, cancel)
			defer stop()
			resp := &http.Response{Header: http.Header{"Content-Type": {tt.contentType}}, Body: io.NopCloser(idle)}
			_ = providers.OpenRouter{}.ParseServed(resp)
			got, err := io.ReadAll(resp.Body)
			if !errors.Is(err, errUpstreamIdle) {
				t.Errorf("read error %v, want %v", err, errUpstreamIdle)
			}
			if string(got) != tt.data {
				t.Errorf("read %q, want %q", got, tt.data)
			}
		})
	}
}

func TestDeadlineWriter(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if got := newDeadlineWriter(rec, 0); got != http.ResponseWriter(rec) {
		t.Error("zero timeout must return the writer unchanged")
	}
	w := newDeadlineWriter(rec, time.Second)
	dw, ok := w.(*deadlineWriter)
	if !ok || dw.Unwrap() != http.ResponseWriter(rec) {
		t.Fatal("deadlineWriter must unwrap to the original writer")
	}
	// The recorder has no deadline support: writes and flushes still work.
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, "data: x\n\n"); err != nil {
		t.Fatal(err)
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Fatalf("flush through Unwrap: %v", err)
	}
	if rec.Body.String() != "data: x\n\n" || !rec.Flushed {
		t.Errorf("body=%q flushed=%v", rec.Body.String(), rec.Flushed)
	}
}

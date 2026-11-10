package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// errUpstreamIdle is the cancel cause when the upstream stays silent longer
// than Options.StreamIdleTimeout while sending a body.
var errUpstreamIdle = errors.New("proxy: upstream idle timeout")

// deadlineWriter renews the connection's write deadline before every write to
// the agent, so a client that stops reading makes the write fail instead of
// blocking the handler forever. Unwrap lets http.ResponseController (used by
// sse.Writer and flushWriter) reach the underlying Flusher.
type deadlineWriter struct {
	http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
}

// newDeadlineWriter wraps w; with timeout <= 0 it returns w unchanged.
func newDeadlineWriter(w http.ResponseWriter, timeout time.Duration) http.ResponseWriter {
	if timeout <= 0 {
		return w
	}
	return &deadlineWriter{ResponseWriter: w, rc: http.NewResponseController(w), timeout: timeout}
}

// renew moves the write deadline to now + timeout. Writers without deadline
// support (e.g. httptest.ResponseRecorder) are left as they are.
func (d *deadlineWriter) renew() {
	_ = d.rc.SetWriteDeadline(time.Now().Add(d.timeout))
}

func (d *deadlineWriter) WriteHeader(status int) {
	d.renew()
	d.ResponseWriter.WriteHeader(status)
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	d.renew()
	return d.ResponseWriter.Write(p) //nolint:wrapcheck // callers (sse.Writer, flushWriter) wrap it
}

// Unwrap is used by http.ResponseController.
func (d *deadlineWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }

// idleReader cancels the upstream request when a single Read blocks longer
// than timeout; that Read and every later one return errUpstreamIdle. The
// timer runs only while blocked in Read, so time spent writing to a slow
// agent does not count as upstream silence. Reads and stop must run on one
// goroutine.
type idleReader struct {
	r       io.Reader
	timer   *time.Timer
	timeout time.Duration
	err     error // sticky errUpstreamIdle once the timer fired
}

// newIdleReader returns r guarded by timeout, and a stop func that must be
// called when reading is over. With timeout <= 0 r is returned unchanged.
func newIdleReader(r io.Reader, timeout time.Duration, cancel context.CancelCauseFunc) (io.Reader, func()) {
	if timeout <= 0 {
		return r, func() {}
	}
	t := time.AfterFunc(timeout, func() { cancel(errUpstreamIdle) })
	t.Stop()
	ir := &idleReader{r: r, timer: t, timeout: timeout}
	return ir, func() { t.Stop() }
}

func (i *idleReader) Read(p []byte) (int, error) {
	if i.err != nil {
		return 0, i.err
	}
	i.timer.Reset(i.timeout)
	n, err := i.r.Read(p)
	if !i.timer.Stop() {
		// Stop reports false once the timer fired: we cancelled the request,
		// so whatever ended this read is a truncation, even a clean EOF
		// (over TLS the cancel can make the provider end the body cleanly).
		// A read that completed just as the timer fired counts too: the
		// attempt's context is cancelled either way. The error sticks: a
		// caller that drops it (a peek) must not then read a clean EOF.
		i.err = errUpstreamIdle
		return n, i.err
	}
	return n, err //nolint:wrapcheck // passthrough reader: callers inspect io.EOF
}

package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/providers"
	"github.com/Foncifelo/sidecar-proxy/internal/sse"
)

// maxUsageScan caps the in-memory copy of a non-streamed body kept only to
// read its usage. It is never stored or logged.
const maxUsageScan = 8 << 20

// streamErrorType is the SSE error sent after the first byte (contract, guarantee 5).
const streamErrorType = "upstream_stream_error"

// hopHeaders are never copied from the upstream response.
var hopHeaders = [...]string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Connection",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length",
}

// forward sends the buffered request upstream and copies the answer back.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, req *requestState, body []byte) {
	// upCtx is cancelled when the agent leaves or when the upstream stays
	// silent longer than StreamIdleTimeout mid-body.
	upCtx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	out, err := h.buildRequest(upCtx, r, req, body)
	if err != nil {
		h.localError(w, req, http.StatusBadGateway, ReasonUpstreamErr, "could not build upstream request")
		return
	}
	h.observeOverhead(req)
	// The URL is built from the YAML base_url, never from the agent.
	resp, err := h.o.Client.Do(out) //nolint:gosec // G704: target host comes from the client YAML; only the path is the agent's
	if err != nil {
		req.total = h.o.Now().Sub(req.start)
		h.upstreamFailed(w, r, req)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	req.status = resp.StatusCode
	// Every write to the agent from here on renews the write deadline.
	w = newDeadlineWriter(w, h.o.WriteTimeout)
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	_ = rc.Flush() // headers out now: the answer is always chunked

	upBody, stopIdle := newIdleReader(resp.Body, h.o.StreamIdleTimeout, cancel)
	defer stopIdle()
	fb := &firstByteReader{r: upBody, now: h.o.Now}
	if isEventStream(resp.Header.Get("Content-Type")) {
		h.copyStream(r.Context(), w, req, fb)
	} else {
		h.copyBody(r.Context(), w, req, fb)
	}
	req.total = h.o.Now().Sub(req.start)
	if !fb.first.IsZero() {
		req.ttft = fb.first.Sub(req.start)
	}
	h.emit(req)
}

// buildRequest returns the outgoing request. Chat goes through the generic
// adapter (model untouched in shadow, include_usage added when streaming)
// and keeps the query; everything else keeps method, path, query and body.
// Only allowlisted agent headers pass; the agent's Authorization never does (CONTRIBUTING.md rule 3).
func (h *Handler) buildRequest(ctx context.Context, r *http.Request, req *requestState, body []byte) (*http.Request, error) {
	up := h.o.Upstream
	if req.kind == events.KindChat {
		ep := providers.Endpoint{
			ID: UpstreamEndpointID, Provider: up.Provider, Channel: providers.ChannelDirect,
			BaseURL: up.BaseURL, APIKey: up.APIKey, RewriteModel: false,
		}
		out, err := providers.OpenAICompatible{}.BuildRequest(ctx, ep, body)
		if err == nil {
			out.URL.RawQuery = r.URL.RawQuery // same as the other routes: only path and query are the agent's
			providers.CopyAllowedHeaders(out, r.Header)
			return out, nil
		}
		if !errors.Is(err, providers.ErrInvalidBody) {
			return nil, fmt.Errorf("proxy: build chat request: %w", err)
		}
		// Not a JSON object: pass it on as is and let the provider answer.
	}
	target := up.BaseURL + strings.TrimPrefix(r.URL.Path, "/v1")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	out, err := http.NewRequestWithContext(ctx, r.Method, target, rd) //nolint:gosec // G704: host from the YAML base_url; agent controls only the path under it
	if err != nil {
		return nil, fmt.Errorf("proxy: build upstream request: %w", err)
	}
	out.Header.Set("Authorization", "Bearer "+up.APIKey.Reveal())
	out.Header.Set("Accept-Encoding", "identity")
	providers.CopyAllowedHeaders(out, r.Header)
	return out, nil
}

// upstreamFailed answers when no response arrived at all.
func (h *Handler) upstreamFailed(w http.ResponseWriter, r *http.Request, req *requestState) {
	req.noResponse = true
	if r.Context().Err() != nil {
		req.errorType = "client_gone"
		req.streamState = streamStateIf(req.stream, events.StreamAborted)
		h.setDecision(w, req, ReasonUpstreamErr)
		h.emit(req)
		return
	}
	req.errorType = "upstream_unreachable"
	h.setDecision(w, req, ReasonUpstreamErr)
	writeLocalError(w, http.StatusBadGateway, ReasonUpstreamErr, "upstream did not answer")
	h.emit(req)
}

// copyStream copies SSE bytes unchanged (minus the usage chunk the agent did
// not ask for and, in route, with the agent's top-level model in each chunk,
// ADR 0008) while parsing the provider's bytes for usage.
func (h *Handler) copyStream(ctx context.Context, w http.ResponseWriter, req *requestState, body *firstByteReader) {
	sw := sse.NewWriter(w)
	tail := &tailWriter{w: sw}
	var dst io.Writer = tail
	var closers []io.Closer // innermost last; closed outermost first
	if req.rewriteModel {
		rw := sse.NewModelRewriter(dst, req.rawModel)
		dst = rw
		closers = append(closers, rw)
	}
	if req.kind == events.KindChat && req.stream && !req.includeUsage {
		stripper := sse.NewUsageStripper(dst)
		dst = stripper
		closers = append(closers, stripper)
	}
	_, err := sse.Tee(dst, body, func(ev sse.Event) error {
		if ev.HasData && !ev.IsDone() {
			req.onData([]byte(ev.Data))
		}
		return nil
	})
	if err == nil {
		err = closeAll(closers)
	}
	switch {
	case body.err != nil && ctx.Err() == nil:
		_ = closeAll(closers)
		req.streamState = events.StreamTruncated
		req.errorType = streamErrorType
		h.writeStreamError(tail, sw, req.id)
	case err != nil || ctx.Err() != nil:
		req.streamState = events.StreamAborted
		req.errorType = "client_gone"
	default:
		req.streamState = events.StreamComplete
	}
}

// closeAll flushes the pending block of every SSE filter, outermost first, so
// each one hands its tail to the next. It stops at the first error.
func closeAll(closers []io.Closer) error {
	for i := len(closers) - 1; i >= 0; i-- {
		if err := closers[i].Close(); err != nil {
			return fmt.Errorf("proxy: flush stream: %w", err)
		}
	}
	return nil
}

// writeStreamError closes any half-written block and sends event: error.
func (h *Handler) writeStreamError(tail *tailWriter, sw *sse.Writer, id string) {
	if sep := tail.missingSeparator(); sep != "" {
		_, _ = sw.Write([]byte(sep))
	}
	data := fmt.Sprintf(`{"error":{"type":%q,"request_id":%q}}`, streamErrorType, id)
	if err := sw.WriteEvent(sse.Event{Type: "error", Data: data}); err != nil {
		h.o.Logger.Debug("stream error event not delivered", "request_id", id)
	}
}

// copyBody copies a non-SSE body with a flush per read and keeps a bounded
// in-memory copy to read usage.
func (h *Handler) copyBody(ctx context.Context, w http.ResponseWriter, req *requestState, body *firstByteReader) {
	scan := &limitedBuffer{max: maxUsageScan}
	fw := &flushWriter{w: w, rc: http.NewResponseController(w)}
	_, err := io.Copy(io.MultiWriter(fw, scan), body)
	if req.stream {
		req.streamState = events.StreamComplete
	}
	switch {
	case body.err != nil && ctx.Err() == nil:
		req.errorType = "upstream_body_error"
		req.streamState = streamStateIf(req.stream, events.StreamTruncated)
		return
	case err != nil:
		req.errorType = "client_gone"
		req.streamState = streamStateIf(req.stream, events.StreamAborted)
		return
	}
	if !scan.overflow && req.status < http.StatusBadRequest {
		req.onBody(scan.buf.Bytes())
	}
}

func streamStateIf(stream bool, s events.StreamState) events.StreamState {
	if stream {
		return s
	}
	return events.StreamNone
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		if strings.HasPrefix(k, "X-Sidecar-") {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	for _, k := range hopHeaders {
		dst.Del(k)
	}
}

func isEventStream(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "text/event-stream"
}

// firstByteReader records when the first body byte arrived and the first
// non-EOF read error (an upstream cut).
type firstByteReader struct {
	r     io.Reader
	now   func() time.Time
	first time.Time
	err   error
}

func (f *firstByteReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 && f.first.IsZero() {
		f.first = f.now()
	}
	if err != nil && !errors.Is(err, io.EOF) && f.err == nil {
		f.err = err
	}
	return n, err //nolint:wrapcheck // passthrough reader: callers inspect io.EOF
}

// tailWriter remembers the last two bytes written to the agent.
type tailWriter struct {
	w    io.Writer
	last [2]byte
	n    int64
}

func (t *tailWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if n > 0 {
		t.n += int64(n)
		if n >= 2 {
			t.last = [2]byte{p[n-2], p[n-1]}
		} else {
			t.last = [2]byte{t.last[1], p[0]}
		}
	}
	return n, err //nolint:wrapcheck // sse.Writer already wraps its errors
}

// missingSeparator returns what must be written so that the next event does
// not merge with a half-written block.
func (t *tailWriter) missingSeparator() string {
	switch {
	case t.n == 0 || t.last == [2]byte{'\n', '\n'}:
		return ""
	case t.last[1] == '\n':
		return "\n"
	default:
		return "\n\n"
	}
}

// flushWriter flushes after every write.
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		return n, fmt.Errorf("proxy: write: %w", err)
	}
	if err := f.rc.Flush(); err != nil {
		return n, fmt.Errorf("proxy: flush: %w", err)
	}
	return n, nil
}

// limitedBuffer keeps up to max bytes and then only reports overflow.
type limitedBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.overflow || l.buf.Len()+len(p) > l.max {
		l.overflow = true
		l.buf.Reset()
		return len(p), nil
	}
	return l.buf.Write(p) //nolint:wrapcheck // bytes.Buffer.Write never fails
}

package sse

import (
	"errors"
	"fmt"
	"io"
)

// TeeResult summarizes a Tee run.
type TeeResult struct {
	Bytes       int64 // bytes copied to dst
	Events      int   // events delivered to the callback
	CallbackErr error // first error returned by the callback; later events were not delivered
	ParseErr    error // parse error after which events stopped but the copy went on
}

// Tee copies src to dst byte for byte and, alongside, parses the same bytes
// and calls fn for every event. Bytes reach dst before they are parsed, so
// parsing never alters or holds back the copy. If fn returns an error it is
// not called again and the copy continues; the same holds for parse errors.
// fn runs synchronously and should be cheap. The returned error is only for
// read or write failures.
func Tee(dst io.Writer, src io.Reader, fn func(Event) error) (TeeResult, error) {
	tr := &teeReader{r: src, w: dst}
	rd := NewReader(tr)
	var res TeeResult
	for {
		ev, err := rd.Next()
		if err != nil {
			res.Bytes = tr.n
			return finishTee(res, tr, err)
		}
		if res.CallbackErr != nil {
			continue
		}
		res.Events++
		if cbErr := fn(ev); cbErr != nil {
			res.CallbackErr = cbErr
		}
	}
}

// finishTee classifies the reader error: EOF ends cleanly, I/O errors are
// returned, and parse errors drain the rest of src into dst.
func finishTee(res TeeResult, tr *teeReader, err error) (TeeResult, error) {
	if tr.err != nil {
		return res, tr.err
	}
	if errors.Is(err, io.EOF) {
		return res, nil
	}
	if !errors.Is(err, ErrEventTooLarge) {
		return res, err
	}
	res.ParseErr = err
	_, cpErr := io.Copy(io.Discard, tr)
	res.Bytes = tr.n
	if tr.err != nil {
		return res, tr.err
	}
	if cpErr != nil {
		return res, fmt.Errorf("sse: tee drain: %w", cpErr)
	}
	return res, nil
}

// teeReader writes every byte it reads to w before returning it and keeps
// the first I/O error so Tee can tell it apart from parse errors.
type teeReader struct {
	r   io.Reader
	w   io.Writer
	n   int64
	err error
}

func (t *teeReader) Read(p []byte) (int, error) {
	if t.err != nil {
		return 0, t.err
	}
	n, err := t.r.Read(p)
	if n > 0 {
		wn, werr := t.w.Write(p[:n])
		t.n += int64(wn)
		if werr == nil && wn < n {
			werr = io.ErrShortWrite
		}
		if werr != nil {
			t.err = fmt.Errorf("sse: tee write: %w", werr)
			return n, t.err
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		t.err = fmt.Errorf("sse: tee read: %w", err)
		return n, t.err
	}
	return n, err //nolint:wrapcheck // io.EOF is returned unwrapped by convention
}

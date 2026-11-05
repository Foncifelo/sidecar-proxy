package sse

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWriterFlushesAndIsChunked(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Length", "999")
		rw.Header().Set("Content-Type", "text/event-stream")
		w := NewWriter(rw)
		if err := w.WriteEvent(Event{Type: "message", ID: "1", Data: "a\nb"}); err != nil {
			t.Errorf("WriteEvent: %v", err)
		}
		<-release
		if err := w.WriteComment("bye\nnow"); err != nil {
			t.Errorf("WriteComment: %v", err)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ContentLength != -1 || resp.Header.Get("Content-Length") != "" {
		t.Errorf("Content-Length set: %d %q", resp.ContentLength, resp.Header.Get("Content-Length"))
	}
	if len(resp.TransferEncoding) == 0 || resp.TransferEncoding[0] != "chunked" {
		t.Errorf("TransferEncoding = %v, want chunked", resp.TransferEncoding)
	}
	// The first event must arrive while the handler is still blocked: proof of Flush.
	ev, err := NewReader(resp.Body).Next()
	if err != nil {
		t.Fatalf("reading flushed event: %v", err)
	}
	want := "event: message\nid: 1\ndata: a\ndata: b\n\n"
	if string(ev.Raw) != want {
		t.Errorf("raw = %q, want %q", ev.Raw, want)
	}
	close(release)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != ": bye now\n\n" {
		t.Errorf("comment = %q", rest)
	}
}

func TestWriterWriteEventInvalid(t *testing.T) {
	tests := []Event{
		{Type: "a\nb", Data: "x"},
		{ID: "1\r", Data: "x"},
	}
	for _, ev := range tests {
		w := NewWriter(httptest.NewRecorder())
		if err := w.WriteEvent(ev); !errors.Is(err, ErrInvalidField) {
			t.Errorf("WriteEvent(%+v) err = %v, want ErrInvalidField", ev, err)
		}
	}
}

func TestWriterDataLineBreaks(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := NewWriter(rec).WriteEvent(Event{Data: "a\r\nb\rc"}); err != nil {
		t.Fatal(err)
	}
	if got, want := rec.Body.String(), "data: a\ndata: b\ndata: c\n\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if !rec.Flushed {
		t.Error("recorder not flushed")
	}
}

type noFlush struct{ h http.Header }

func (n *noFlush) Header() http.Header         { return n.h }
func (n *noFlush) Write(p []byte) (int, error) { return len(p), nil }
func (n *noFlush) WriteHeader(int)             {}

type failWrite struct{ noFlush }

func (f *failWrite) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestWriterErrors(t *testing.T) {
	tests := []struct {
		name string
		rw   http.ResponseWriter
		want error
	}{
		{"flush unsupported", &noFlush{h: http.Header{}}, http.ErrNotSupported},
		{"write fails", &failWrite{noFlush{h: http.Header{}}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewWriter(tt.rw).Write([]byte("data: x\n\n"))
			if err == nil {
				t.Fatal("expected error")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if !strings.HasPrefix(err.Error(), "sse: ") {
				t.Errorf("error not wrapped with context: %v", err)
			}
		})
	}
}

package sse

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"testing/iotest"
)

func loadFixture(t testing.TB) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/stream_with_usage.sse")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTee(t *testing.T) {
	fixture := string(loadFixture(t))
	cbFail := errors.New("callback failed")
	tests := []struct {
		name       string
		in         string
		failAt     int // callback fails on this call (1-based); 0 never
		wantEvents int
		wantCbErr  bool
	}{
		{"fixture", fixture, 0, 5, false},
		{"callback fails, copy continues", fixture, 2, 2, true},
		{"crlf without trailing blank line", "data: a\r\n\r\ndata: b", 0, 2, false},
		{"empty", "", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, oneByte := range []bool{false, true} {
				var src io.Reader = strings.NewReader(tt.in)
				if oneByte {
					src = iotest.OneByteReader(src)
				}
				var dst bytes.Buffer
				calls := 0
				var types []string
				res, err := Tee(&dst, src, func(ev Event) error {
					calls++
					types = append(types, ev.Data)
					if calls == tt.failAt {
						return cbFail
					}
					return nil
				})
				if err != nil {
					t.Fatalf("Tee: %v", err)
				}
				if dst.String() != tt.in {
					t.Errorf("copy differs: got %q", dst.String())
				}
				if res.Bytes != int64(len(tt.in)) {
					t.Errorf("Bytes = %d, want %d", res.Bytes, len(tt.in))
				}
				if res.Events != tt.wantEvents || calls != tt.wantEvents {
					t.Errorf("Events = %d calls = %d, want %d", res.Events, calls, tt.wantEvents)
				}
				if tt.wantCbErr != errors.Is(res.CallbackErr, cbFail) {
					t.Errorf("CallbackErr = %v", res.CallbackErr)
				}
			}
		})
	}
}

func TestTeeDeliversDoneLast(t *testing.T) {
	var last Event
	if _, err := Tee(io.Discard, bytes.NewReader(loadFixture(t)), func(ev Event) error {
		last = ev
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !last.IsDone() {
		t.Errorf("last event = %+v, want [DONE]", last)
	}
}

func TestTeeParseErrorCopyContinues(t *testing.T) {
	in := "data: a\n\ndata: " + strings.Repeat("x", MaxEventSize) + "\n\ndata: tail\n\n"
	var dst bytes.Buffer
	res, err := Tee(&dst, strings.NewReader(in), func(Event) error { return nil })
	if err != nil {
		t.Fatalf("Tee: %v", err)
	}
	if !errors.Is(res.ParseErr, ErrEventTooLarge) {
		t.Errorf("ParseErr = %v", res.ParseErr)
	}
	if dst.String() != in || res.Bytes != int64(len(in)) {
		t.Errorf("copy incomplete: %d of %d bytes", dst.Len(), len(in))
	}
	if res.Events != 1 {
		t.Errorf("Events = %d, want 1", res.Events)
	}
}

type errWriter struct{ n int }

func (e errWriter) Write(p []byte) (int, error) {
	if e.n >= 0 {
		return min(e.n, len(p)), nil
	}
	return 0, errors.New("agent gone")
}

func TestTeeIOErrors(t *testing.T) {
	boom := errors.New("upstream reset")
	big := "data: " + strings.Repeat("x", MaxEventSize) + "\n\n"
	tests := []struct {
		name string
		dst  io.Writer
		src  io.Reader
		want error
	}{
		{"write error", errWriter{n: -1}, strings.NewReader("data: a\n\n"), nil},
		{"short write", errWriter{n: 1}, strings.NewReader("data: a\n\n"), io.ErrShortWrite},
		{"read error", io.Discard, io.MultiReader(strings.NewReader("data: a\n\n"), iotest.ErrReader(boom)), boom},
		{"read error while draining", io.Discard, io.MultiReader(strings.NewReader(big), iotest.ErrReader(boom)), boom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Tee(tt.dst, tt.src, func(Event) error { return nil })
			if err == nil {
				t.Fatal("expected error")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func BenchmarkTee(b *testing.B) {
	stream := bytes.Repeat(loadFixture(b), 100)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Tee(io.Discard, bytes.NewReader(stream), func(Event) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

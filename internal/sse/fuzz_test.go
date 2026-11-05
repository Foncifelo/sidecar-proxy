package sse

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func FuzzReader(f *testing.F) {
	seeds := []string{
		": keepalive\n\ndata: a\ndata: b\n\n",
		"event: x\r\ndata: y\r\n\r\ndata: [DONE]",
		"data: {\"choices\":[],\"usage\":{}}\n\n",
		"id\nfoo:bar\n\r\n\n:",
		"\r\r\n\n\x00data",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		rd := NewReader(bytes.NewReader(in))
		var raw []byte
		for {
			ev, err := rd.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			raw = append(raw, ev.Raw...)
		}
		if !bytes.Equal(raw, in) {
			t.Fatalf("raw blocks do not reassemble the input")
		}
		var dst bytes.Buffer
		if _, err := Tee(&dst, bytes.NewReader(in), func(Event) error { return nil }); err != nil {
			t.Fatalf("Tee: %v", err)
		}
		if !bytes.Equal(dst.Bytes(), in) {
			t.Fatalf("Tee copy differs")
		}
		s := NewUsageStripper(io.Discard)
		if _, err := s.Write(in); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

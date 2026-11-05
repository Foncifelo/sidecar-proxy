package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

type wantEvent struct {
	typ      string
	data     string
	id       string
	hasData  bool
	comments int
	raw      string
}

func readAll(t *testing.T, r io.Reader) ([]Event, error) {
	t.Helper()
	rd := NewReader(r)
	var out []Event
	for {
		ev, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
}

func TestReader(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []wantEvent
	}{
		{
			name: "keepalive comment",
			in:   ": keepalive\n\ndata: x\n\n",
			want: []wantEvent{
				{comments: 1, raw: ": keepalive\n\n"},
				{data: "x", hasData: true, raw: "data: x\n\n"},
			},
		},
		{
			name: "multiple data lines",
			in:   "data: a\ndata: b\ndata:c\n\n",
			want: []wantEvent{{data: "a\nb\nc", hasData: true, raw: "data: a\ndata: b\ndata:c\n\n"}},
		},
		{
			name: "CRLF",
			in:   "event: ping\r\ndata: a\r\ndata: b\r\n\r\n",
			want: []wantEvent{{typ: "ping", data: "a\nb", hasData: true, raw: "event: ping\r\ndata: a\r\ndata: b\r\n\r\n"}},
		},
		{
			name: "mixed CRLF and LF",
			in:   "data: a\r\n\ndata: b\n\r\n",
			want: []wantEvent{
				{data: "a", hasData: true, raw: "data: a\r\n\n"},
				{data: "b", hasData: true, raw: "data: b\n\r\n"},
			},
		},
		{
			name: "event field",
			in:   "event: error\ndata: {\"error\":{}}\n\n",
			want: []wantEvent{{typ: "error", data: `{"error":{}}`, hasData: true, raw: "event: error\ndata: {\"error\":{}}\n\n"}},
		},
		{
			name: "last event without blank line",
			in:   "data: a\n\ndata: b\n",
			want: []wantEvent{
				{data: "a", hasData: true, raw: "data: a\n\n"},
				{data: "b", hasData: true, raw: "data: b\n"},
			},
		},
		{
			name: "last line without newline",
			in:   "data: b",
			want: []wantEvent{{data: "b", hasData: true, raw: "data: b"}},
		},
		{
			name: "done sentinel",
			in:   "data: [DONE]\n\n",
			want: []wantEvent{{data: "[DONE]", hasData: true, raw: "data: [DONE]\n\n"}},
		},
		{
			name: "id, unknown field, field without colon",
			in:   "id: 7\nretry: 10\nfoo\ndata\n\n",
			want: []wantEvent{{id: "7", data: "", hasData: true, raw: "id: 7\nretry: 10\nfoo\ndata\n\n"}},
		},
		{
			name: "id with NUL ignored",
			in:   "id: a\x00b\ndata: x\n\n",
			want: []wantEvent{{data: "x", hasData: true, raw: "id: a\x00b\ndata: x\n\n"}},
		},
		{
			name: "empty input",
			in:   "",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, mode := range []string{"whole", "onebyte"} {
				var r io.Reader = strings.NewReader(tt.in)
				if mode == "onebyte" {
					r = iotest.OneByteReader(r)
				}
				got, err := readAll(t, r)
				if err != nil {
					t.Fatalf("%s: unexpected error: %v", mode, err)
				}
				if len(got) != len(tt.want) {
					t.Fatalf("%s: got %d events, want %d: %+v", mode, len(got), len(tt.want), got)
				}
				for i, w := range tt.want {
					g := got[i]
					if g.Type != w.typ || g.Data != w.data || g.ID != w.id || g.HasData != w.hasData ||
						len(g.Comments) != w.comments || string(g.Raw) != w.raw {
						t.Errorf("%s: event %d = %+v (raw %q), want %+v", mode, i, g, g.Raw, w)
					}
				}
			}
		})
	}
}

func TestEventIsDone(t *testing.T) {
	tests := []struct {
		ev   Event
		want bool
	}{
		{Event{Data: "[DONE]", HasData: true}, true},
		{Event{Data: "[DONE]"}, false},
		{Event{Data: "x", HasData: true}, false},
	}
	for _, tt := range tests {
		if got := tt.ev.IsDone(); got != tt.want {
			t.Errorf("IsDone(%+v) = %v, want %v", tt.ev, got, tt.want)
		}
	}
}

func TestReaderEventTooLarge(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"one long line", "data: " + strings.Repeat("x", MaxEventSize) + "\n\n"},
		{"many lines", strings.Repeat("data: "+strings.Repeat("x", 1<<20)+"\n", 9) + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readAll(t, strings.NewReader(tt.in))
			if !errors.Is(err, ErrEventTooLarge) {
				t.Fatalf("err = %v, want ErrEventTooLarge", err)
			}
		})
	}
}

func TestReaderReadError(t *testing.T) {
	boom := errors.New("boom")
	_, err := readAll(t, io.MultiReader(strings.NewReader("data: a\n"), iotest.ErrReader(boom)))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}

package sse

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestHoldFirstData(t *testing.T) {
	errCut := errors.New("fictitious cut")
	big := ": " + strings.Repeat("k", 4096) + "\n\n"
	tests := []struct {
		name     string
		src      func() io.Reader
		wantErr  error
		wantHeld string // prefix the held bytes must start with; "" skips
		replay   string // full stream that MultiReader(held, rest) must yield; "" skips
	}{
		{
			name:     "first data block",
			src:      func() io.Reader { return strings.NewReader("data: a\n\ndata: b\n\n") },
			wantHeld: "data: a\n\n",
			replay:   "data: a\n\ndata: b\n\n",
		},
		{
			name:     "keepalives before data are held",
			src:      func() io.Reader { return strings.NewReader(": ping\n\n: ping\n\ndata: a\n\ndata: [DONE]\n\n") },
			wantHeld: ": ping\n\n: ping\n\ndata: a\n\n",
			replay:   ": ping\n\n: ping\n\ndata: a\n\ndata: [DONE]\n\n",
		},
		{
			name:   "one byte at a time keeps every byte",
			src:    func() io.Reader { return iotest.OneByteReader(strings.NewReader("event: x\ndata: a\n\ndata: b\n\n")) },
			replay: "event: x\ndata: a\n\ndata: b\n\n",
		},
		{
			name:     "eof without data",
			src:      func() io.Reader { return strings.NewReader(": only a comment\n\n") },
			wantErr:  io.EOF,
			wantHeld: ": only a comment\n\n",
		},
		{
			name:     "read error before data",
			src:      func() io.Reader { return io.MultiReader(strings.NewReader(": ping\n\n"), iotest.ErrReader(errCut)) },
			wantErr:  errCut,
			wantHeld: ": ping\n\n",
		},
		{
			name:    "held bytes are bounded",
			src:     func() io.Reader { return strings.NewReader(strings.Repeat(big, MaxEventSize/len(big)+2)) },
			wantErr: ErrEventTooLarge,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := tt.src()
			held, err := HoldFirstData(src)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err %v, want %v", err, tt.wantErr)
			}
			if len(held) > MaxEventSize {
				t.Errorf("held %d bytes, over MaxEventSize", len(held))
			}
			if !strings.HasPrefix(string(held), tt.wantHeld) {
				t.Errorf("held %q, want prefix %q", held, tt.wantHeld)
			}
			if tt.replay == "" {
				return
			}
			all, rerr := io.ReadAll(io.MultiReader(bytes.NewReader(held), src))
			if rerr != nil || string(all) != tt.replay {
				t.Errorf("replay %q (%v), want %q", all, rerr, tt.replay)
			}
		})
	}
}

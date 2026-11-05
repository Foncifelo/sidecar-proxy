package sse

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// UsageStripper is an io.Writer placed on the agent side of the copy. It
// forwards SSE bytes unchanged, block by block, except the trailing usage
// chunk (a JSON object with empty "choices" and non-null "usage") that the
// sidecar requested on the agent's behalf. Blocks larger than MaxEventSize
// are forwarded without inspection. Call Close at the end of the stream to
// emit a final block that had no terminating blank line.
type UsageStripper struct {
	dst      io.Writer
	blocks   blockSplitter
	stripped bool
}

// NewUsageStripper returns a stripper that writes to dst.
func NewUsageStripper(dst io.Writer) *UsageStripper {
	return &UsageStripper{dst: dst}
}

// Stripped reports whether a usage chunk was removed.
func (s *UsageStripper) Stripped() bool { return s.stripped }

// Write buffers p and forwards every complete block it closes.
func (s *UsageStripper) Write(p []byte) (int, error) {
	if err := s.blocks.write(p, s.emit, s.forward); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close emits any pending bytes as a final block. It does not close dst.
func (s *UsageStripper) Close() error {
	return s.blocks.flush(s.emit)
}

func (s *UsageStripper) emit(block []byte) error {
	if isUsageBlock(block) {
		s.stripped = true
		return nil
	}
	return s.forward(block)
}

func (s *UsageStripper) forward(b []byte) error {
	if _, err := s.dst.Write(b); err != nil {
		return fmt.Errorf("sse: usage stripper: %w", err)
	}
	return nil
}

var usageKey = []byte(`"usage"`)

// isUsageBlock reports whether block carries the final usage-only chunk.
func isUsageBlock(block []byte) bool {
	if !bytes.Contains(block, usageKey) {
		return false
	}
	ev, err := NewReader(bytes.NewReader(block)).Next()
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	if !ev.HasData || (ev.Type != "" && ev.Type != "message") {
		return false
	}
	var chunk struct {
		Choices *[]json.RawMessage `json:"choices"`
		Usage   json.RawMessage    `json:"usage"`
	}
	if json.Unmarshal([]byte(ev.Data), &chunk) != nil {
		return false
	}
	return chunk.Choices != nil && len(*chunk.Choices) == 0 &&
		len(chunk.Usage) > 0 && !bytes.Equal(chunk.Usage, []byte("null"))
}

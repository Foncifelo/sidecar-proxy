package sse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Foncifelo/sidecar-proxy/pkg/openai"
)

// ModelRewriter is an io.Writer placed on the agent side of a routed stream
// (ADR 0008). In every block whose data payload is a JSON object with a
// top-level "model" member it replaces that member's value with the agent's
// model; every other byte, and every other block, is forwarded unchanged.
// Blocks larger than MaxEventSize are forwarded without inspection. Call
// Close at the end of the stream to emit a final block that had no
// terminating blank line.
type ModelRewriter struct {
	dst    io.Writer
	val    []byte // the agent's model as a JSON string
	blocks blockSplitter
}

// NewModelRewriter returns a rewriter that writes to dst and sets the
// top-level "model" of every chunk to model.
func NewModelRewriter(dst io.Writer, model string) *ModelRewriter {
	val, err := json.Marshal(model)
	if err != nil {
		val = nil // unreachable for a Go string; nil disables rewriting
	}
	return &ModelRewriter{dst: dst, val: val}
}

// Write buffers p and forwards every complete block it closes.
func (m *ModelRewriter) Write(p []byte) (int, error) {
	if err := m.blocks.write(p, m.emit, m.forward); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close emits any pending bytes as a final block. It does not close dst.
func (m *ModelRewriter) Close() error {
	return m.blocks.flush(m.emit)
}

func (m *ModelRewriter) emit(block []byte) error {
	if len(block) > MaxEventSize {
		return m.forward(block)
	}
	return m.forward(rewriteModel(block, m.val))
}

func (m *ModelRewriter) forward(b []byte) error {
	if _, err := m.dst.Write(b); err != nil {
		return fmt.Errorf("sse: model rewriter: %w", err)
	}
	return nil
}

// dataSegment is the value of one data: line, as a byte range of the block
// and as an offset into the joined payload.
type dataSegment struct {
	rawStart, rawEnd int
	joinedStart      int
}

var (
	modelKey  = []byte("model")
	escapeSeq = []byte(`\u`)
)

// rewriteModel returns block with the value of the top-level "model" of its
// data payload replaced by val (an encoded JSON value). The payload is the
// data: lines joined with "\n", as Reader builds it. The block is returned
// unchanged when val is empty, the payload is not a JSON object, it has no
// top-level "model", or a value spans two data: lines.
func rewriteModel(block, val []byte) []byte {
	if len(val) == 0 || (!bytes.Contains(block, modelKey) && !bytes.Contains(block, escapeSeq)) {
		return block
	}
	segs, joined := dataSegments(block)
	if len(segs) == 0 {
		return block
	}
	spans := openai.TopLevelSpans(joined, "model")
	if len(spans) == 0 {
		return block
	}
	raw := make([]openai.Span, 0, len(spans))
	for _, sp := range spans {
		r, ok := toRaw(segs, sp)
		if !ok {
			return block
		}
		raw = append(raw, r)
	}
	return openai.ReplaceSpans(block, raw, val)
}

// dataSegments locates the value of every data: line of block and returns
// them with the joined payload.
func dataSegments(block []byte) ([]dataSegment, []byte) {
	var (
		segs   []dataSegment
		joined []byte
	)
	for pos := 0; pos < len(block); {
		end := len(block)
		if i := bytes.IndexByte(block[pos:], '\n'); i >= 0 {
			end = pos + i + 1
		}
		line := trimEOL(block[pos:end])
		if start, ok := dataValueStart(line); ok {
			if len(segs) > 0 {
				joined = append(joined, '\n')
			}
			segs = append(segs, dataSegment{rawStart: pos + start, rawEnd: pos + len(line), joinedStart: len(joined)})
			joined = append(joined, line[start:]...)
		}
		pos = end
	}
	return segs, joined
}

// dataValueStart reports whether line is a data: field and where its value
// starts, following parseField: one optional space after the colon.
func dataValueStart(line []byte) (int, bool) {
	name, start := line, len(line)
	if i := bytes.IndexByte(line, ':'); i >= 0 {
		name, start = line[:i], i+1
		if start < len(line) && line[start] == ' ' {
			start++
		}
	}
	return start, string(name) == "data"
}

// toRaw maps a span of the joined payload to the block, if it lies within a
// single data: line.
func toRaw(segs []dataSegment, sp openai.Span) (openai.Span, bool) {
	for _, s := range segs {
		lineEnd := s.joinedStart + (s.rawEnd - s.rawStart)
		if sp.Start >= s.joinedStart && sp.End <= lineEnd {
			off := s.rawStart - s.joinedStart
			return openai.Span{Start: sp.Start + off, End: sp.End + off}, true
		}
	}
	return openai.Span{}, false
}

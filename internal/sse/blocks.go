package sse

import "bytes"

// blockSplitter buffers SSE bytes written in arbitrary pieces and hands out
// complete blocks (up to and including the blank line that ends them). A
// pending block larger than MaxEventSize is handed out raw, uninspected.
type blockSplitter struct {
	buf     []byte
	scanned int // bytes of buf already known not to end a block
}

// write appends p and calls block for every complete block, then raw for an
// oversized pending block. It stops at the first error.
func (s *blockSplitter) write(p []byte, block, raw func([]byte) error) error {
	s.buf = append(s.buf, p...)
	for {
		end := s.blockEnd()
		if end < 0 {
			break
		}
		if err := block(s.buf[:end]); err != nil {
			return err
		}
		s.buf = s.buf[end:]
		s.scanned = 0
	}
	if len(s.buf) > MaxEventSize {
		if err := raw(s.buf); err != nil {
			return err
		}
		s.buf, s.scanned = nil, 0
	}
	return nil
}

// flush calls block with any pending bytes, a final block without a
// terminating blank line.
func (s *blockSplitter) flush(block func([]byte) error) error {
	if len(s.buf) == 0 {
		return nil
	}
	b := s.buf
	s.buf, s.scanned = nil, 0
	return block(b)
}

// blockEnd returns the index just past the blank line that ends the first
// block in buf, or -1 if no block is complete yet.
func (s *blockSplitter) blockEnd() int {
	for {
		i := bytes.IndexByte(s.buf[s.scanned:], '\n')
		if i < 0 {
			return -1
		}
		lineEnd := s.scanned + i + 1
		line := trimEOL(s.buf[s.scanned:lineEnd])
		s.scanned = lineEnd
		if len(line) == 0 {
			return lineEnd
		}
	}
}

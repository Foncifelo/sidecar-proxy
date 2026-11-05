package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MaxEventSize bounds the raw size of a single event block so a broken or
// hostile stream cannot make the reader buffer without limit.
const MaxEventSize = 8 << 20

// doneSentinel is the data payload OpenAI-compatible providers send last.
const doneSentinel = "[DONE]"

// ErrEventTooLarge is returned when an event block exceeds MaxEventSize.
var ErrEventTooLarge = errors.New("sse: event exceeds maximum size")

// Event is one SSE block: the lines up to and including a blank line.
// Blocks made only of comments are returned too (HasData false) so callers
// that forward bytes do not lose keepalives.
type Event struct {
	Type     string   // value of the event: field; empty means "message"
	Data     string   // data: lines joined with "\n"
	ID       string   // value of the id: field
	HasData  bool     // at least one data: line was present
	Comments []string // comment lines without the leading ':'
	Raw      []byte   // exact bytes of the block, including the terminating blank line if any
}

// IsDone reports whether the event is the "data: [DONE]" terminator.
func (e Event) IsDone() bool {
	return e.HasData && e.Data == doneSentinel
}

// Reader parses SSE blocks from an io.Reader. Line terminators are "\n" and
// "\r\n"; a lone "\r" is not treated as a terminator. A final block without a
// trailing blank line is still returned.
type Reader struct {
	br *bufio.Reader
}

// NewReader returns a Reader that parses events from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReader(r)}
}

// Next returns the next block. It returns io.EOF when the stream ended with
// no pending block, and ErrEventTooLarge when a block exceeds MaxEventSize.
func (r *Reader) Next() (Event, error) {
	var (
		ev   Event
		data strings.Builder
		raw  []byte
	)
	for {
		line, err := r.readLine(len(raw))
		raw = append(raw, line...)
		if len(line) > 0 {
			content := trimEOL(line)
			if len(content) == 0 {
				ev.Raw = raw
				ev.Data = data.String()
				return ev, nil
			}
			parseField(&ev, &data, content)
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) && len(raw) > 0 {
			ev.Raw = raw
			ev.Data = data.String()
			return ev, nil
		}
		return Event{}, err
	}
}

// readLine reads one line including its terminator, enforcing MaxEventSize
// on the block accumulated so far (used bytes).
func (r *Reader) readLine(used int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.br.ReadSlice('\n')
		if used+len(line)+len(chunk) > MaxEventSize {
			return nil, ErrEventTooLarge
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return line, fmt.Errorf("sse: read: %w", err)
		}
		return line, err //nolint:wrapcheck // io.EOF is returned unwrapped by convention
	}
}

func trimEOL(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r"))
}

// parseField applies one non-empty line to the event being built.
func parseField(ev *Event, data *strings.Builder, line []byte) {
	if line[0] == ':' {
		ev.Comments = append(ev.Comments, strings.TrimPrefix(string(line[1:]), " "))
		return
	}
	name, value := string(line), ""
	if i := bytes.IndexByte(line, ':'); i >= 0 {
		name = string(line[:i])
		value = strings.TrimPrefix(string(line[i+1:]), " ")
	}
	switch name {
	case "data":
		if ev.HasData {
			data.WriteByte('\n')
		}
		data.WriteString(value)
		ev.HasData = true
	case "event":
		ev.Type = value
	case "id":
		if !strings.ContainsRune(value, 0) {
			ev.ID = value
		}
	}
}

package sse

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrInvalidField is returned when an event type or id contains a newline.
var ErrInvalidField = errors.New("sse: event field contains a line break")

// Writer writes SSE bytes to an http.ResponseWriter and flushes after every
// write through http.ResponseController. It never sets Content-Length, so the
// response to the agent is always chunked.
type Writer struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

// NewWriter wraps w. It removes any Content-Length header copied from upstream.
func NewWriter(w http.ResponseWriter) *Writer {
	w.Header().Del("Content-Length")
	return &Writer{w: w, rc: http.NewResponseController(w)}
}

// Write writes p unchanged and flushes.
func (w *Writer) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if err != nil {
		return n, fmt.Errorf("sse: write: %w", err)
	}
	if err := w.rc.Flush(); err != nil {
		return n, fmt.Errorf("sse: flush: %w", err)
	}
	return n, nil
}

// WriteEvent serializes ev's Type, ID and Data (one data: line per line of
// Data) followed by a blank line, and flushes. Raw and Comments are ignored.
func (w *Writer) WriteEvent(ev Event) error {
	if strings.ContainsAny(ev.Type, "\r\n") || strings.ContainsAny(ev.ID, "\r\n") {
		return ErrInvalidField
	}
	var b strings.Builder
	if ev.Type != "" {
		b.WriteString("event: " + ev.Type + "\n")
	}
	if ev.ID != "" {
		b.WriteString("id: " + ev.ID + "\n")
	}
	data := strings.ReplaceAll(ev.Data, "\r\n", "\n")
	data = strings.ReplaceAll(data, "\r", "\n")
	for line := range strings.SplitSeq(data, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	_, err := w.Write([]byte(b.String()))
	return err
}

// WriteComment writes a comment block (": text") and flushes. Line breaks in
// text are replaced with spaces.
func (w *Writer) WriteComment(text string) error {
	text = strings.NewReplacer("\r", " ", "\n", " ").Replace(text)
	_, err := w.Write([]byte(": " + text + "\n\n"))
	return err
}

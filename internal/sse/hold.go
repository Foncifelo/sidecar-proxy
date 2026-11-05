package sse

import (
	"bytes"
	"io"
)

// HoldFirstData reads r until the first block with a data: line and returns
// every byte consumed from r, read-ahead included, so that
// io.MultiReader(bytes.NewReader(held), r) yields the original stream byte
// for byte. Comment-only blocks before it (keepalives) are held too. The
// held bytes are bounded by MaxEventSize (ErrEventTooLarge). On a read error,
// or io.EOF before any data, it returns what was read and the error.
func HoldFirstData(r io.Reader) ([]byte, error) {
	rec := &recorder{r: r}
	rd := NewReader(rec)
	for {
		ev, err := rd.Next()
		if err != nil {
			return rec.buf.Bytes(), err
		}
		if ev.HasData {
			return rec.buf.Bytes(), nil
		}
	}
}

// recorder keeps a copy of every byte read through it, up to MaxEventSize.
type recorder struct {
	r   io.Reader
	buf bytes.Buffer
}

func (c *recorder) Read(p []byte) (int, error) {
	if c.buf.Len() >= MaxEventSize {
		return 0, ErrEventTooLarge
	}
	if room := MaxEventSize - c.buf.Len(); len(p) > room {
		p = p[:room]
	}
	n, err := c.r.Read(p)
	c.buf.Write(p[:n])
	return n, err //nolint:wrapcheck // passthrough reader: callers inspect io.EOF
}

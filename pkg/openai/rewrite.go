package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// member locates one top-level member of a JSON object by byte offsets.
type member struct {
	key      string
	valStart int
	valEnd   int
}

// objectLayout is the byte layout of a JSON object.
type objectLayout struct {
	members []member
	// closeIdx is the index of the closing '}'.
	closeIdx int
}

// SetModel returns a copy of body with the top-level "model" set to model.
// The member is added at the end of the object if absent. Every other byte of
// body is preserved.
func SetModel(body []byte, model string) ([]byte, error) {
	val, err := json.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("openai: encode model: %w", err)
	}
	out, err := setMember(body, "model", val)
	if err != nil {
		return nil, fmt.Errorf("openai: set model: %w", err)
	}
	return out, nil
}

// SetIncludeUsage returns a copy of body with stream_options.include_usage set
// to include. stream_options is created if absent or null; its other members
// are preserved. Every other byte of body is preserved.
func SetIncludeUsage(body []byte, include bool) ([]byte, error) {
	layout, err := scanObject(body)
	if err != nil {
		return nil, fmt.Errorf("openai: set include_usage: %w", err)
	}
	flag := []byte("false")
	if include {
		flag = []byte("true")
	}
	opts := []byte(`{}`)
	if m, ok := layout.last("stream_options"); ok {
		cur := body[m.valStart:m.valEnd]
		switch {
		case isNull(cur):
		case isObject(cur):
			opts = cur
		default:
			return nil, fmt.Errorf("openai: stream_options: %w", ErrInvalidField)
		}
	}
	newOpts, err := setMember(opts, "include_usage", flag)
	if err != nil {
		return nil, fmt.Errorf("openai: set include_usage: %w", err)
	}
	return spliceMember(body, layout, "stream_options", newOpts), nil
}

// SetMember returns a copy of body with every top-level member named key set
// to val, or with key appended at the end of the object if absent. val must be
// a single valid JSON value. Every other byte of body is preserved.
func SetMember(body []byte, key string, val json.RawMessage) ([]byte, error) {
	if !json.Valid(val) {
		return nil, fmt.Errorf("openai: set %q: %w", key, ErrInvalidField)
	}
	out, err := setMember(body, key, val)
	if err != nil {
		return nil, fmt.Errorf("openai: set %q: %w", key, err)
	}
	return out, nil
}

// setMember returns a copy of obj with every top-level member named key set
// to val, or with key appended if absent.
func setMember(obj []byte, key string, val []byte) ([]byte, error) {
	layout, err := scanObject(obj)
	if err != nil {
		return nil, err
	}
	return spliceMember(obj, layout, key, val), nil
}

// spliceMember is setMember on an already scanned object.
func spliceMember(obj []byte, layout objectLayout, key string, val []byte) []byte {
	out := make([]byte, 0, len(obj)+len(key)+len(val)+4)
	prev := 0
	found := false
	for _, m := range layout.members {
		if m.key != key {
			continue
		}
		found = true
		out = append(out, obj[prev:m.valStart]...)
		out = append(out, val...)
		prev = m.valEnd
	}
	if found {
		return append(out, obj[prev:]...)
	}
	keyJSON, _ := json.Marshal(key) //nolint:errcheck // marshaling a string cannot fail
	out = append(out, obj[:layout.closeIdx]...)
	if len(layout.members) > 0 {
		out = append(out, ',')
	}
	out = append(out, keyJSON...)
	out = append(out, ':')
	out = append(out, val...)
	return append(out, obj[layout.closeIdx:]...)
}

// last returns the last member named key; Go's decoder also keeps the last.
func (l objectLayout) last(key string) (member, bool) {
	for i := len(l.members) - 1; i >= 0; i-- {
		if l.members[i].key == key {
			return l.members[i], true
		}
	}
	return member{}, false
}

// scanObject returns the byte layout of the single JSON object in data.
func scanObject(data []byte) (objectLayout, error) {
	if !isObject(data) || !json.Valid(data) {
		return objectLayout{}, ErrNotObject
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if _, err := dec.Token(); err != nil {
		return objectLayout{}, fmt.Errorf("read object start: %w", err)
	}
	var layout objectLayout
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return objectLayout{}, fmt.Errorf("read key: %w", err)
		}
		key, ok := tok.(string)
		if !ok {
			return objectLayout{}, ErrNotObject
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return objectLayout{}, fmt.Errorf("read value: %w", err)
		}
		end := int(dec.InputOffset())
		layout.members = append(layout.members, member{key: key, valStart: end - len(raw), valEnd: end})
	}
	if _, err := dec.Token(); err != nil {
		return objectLayout{}, fmt.Errorf("read object end: %w", err)
	}
	layout.closeIdx = int(dec.InputOffset()) - 1
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return objectLayout{}, ErrNotObject
	}
	return layout, nil
}

// Span is the byte range [Start, End) of a value inside a JSON document.
type Span struct {
	Start int
	End   int
}

// TopLevelSpans returns the value range of every top-level member named key
// of the JSON object in body, in document order. It returns nil when body is
// not a single valid JSON object or has no such member. Keys are compared
// after unescaping, as a JSON decoder would.
func TopLevelSpans(body []byte, key string) []Span {
	layout, err := scanObject(body)
	if err != nil {
		return nil
	}
	var spans []Span
	for _, m := range layout.members {
		if m.key == key {
			spans = append(spans, Span{Start: m.valStart, End: m.valEnd})
		}
	}
	return spans
}

// ReplaceModel returns body with the value of every top-level "model" member
// replaced by model as a JSON string (ADR 0008). It never adds the member:
// when body is not a JSON object or has no top-level "model", body itself is
// returned. Every other byte is preserved.
func ReplaceModel(body []byte, model string) []byte {
	spans := TopLevelSpans(body, "model")
	if len(spans) == 0 {
		return body
	}
	val, err := json.Marshal(model)
	if err != nil {
		return body
	}
	return ReplaceSpans(body, spans, val)
}

// ReplaceSpans returns a copy of b with every span replaced by val. Spans
// must be sorted and must not overlap.
func ReplaceSpans(b []byte, spans []Span, val []byte) []byte {
	out := make([]byte, 0, len(b)+len(spans)*len(val))
	prev := 0
	for _, s := range spans {
		out = append(out, b[prev:s.Start]...)
		out = append(out, val...)
		prev = s.End
	}
	return append(out, b[prev:]...)
}

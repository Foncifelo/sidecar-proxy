package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Errors returned when a request body cannot be read or rewritten.
var (
	// ErrNotObject means the body is not a single JSON object.
	ErrNotObject = errors.New("openai: body is not a JSON object")
	// ErrInvalidField means a known field has an unexpected JSON type.
	ErrInvalidField = errors.New("openai: field has an unexpected JSON type")
)

// StreamOptions is the stream_options object of a chat completions request.
type StreamOptions struct {
	IncludeUsage *bool `json:"include_usage,omitempty"`
}

// ChatRequest is a chat completions request. Content fields are opaque.
type ChatRequest struct {
	Model          string          `json:"model"`
	Messages       json.RawMessage `json:"messages,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	StreamOptions  *StreamOptions  `json:"stream_options,omitempty"`
	Tools          json.RawMessage `json:"tools,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
	N              *int            `json:"n,omitempty"`
	Logprobs       *bool           `json:"logprobs,omitempty"`
	// MaxTokens and MaxCompletionTokens are read leniently: a value that is
	// not an integer is ignored, never an error.
	MaxTokens           json.RawMessage `json:"max_tokens,omitempty"`
	MaxCompletionTokens json.RawMessage `json:"max_completion_tokens,omitempty"`
}

// ResponsesRequest is the subset of a Responses API request the sidecar reads.
type ResponsesRequest struct {
	Model  string          `json:"model"`
	Input  json.RawMessage `json:"input,omitempty"`
	Tools  json.RawMessage `json:"tools,omitempty"`
	Stream bool            `json:"stream,omitempty"`
}

// EmbeddingsRequest is the subset of an embeddings request the sidecar reads.
type EmbeddingsRequest struct {
	Model string          `json:"model"`
	Input json.RawMessage `json:"input,omitempty"`
}

// RequestInfo is everything the sidecar needs to know about a chat request.
type RequestInfo struct {
	Model              string
	Stream             bool
	IncludeUsage       bool
	HasTools           bool
	ResponseFormatType string
	// N is the number of choices requested; 1 when absent.
	N        int
	Logprobs bool
	// Vision is true when any message has a content part of type image_url.
	// Only the part types are read; no content is kept.
	Vision bool
	// MaxOutputTokens is max_completion_tokens when present, else
	// max_tokens; 0 when neither is a positive integer.
	MaxOutputTokens int64
}

// bytesPerToken is the bytes-to-tokens ratio of EstimateContextTokens:
// about what English text averages. The JSON framing (keys, quotes, escapes)
// adds bytes, so the estimate usually errs high; it is a guard that removes
// candidates with too short a context, not a token count.
const bytesPerToken = 4

// EstimateContextTokens is the context a request needs: its body size in
// bytes / 4 plus the output it asks for (docs/politica.md). It reads only
// the length of the body, never its content.
func EstimateContextTokens(bodyBytes int, info RequestInfo) int64 {
	return int64(bodyBytes)/bytesPerToken + info.MaxOutputTokens
}

// defaultN is the number of choices when the request does not set n.
const defaultN = 1

// ParseRequest reads the RequestInfo of a chat completions body.
func ParseRequest(body []byte) (RequestInfo, error) {
	if !isObject(body) {
		return RequestInfo{}, ErrNotObject
	}
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return RequestInfo{}, fmt.Errorf("openai: parse chat request: %w", err)
	}
	return req.Info()
}

// Info derives the RequestInfo from a decoded ChatRequest.
func (r ChatRequest) Info() (RequestInfo, error) {
	hasTools, err := nonEmptyArray(r.Tools)
	if err != nil {
		return RequestInfo{}, fmt.Errorf("openai: tools: %w", err)
	}
	rfType, err := responseFormatType(r.ResponseFormat)
	if err != nil {
		return RequestInfo{}, fmt.Errorf("openai: response_format: %w", err)
	}
	info := RequestInfo{
		Model:              r.Model,
		Stream:             r.Stream,
		HasTools:           hasTools,
		ResponseFormatType: rfType,
		N:                  defaultN,
	}
	if r.StreamOptions != nil && r.StreamOptions.IncludeUsage != nil {
		info.IncludeUsage = *r.StreamOptions.IncludeUsage
	}
	if r.N != nil {
		info.N = *r.N
	}
	if r.Logprobs != nil {
		info.Logprobs = *r.Logprobs
	}
	info.Vision = hasImagePart(r.Messages)
	info.MaxOutputTokens = positiveInt(r.MaxCompletionTokens)
	if info.MaxOutputTokens == 0 {
		info.MaxOutputTokens = positiveInt(r.MaxTokens)
	}
	return info, nil
}

// imagePartType is the content part type that requires vision.
const imagePartType = "image_url"

// hasImagePart reports whether any message has a content part of type
// image_url. Only each part's "type" is decoded; messages that are not in
// the expected shape count as no image.
func hasImagePart(messages json.RawMessage) bool {
	if isNull(messages) {
		return false
	}
	var msgs []struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(messages, &msgs) != nil {
		return false
	}
	for _, m := range msgs {
		c := bytes.TrimSpace(m.Content)
		if len(c) == 0 || c[0] != '[' {
			continue // a string, null or absent content has no parts
		}
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(c, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if p.Type == imagePartType {
				return true
			}
		}
	}
	return false
}

// positiveInt returns raw as an integer when it is a positive one, else 0.
func positiveInt(raw json.RawMessage) int64 {
	var n int64
	if isNull(raw) || json.Unmarshal(raw, &n) != nil || n < 0 {
		return 0
	}
	return n
}

// isNull reports whether raw is absent or the JSON literal null.
func isNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// isObject reports whether data starts (after whitespace) with '{'.
func isObject(data []byte) bool {
	t := bytes.TrimSpace(data)
	return len(t) > 0 && t[0] == '{'
}

// nonEmptyArray reports whether raw is a JSON array with at least one element.
// Elements are not decoded.
func nonEmptyArray(raw json.RawMessage) (bool, error) {
	if isNull(raw) {
		return false, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return false, ErrInvalidField
	}
	return len(items) > 0, nil
}

// responseFormatType reads only the "type" member of response_format.
func responseFormatType(raw json.RawMessage) (string, error) {
	if isNull(raw) {
		return "", nil
	}
	var rf struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &rf); err != nil {
		return "", ErrInvalidField
	}
	return rf.Type, nil
}

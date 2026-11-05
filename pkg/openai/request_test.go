package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func TestParseRequest(t *testing.T) {
	tests := []struct {
		name    string
		body    []byte
		want    RequestInfo
		wantErr error
	}{
		{
			name: "full fixture",
			body: readFixture(t, "chat_request_full.json"),
			want: RequestInfo{
				Model: "example/fictional-model-70b", Stream: true, IncludeUsage: true,
				HasTools: true, ResponseFormatType: "json_schema", N: 2, Logprobs: true,
			},
		},
		{
			name: "minimal fixture defaults",
			body: readFixture(t, "chat_request_minimal.json"),
			want: RequestInfo{Model: "example/fictional-model-8b", N: 1},
		},
		{
			name: "stream without stream_options",
			body: readFixture(t, "chat_request_stream_no_options.json"),
			want: RequestInfo{Model: "m1", Stream: true, N: 1},
		},
		{
			name: "empty tools and null response_format",
			body: []byte(`{"model":"m","tools":[],"response_format":null,"stream_options":{"include_usage":false}}`),
			want: RequestInfo{Model: "m", N: 1},
		},
		{
			name: "null tools",
			body: []byte(`{"model":"m","tools":null,"response_format":{"type":"json_object"}}`),
			want: RequestInfo{Model: "m", N: 1, ResponseFormatType: "json_object"},
		},
		{name: "not an object", body: []byte(`[1,2]`), wantErr: ErrNotObject},
		{name: "null body", body: []byte(`null`), wantErr: ErrNotObject},
		{name: "empty body", body: nil, wantErr: ErrNotObject},
		{name: "invalid json", body: []byte(`{"model":`)},
		{name: "model wrong type", body: []byte(`{"model":1}`)},
		{name: "tools not array", body: []byte(`{"model":"m","tools":{"a":1}}`), wantErr: ErrInvalidField},
		{name: "response_format not object", body: []byte(`{"model":"m","response_format":"text"}`), wantErr: ErrInvalidField},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRequest(tt.body)
			wantFail := tt.wantErr != nil || tt.want == (RequestInfo{})
			if wantFail {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestContentIsOpaque checks that content fields keep their exact bytes.
func TestContentIsOpaque(t *testing.T) {
	body := readFixture(t, "chat_request_full.json")
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		got  json.RawMessage
	}{
		{"messages", req.Messages},
		{"tools", req.Tools},
		{"response_format", req.ResponseFormat},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !bytes.Equal(tt.got, raw[tt.name]) {
				t.Fatalf("%s = %s, want %s", tt.name, tt.got, raw[tt.name])
			}
		})
	}

	inputs := []struct {
		name string
		body string
		dst  interface{ input() json.RawMessage }
	}{
		{"responses input", `{"model":"m","input":[{"role":"user","content":"x"}]}`, &ResponsesRequest{}},
		{"embeddings input", `{"model":"m","input":["a","b"]}`, &EmbeddingsRequest{}},
	}
	for _, tt := range inputs {
		t.Run(tt.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tt.body), tt.dst); err != nil {
				t.Fatal(err)
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tt.body), &m); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(tt.dst.input(), m["input"]) {
				t.Fatalf("input = %s, want %s", tt.dst.input(), m["input"])
			}
		})
	}
}

func (r *ResponsesRequest) input() json.RawMessage  { return r.Input }
func (r *EmbeddingsRequest) input() json.RawMessage { return r.Input }

func TestParseRequestVisionAndMaxOutput(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantVision bool
		wantMaxOut int64
	}{
		{"string content", `{"model":"m","messages":[{"role":"user","content":"fictitious"}]}`, false, 0},
		{"text parts only", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}]}`, false, 0},
		{
			"image part in a later message",
			`{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"https://img.example/fict.png"}}]}]}`,
			true, 0,
		},
		{"null content", `{"model":"m","messages":[{"role":"assistant","content":null}]}`, false, 0},
		{"messages not an array", `{"model":"m","messages":"x"}`, false, 0},
		{"parts not objects", `{"model":"m","messages":[{"role":"user","content":[1,2]}]}`, false, 0},
		{"max_tokens", `{"model":"m","max_tokens":512}`, false, 512},
		{"max_completion_tokens wins", `{"model":"m","max_tokens":512,"max_completion_tokens":2048}`, false, 2048},
		{"max_completion_tokens null falls back", `{"model":"m","max_tokens":512,"max_completion_tokens":null}`, false, 512},
		{"max_tokens not an integer is ignored", `{"model":"m","max_tokens":"512"}`, false, 0},
		{"negative max_tokens is ignored", `{"model":"m","max_tokens":-1}`, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := ParseRequest([]byte(tt.body))
			if err != nil {
				t.Fatalf("ParseRequest: %v", err)
			}
			if info.Vision != tt.wantVision || info.MaxOutputTokens != tt.wantMaxOut {
				t.Errorf("vision=%v max_out=%d, want %v %d", info.Vision, info.MaxOutputTokens, tt.wantVision, tt.wantMaxOut)
			}
		})
	}
}

func TestEstimateContextTokens(t *testing.T) {
	tests := []struct {
		name  string
		bytes int
		info  RequestInfo
		want  int64
	}{
		{"empty", 0, RequestInfo{}, 0},
		{"body only", 4000, RequestInfo{}, 1000},
		{"rounds down", 4003, RequestInfo{}, 1000},
		{"plus requested output", 4000, RequestInfo{MaxOutputTokens: 2048}, 3048},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EstimateContextTokens(tt.bytes, tt.info); got != tt.want {
				t.Errorf("EstimateContextTokens = %d, want %d", got, tt.want)
			}
		})
	}
}

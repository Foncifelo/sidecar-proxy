package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

const yamlKey = "sk-yaml-fictitious"

func testEndpoint(ch Channel) Endpoint {
	return Endpoint{
		ID: "ep-fict", Provider: "together", Channel: ch,
		BaseURL: "https://api.example.test/v1/", ProviderModelName: "vendor/model-fp8",
		APIKey: yamlKey,
	}
}

func decodeBody(t *testing.T, req *http.Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(req.Body).Decode(&m); err != nil {
		t.Fatalf("decode outgoing body: %v", err)
	}
	return m
}

func TestFor(t *testing.T) {
	tests := []struct {
		ch   Channel
		want Provider
	}{
		{ChannelDirect, OpenAICompatible{}},
		{ChannelOpenRouter, OpenRouter{}},
		{"unknown", OpenAICompatible{}},
	}
	for _, tt := range tests {
		if got := For(tt.ch); got != tt.want {
			t.Errorf("For(%q) = %T, want %T", tt.ch, got, tt.want)
		}
	}
}

func TestGenericBuildRequest(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Endpoint)
		body       string
		wantErr    error
		wantModel  string
		wantAuthH  string
		wantAuthV  string
		wantUsage  bool
		wantOptKey string // extra stream_options key that must survive
	}{
		{name: "defaults keep model", body: `{"model":"agent-model","messages":[]}`,
			wantModel: "agent-model", wantAuthH: "Authorization", wantAuthV: "Bearer " + yamlKey},
		{name: "rewrite model when asked", mutate: func(e *Endpoint) { e.RewriteModel = true },
			body: `{"model":"agent-model"}`, wantModel: "vendor/model-fp8", wantAuthH: "Authorization", wantAuthV: "Bearer " + yamlKey},
		{name: "custom auth header", mutate: func(e *Endpoint) { e.AuthHeader = "X-Api-Key" },
			body: `{"model":"m"}`, wantModel: "m", wantAuthH: "X-Api-Key", wantAuthV: yamlKey},
		{name: "stream adds include_usage", body: `{"model":"m","stream":true}`,
			wantModel: "m", wantAuthH: "Authorization", wantAuthV: "Bearer " + yamlKey, wantUsage: true},
		{name: "stream keeps other stream_options", body: `{"model":"m","stream":true,"stream_options":{"x":1,"include_usage":false}}`,
			wantModel: "m", wantAuthH: "Authorization", wantAuthV: "Bearer " + yamlKey, wantUsage: true, wantOptKey: "x"},
		{name: "not json", body: `nope`, wantErr: ErrInvalidBody},
		{name: "stream not bool", body: `{"stream":"yes"}`, wantErr: ErrInvalidBody},
		{name: "bad base url", mutate: func(e *Endpoint) { e.BaseURL = "ftp://x" }, body: `{}`, wantErr: ErrInvalidEndpoint},
		{name: "missing key", mutate: func(e *Endpoint) { e.APIKey = "" }, body: `{}`, wantErr: ErrInvalidEndpoint},
		{name: "rewrite without name", mutate: func(e *Endpoint) { e.RewriteModel = true; e.ProviderModelName = "" }, body: `{}`, wantErr: ErrInvalidEndpoint},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep := testEndpoint(ChannelDirect)
			if tt.mutate != nil {
				tt.mutate(&ep)
			}
			req, err := OpenAICompatible{}.BuildRequest(context.Background(), ep, []byte(tt.body))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if req.Method != http.MethodPost || req.URL.String() != "https://api.example.test/v1/chat/completions" {
				t.Errorf("got %s %s", req.Method, req.URL)
			}
			if got := req.Header.Get(tt.wantAuthH); got != tt.wantAuthV {
				t.Errorf("auth %s = %q, want %q", tt.wantAuthH, got, tt.wantAuthV)
			}
			if req.Header.Get("Accept-Encoding") != "identity" {
				t.Error("Accept-Encoding must be identity")
			}
			m := decodeBody(t, req)
			if m["model"] != tt.wantModel {
				t.Errorf("model = %v, want %q", m["model"], tt.wantModel)
			}
			opts, _ := m["stream_options"].(map[string]any)
			if got := opts["include_usage"] == true; got != tt.wantUsage {
				t.Errorf("include_usage = %v, want %v", got, tt.wantUsage)
			}
			if tt.wantOptKey != "" {
				if _, ok := opts[tt.wantOptKey]; !ok {
					t.Errorf("stream_options lost %q", tt.wantOptKey)
				}
			}
		})
	}
}

func TestOpenRouterPinsProvider(t *testing.T) {
	body := `{"model":"m","provider":{"order":["Other"],"allow_fallbacks":true}}`
	req, err := OpenRouter{}.BuildRequest(context.Background(), testEndpoint(ChannelOpenRouter), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := decodeBody(t, req)["provider"].(map[string]any)
	order, _ := p["order"].([]any)
	if len(order) != 1 || order[0] != "together" || p["allow_fallbacks"] != false {
		t.Errorf("provider = %v, want order [together] and allow_fallbacks false", p)
	}
	ep := testEndpoint(ChannelOpenRouter)
	ep.Provider = ""
	if _, err := (OpenRouter{}).BuildRequest(context.Background(), ep, []byte(body)); !errors.Is(err, ErrInvalidEndpoint) {
		t.Errorf("empty provider: err = %v", err)
	}
}

// Guarantee 1: only the allowed members change; every other byte (spacing,
// key order, duplicates, unescaped <&>) reaches the provider as sent.
func TestBuildRequestPreservesBytes(t *testing.T) {
	const prompt = `{ "z_last":1, "messages" : [ {"role":"user","content":"a <&> b"} ],"model":"agent-model" }`
	const streamed = `{"stream" : true, "messages":[{"content":"<&>  x"}], "model":"m"}`
	tests := []struct {
		name   string
		p      Provider
		ch     Channel
		mutate func(*Endpoint)
		body   string
		want   string
	}{
		{name: "nothing to rewrite", p: OpenAICompatible{}, ch: ChannelDirect, body: prompt, want: prompt},
		{name: "stream false untouched", p: OpenAICompatible{}, ch: ChannelDirect,
			body: `{"stream":false,"a":"<&>"}`, want: `{"stream":false,"a":"<&>"}`},
		{name: "stream adds include_usage only", p: OpenAICompatible{}, ch: ChannelDirect, body: streamed,
			want: `{"stream" : true, "messages":[{"content":"<&>  x"}], "model":"m","stream_options":{"include_usage":true}}`},
		{name: "rewrite model only", p: OpenAICompatible{}, ch: ChannelDirect,
			mutate: func(e *Endpoint) { e.RewriteModel = true }, body: prompt,
			want: `{ "z_last":1, "messages" : [ {"role":"user","content":"a <&> b"} ],"model":"vendor/model-fp8" }`},
		{name: "openrouter pin only", p: OpenRouter{}, ch: ChannelOpenRouter,
			body: `{"provider" : {"order":["Other"]}, "messages":[{"content":"<&>"}]}`,
			want: `{"provider" : {"order":["together"],"allow_fallbacks":false}, "messages":[{"content":"<&>"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep := testEndpoint(tt.ch)
			if tt.mutate != nil {
				tt.mutate(&ep)
			}
			req, err := tt.p.BuildRequest(context.Background(), ep, []byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("body:\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestParseServedRestoresBody(t *testing.T) {
	tests := []struct {
		name        string
		p           Provider
		contentType string
		body        string
		want        string
	}{
		{"openrouter json", OpenRouter{}, "application/json", `{"id":"x","provider":"FictA"}`, "FictA"},
		{"openrouter sse", OpenRouter{}, "text/event-stream", ": c\n\ndata: {\"provider\":\"FictB\"}\n\ndata: [DONE]\n\n", "FictB"},
		{"openrouter no field", OpenRouter{}, "application/json", `{"id":"x"}`, ""},
		{"openrouter garbage", OpenRouter{}, "text/plain", `oops`, ""},
		{"generic", OpenAICompatible{}, "application/json", `{"provider":"FictA"}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{"Content-Type": {tt.contentType}}, Body: io.NopCloser(strings.NewReader(tt.body))}
			if got := tt.p.ParseServed(resp); got != tt.want {
				t.Errorf("ParseServed = %q, want %q", got, tt.want)
			}
			rest, err := io.ReadAll(resp.Body)
			if err != nil || string(rest) != tt.body {
				t.Errorf("body after ParseServed = %q, %v; want unchanged", rest, err)
			}
		})
	}
	if (OpenRouter{}).ParseServed(nil) != "" || (OpenRouter{}).ParseServed(&http.Response{Body: http.NoBody}) != "" {
		t.Error("nil or empty response must yield empty provider")
	}
}

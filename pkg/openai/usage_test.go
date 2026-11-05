package openai

import (
	"encoding/json"
	"testing"
)

func TestFromChat(t *testing.T) {
	tests := []struct {
		name   string
		usage  *ChatUsage
		want   Usage
		wantOK bool
	}{
		{name: "absent", usage: nil, wantOK: false},
		{name: "zero is present", usage: &ChatUsage{}, want: Usage{}, wantOK: true},
		{
			name:   "plain",
			usage:  &ChatUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
			want:   Usage{In: 10, Out: 5},
			wantOK: true,
		},
		{
			name: "cached and reasoning details",
			usage: &ChatUsage{
				PromptTokens: 120, CompletionTokens: 50,
				PromptTokensDetails:     &PromptTokensDetails{CachedTokens: 100},
				CompletionTokensDetails: &CompletionTokensDetails{ReasoningTokens: 20},
			},
			want:   Usage{In: 20, Out: 30, Reasoning: 20, CachedRead: 100},
			wantOK: true,
		},
		{
			name:   "input_tokens and output_tokens fallback",
			usage:  &ChatUsage{InputTokens: 7, OutputTokens: 3},
			want:   Usage{In: 7, Out: 3},
			wantOK: true,
		},
		{
			name: "details larger than totals are clamped",
			usage: &ChatUsage{
				PromptTokens: 5, CompletionTokens: 2,
				PromptTokensDetails:     &PromptTokensDetails{CachedTokens: 9},
				CompletionTokensDetails: &CompletionTokensDetails{ReasoningTokens: 4},
			},
			want:   Usage{In: 0, Out: 0, Reasoning: 2, CachedRead: 5},
			wantOK: true,
		},
		{
			name:   "negative values clamped",
			usage:  &ChatUsage{PromptTokens: -3, CompletionTokens: -1},
			want:   Usage{},
			wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := FromChat(tt.usage)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("got %+v ok=%v, want %+v ok=%v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestFromResponses(t *testing.T) {
	tests := []struct {
		name   string
		usage  *ResponsesUsage
		want   Usage
		wantOK bool
	}{
		{name: "absent", usage: nil},
		{name: "zero is present", usage: &ResponsesUsage{}, wantOK: true},
		{
			name: "details",
			usage: &ResponsesUsage{
				InputTokens: 300, OutputTokens: 80,
				InputTokensDetails:  &InputTokensDetails{CachedTokens: 200},
				OutputTokensDetails: &OutputTokensDetails{ReasoningTokens: 60},
			},
			want:   Usage{In: 100, Out: 20, Reasoning: 60, CachedRead: 200},
			wantOK: true,
		},
		{name: "no details", usage: &ResponsesUsage{InputTokens: 4, OutputTokens: 6}, want: Usage{In: 4, Out: 6}, wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := FromResponses(tt.usage)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("got %+v ok=%v, want %+v ok=%v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestFromEmbeddings(t *testing.T) {
	tests := []struct {
		name   string
		usage  *EmbeddingsUsage
		want   Usage
		wantOK bool
	}{
		{name: "absent", usage: nil},
		{name: "present", usage: &EmbeddingsUsage{PromptTokens: 9, TotalTokens: 9}, want: Usage{In: 9}, wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := FromEmbeddings(tt.usage)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("got %+v ok=%v, want %+v ok=%v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestUsageFromFixtures decodes the fictional fixtures end to end.
func TestUsageFromFixtures(t *testing.T) {
	tests := []struct {
		name   string
		file   string
		decode func([]byte) (Usage, bool, error)
		want   Usage
		wantOK bool
	}{
		{name: "chat response", file: "chat_response.json", decode: decodeChat,
			want: Usage{In: 20, Out: 30, Reasoning: 20, CachedRead: 100}, wantOK: true},
		{name: "chat response without usage", file: "chat_response_no_usage.json", decode: decodeChat},
		{name: "chunk with null usage", file: "chat_chunk_delta.json", decode: decodeChunk},
		{name: "final chunk with zero usage", file: "chat_chunk_usage.json", decode: decodeChunk, wantOK: true},
		{name: "responses", file: "responses_response.json", decode: decodeResponses,
			want: Usage{In: 100, Out: 20, Reasoning: 60, CachedRead: 200}, wantOK: true},
		{name: "embeddings", file: "embeddings_response.json", decode: decodeEmbeddings,
			want: Usage{In: 9}, wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := tt.decode(readFixture(t, tt.file))
			if err != nil {
				t.Fatal(err)
			}
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("got %+v ok=%v, want %+v ok=%v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestChunkFields(t *testing.T) {
	var c ChatChunk
	if err := json.Unmarshal(readFixture(t, "chat_chunk_delta.json"), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Choices) != 1 || c.Choices[0].FinishReason != nil || len(c.Choices[0].Delta) == 0 {
		t.Fatalf("unexpected chunk %+v", c)
	}
	var r ChatResponse
	if err := json.Unmarshal(readFixture(t, "chat_response.json"), &r); err != nil {
		t.Fatal(err)
	}
	if r.Choices[0].FinishReason != "stop" || r.Model != "example/fictional-model-70b" {
		t.Fatalf("unexpected response %+v", r)
	}
}

func TestUsageTotal(t *testing.T) {
	u := Usage{In: 1, Out: 2, Reasoning: 3, CachedRead: 4, CachedWrite: 5}
	if got := u.Total(); got != 15 {
		t.Fatalf("Total = %d, want 15", got)
	}
}

func decodeChat(b []byte) (Usage, bool, error) {
	var r ChatResponse
	if err := json.Unmarshal(b, &r); err != nil {
		return Usage{}, false, err
	}
	u, ok := FromChat(r.Usage)
	return u, ok, nil
}

func decodeChunk(b []byte) (Usage, bool, error) {
	var c ChatChunk
	if err := json.Unmarshal(b, &c); err != nil {
		return Usage{}, false, err
	}
	u, ok := FromChat(c.Usage)
	return u, ok, nil
}

func decodeResponses(b []byte) (Usage, bool, error) {
	var r ResponsesResponse
	if err := json.Unmarshal(b, &r); err != nil {
		return Usage{}, false, err
	}
	u, ok := FromResponses(r.Usage)
	return u, ok, nil
}

func decodeEmbeddings(b []byte) (Usage, bool, error) {
	var r EmbeddingsResponse
	if err := json.Unmarshal(b, &r); err != nil {
		return Usage{}, false, err
	}
	u, ok := FromEmbeddings(r.Usage)
	return u, ok, nil
}

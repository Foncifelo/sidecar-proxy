package openai

import "encoding/json"

// ChatUsage is the usage object of chat completions. Some OpenAI-compatible
// providers report input_tokens/output_tokens instead of prompt/completion.
type ChatUsage struct {
	PromptTokens            int64                    `json:"prompt_tokens"`
	CompletionTokens        int64                    `json:"completion_tokens"`
	TotalTokens             int64                    `json:"total_tokens"`
	InputTokens             int64                    `json:"input_tokens,omitempty"`
	OutputTokens            int64                    `json:"output_tokens,omitempty"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// PromptTokensDetails breaks down chat prompt tokens.
type PromptTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// CompletionTokensDetails breaks down chat completion tokens.
type CompletionTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// ChatChoice is one choice of a non-stream chat completion. Message is opaque.
type ChatChoice struct {
	Index        int             `json:"index"`
	Message      json.RawMessage `json:"message,omitempty"`
	FinishReason string          `json:"finish_reason,omitempty"`
}

// ChatResponse is a non-stream chat completion. Usage is nil when absent.
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChatUsage   `json:"usage,omitempty"`
}

// ChunkChoice is one choice of a stream chunk. Delta is opaque.
type ChunkChoice struct {
	Index        int             `json:"index"`
	Delta        json.RawMessage `json:"delta,omitempty"`
	FinishReason *string         `json:"finish_reason"`
}

// ChatChunk is one chat.completion.chunk of a stream. Usage is nil when absent
// or null.
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *ChatUsage    `json:"usage,omitempty"`
}

// ResponsesUsage is the usage object of the Responses API.
type ResponsesUsage struct {
	InputTokens         int64                `json:"input_tokens"`
	OutputTokens        int64                `json:"output_tokens"`
	TotalTokens         int64                `json:"total_tokens"`
	InputTokensDetails  *InputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *OutputTokensDetails `json:"output_tokens_details,omitempty"`
}

// InputTokensDetails breaks down Responses API input tokens.
type InputTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// OutputTokensDetails breaks down Responses API output tokens.
type OutputTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// ResponsesResponse is the subset of a Responses API response the sidecar
// reads: only identity and usage.
type ResponsesResponse struct {
	ID     string          `json:"id"`
	Object string          `json:"object"`
	Model  string          `json:"model"`
	Usage  *ResponsesUsage `json:"usage,omitempty"`
}

// EmbeddingsUsage is the usage object of the embeddings API.
type EmbeddingsUsage struct {
	PromptTokens int64 `json:"prompt_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// EmbeddingsResponse is the subset of an embeddings response the sidecar
// reads: only model and usage.
type EmbeddingsResponse struct {
	Object string           `json:"object"`
	Model  string           `json:"model"`
	Usage  *EmbeddingsUsage `json:"usage,omitempty"`
}

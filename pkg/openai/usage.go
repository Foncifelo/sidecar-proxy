package openai

// Usage is token usage normalized across APIs. The buckets are disjoint so
// that each one is priced exactly once (docs/metodologia-ahorro.md):
//
//   - In: uncached input tokens (total input minus CachedRead).
//   - Out: non-reasoning output tokens (total output minus Reasoning).
//   - Reasoning: reasoning output tokens.
//   - CachedRead: input tokens served from the provider's prompt cache.
//   - CachedWrite: input tokens written to the cache; the OpenAI wire format
//     does not report it, so it stays 0 here.
type Usage struct {
	In          int64
	Out         int64
	Reasoning   int64
	CachedRead  int64
	CachedWrite int64
}

// Total returns the sum of every bucket.
func (u Usage) Total() int64 {
	return u.In + u.Out + u.Reasoning + u.CachedRead + u.CachedWrite
}

// FromChat normalizes a chat completions usage. ok is false when the
// provider sent no usage (nil), which differs from a usage of zero tokens.
// input_tokens/output_tokens are used when prompt/completion are zero.
func FromChat(u *ChatUsage) (usage Usage, ok bool) {
	if u == nil {
		return Usage{}, false
	}
	in, out := u.PromptTokens, u.CompletionTokens
	if in == 0 {
		in = u.InputTokens
	}
	if out == 0 {
		out = u.OutputTokens
	}
	var cached, reasoning int64
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	return split(in, out, cached, reasoning), true
}

// FromResponses normalizes a Responses API usage. ok is false when usage is
// absent (nil).
func FromResponses(u *ResponsesUsage) (usage Usage, ok bool) {
	if u == nil {
		return Usage{}, false
	}
	var cached, reasoning int64
	if u.InputTokensDetails != nil {
		cached = u.InputTokensDetails.CachedTokens
	}
	if u.OutputTokensDetails != nil {
		reasoning = u.OutputTokensDetails.ReasoningTokens
	}
	return split(u.InputTokens, u.OutputTokens, cached, reasoning), true
}

// FromEmbeddings normalizes an embeddings usage (input only). ok is false
// when usage is absent (nil).
func FromEmbeddings(u *EmbeddingsUsage) (usage Usage, ok bool) {
	if u == nil {
		return Usage{}, false
	}
	return Usage{In: nonNegative(u.PromptTokens)}, true
}

// split turns provider totals into disjoint buckets. Details larger than the
// total they belong to are clamped so no bucket is negative.
func split(in, out, cached, reasoning int64) Usage {
	in, out = nonNegative(in), nonNegative(out)
	cached = min(nonNegative(cached), in)
	reasoning = min(nonNegative(reasoning), out)
	return Usage{
		In:         in - cached,
		Out:        out - reasoning,
		Reasoning:  reasoning,
		CachedRead: cached,
	}
}

// nonNegative clamps v to zero.
func nonNegative(v int64) int64 {
	return max(v, 0)
}

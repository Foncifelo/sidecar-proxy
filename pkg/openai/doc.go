// Package openai holds request/response types for the OpenAI-compatible
// surface (chat completions, Responses API, embeddings) and a normalized
// Usage. No business logic.
//
// Content is opaque: messages, input, tools and response_format are kept as
// json.RawMessage and are never interpreted beyond their JSON type. Request
// rewriting (SetModel, SetIncludeUsage) splices bytes into the original body
// so that every other byte of the JSON document is preserved exactly.
//
// Errors returned by this package never include body content.
package openai

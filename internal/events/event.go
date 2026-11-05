// Package events defines what leaves the customer's infrastructure: one Event
// per request, metadata only. There is no field for prompts, completions or
// headers, and event_test.go enforces that by reflection (CONTRIBUTING.md rule 1).
//
// Any change that adds a field must bump SchemaVersion and update docs/evento.md.
// Fields are only ever added; their meaning never changes (CONTRIBUTING.md rule 11).
package events

import "time"

// SchemaVersion travels in every ingest envelope. The control plane accepts N, N-1 and N-2.
const SchemaVersion = 1

// Mode is the sidecar operating mode when the request was handled.
type Mode string

// Modes.
const (
	ModeShadow      Mode = "shadow"
	ModeRoute       Mode = "route"
	ModePassthrough Mode = "passthrough"
)

// EndpointKind classifies the API surface used by the request.
type EndpointKind string

// Endpoint kinds.
const (
	KindChat        EndpointKind = "chat"
	KindResponses   EndpointKind = "responses"
	KindEmbeddings  EndpointKind = "embeddings"
	KindPassthrough EndpointKind = "passthrough"
)

// StreamState records how a streamed response ended.
type StreamState string

// Stream states.
const (
	StreamNone      StreamState = ""          // non-streaming request
	StreamComplete  StreamState = "complete"  // all chunks delivered
	StreamTruncated StreamState = "truncated" // failed after the first byte; SSE error event sent
	StreamAborted   StreamState = "aborted"   // client went away
)

// UsageSource says where token counts came from.
type UsageSource string

// Usage sources.
const (
	UsageProvider  UsageSource = "provider"
	UsageEstimated UsageSource = "estimated"
	UsageNone      UsageSource = "none"
)

// Micros is an amount of money in microdollars (1e-6 USD). Never use floats for money (CONTRIBUTING.md rule 9).
type Micros int64

// MaxAttemptCostMicros (100 USD) is the ceiling of one attempt's cost_est and
// of an event's cost_actual. The sidecar caps its conservative estimates and
// the route sum there instead of saturating at math.MaxInt64, and the
// control plane rejects events above it, so sum(cost_actual) over a tenant
// cannot overflow bigint in rollups and billing.
const MaxAttemptCostMicros Micros = 100_000_000

// Attempt is one upstream call made while serving a request. A request that fell
// back has two attempts; the cancelled one still carries its estimated cost because
// the provider may bill it (ADR 0003).
type Attempt struct {
	EndpointID   string `json:"endpoint_id"`
	ProviderName string `json:"provider"`
	Channel      string `json:"channel"` // direct | openrouter
	Status       int    `json:"status"`  // HTTP status, 0 if no response
	TTFTMs       int64  `json:"ttft_ms"`
	TotalMs      int64  `json:"total_ms"`
	TokensOutEst int64  `json:"tokens_out_est"`
	CostEst      Micros `json:"cost_est"`
	AbortReason  string `json:"abort_reason,omitempty"` // first_byte_timeout | ttft_over_p95 | http_error | rate_limited
}

// Event is one request as seen by the sidecar. JSON tags are the wire format.
type Event struct {
	// Identity
	ID             string            `json:"id"`                    // ULID, also returned to the agent as X-Sidecar-Request-Id
	TS             time.Time         `json:"ts"`                    // set once by the sidecar; server rejects > now + 5m
	CustomerID     string            `json:"customer_id,omitempty"` // filled by the control plane from the API key
	APIKeyID       string            `json:"api_key_id,omitempty"`
	SidecarVersion string            `json:"sidecar_version"`
	Tags           map[string]string `json:"tags,omitempty"` // allowlisted keys only; no PII

	// Request
	Kind         EndpointKind `json:"endpoint_kind"`
	Stream       bool         `json:"stream"`
	RawModel     string       `json:"raw_model"` // exactly what the agent sent
	ModelID      string       `json:"model_id,omitempty"`
	Requirements []string     `json:"requirements,omitempty"` // tools, json_schema, vision, logprobs, n>1, ctx>Nk

	// Decision
	Mode           Mode        `json:"mode"`
	EndpointUsed   string      `json:"endpoint_used,omitempty"`
	ProviderServed string      `json:"provider_served,omitempty"` // what the aggregator reports actually served it
	EndpointBest   string      `json:"endpoint_best,omitempty"`
	DecisionReason string      `json:"decision_reason,omitempty"`
	PolicyVersion  string      `json:"policy_version,omitempty"`
	Attempts       []Attempt   `json:"attempts"`
	StreamState    StreamState `json:"stream_state,omitempty"`

	// Usage
	TokensIn          int64       `json:"tokens_in"`
	TokensOut         int64       `json:"tokens_out"`
	TokensReasoning   int64       `json:"tokens_reasoning"`
	TokensCachedRead  int64       `json:"tokens_cached_read"`
	TokensCachedWrite int64       `json:"tokens_cached_write"`
	UsageSource       UsageSource `json:"usage_source"`
	FinishReason      string      `json:"finish_reason,omitempty"`
	ErrorType         string      `json:"error_type,omitempty"`
	TTFTMs            int64       `json:"ttft_ms"`
	TotalMs           int64       `json:"total_ms"`
	TPS               float64     `json:"tps"` // tokens per second of generation; not money
	Status            int         `json:"status"`

	// Money (microdollars)
	CostActual    Micros `json:"cost_actual"` // sum of all attempts
	CostBest      Micros `json:"cost_best"`
	PriceIDActual string `json:"price_id_actual,omitempty"`
	PriceIDBest   string `json:"price_id_best,omitempty"`
	Billable      bool   `json:"billable"` // true only in route mode

	// Retries (detected locally; no body hash leaves the sidecar — ADR 0006)
	RetryOfRequestID string `json:"retry_of_request_id,omitempty"`
	ClientRetryCount int    `json:"client_retry_count,omitempty"`
}

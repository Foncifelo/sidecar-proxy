package decision

import (
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/pricing"
	"github.com/Foncifelo/sidecar-proxy/pkg/openai"
)

// Quantization is the numeric format an endpoint serves a model with. The
// values mirror the CHECK constraint on endpoints.quantization.
type Quantization string

// Known quantizations, from best to worst. QuantUnknown only matches itself.
const (
	QuantBF16    Quantization = "bf16"
	QuantFP16    Quantization = "fp16"
	QuantFP8     Quantization = "fp8"
	QuantInt8    Quantization = "int8"
	QuantInt4    Quantization = "int4"
	QuantUnknown Quantization = "unknown"
)

// Caps are the capabilities of an endpoint (endpoints.caps and
// endpoints.context_length).
type Caps struct {
	Tools      bool
	JSONSchema bool
	Vision     bool
	Logprobs   bool
	// MaxN is the largest n the endpoint accepts; 0 is treated as 1.
	MaxN int
	// ContextLength is the context window in tokens; 0 means unknown.
	ContextLength int64
}

// Stats are the observed health and latency figures of an endpoint.
type Stats struct {
	// Uptime1d is the fraction (0..1) of successful probes in the last day.
	Uptime1d float64
	// TTFTP90 is the p90 time to first token; 0 means no data.
	TTFTP90 time.Duration
	// TPSP50 is the median output tokens per second; 0 means no data.
	TPSP50 float64
}

// Candidate is one endpoint the client's YAML allows, with its current price.
type Candidate struct {
	EndpointID string
	// Model is the canonical model id (models.id), not the provider name.
	Model        string
	Quantization Quantization
	Caps         Caps
	Stats        Stats
	// Price is the price in effect (list or negotiated, see pricing.Select);
	// its ChannelFeeBps is applied by pricing.Cost.
	Price pricing.Price
}

// Requirements describe what the request needs and the usage the candidates
// are priced against.
type Requirements struct {
	Tools      bool
	JSONSchema bool
	Vision     bool
	Logprobs   bool
	// N is the number of choices requested; 0 is treated as 1.
	N int
	// ContextTokens is the context the request needs; 0 means no requirement.
	ContextTokens int64
	// RefUsage is the reference usage every candidate is priced with.
	RefUsage openai.Usage
	// Model is the canonical model id every candidate must serve, and
	// Quantization the worst quantization accepted (same semantics as the
	// quantization filter: QuantUnknown only matches QuantUnknown). Rank
	// filters against them; Decide ignores them and uses current's values.
	Model        string
	Quantization Quantization
}

// Thresholds hold the health limits and the client's own latency figures
// for the model. A zero ClientTTFTP95 or ClientTPSP50 disables that filter.
type Thresholds struct {
	// MinUptime1d is the minimum Uptime1d (fraction, e.g. 0.99).
	MinUptime1d float64
	// ClientTTFTP95 is the client's p95 time to first token over 7 days.
	ClientTTFTP95 time.Duration
	// ClientTPSP50 is the client's median tokens per second over 7 days.
	ClientTPSP50 float64
}

// DefaultMinUptime1d is the uptime threshold from docs/metodologia-ahorro.md.
const DefaultMinUptime1d = 0.99

// Reason explains why a candidate was discarded or why Best was chosen.
type Reason string

// Discard reasons, in the order the filters run.
const (
	ReasonSameAsCurrent  Reason = "same_as_current"
	ReasonNoTools        Reason = "caps_tools"
	ReasonNoJSONSchema   Reason = "caps_json_schema"
	ReasonNoVision       Reason = "caps_vision"
	ReasonNoLogprobs     Reason = "caps_logprobs"
	ReasonNTooLarge      Reason = "caps_n"
	ReasonContextTooBig  Reason = "caps_context"
	ReasonOtherModel     Reason = "different_model"
	ReasonWorseQuant     Reason = "worse_quantization"
	ReasonLowUptime      Reason = "low_uptime"
	ReasonSlowTTFT       Reason = "ttft_p90_over_client_p95"
	ReasonSlowTPS        Reason = "tps_p50_under_client"
	ReasonPriceError     Reason = "price_error"
	ReasonNotCheapest    Reason = "not_cheapest"
	ReasonCheaper        Reason = "cheaper_endpoint"
	ReasonCurrentCheaper Reason = "current_is_cheapest"
	ReasonNoCandidate    Reason = "no_candidate_passes"
	ReasonCurrentNoPrice Reason = "current_price_error"
)

// Discard records a candidate that did not win and why.
type Discard struct {
	EndpointID string
	Reason     Reason
}

// Decision is the outcome of Decide.
type Decision struct {
	// Best is the chosen endpoint; it equals current when nothing beats it.
	Best   Candidate
	Reason Reason
	// CostCurrent and CostBest are the reference-usage costs; CostBest equals
	// CostCurrent when Best is current.
	CostCurrent events.Micros
	CostBest    events.Micros
	// Discards lists every non-winning candidate in input order.
	Discards []Discard
}

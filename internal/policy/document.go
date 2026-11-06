package policy

import (
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/decision"
	"github.com/Foncifelo/sidecar-proxy/internal/pricing"
	"github.com/Foncifelo/sidecar-proxy/pkg/openai"
)

// SchemaVersion is the highest policy schema_version this binary understands
// (docs/politica.md). It is independent of the ingest envelope version.
// Schema 2 adds the fp4 quantization; the control plane serves schema 1
// without it.
const SchemaVersion = 2

// Channel is the execution channel of a candidate.
type Channel string

// Channels defined by schema_version 1.
const (
	ChannelDirect     Channel = "direct"
	ChannelOpenRouter Channel = "openrouter"
)

// Stats sources defined by schema_version 1.
const (
	SourceProbe      = "probe"
	SourceOpenRouter = "openrouter"
)

// Policy is the document served by GET /v1/policy. By design (rule 8, ADR
// 0008) no type in this package has a field for a URL, header, key or secret:
// such fields in the JSON are ignored.
type Policy struct {
	SchemaVersion int              `json:"schema_version"`
	Version       string           `json:"version"`
	ComputedAt    time.Time        `json:"computed_at"`
	Flags         Flags            `json:"flags"`
	Models        map[string]Model `json:"models"`
}

// Flags holds global switches. schema_version 1 defines none; unknown flags
// are ignored.
type Flags struct{}

// Model is the policy of one canonical model.
type Model struct {
	Aliases         map[string][]string `json:"aliases"`
	ClientTTFTP95Ms int64               `json:"client_ttft_p95_ms"`
	ClientTPSP50    float64             `json:"client_tps_p50"`
	RefUsage        RefUsage            `json:"ref_usage"`
	RouteEnabled    bool                `json:"route_enabled"`
	// Current is reference only and never routable (docs/politica.md).
	Current []Current `json:"current,omitempty"`
	// Candidates are ordered by pricing.Cost over RefUsage, ties by EndpointID.
	Candidates []Candidate `json:"candidates"`
}

// RefUsage is the reference usage candidates are ordered with.
type RefUsage struct {
	TokensIn          int64 `json:"tokens_in"`
	TokensOut         int64 `json:"tokens_out"`
	TokensReasoning   int64 `json:"tokens_reasoning"`
	TokensCachedRead  int64 `json:"tokens_cached_read"`
	TokensCachedWrite int64 `json:"tokens_cached_write"`
}

// Usage converts u to the counters pricing.Cost takes.
func (u RefUsage) Usage() openai.Usage {
	return openai.Usage{
		In:          u.TokensIn,
		Out:         u.TokensOut,
		Reasoning:   u.TokensReasoning,
		CachedRead:  u.TokensCachedRead,
		CachedWrite: u.TokensCachedWrite,
	}
}

// Candidate is one endpoint the sidecar may route to, if the YAML allows it.
type Candidate struct {
	EndpointID        string                `json:"endpoint_id"`
	Provider          string                `json:"provider"`
	Channel           Channel               `json:"channel"`
	ProviderModelName string                `json:"provider_model_name"`
	Quantization      decision.Quantization `json:"quantization"`
	Caps              Caps                  `json:"caps"`
	ContextLength     int64                 `json:"context_length"`
	Price             Price                 `json:"price"`
	Stats             Stats                 `json:"stats"`
}

// Current is the endpoint a direct alias resolves to, with its price.
type Current struct {
	Provider     string                `json:"provider"`
	RawModel     string                `json:"raw_model"`
	EndpointID   string                `json:"endpoint_id"`
	Quantization decision.Quantization `json:"quantization"`
	Price        Price                 `json:"price"`
}

// Caps mirrors endpoints.caps.
type Caps struct {
	Tools      bool `json:"tools"`
	JSONSchema bool `json:"json_schema"`
	Vision     bool `json:"vision"`
	Logprobs   bool `json:"logprobs"`
	N          int  `json:"n"`
}

// Price is a list price in microdollars (rule 9): rates per million tokens,
// PerRequest per request.
type Price struct {
	PriceID          string `json:"price_id"`
	InputPerM        int64  `json:"input_per_m"`
	OutputPerM       int64  `json:"output_per_m"`
	CachedInPerM     int64  `json:"cached_in_per_m"`
	CacheWritePerM   int64  `json:"cache_write_per_m"`
	ReasoningOutPerM int64  `json:"reasoning_out_per_m"`
	PerRequest       int64  `json:"per_request"`
	ChannelFeeBps    int32  `json:"channel_fee_bps"`
}

// Pricing converts p to a pricing.Price for endpointID.
func (p Price) Pricing(endpointID string) pricing.Price {
	return pricing.Price{
		PriceID:          p.PriceID,
		EndpointID:       endpointID,
		InputPerM:        p.InputPerM,
		OutputPerM:       p.OutputPerM,
		CachedInPerM:     p.CachedInPerM,
		CacheWritePerM:   p.CacheWritePerM,
		ReasoningOutPerM: p.ReasoningOutPerM,
		PerRequest:       p.PerRequest,
		ChannelFeeBps:    p.ChannelFeeBps,
	}
}

// Stats are the observed health and latency of a candidate.
type Stats struct {
	Uptime1d  float64 `json:"uptime_1d"`
	P90TTFTMs int64   `json:"p90_ttft_ms"`
	TPSP50    float64 `json:"tps_p50"`
	Source    string  `json:"source"`
}

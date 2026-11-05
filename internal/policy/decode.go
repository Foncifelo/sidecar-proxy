package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/Foncifelo/sidecar-proxy/internal/decision"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/pricing"
)

var (
	// ErrUnsupportedSchema is returned when schema_version is above
	// SchemaVersion or below 1. The caller keeps its previous policy.
	ErrUnsupportedSchema = errors.New("policy: unsupported schema_version")
	// ErrInvalid is returned when the document fails validation.
	ErrInvalid = errors.New("policy: invalid document")
)

// Decode parses and validates a policy document. Unknown fields are ignored
// (rule 11); a non-integer amount, an unsupported schema_version or any
// failed check returns an error and no policy.
func Decode(data []byte) (Policy, error) {
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return Policy{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if p.SchemaVersion < 1 || p.SchemaVersion > SchemaVersion {
		return Policy{}, fmt.Errorf("%w: got %d, support 1..%d", ErrUnsupportedSchema, p.SchemaVersion, SchemaVersion)
	}
	if err := p.validate(); err != nil {
		return Policy{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return p, nil
}

func (p Policy) validate() error {
	if p.Version == "" {
		return errors.New("version: empty")
	}
	for id, m := range p.Models {
		path := fmt.Sprintf("models[%q]", id)
		if id == "" {
			return fmt.Errorf("%s: empty model id", path)
		}
		if err := m.validate(path); err != nil {
			return err
		}
	}
	return nil
}

func (m Model) validate(path string) error {
	if m.ClientTTFTP95Ms < 0 {
		return fmt.Errorf("%s.client_ttft_p95_ms: negative", path)
	}
	if !nonNegativeFinite(m.ClientTPSP50) {
		return fmt.Errorf("%s.client_tps_p50: negative", path)
	}
	u := m.RefUsage
	for _, n := range []int64{u.TokensIn, u.TokensOut, u.TokensReasoning, u.TokensCachedRead, u.TokensCachedWrite} {
		if n < 0 {
			return fmt.Errorf("%s.ref_usage: negative counter", path)
		}
	}
	for i, c := range m.Current {
		if err := c.validate(fmt.Sprintf("%s.current[%d]", path, i)); err != nil {
			return err
		}
	}
	return m.validateCandidates(path)
}

// validateCandidates checks each candidate and that the list is strictly
// ordered by (pricing.Cost over ref_usage, endpoint_id), which also rejects
// duplicate endpoint ids.
func (m Model) validateCandidates(path string) error {
	usage := m.RefUsage.Usage()
	var prevCost events.Micros
	for i, c := range m.Candidates {
		cp := fmt.Sprintf("%s.candidates[%d]", path, i)
		if err := c.validate(cp); err != nil {
			return err
		}
		cost, err := pricing.Cost(c.Price.Pricing(c.EndpointID), usage)
		if err != nil {
			return fmt.Errorf("%s.price: %w", cp, err)
		}
		if i > 0 {
			prev := m.Candidates[i-1]
			if cost < prevCost || (cost == prevCost && c.EndpointID <= prev.EndpointID) {
				return fmt.Errorf("%s: not ordered by cost then endpoint_id", cp)
			}
		}
		prevCost = cost
	}
	return nil
}

func (c Candidate) validate(path string) error {
	switch {
	case c.EndpointID == "":
		return fmt.Errorf("%s.endpoint_id: empty", path)
	case c.Provider == "":
		return fmt.Errorf("%s.provider: empty", path)
	case c.ProviderModelName == "":
		return fmt.Errorf("%s.provider_model_name: empty", path)
	case c.Channel != ChannelDirect && c.Channel != ChannelOpenRouter:
		return fmt.Errorf("%s.channel: unknown %q", path, c.Channel)
	case !ValidQuantization(c.Quantization):
		return fmt.Errorf("%s.quantization: unknown %q", path, c.Quantization)
	case c.Caps.N < 0:
		return fmt.Errorf("%s.caps.n: negative", path)
	case c.ContextLength < 0:
		return fmt.Errorf("%s.context_length: negative", path)
	}
	if err := c.Stats.validate(path + ".stats"); err != nil {
		return err
	}
	return c.Price.validate(path + ".price")
}

func (c Current) validate(path string) error {
	switch {
	case c.EndpointID == "":
		return fmt.Errorf("%s.endpoint_id: empty", path)
	case c.Provider == "":
		return fmt.Errorf("%s.provider: empty", path)
	case c.RawModel == "":
		return fmt.Errorf("%s.raw_model: empty", path)
	case !ValidQuantization(c.Quantization):
		return fmt.Errorf("%s.quantization: unknown %q", path, c.Quantization)
	}
	return c.Price.validate(path + ".price")
}

func (s Stats) validate(path string) error {
	switch {
	case math.IsNaN(s.Uptime1d) || s.Uptime1d < 0 || s.Uptime1d > 1:
		return fmt.Errorf("%s.uptime_1d: outside 0..1", path)
	case s.P90TTFTMs < 0:
		return fmt.Errorf("%s.p90_ttft_ms: negative", path)
	case !nonNegativeFinite(s.TPSP50):
		return fmt.Errorf("%s.tps_p50: negative", path)
	case s.Source != SourceProbe && s.Source != SourceOpenRouter:
		return fmt.Errorf("%s.source: unknown %q", path, s.Source)
	}
	return nil
}

// Valid reports whether p passes the checks Decode applies to every price;
// the control plane publishes only valid prices.
func (p Price) Valid() bool { return p.validate("") == nil }

func (p Price) validate(path string) error {
	if !isDecimal(p.PriceID) {
		return fmt.Errorf("%s.price_id: not a decimal id", path)
	}
	for _, v := range []int64{p.InputPerM, p.OutputPerM, p.CachedInPerM, p.CacheWritePerM, p.ReasoningOutPerM, p.PerRequest} {
		if v < 0 {
			return fmt.Errorf("%s: negative amount", path)
		}
	}
	if p.ChannelFeeBps < 0 {
		return fmt.Errorf("%s.channel_fee_bps: negative", path)
	}
	return nil
}

// ValidQuantization reports whether q is one of the quantizations the document
// may carry. Decode and the control plane share it.
func ValidQuantization(q decision.Quantization) bool {
	switch q {
	case decision.QuantBF16, decision.QuantFP16, decision.QuantFP8,
		decision.QuantInt8, decision.QuantInt4, decision.QuantUnknown:
		return true
	}
	return false
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func nonNegativeFinite(f float64) bool {
	return f >= 0 && !math.IsInf(f, 1)
}

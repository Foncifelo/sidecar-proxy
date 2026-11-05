package proxy

import (
	"net/http"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/decision"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/policy"
	"github.com/Foncifelo/sidecar-proxy/internal/pricing"
	"github.com/Foncifelo/sidecar-proxy/internal/savings"
)

// decide resolves the model against the active policy and runs the shadow
// decision before the request is forwarded (docs/politica.md). It only reads
// the policy: the body sent upstream is not affected. Without a policy, or for
// kinds without a model, the reason stays no_policy.
func (h *Handler) decide(w http.ResponseWriter, req *requestState) {
	if h.o.Mode != config.ModeShadow || h.o.Policy == nil || req.kind == events.KindPassthrough {
		return
	}
	a := h.o.Policy()
	if a == nil {
		return
	}
	req.policyVersion = a.Version()
	provider := h.o.Upstream.Provider
	id, m, ok := a.Resolve(provider, req.rawModel)
	if !ok {
		h.setDecision(w, req, ReasonUnknownModel)
		return
	}
	req.modelID = id
	cur, ok := m.CurrentFor(provider, req.rawModel)
	if !ok {
		// No reference price for the upstream: nothing can be compared
		// (docs/politica.md, "Endpoint actual").
		req.decision = decision.Decision{
			Best:   decision.Candidate{EndpointID: UpstreamEndpointID, Model: id},
			Reason: decision.ReasonCurrentNoPrice,
		}
	} else {
		current := decision.Candidate{
			EndpointID:   cur.EndpointID,
			Model:        id,
			Quantization: cur.Quantization,
			Price:        cur.Price.Pricing(cur.EndpointID),
		}
		req.endpointUsed = cur.EndpointID
		req.curPrice, req.hasCurPrice = current.Price, true
		req.decision = decision.Decide(req.decisionRequirements(m), current, candidates(id, m), h.thresholds(m))
		w.Header().Set(HeaderEndpoint, req.endpointUsed)
	}
	h.setDecision(w, req, string(req.decision.Reason))
}

// needs are the request's caps and context, which the decision filters
// check against every candidate (docs/politica.md).
func (s *requestState) needs() decision.Requirements {
	return decision.Requirements{
		Tools:         s.info.HasTools,
		JSONSchema:    s.info.ResponseFormatType == "json_schema",
		Vision:        s.info.Vision,
		Logprobs:      s.info.Logprobs,
		N:             s.info.N,
		ContextTokens: s.contextTokens,
	}
}

// decisionRequirements are the request's needs priced over the model's
// reference usage.
func (s *requestState) decisionRequirements(m policy.Model) decision.Requirements {
	r := s.needs()
	r.RefUsage = m.RefUsage.Usage()
	return r
}

// thresholds combine the policy's client figures with the YAML limits; the
// YAML can only remove candidates (docs/politica.md).
func (h *Handler) thresholds(m policy.Model) decision.Thresholds {
	ttft := time.Duration(m.ClientTTFTP95Ms) * time.Millisecond
	if h.o.MaxTTFTP95 > 0 && (ttft == 0 || h.o.MaxTTFTP95 < ttft) {
		ttft = h.o.MaxTTFTP95
	}
	return decision.Thresholds{
		MinUptime1d:   max(h.o.MinUptime1d, decision.DefaultMinUptime1d),
		ClientTTFTP95: ttft,
		ClientTPSP50:  m.ClientTPSP50,
	}
}

// candidates converts the policy's (already YAML-filtered) candidates. The
// model's Current entries are never included (rule 8).
func candidates(modelID string, m policy.Model) []decision.Candidate {
	out := make([]decision.Candidate, 0, len(m.Candidates))
	for _, c := range m.Candidates {
		out = append(out, decision.Candidate{
			EndpointID:   c.EndpointID,
			Model:        modelID,
			Quantization: c.Quantization,
			Caps: decision.Caps{
				Tools:         c.Caps.Tools,
				JSONSchema:    c.Caps.JSONSchema,
				Vision:        c.Caps.Vision,
				Logprobs:      c.Caps.Logprobs,
				MaxN:          c.Caps.N,
				ContextLength: c.ContextLength,
			},
			Stats: decision.Stats{
				Uptime1d: c.Stats.Uptime1d,
				TTFTP90:  time.Duration(c.Stats.P90TTFTMs) * time.Millisecond,
				TPSP50:   c.Stats.TPSP50,
			},
			Price: c.Price.Pricing(c.EndpointID),
		})
	}
	return out
}

// applyCosts fills cost_actual and price_id_actual with the current
// endpoint's price over the real usage, then cost_best and price_id_best with
// savings.Simulated. Without a decision Simulated keeps cost_best = cost_actual.
// Both amounts are capped at events.MaxAttemptCostMicros, the ceiling the
// control plane accepts, as in route mode; the synthesized attempt's cost_est
// copies the capped cost_actual.
func (s *requestState) applyCosts(ev *events.Event) {
	if s.hasUsage && s.hasCurPrice {
		if c, err := pricing.Cost(s.curPrice, s.usage); err == nil {
			ev.CostActual, ev.PriceIDActual = min(c, events.MaxAttemptCostMicros), s.curPrice.PriceID
		}
	}
	var book savings.PriceBook
	if best := s.decision.Best; best.EndpointID != "" {
		book = savings.PriceBook{best.EndpointID: {List: []pricing.Price{best.Price}}}
	}
	sim := savings.Simulated(*ev, s.decision, book)
	ev.CostBest, ev.PriceIDBest = min(sim.CostBest, events.MaxAttemptCostMicros), sim.PriceIDBest
}

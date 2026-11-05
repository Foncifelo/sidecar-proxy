package decision

import (
	"github.com/Foncifelo/sidecar-proxy/internal/pricing"
)

// quantRank orders quantizations: higher is better; bf16 and fp16 tie.
// QuantUnknown is handled separately because it only matches itself.
func quantRank(q Quantization) (int, bool) {
	switch q {
	case QuantBF16, QuantFP16:
		return 4, true
	case QuantFP8:
		return 3, true
	case QuantInt8:
		return 2, true
	case QuantInt4:
		return 1, true
	case QuantUnknown:
		return 0, false
	default:
		return 0, false
	}
}

// quantAtLeast reports whether candidate quantization c is equal to or
// better than the current quantization cur. Unknown (or any unrecognised
// value) only matches unknown.
func quantAtLeast(c, cur Quantization) bool {
	cr, cok := quantRank(c)
	ur, uok := quantRank(cur)
	if !cok || !uok {
		return !cok && !uok && c == QuantUnknown && cur == QuantUnknown
	}
	return cr >= ur
}

// capsReason returns the first capability the candidate lacks, or "".
func capsReason(req Requirements, c Caps) Reason {
	switch {
	case req.Tools && !c.Tools:
		return ReasonNoTools
	case req.JSONSchema && !c.JSONSchema:
		return ReasonNoJSONSchema
	case req.Vision && !c.Vision:
		return ReasonNoVision
	case req.Logprobs && !c.Logprobs:
		return ReasonNoLogprobs
	case atLeastOne(req.N) > atLeastOne(c.MaxN):
		return ReasonNTooLarge
	case req.ContextTokens > 0 && (c.ContextLength == 0 || req.ContextTokens > c.ContextLength):
		// Unknown context length cannot be shown to fit, so it is discarded.
		return ReasonContextTooBig
	}
	return ""
}

func atLeastOne(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// filterReason runs every filter in the documented order and returns the
// first one the candidate fails, or "" when it passes them all. The model and
// quantization filters compare against req.Model and req.Quantization.
func filterReason(req Requirements, c Candidate, th Thresholds) Reason {
	if r := capsReason(req, c.Caps); r != "" {
		return r
	}
	if c.Model != req.Model {
		return ReasonOtherModel
	}
	if !quantAtLeast(c.Quantization, req.Quantization) {
		return ReasonWorseQuant
	}
	if c.Stats.Uptime1d < th.MinUptime1d {
		return ReasonLowUptime
	}
	// Missing candidate data (0) cannot be shown to meet the client figure.
	if th.ClientTTFTP95 > 0 && (c.Stats.TTFTP90 <= 0 || c.Stats.TTFTP90 > th.ClientTTFTP95) {
		return ReasonSlowTTFT
	}
	if th.ClientTPSP50 > 0 && c.Stats.TPSP50 < th.ClientTPSP50 {
		return ReasonSlowTPS
	}
	return ""
}

// Decide chooses the cheapest endpoint among candidates that can replace
// current for a request with requirements req. It is pure and deterministic:
// the result depends only on its arguments and does not mutate them.
//
// Filters run in this order and the first failure is recorded as the discard
// reason: capabilities (tools, json_schema, vision, logprobs, n, context),
// same canonical model, quantization equal or better, uptime, TTFT p90 vs
// client p95, TPS p50 vs client. Survivors are priced with pricing.Cost over
// req.RefUsage (channel fee included); the lowest cost wins and ties go to
// the lexicographically smallest endpoint id. Best stays current unless the
// winner is strictly cheaper than current.
//
// Decide is Rank with current as the reference: req.Model and
// req.Quantization are replaced by current's, and a candidate with current's
// endpoint id is discarded as same_as_current. The winner is the first
// endpoint Rank would return.
func Decide(req Requirements, current Candidate, candidates []Candidate, th Thresholds) Decision {
	d := Decision{Best: current, Discards: make([]Discard, 0, len(candidates))}

	curCost, err := pricing.Cost(current.Price, req.RefUsage)
	if err != nil {
		d.Reason = ReasonCurrentNoPrice
		for _, c := range candidates {
			d.Discards = append(d.Discards, Discard{EndpointID: c.EndpointID, Reason: ReasonCurrentNoPrice})
		}
		return d
	}
	d.CostCurrent, d.CostBest = curCost, curCost

	ref := req
	ref.Model, ref.Quantization = current.Model, current.Quantization
	survivors, reasons := evaluate(ref, candidates, th, func(c Candidate) bool {
		return c.EndpointID == current.EndpointID
	})

	winner := -1
	switch {
	case len(survivors) == 0:
		d.Reason = ReasonNoCandidate
	case survivors[0].cost >= curCost:
		d.Reason = ReasonCurrentCheaper
	default:
		winner = survivors[0].idx
		d.Reason = ReasonCheaper
		d.Best = candidates[winner]
		d.CostBest = survivors[0].cost
	}

	for i, c := range candidates {
		if i == winner {
			continue
		}
		r := reasons[i]
		if r == "" {
			r = ReasonNotCheapest
		}
		d.Discards = append(d.Discards, Discard{EndpointID: c.EndpointID, Reason: r})
	}
	return d
}

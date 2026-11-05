package decision

import (
	"sort"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/pricing"
)

// priced is a candidate that passed every filter, with its reference cost.
type priced struct {
	idx  int
	cost events.Micros
}

// Rank returns the candidates that pass every filter of Decide, cheapest
// first, for building an ordered fallback plan. It is pure and deterministic
// and does not mutate its arguments; the result is never nil.
//
// Filters and their order are those of Decide, with req.Model and
// req.Quantization as the reference model and minimum quantization. A
// candidate whose price cannot be computed is dropped. Survivors are ordered
// by pricing.Cost over req.RefUsage (channel fee included), ties by the
// lexicographically smallest endpoint id, then by input order.
func Rank(req Requirements, candidates []Candidate, th Thresholds) []Candidate {
	survivors, _ := evaluate(req, candidates, th, nil)
	out := make([]Candidate, len(survivors))
	for i, s := range survivors {
		out[i] = candidates[s.idx]
	}
	return out
}

// evaluate filters and prices candidates. reasons[i] is the discard reason of
// candidates[i], or "" when it survived; survivors are sorted by preference.
// A candidate for which exclude reports true is discarded as same_as_current.
func evaluate(req Requirements, candidates []Candidate, th Thresholds, exclude func(Candidate) bool) ([]priced, []Reason) {
	reasons := make([]Reason, len(candidates))
	survivors := make([]priced, 0, len(candidates))
	for i, c := range candidates {
		if exclude != nil && exclude(c) {
			reasons[i] = ReasonSameAsCurrent
			continue
		}
		if reasons[i] = filterReason(req, c, th); reasons[i] != "" {
			continue
		}
		cost, err := pricing.Cost(c.Price, req.RefUsage)
		if err != nil {
			reasons[i] = ReasonPriceError
			continue
		}
		survivors = append(survivors, priced{idx: i, cost: cost})
	}
	sort.SliceStable(survivors, func(a, b int) bool {
		sa, sb := survivors[a], survivors[b]
		return beats(sa.cost, candidates[sa.idx].EndpointID, sb.cost, candidates[sb.idx].EndpointID)
	})
	return survivors, reasons
}

// beats reports whether (cost, id) is preferred over (bestCost, bestID).
func beats(cost events.Micros, id string, bestCost events.Micros, bestID string) bool {
	if cost != bestCost {
		return cost < bestCost
	}
	return id < bestID
}

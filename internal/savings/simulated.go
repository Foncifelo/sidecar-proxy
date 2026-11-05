package savings

import (
	"github.com/Foncifelo/sidecar-proxy/internal/decision"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
)

// Reason explains a Simulation result or why an event is left out of the real savings.
type Reason string

// Reasons. ReasonCheaper is the only one with a possibly non-zero saving.
const (
	ReasonCheaper        Reason = "cheaper_endpoint"
	ReasonCurrentCheaper Reason = "current_is_cheapest"
	ReasonNoCandidate    Reason = Reason(decision.ReasonNoCandidate)
	ReasonCurrentNoPrice Reason = Reason(decision.ReasonCurrentNoPrice)
	ReasonUnknownModel   Reason = "unknown_model"
	ReasonNoPolicy       Reason = "no_policy"
	ReasonNoPrice        Reason = "no_price"
	ReasonNoUsage        Reason = "usage_source_none"
	ReasonPassthrough    Reason = "passthrough"
	ReasonAgentError     Reason = "agent_error_4xx"
	ReasonNoBaseline     Reason = "no_previous_endpoint"
)

// Simulation is the simulated saving of one event.
type Simulation struct {
	CostBest    events.Micros
	PriceIDBest string
	// Saving is max(0, CostActual - CostBest).
	Saving events.Micros
	Reason Reason
	// Counts is false when the event is left out of the totals
	// (usage_source none, passthrough, agent 4xx, unknown model).
	Counts bool
}

// Simulated prices the event's usage on d.Best with the price in effect at
// ev.TS and returns the simulated saving. It never fails: when no decision is
// possible the result is CostBest = CostActual, Saving = 0 and an explicit
// Reason. A zero d (no Best endpoint) means there was no policy.
func Simulated(ev events.Event, d decision.Decision, prices PriceBook) Simulation {
	none := Simulation{CostBest: ev.CostActual, PriceIDBest: ev.PriceIDActual}
	if r := ExcludedReason(ev); r != "" {
		none.Reason = r
		return none
	}
	if ev.ModelID == "" {
		none.Reason = ReasonUnknownModel
		return none
	}
	none.Counts = true
	switch d.Best.EndpointID {
	case "":
		none.Reason = ReasonNoPolicy
		return none
	case ev.EndpointUsed:
		none.Reason = currentReason(d.Reason)
		return none
	}
	cost, priceID, err := prices.Cost(ev.CustomerID, d.Best.EndpointID, ev.TS, UsageOf(ev))
	if err != nil {
		none.Reason = ReasonNoPrice
		return none
	}
	return Simulation{
		CostBest:    cost,
		PriceIDBest: priceID,
		Saving:      max(0, ev.CostActual-cost),
		Reason:      ReasonCheaper,
		Counts:      true,
	}
}

// currentReason explains why the decision kept the current endpoint: no
// candidate passed or the current price failed, else current is cheapest.
func currentReason(r decision.Reason) Reason {
	if r == decision.ReasonNoCandidate || r == decision.ReasonCurrentNoPrice {
		return Reason(r)
	}
	return ReasonCurrentCheaper
}

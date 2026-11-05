package savings

import (
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/decision"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/pricing"
)

// All endpoints, prices and amounts below are fictitious.

var (
	t0   = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	tMid = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
)

// book: "cheap" costs 1 µ$/token in and out (1e6 per M); from Oct 15 00:00
// the list price doubles. "prev" costs 3 µ$/token; customer c1 negotiated 2
// and customer c4 negotiated 1.5; any other customer pays the list price.
// "broken" has no price in October.
func book() PriceBook {
	return PriceBook{
		"cheap": {List: []pricing.Price{
			{PriceID: "cheap-1", EffectiveFrom: t0, EffectiveTo: t0.AddDate(0, 0, 14), InputPerM: 1_000_000, OutputPerM: 1_000_000},
			{PriceID: "cheap-2", EffectiveFrom: t0.AddDate(0, 0, 14), InputPerM: 2_000_000, OutputPerM: 2_000_000},
		}},
		"prev": {
			List: []pricing.Price{{PriceID: "prev-list", EffectiveFrom: t0, InputPerM: 3_000_000, OutputPerM: 3_000_000}},
			Customer: map[string][]pricing.Price{
				"c1": {{PriceID: "prev-neg-c1", EffectiveFrom: t0, InputPerM: 2_000_000, OutputPerM: 2_000_000, Negotiated: true}},
				"c4": {{PriceID: "prev-neg-c4", EffectiveFrom: t0, InputPerM: 1_500_000, OutputPerM: 1_500_000, Negotiated: true}},
			},
		},
		"or":     {List: []pricing.Price{{PriceID: "or-1", EffectiveFrom: t0, InputPerM: 1_000_000, OutputPerM: 1_000_000, ChannelFeeBps: 500}}},
		"broken": {List: []pricing.Price{{PriceID: "old", EffectiveFrom: t0.AddDate(-1, 0, 0), EffectiveTo: t0}}},
		"neg":    {List: []pricing.Price{{PriceID: "neg", EffectiveFrom: t0, InputPerM: -1}}},
	}
}

// event is a shadow request by c5, who has no negotiated price, so the list
// price "prev-list" of the endpoint it used is the one in cost_actual.
func event(ts time.Time) events.Event {
	return events.Event{
		TS: ts, CustomerID: "c5", ModelID: "m1", Mode: events.ModeShadow,
		EndpointUsed: "prev", TokensIn: 60, TokensOut: 40,
		UsageSource: events.UsageProvider, Status: 200,
		CostActual: 300, PriceIDActual: "prev-list",
	}
}

func best(id string) decision.Decision {
	return decision.Decision{Best: decision.Candidate{EndpointID: id}, Reason: decision.ReasonCheaper}
}

func TestSimulated(t *testing.T) {
	early := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	mod := func(f func(*events.Event)) events.Event {
		ev := event(early)
		f(&ev)
		return ev
	}
	tests := []struct {
		name string
		ev   events.Event
		d    decision.Decision
		want Simulation
	}{
		{"cheaper endpoint", event(early), best("cheap"),
			Simulation{CostBest: 100, PriceIDBest: "cheap-1", Saving: 200, Reason: ReasonCheaper, Counts: true}},
		{"price in effect at ev.TS", event(tMid), best("cheap"),
			Simulation{CostBest: 200, PriceIDBest: "cheap-2", Saving: 100, Reason: ReasonCheaper, Counts: true}},
		{"channel fee included", event(early), best("or"),
			Simulation{CostBest: 105, PriceIDBest: "or-1", Saving: 195, Reason: ReasonCheaper, Counts: true}},
		{"saving never negative", mod(func(e *events.Event) { e.CostActual = 50 }), best("cheap"),
			Simulation{CostBest: 100, PriceIDBest: "cheap-1", Saving: 0, Reason: ReasonCheaper, Counts: true}},
		{"fallback attempts count in cost_actual", mod(func(e *events.Event) { e.CostActual = 450 }), best("cheap"),
			Simulation{CostBest: 100, PriceIDBest: "cheap-1", Saving: 350, Reason: ReasonCheaper, Counts: true}},
		{"already cheapest", event(early), best("prev"),
			Simulation{CostBest: 300, PriceIDBest: "prev-list", Reason: ReasonCurrentCheaper, Counts: true}},
		{"no candidate passes", event(early),
			decision.Decision{Best: decision.Candidate{EndpointID: "prev"}, Reason: decision.ReasonNoCandidate},
			Simulation{CostBest: 300, PriceIDBest: "prev-list", Reason: ReasonNoCandidate, Counts: true}},
		{"current price error", event(early),
			decision.Decision{Best: decision.Candidate{EndpointID: "prev"}, Reason: decision.ReasonCurrentNoPrice},
			Simulation{CostBest: 300, PriceIDBest: "prev-list", Reason: ReasonCurrentNoPrice, Counts: true}},
		{"no policy", event(early), decision.Decision{},
			Simulation{CostBest: 300, PriceIDBest: "prev-list", Reason: ReasonNoPolicy, Counts: true}},
		{"unknown model", mod(func(e *events.Event) { e.ModelID = "" }), best("cheap"),
			Simulation{CostBest: 300, PriceIDBest: "prev-list", Reason: ReasonUnknownModel}},
		{"endpoint missing from book", event(early), best("ghost"),
			Simulation{CostBest: 300, PriceIDBest: "prev-list", Reason: ReasonNoPrice, Counts: true}},
		{"no price at ev.TS", event(early), best("broken"),
			Simulation{CostBest: 300, PriceIDBest: "prev-list", Reason: ReasonNoPrice, Counts: true}},
		{"invalid price", event(early), best("neg"),
			Simulation{CostBest: 300, PriceIDBest: "prev-list", Reason: ReasonNoPrice, Counts: true}},
		{"usage_source none", mod(func(e *events.Event) { e.UsageSource = events.UsageNone }), best("cheap"),
			Simulation{CostBest: 300, PriceIDBest: "prev-list", Reason: ReasonNoUsage}},
		{"passthrough", mod(func(e *events.Event) { e.Mode = events.ModePassthrough }), best("cheap"),
			Simulation{CostBest: 300, PriceIDBest: "prev-list", Reason: ReasonPassthrough}},
		{"agent 4xx", mod(func(e *events.Event) { e.Status = 422 }), best("cheap"),
			Simulation{CostBest: 300, PriceIDBest: "prev-list", Reason: ReasonAgentError}},
		{"best priced with the event customer's negotiated price",
			mod(func(e *events.Event) { e.CustomerID, e.EndpointUsed, e.CostActual = "c1", "cheap", 250 }), best("prev"),
			Simulation{CostBest: 200, PriceIDBest: "prev-neg-c1", Saving: 50, Reason: ReasonCheaper, Counts: true}},
		{"another customer's negotiated price never applies",
			mod(func(e *events.Event) { e.CustomerID, e.EndpointUsed, e.CostActual = "c5", "cheap", 350 }), best("prev"),
			Simulation{CostBest: 300, PriceIDBest: "prev-list", Saving: 50, Reason: ReasonCheaper, Counts: true}},
		{"upstream 5xx still counts", mod(func(e *events.Event) { e.Status = 502 }), best("cheap"),
			Simulation{CostBest: 100, PriceIDBest: "cheap-1", Saving: 200, Reason: ReasonCheaper, Counts: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Simulated(tc.ev, tc.d, book())
			if got != tc.want {
				t.Fatalf("Simulated() = %+v, want %+v", got, tc.want)
			}
			if got.Saving != max(0, tc.ev.CostActual-got.CostBest) {
				t.Fatalf("saving %d != max(0, %d-%d)", got.Saving, tc.ev.CostActual, got.CostBest)
			}
		})
	}
}

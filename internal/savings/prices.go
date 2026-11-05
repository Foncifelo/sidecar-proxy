package savings

import (
	"errors"
	"fmt"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/pricing"
	"github.com/Foncifelo/sidecar-proxy/pkg/openai"
)

// ErrUnknownEndpoint is returned when the price book has no entry for an endpoint.
var ErrUnknownEndpoint = errors.New("savings: endpoint not in price book")

// EndpointPrices holds every version of one endpoint's prices: list prices
// and each customer's negotiated prices keyed by customer id (see
// pricing.Select). A customer's negotiated prices never apply to another.
type EndpointPrices struct {
	List     []pricing.Price
	Customer map[string][]pricing.Price
}

// PriceBook maps an endpoint id to its prices. It is read-only input.
type PriceBook map[string]EndpointPrices

// Cost prices usage u on endpoint for customer at instant at and returns the
// cost with the price_id used. Only customer's own negotiated prices are
// considered.
func (b PriceBook) Cost(customer, endpoint string, at time.Time, u openai.Usage) (events.Micros, string, error) {
	ep, ok := b[endpoint]
	if !ok {
		return 0, "", fmt.Errorf("endpoint %q: %w", endpoint, ErrUnknownEndpoint)
	}
	p, err := pricing.Select(ep.List, ep.Customer[customer], at)
	if err != nil {
		return 0, "", fmt.Errorf("endpoint %q at %s: %w", endpoint, at.Format(time.RFC3339), err)
	}
	c, err := pricing.Cost(p, u)
	if err != nil {
		return 0, "", fmt.Errorf("endpoint %q: %w", endpoint, err)
	}
	return c, p.PriceID, nil
}

// UsageOf returns the event's disjoint usage buckets.
func UsageOf(ev events.Event) openai.Usage {
	return openai.Usage{
		In:          ev.TokensIn,
		Out:         ev.TokensOut,
		Reasoning:   ev.TokensReasoning,
		CachedRead:  ev.TokensCachedRead,
		CachedWrite: ev.TokensCachedWrite,
	}
}

// ExcludedReason returns why an event does not count, or "" if it counts
// ("Lo que no se cuenta" in docs/metodologia-ahorro.md).
func ExcludedReason(ev events.Event) Reason {
	switch {
	case ev.UsageSource == events.UsageNone:
		return ReasonNoUsage
	case ev.Mode == events.ModePassthrough:
		return ReasonPassthrough
	case ev.Status >= statusClientErrorMin && ev.Status <= statusClientErrorMax:
		return ReasonAgentError
	}
	return ""
}

const (
	statusClientErrorMin = 400
	statusClientErrorMax = 499
)

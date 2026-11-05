package pricing

import (
	"errors"
	"time"
)

// ErrNoPrice is returned by Select when no price is valid at the given instant.
var ErrNoPrice = errors.New("pricing: no price in effect")

// Price is one versioned price of an endpoint. Fields mirror the prices and
// customer_prices tables; rates are microdollars per million tokens and
// PerRequest is microdollars per request.
type Price struct {
	// PriceID identifies the row the cost was computed with; it is stored
	// verbatim in the event (price_id_actual / price_id_best).
	PriceID       string
	EndpointID    string
	Negotiated    bool // true when it comes from customer_prices
	EffectiveFrom time.Time
	// EffectiveTo is exclusive; the zero value means "still current".
	EffectiveTo time.Time

	InputPerM        int64
	OutputPerM       int64
	CachedInPerM     int64
	CacheWritePerM   int64
	ReasoningOutPerM int64
	PerRequest       int64
	ImagePerK        int64 // not used by Cost: Usage carries no image counts
	ChannelFeeBps    int32
}

// ValidAt reports whether the price is in effect at t, using the half-open
// interval [EffectiveFrom, EffectiveTo).
func (p Price) ValidAt(t time.Time) bool {
	if t.Before(p.EffectiveFrom) {
		return false
	}
	return p.EffectiveTo.IsZero() || t.Before(p.EffectiveTo)
}

// Select returns the price in effect at instant at: a negotiated customer
// price if one is valid, otherwise the list price. Both slices must belong to
// the same endpoint. When several prices of the same kind are valid (which
// the store should prevent), the one with the latest EffectiveFrom wins.
func Select(listPrices, customerPrices []Price, at time.Time) (Price, error) {
	if p, ok := latestValid(customerPrices, at); ok {
		return p, nil
	}
	if p, ok := latestValid(listPrices, at); ok {
		return p, nil
	}
	return Price{}, ErrNoPrice
}

func latestValid(prices []Price, at time.Time) (Price, bool) {
	var best Price
	found := false
	for _, p := range prices {
		if !p.ValidAt(at) {
			continue
		}
		if !found || p.EffectiveFrom.After(best.EffectiveFrom) {
			best, found = p, true
		}
	}
	return best, found
}

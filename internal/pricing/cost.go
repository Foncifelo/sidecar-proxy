package pricing

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/pkg/openai"
)

const (
	tokensPerRateUnit = 1_000_000 // rates are per million tokens
	bpsDenominator    = 10_000    // basis points in 100%
)

var (
	// ErrOverflow is returned when the cost does not fit in int64 microdollars.
	ErrOverflow = errors.New("pricing: cost overflows int64 microdollars")
	// ErrNegative is returned when a token count, rate or fee is negative.
	ErrNegative = errors.New("pricing: negative token count, rate or fee")
)

// Cost returns the cost of one request with usage u at price p, rounded half
// up to the microdollar. See the package documentation for the formula.
func Cost(p Price, u openai.Usage) (events.Micros, error) {
	terms := [][2]int64{
		{u.In, p.InputPerM},
		{u.Out, p.OutputPerM},
		{u.Reasoning, orElse(p.ReasoningOutPerM, p.OutputPerM)},
		{u.CachedRead, orElse(p.CachedInPerM, p.InputPerM)},
		{u.CachedWrite, orElse(p.CacheWritePerM, p.InputPerM)},
	}
	if p.PerRequest < 0 || p.ChannelFeeBps < 0 {
		return 0, ErrNegative
	}

	// num is in units of 1e-6 microdollars before the channel fee.
	num := new(big.Int)
	var term big.Int
	for _, t := range terms {
		if t[0] < 0 || t[1] < 0 {
			return 0, ErrNegative
		}
		num.Add(num, term.Mul(big.NewInt(t[0]), big.NewInt(t[1])))
	}
	num.Add(num, term.Mul(big.NewInt(p.PerRequest), big.NewInt(tokensPerRateUnit)))
	num.Mul(num, big.NewInt(bpsDenominator+int64(p.ChannelFeeBps)))

	den := big.NewInt(tokensPerRateUnit * bpsDenominator)
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	// Half up: round when 2r >= den. num is non-negative, so r is too.
	if r.Lsh(r, 1).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("price %q: %w", p.PriceID, ErrOverflow)
	}
	return events.Micros(q.Int64()), nil
}

// orElse returns rate, or fallback when the specialised rate is unset (0).
func orElse(rate, fallback int64) int64 {
	if rate == 0 {
		return fallback
	}
	return rate
}

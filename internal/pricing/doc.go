// Package pricing computes the cost of a request in microdollars from a
// versioned endpoint price, always with the price_id that was used.
//
// Money is int64 microdollars (events.Micros); no floats are involved.
//
// Rates are microdollars per million tokens. The exact cost is
//
//	(Σ tokens_bucket × rate_bucket / 1e6 + per_request) × (10000 + channel_fee_bps) / 10000
//
// computed as a single exact integer fraction (math/big) and rounded once,
// half up, to the microdollar. A result that does not fit in int64 is an error.
//
// Buckets follow pkg/openai.Usage, which are disjoint, so the cost is additive
// (docs/metodologia-ahorro.md): In × input, Out × output,
// Reasoning × reasoning_out, CachedRead × cached_in, CachedWrite × cache_write.
// The specialised rates default to 0 in the prices table, which means "not
// published separately", not "free": a zero reasoning_out rate falls back to
// the output rate, and zero cached_in / cache_write rates fall back to the
// input rate, which is what the provider bills when it does not distinguish
// those tokens.
package pricing

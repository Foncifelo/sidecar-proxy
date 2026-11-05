// Package decision is a pure function: requirements -> caps filter, same canonical model, quantization at least as good, uptime >= threshold, p90 TTFT <= client p95, cheapest wins; every discarded candidate carries a reason.
//
// Decide prices every surviving candidate with pricing.Cost over a reference
// usage (channel fee included), so the sidecar and the panel obtain the same
// number. Best only moves away from the current endpoint when a candidate is
// strictly cheaper; ties between candidates are broken by endpoint id.
//
// Rank applies the same filters and order without a current endpoint and
// returns every survivor cheapest first, for ordered fallback plans; Decide
// is built on the same evaluation, so its winner is Rank's first endpoint.
package decision

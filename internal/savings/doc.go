// Package savings computes simulated (shadow) savings and holds the pricing
// helpers every saving uses: PriceBook.Cost, UsageOf and ExcludedReason. The
// sidecar and the panel share it so both report the same number
// (docs/metodologia-ahorro.md); the control plane's real (route) savings are
// built on the same helpers.
//
// Every cost is priced with pricing.Cost over the event's own usage, so the
// channel fee and the rate fallbacks of the pricing package apply unchanged.
// Money is events.Micros; no floats are involved.
//
// Simulated compares one event against the endpoint chosen by
// decision.Decide: saving = max(0, cost_actual - cost_best). When no decision
// is possible (unknown model, no policy, no price) or the request does not
// count, cost_best equals cost_actual, the saving is zero and Reason says why.
package savings

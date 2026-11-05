package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/pricing"
	"github.com/Foncifelo/sidecar-proxy/internal/providers"
	"github.com/Foncifelo/sidecar-proxy/internal/sse"
	"github.com/Foncifelo/sidecar-proxy/pkg/openai"
)

// deliverStream holds a 2xx SSE answer until its first data event, without
// writing anything to the agent (ADR 0003). If that event comes later than
// req.ttftLimit after the attempt started, or the stream fails before it,
// and another attempt remains, it records the attempt as aborted and returns
// false. Otherwise it writes the held bytes and copies the rest; a failure
// from then on is an SSE error with stream_state = truncated, never a retry
// (rule 5).
func (h *Handler) deliverStream(w http.ResponseWriter, r *http.Request, req *requestState, ep providers.Endpoint, resp *http.Response, att *attempt, last bool, cancel context.CancelCauseFunc) bool {
	defer func() { _ = resp.Body.Close() }()
	idle, stopIdle := newIdleReader(resp.Body, h.o.StreamIdleTimeout, cancel)
	defer stopIdle()

	limit := req.ttftLimit
	if last {
		limit = 0 // nowhere to fall back to
	}
	held, late, err := h.holdFirstData(idle, att.start, limit, cancel)
	if (late || err != nil) && !last {
		att.AbortReason = abortHTTPError
		if late {
			att.AbortReason = abortTTFTOverP95
			att.CostEst = req.abortedCost(ep.ID, att.TokensOutEst)
		}
		att.TotalMs = h.o.Now().Sub(att.start).Milliseconds()
		req.attempts = append(req.attempts, att.Attempt)
		return false
	}

	first := h.o.Now()
	att.TTFTMs = first.Sub(att.start).Milliseconds()
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(held), idle), resp.Body}
	// ParseServed runs before any header reaches the agent. A channel that
	// peeks (OpenRouter) reads up to the first data line holding a JSON
	// object; held ends with the first data block, so it normally stops
	// inside held. If that block has no such line (e.g. "data: [DONE]"), the
	// peek reads on into the live stream, up to the provider's peek limit and
	// bounded only by the stream idle timeout, since the TTFT timer has
	// already stopped. Whatever it reads is put back in resp.Body.
	served := providers.For(ep.Channel).ParseServed(resp)
	req.setServed(ep, resp.StatusCode, served)

	w = newDeadlineWriter(w, h.o.WriteTimeout)
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_ = http.NewResponseController(w).Flush()
	req.rewriteModel = true
	h.copyStream(r.Context(), w, req, &firstByteReader{r: resp.Body, now: h.o.Now})
	req.ttft = first.Sub(req.start)
	h.finishAttempt(req, ep, att)
	return true
}

// holdFirstData reads body up to its first data event. With limit > 0 the
// attempt is cancelled when no data event arrived by start + limit, and late
// reports it; headers already past the limit count as late at once. Any
// error, io.EOF included, means the stream ended or failed before data.
func (h *Handler) holdFirstData(body io.Reader, start time.Time, limit time.Duration, cancel context.CancelCauseFunc) ([]byte, bool, error) {
	var timer *time.Timer
	if limit > 0 {
		wait := limit - h.o.Now().Sub(start)
		if wait <= 0 {
			return nil, true, nil
		}
		timer = time.AfterFunc(wait, func() { cancel(errTTFTOverP95) })
	}
	held, err := sse.HoldFirstData(body)
	// Stop reports false once the timer fired: cancel has run or is about
	// to run and will kill the attempt, so it is late even if data arrived.
	late := timer != nil && !timer.Stop()
	if err != nil {
		return held, late, fmt.Errorf("proxy: before first event: %w", err)
	}
	return held, late, nil
}

const (
	// maxAssumedOutputTokens bounds, per choice, the output assumed for an
	// attempt abandoned at the first byte timeout when the endpoint's
	// context_length is unknown: the agent's max_tokens is client
	// controlled and may be absurd.
	maxAssumedOutputTokens = 32768
	// maxAssumedChoices bounds the agent's n in that estimate.
	maxAssumedChoices = 128
)

// abortedCost estimates what the provider may bill for an attempt cancelled
// after it accepted the request: the estimated prompt plus tokensOut output
// tokens, at the endpoint's policy price. Zero without a price; a cost that
// overflows int64 or exceeds events.MaxAttemptCostMicros is capped there, so
// it is never underestimated below the ceiling nor rejected at ingest.
func (s *requestState) abortedCost(endpointID string, tokensOut int64) events.Micros {
	price, ok := s.prices[endpointID]
	if !ok {
		return 0
	}
	return attemptCost(price, openai.Usage{In: s.promptTokensEst, Out: tokensOut})
}

// attemptCost prices one attempt's usage capped at
// events.MaxAttemptCostMicros: an overflow or a larger amount yields the
// ceiling, never math.MaxInt64; any other pricing error yields 0.
func attemptCost(price pricing.Price, u openai.Usage) events.Micros {
	c, err := pricing.Cost(price, u)
	if errors.Is(err, pricing.ErrOverflow) {
		return events.MaxAttemptCostMicros
	}
	if err != nil {
		return 0
	}
	return min(c, events.MaxAttemptCostMicros)
}

// timeoutCost is the conservative cost_est of an attempt abandoned at the
// first byte timeout after its request was written, and the output tokens
// it assumes: an upper bound, so cost_actual is never underestimated. The
// output per choice is the agent's max_completion_tokens or max_tokens,
// capped at the endpoint's context_length or, when that is unknown, at
// maxAssumedOutputTokens; without an agent limit, what is left of the
// context_length after the estimated prompt; without either, none. It is
// multiplied by n, capped at maxAssumedChoices. The cost is capped at
// events.MaxAttemptCostMicros by abortedCost.
func (s *requestState) timeoutCost(endpointID string) (events.Micros, int64) {
	ctx := s.contexts[endpointID]
	out := s.info.MaxOutputTokens
	switch {
	case out <= 0:
		out = max(ctx-s.promptTokensEst, 0)
	case ctx > 0:
		out = min(out, ctx)
	default:
		out = min(out, maxAssumedOutputTokens)
	}
	out = satMul(out, int64(min(max(s.info.N, 1), maxAssumedChoices)))
	c := s.abortedCost(endpointID, out)
	if c == 0 {
		return 0, 0
	}
	return c, out
}

// satMul multiplies two non-negative token counts without wrapping. The
// product is at most a policy context_length times maxAssumedChoices, so it
// only saturates for a context_length no model has; it is a token count,
// never summed, and the cost derived from it is capped by abortedCost.
func satMul(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

// addCapped adds a non-negative attempt cost to a running total, capping the
// result at events.MaxAttemptCostMicros. Comparing before adding means it
// never overflows, whatever b is.
func addCapped(a, b events.Micros) events.Micros {
	if b >= events.MaxAttemptCostMicros-a {
		return events.MaxAttemptCostMicros
	}
	return a + b
}

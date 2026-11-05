package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/policy"
	"github.com/Foncifelo/sidecar-proxy/internal/pricing"
	"github.com/Foncifelo/sidecar-proxy/internal/providers"
	"github.com/Foncifelo/sidecar-proxy/internal/transport"
	"github.com/Foncifelo/sidecar-proxy/pkg/openai"
)

// ReasonNotRouted is the decision for requests that route mode sends to the
// YAML upstream unchanged: every non-chat route.
const ReasonNotRouted = "not_routed"

// Abort reasons of an attempt (docs/evento.md).
const (
	abortFirstByteTimeout = "first_byte_timeout"
	abortHTTPError        = "http_error"
	abortRateLimited      = "rate_limited"
	abortTTFTOverP95      = "ttft_over_p95"
)

var (
	errFirstByteTimeout = errors.New("proxy: first byte timeout")
	errTTFTOverP95      = errors.New("proxy: first token later than client p95 + slack")
)

// Route configures route mode.
type Route struct {
	// Config is the client's YAML: the only source of URLs, keys, channels
	// and attribution headers (rule 8, ADR 0008).
	Config config.Config
	// Transports holds one transport per provider (keyed by provider, or
	// "openrouter" for that channel) and the breaker and bulkhead of each
	// endpoint.
	Transports *transport.Set
	// FirstByteTimeout is limits.first_byte_timeout: an attempt that has
	// not sent headers by then is abandoned for the next one. The last
	// attempt has nowhere to fall back to and is not bounded by it.
	FirstByteTimeout time.Duration
	// NonStreamFirstByteTimeout is limits.nonstream_first_byte_timeout and
	// replaces FirstByteTimeout for requests without stream, whose headers
	// only come with the whole answer. Zero means
	// Config.Limits.NonStreamFirstByteTimeout, or the config default when
	// that is zero too.
	NonStreamFirstByteTimeout time.Duration
}

// firstByteTimeout is how long a non-last attempt may go without headers.
func (rt *Route) firstByteTimeout(stream bool) time.Duration {
	if stream {
		return rt.FirstByteTimeout
	}
	if rt.NonStreamFirstByteTimeout > 0 {
		return rt.NonStreamFirstByteTimeout
	}
	if d := rt.Config.Limits.NonStreamFirstByteTimeout.Std(); d > 0 {
		return d
	}
	return config.DefaultNonStreamFirstByteTimeout
}

// openBreakers adapts transport.Set to policy.Breakers.
type openBreakers struct{ s *transport.Set }

func (b openBreakers) IsOpen(id string) bool {
	return b.s.Endpoint(id).Breaker.State() == transport.StateOpen
}

// routable reports whether route mode tries the plan for this request: only
// chat, streamed or not, with a parsed body.
func (h *Handler) routable(req *requestState) bool {
	return h.o.Mode == config.ModeRoute && req.kind == events.KindChat && req.rawModel != ""
}

// serveRoute runs the policy's plan: attempts in order, falling back before
// any byte reaches the agent (ADR 0003).
func (h *Handler) serveRoute(w http.ResponseWriter, r *http.Request, req *requestState, body []byte) {
	var a *policy.Active
	if h.o.Policy != nil {
		a = h.o.Policy()
	}
	if a != nil {
		req.policyVersion = a.Version()
	}
	req.routed = true
	req.attempts = []events.Attempt{}
	var p95 time.Duration
	req.modelID, req.prices, req.contexts, p95 = routePrices(a, h.o.Upstream.Provider, req.rawModel)
	if p95 > 0 {
		req.ttftLimit = p95 + h.o.Route.Config.Limits.TTFTP95Slack.Std()
	}
	plan, why := policy.Plan(a, policy.Request{RawModel: req.rawModel, Needs: req.needs()}, h.o.Route.Config, openBreakers{h.o.Route.Transports})
	req.endpointBest = plan[0].ID
	h.setDecision(w, req, string(why))

	for i, ep := range plan {
		ep = h.withAttribution(ep)
		last := i == len(plan)-1
		h.setServing(w, ep, i)
		if i == 0 {
			h.observeOverhead(req)
		}
		att, resp, cancel := h.try(r, req, ep, body, last)
		if resp != nil && (att.AbortReason == "" || last) {
			att.AbortReason = ""
			var ok bool
			if req.stream && resp.StatusCode < http.StatusMultipleChoices && isEventStream(resp.Header.Get("Content-Type")) {
				ok = h.deliverStream(w, r, req, ep, resp, att, last, cancel)
			} else {
				ok = h.deliver(w, req, ep, resp, att, last, cancel)
			}
			cancel(nil)
			if ok {
				return
			}
		} else {
			if resp != nil {
				_ = resp.Body.Close()
			}
			cancel(nil)
			req.attempts = append(req.attempts, att.Attempt)
		}
		if r.Context().Err() != nil {
			req.errorType = "client_gone"
			req.total = h.o.Now().Sub(req.start)
			h.emit(req)
			return
		}
	}
	req.noResponse = true
	req.status = http.StatusBadGateway
	req.errorType = "upstream_unreachable"
	req.total = h.o.Now().Sub(req.start)
	writeLocalError(w, http.StatusBadGateway, ReasonUpstreamErr, "no endpoint answered")
	h.emit(req)
}

// setServing writes the real endpoint headers for attempt i (ADR 0003).
// They are rewritten per attempt so the agent sees the one that answered.
func (h *Handler) setServing(w http.ResponseWriter, ep providers.Endpoint, i int) {
	hd := w.Header()
	hd.Set(HeaderProvider, ep.Provider)
	hd.Set(HeaderEndpoint, ep.ID)
	hd.Set(HeaderFallback, strconv.Itoa(min(i, 1)))
}

// withAttribution adds HTTP-Referer and X-Title from the YAML on the
// openrouter channel (ADR 0008).
func (h *Handler) withAttribution(ep providers.Endpoint) providers.Endpoint {
	if ep.Channel == providers.ChannelOpenRouter {
		pc := h.o.Route.Config.Providers[config.ProviderOpenRouter]
		ep.HTTPReferer, ep.XTitle = pc.HTTPReferer, pc.XTitle
	}
	return ep
}

// transportKey is the provider whose transport carries ep.
func transportKey(ep providers.Endpoint) string {
	if ep.Channel == providers.ChannelOpenRouter {
		return config.ProviderOpenRouter
	}
	return ep.Provider
}

// attempt is one try plus the instants its durations are measured from.
type attempt struct {
	events.Attempt
	start, headers time.Time
}

// try sends one attempt. A non-nil response always comes with headers; a
// non-empty AbortReason means the attempt failed before the first byte.
// The returned cancel ends the attempt's context and must always be called.
// An attempt abandoned at the first byte timeout after its request was
// written carries a conservative cost_est (timeoutCost): the provider may
// bill it anyway.
func (h *Handler) try(r *http.Request, req *requestState, ep providers.Endpoint, body []byte, last bool) (*attempt, *http.Response, context.CancelCauseFunc) {
	start := h.o.Now()
	att := &attempt{start: start, Attempt: events.Attempt{
		EndpointID: ep.ID, ProviderName: ep.Provider, Channel: string(ep.Channel), AbortReason: abortHTTPError,
	}}
	ctx, cancel := context.WithCancelCause(r.Context())
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteHeaders: func() { wrote.Store(true) }})
	finish := func() { att.TotalMs = h.o.Now().Sub(start).Milliseconds() }

	out, err := providers.For(ep.Channel).BuildRequest(ctx, ep, body)
	if err != nil {
		finish()
		return att, nil, cancel
	}
	out.URL.RawQuery = r.URL.RawQuery // only path and query are the agent's
	providers.CopyAllowedHeaders(out, r.Header)
	rt, err := h.o.Route.Transports.RoundTripper(transportKey(ep), ep.ID)
	if err != nil {
		finish()
		return att, nil, cancel
	}
	var timer *time.Timer
	if fbt := h.o.Route.firstByteTimeout(req.stream); !last && fbt > 0 {
		timer = time.AfterFunc(fbt, func() { cancel(errFirstByteTimeout) })
	}
	// Host and key come from the YAML via policy.Plan; only the query is the agent's.
	resp, err := rt.RoundTrip(out)
	timedOut := timer != nil && !timer.Stop()
	if timedOut || errors.Is(context.Cause(ctx), errFirstByteTimeout) {
		if resp != nil {
			_ = resp.Body.Close()
		}
		att.AbortReason = abortFirstByteTimeout
		if wrote.Load() {
			att.CostEst, att.TokensOutEst = req.timeoutCost(ep.ID)
		}
		finish()
		return att, nil, cancel
	}
	if err != nil {
		finish()
		return att, nil, cancel
	}
	att.Status = resp.StatusCode
	att.headers = h.o.Now()
	att.TTFTMs = att.headers.Sub(start).Milliseconds()
	att.TotalMs = att.TTFTMs
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		att.AbortReason = abortRateLimited
	case resp.StatusCode >= http.StatusInternalServerError:
		att.AbortReason = abortHTTPError
	default:
		att.AbortReason = ""
	}
	return att, resp, cancel
}

// deliver reads the response, rewrites its top-level model to the agent's
// (ADR 0008) and writes it. It returns false, without writing anything, when
// the body could not be read and another attempt remains. A provider silent
// for longer than StreamIdleTimeout mid-body gets the attempt cancelled
// through cancel, so it neither hangs the request nor holds its bulkhead slot.
func (h *Handler) deliver(w http.ResponseWriter, req *requestState, ep providers.Endpoint, resp *http.Response, att *attempt, last bool, cancel context.CancelCauseFunc) bool {
	defer func() { _ = resp.Body.Close() }()
	idle, stopIdle := newIdleReader(resp.Body, h.o.StreamIdleTimeout, cancel)
	defer stopIdle()
	resp.Body = struct {
		io.Reader
		io.Closer
	}{idle, resp.Body}
	served := providers.For(ep.Channel).ParseServed(resp)
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageScan+1))
	if err != nil && !last {
		att.AbortReason = abortHTTPError
		att.TotalMs = h.o.Now().Sub(att.start).Milliseconds()
		req.attempts = append(req.attempts, att.Attempt)
		return false
	}
	req.setServed(ep, resp.StatusCode, served)
	req.ttft = att.headers.Sub(req.start)
	overflow := len(data) > maxUsageScan
	out := data
	if !overflow {
		out = rewriteModel(data, req.rawModel)
		if req.status < http.StatusBadRequest {
			req.onBody(data)
		}
	}

	w = newDeadlineWriter(w, h.o.WriteTimeout)
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	fw := &flushWriter{w: w, rc: http.NewResponseController(w)}
	_, werr := fw.Write(out)
	if werr == nil && overflow {
		_, werr = io.Copy(fw, resp.Body)
	}
	switch {
	case err != nil:
		req.errorType = "upstream_body_error"
	case werr != nil:
		req.errorType = "client_gone"
	}

	h.finishAttempt(req, ep, att)
	return true
}

// setServed records the endpoint that answered and the provider that served
// it (the adapter's report, or the endpoint's provider when empty).
func (req *requestState) setServed(ep providers.Endpoint, status int, served string) {
	req.status = status
	req.endpointUsed = ep.ID
	req.providerCalled, req.providerServed = ep.Provider, ep.Provider
	if served != "" {
		req.providerServed = served
	}
}

// finishAttempt prices the delivered attempt over the real usage, records it
// and emits the event.
func (h *Handler) finishAttempt(req *requestState, ep providers.Endpoint, att *attempt) {
	att.TotalMs = h.o.Now().Sub(att.start).Milliseconds()
	if price, ok := req.prices[ep.ID]; ok {
		req.priceActual, req.hasPriceActual = price, true
		if req.hasUsage {
			att.CostEst = attemptCost(price, req.usage)
		}
	}
	att.TokensOutEst = req.usage.Out + req.usage.Reasoning
	req.attempts = append(req.attempts, att.Attempt)
	req.total = h.o.Now().Sub(req.start)
	h.emit(req)
}

// rewriteModel sets the top-level "model" of a JSON object to model, only if
// the member exists; every other byte is preserved (ADR 0008).
func rewriteModel(body []byte, model string) []byte {
	return openai.ReplaceModel(body, model)
}

// routePrices returns the canonical model, the policy price and the
// context_length (when known) of every endpoint the plan may call, keyed by
// endpoint id, and the model's client_ttft_p95 (zero without data).
func routePrices(a *policy.Active, provider, rawModel string) (string, map[string]pricing.Price, map[string]int64, time.Duration) {
	if a == nil {
		return "", nil, nil, 0
	}
	id, m, ok := a.Resolve(provider, rawModel)
	if !ok {
		return "", nil, nil, 0
	}
	prices := make(map[string]pricing.Price, len(m.Candidates)+1)
	contexts := make(map[string]int64, len(m.Candidates))
	for _, c := range m.Candidates {
		prices[c.EndpointID] = c.Price.Pricing(c.EndpointID)
		if c.ContextLength > 0 {
			contexts[c.EndpointID] = c.ContextLength
		}
	}
	if cur, ok := m.CurrentFor(provider, rawModel); ok {
		prices[cur.EndpointID] = cur.Price.Pricing(cur.EndpointID)
	}
	return id, prices, contexts, time.Duration(m.ClientTTFTP95Ms) * time.Millisecond
}

// applyRouteCosts sums every attempt into cost_actual (rule 5). Route has no
// simulation: cost_best is what the served endpoint cost. The sum is capped
// at events.MaxAttemptCostMicros, the ceiling the control plane accepts, so
// it never wraps and the event is never rejected at ingest.
func (s *requestState) applyRouteCosts(ev *events.Event) {
	var sum events.Micros
	for _, a := range s.attempts {
		sum = addCapped(sum, a.CostEst)
	}
	ev.CostActual, ev.CostBest = sum, sum
	if s.hasPriceActual {
		ev.PriceIDActual, ev.PriceIDBest = s.priceActual.PriceID, s.priceActual.PriceID
	}
}

package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/decision"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/pricing"
	"github.com/Foncifelo/sidecar-proxy/pkg/openai"
)

const chatPath = "/v1/chat/completions"

// requestState is the metadata gathered while serving one request. It never
// holds bodies or headers (CONTRIBUTING.md rule 1).
type requestState struct {
	id           string
	start        time.Time
	kind         events.EndpointKind
	rawModel     string
	stream       bool
	includeUsage bool
	requirements []string
	info         openai.RequestInfo // parsed chat flags; never the body
	// contextTokens is the context the chat request needs, estimated from
	// the body's length (openai.EstimateContextTokens); never its content.
	contextTokens int64

	// Set by decide when a policy is active (shadow only).
	policyVersion string
	modelID       string
	endpointUsed  string
	decision      decision.Decision
	curPrice      pricing.Price
	hasCurPrice   bool
	// Set by serveRoute (route mode, non-streamed chat).
	routed   bool
	attempts []events.Attempt
	prices   map[string]pricing.Price
	// contexts is the policy's context_length per endpoint id, used to
	// bound the output of an attempt abandoned at the first byte timeout.
	contexts       map[string]int64
	endpointBest   string
	providerCalled string
	providerServed string
	priceActual    pricing.Price
	hasPriceActual bool
	// rewriteModel sets the agent's model in each delivered SSE chunk
	// (route only, ADR 0008).
	rewriteModel bool
	// ttftLimit is client_ttft_p95 + ttft_p95_slack for a routed stream;
	// zero means no limit.
	ttftLimit time.Duration
	// promptTokensEst is the input estimated from the body's length, used to
	// price attempts aborted before any usage arrived.
	promptTokensEst int64

	// reason mirrors the X-Sidecar-Decision header; set only via setDecision.
	reason string

	status      int
	errorType   string
	ttft        time.Duration
	total       time.Duration
	usage       openai.Usage
	hasUsage    bool
	finish      string
	streamState events.StreamState
	noResponse  bool
	local       bool // answered by the sidecar without an upstream call
}

func newRequestState(id string, start time.Time, r *http.Request) *requestState {
	return &requestState{id: id, start: start, kind: kindOf(r), endpointUsed: UpstreamEndpointID}
}

// kindOf maps a path to its endpoint kind; only POST chat is handled as chat.
func kindOf(r *http.Request) events.EndpointKind {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == chatPath:
		return events.KindChat
	case r.URL.Path == "/v1/responses" || strings.HasPrefix(r.URL.Path, "/v1/responses/"):
		return events.KindResponses
	case r.URL.Path == "/v1/embeddings":
		return events.KindEmbeddings
	default:
		return events.KindPassthrough
	}
}

// inspect reads model, stream and requirements from the body, best effort.
func (s *requestState) inspect(body []byte) {
	if s.kind == events.KindChat {
		info, err := openai.ParseRequest(body)
		if err != nil {
			return
		}
		s.info = info
		s.contextTokens = openai.EstimateContextTokens(len(body), info)
		s.promptTokensEst = openai.EstimateContextTokens(len(body), openai.RequestInfo{})
		s.rawModel, s.stream, s.includeUsage = info.Model, info.Stream, info.IncludeUsage
		s.requirements = requirements(info)
		return
	}
	var generic struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if json.Unmarshal(body, &generic) == nil {
		s.rawModel, s.stream = generic.Model, generic.Stream
	}
}

func requirements(info openai.RequestInfo) []string {
	var out []string
	if info.HasTools {
		out = append(out, "tools")
	}
	if info.ResponseFormatType == "json_schema" {
		out = append(out, "json_schema")
	}
	if info.Logprobs {
		out = append(out, "logprobs")
	}
	if info.N > 1 {
		out = append(out, "n>1")
	}
	return out
}

// onEvent extracts usage and finish_reason from one SSE data payload.
func (s *requestState) onData(data []byte) {
	switch s.kind {
	case events.KindChat:
		var c openai.ChatChunk
		if json.Unmarshal(data, &c) != nil {
			return
		}
		for _, ch := range c.Choices {
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				s.finish = *ch.FinishReason
			}
		}
		s.setUsage(openai.FromChat(c.Usage))
	case events.KindResponses:
		var ev struct {
			Response *struct {
				Usage *openai.ResponsesUsage `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(data, &ev) == nil && ev.Response != nil {
			s.setUsage(openai.FromResponses(ev.Response.Usage))
		}
	case events.KindEmbeddings, events.KindPassthrough:
	}
}

// onBody extracts usage from a complete non-streamed body.
func (s *requestState) onBody(body []byte) {
	switch s.kind {
	case events.KindChat:
		var c openai.ChatResponse
		if json.Unmarshal(body, &c) != nil {
			return
		}
		if len(c.Choices) > 0 {
			s.finish = c.Choices[0].FinishReason
		}
		s.setUsage(openai.FromChat(c.Usage))
	case events.KindResponses:
		var c struct {
			Usage *openai.ResponsesUsage `json:"usage"`
		}
		if json.Unmarshal(body, &c) == nil {
			s.setUsage(openai.FromResponses(c.Usage))
		}
	case events.KindEmbeddings:
		var c struct {
			Usage *openai.EmbeddingsUsage `json:"usage"`
		}
		if json.Unmarshal(body, &c) == nil {
			s.setUsage(openai.FromEmbeddings(c.Usage))
		}
	case events.KindPassthrough:
	}
}

func (s *requestState) setUsage(u openai.Usage, ok bool) {
	if ok {
		s.usage, s.hasUsage = u, true
	}
}

// emit builds the event and appends it to the sink (shadow and route). A
// failing sink is logged once per episode (dropLog), never per request.
func (h *Handler) emit(s *requestState) {
	h.countRoute(s)
	if !h.measuring() {
		return
	}
	var r dropReport
	if err := h.o.Sink.Append(h.buildEvent(s)); err != nil {
		r = h.drops.failed(h.o.Now(), err)
	} else {
		r = h.drops.succeeded(h.o.Now())
	}
	r.log(h.o.Logger)
}

func (h *Handler) buildEvent(s *requestState) events.Event {
	ev := events.Event{
		ID:             s.id,
		TS:             s.start.UTC(),
		SidecarVersion: h.o.Version,
		Kind:           s.kind,
		Stream:         s.stream,
		RawModel:       s.rawModel,
		ModelID:        s.modelID,
		Requirements:   s.requirements,
		Mode:           events.ModeShadow,
		Billable:       false,
		EndpointUsed:   s.endpointUsed,
		ProviderServed: h.o.Upstream.Provider,
		EndpointBest:   s.decision.Best.EndpointID,
		DecisionReason: s.reason,
		PolicyVersion:  s.policyVersion,
		StreamState:    s.streamState,
		UsageSource:    events.UsageNone,
		FinishReason:   s.finish,
		ErrorType:      s.errorType,
		TTFTMs:         s.ttft.Milliseconds(),
		TotalMs:        s.total.Milliseconds(),
		Status:         s.status,
		Attempts:       []events.Attempt{},
	}
	if h.o.Mode == config.ModeRoute {
		ev.Mode, ev.Billable = events.ModeRoute, true
	}
	if s.hasUsage {
		ev.UsageSource = events.UsageProvider
		ev.TokensIn, ev.TokensOut = s.usage.In, s.usage.Out
		ev.TokensReasoning = s.usage.Reasoning
		ev.TokensCachedRead, ev.TokensCachedWrite = s.usage.CachedRead, s.usage.CachedWrite
		if gen := s.total - s.ttft; gen > 0 {
			ev.TPS = float64(s.usage.Out+s.usage.Reasoning) / gen.Seconds()
		}
	}
	if s.routed {
		ev.EndpointBest = s.endpointBest
		if s.providerServed != "" {
			ev.ProviderServed = s.providerServed
		}
		ev.Attempts = s.attempts
		s.applyRouteCosts(&ev)
		return ev
	}
	s.applyCosts(&ev)
	if !s.local && (s.status != 0 || s.noResponse) {
		a := events.Attempt{
			EndpointID:   s.endpointUsed,
			ProviderName: h.o.Upstream.Provider,
			Channel:      "direct",
			Status:       s.status,
			TTFTMs:       ev.TTFTMs,
			TotalMs:      ev.TotalMs,
			TokensOutEst: s.usage.Out + s.usage.Reasoning,
			CostEst:      ev.CostActual,
		}
		if s.noResponse {
			a.AbortReason = "http_error"
		}
		ev.Attempts = []events.Attempt{a}
	}
	return ev
}

// observeOverhead records the time from the request's arrival to its first
// upstream call (docs/contrato-proxy.md, guarantee 8).
func (h *Handler) observeOverhead(s *requestState) {
	h.o.Metrics.ObserveOverhead(h.o.Now().Sub(s.start))
}

// countRoute records a routed request's attempts and fallback. emit runs
// once per request, so each attempt is counted once.
func (h *Handler) countRoute(s *requestState) {
	if !s.routed {
		return
	}
	for _, a := range s.attempts {
		h.o.Metrics.Attempt(a.AbortReason)
	}
	if len(s.attempts) > 1 {
		h.o.Metrics.Fallback()
	}
}

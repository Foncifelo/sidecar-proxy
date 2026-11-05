package policy

import (
	"slices"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/decision"
	"github.com/Foncifelo/sidecar-proxy/internal/providers"
)

// MaxAttempts is the largest number of attempts in a route plan, counting the
// upstream that always closes it.
const MaxAttempts = 3

// UpstreamEndpointID identifies the YAML upstream when the policy does not
// know which endpoint it resolves to. It matches proxy.UpstreamEndpointID.
const UpstreamEndpointID = "upstream"

// Breakers reports which endpoints have an open circuit breaker. A nil
// Breakers means none is open.
type Breakers interface {
	IsOpen(endpointID string) bool
}

// Request is what Plan needs from one agent request.
type Request struct {
	// RawModel is the model the agent sent.
	RawModel string
	// Needs carries the request's caps, N and ContextTokens. Plan overwrites
	// Model, Quantization and RefUsage from the policy (decision.Rank has no
	// current endpoint and filters against them).
	Needs decision.Requirements
}

// PlanReason says why a plan routes or stays on the upstream.
type PlanReason string

// Plan reasons.
const (
	PlanRouted        PlanReason = "routed"
	PlanNoPolicy      PlanReason = "no_policy"
	PlanUnknownModel  PlanReason = "unknown_model"
	PlanRouteDisabled PlanReason = "route_disabled"
	PlanNoCurrent     PlanReason = "no_current_reference"
	PlanNoCandidate   PlanReason = "no_candidate_passes"
)

// Plan returns the ordered attempts for a request in route mode and why. It
// is pure and never returns an empty plan: the YAML upstream is always the
// last attempt, and the only one when the request cannot be routed.
//
// Every BaseURL, APIKey and AuthHeader comes from cfg (rule 8, ADR 0008); the
// policy only contributes endpoint ids, their order, provider_model_name and
// the figures the filters use. Candidates with an open breaker, or that cfg
// does not allow or cannot reach, are skipped. current is never routable.
func Plan(a *Active, req Request, cfg config.Config, br Breakers) ([]providers.Endpoint, PlanReason) {
	if a == nil {
		return upstreamOnly(cfg, req.RawModel, UpstreamEndpointID, PlanNoPolicy)
	}
	modelID, m, ok := a.Resolve(cfg.Upstream.Provider, req.RawModel)
	if !ok {
		return upstreamOnly(cfg, req.RawModel, UpstreamEndpointID, PlanUnknownModel)
	}
	cur, hasCur := m.CurrentFor(cfg.Upstream.Provider, req.RawModel)
	upID := UpstreamEndpointID
	if hasCur {
		upID = cur.EndpointID
	}
	if !m.RouteEnabled {
		return upstreamOnly(cfg, req.RawModel, upID, PlanRouteDisabled)
	}
	if !hasCur {
		return upstreamOnly(cfg, req.RawModel, upID, PlanNoCurrent)
	}

	needs := req.Needs
	needs.Model, needs.Quantization, needs.RefUsage = modelID, cur.Quantization, m.RefUsage.Usage()
	ranked := decision.Rank(needs, toDecision(modelID, m.Candidates), thresholds(m, cfg.Routing))

	byID := make(map[string]Candidate, len(m.Candidates))
	for _, c := range m.Candidates {
		byID[c.EndpointID] = c
	}
	plan := make([]providers.Endpoint, 0, MaxAttempts)
	for _, rc := range ranked {
		if len(plan) == MaxAttempts-1 {
			break
		}
		if rc.EndpointID == cur.EndpointID || (br != nil && br.IsOpen(rc.EndpointID)) {
			continue
		}
		if ep, ok := candidateEndpoint(byID[rc.EndpointID], cfg); ok {
			plan = append(plan, ep)
		}
	}
	reason := PlanRouted
	if len(plan) == 0 {
		reason = PlanNoCandidate
	}
	return append(plan, upstreamEndpoint(cfg, req.RawModel, upID)), reason
}

func upstreamOnly(cfg config.Config, rawModel, id string, r PlanReason) ([]providers.Endpoint, PlanReason) {
	return []providers.Endpoint{upstreamEndpoint(cfg, rawModel, id)}, r
}

// upstreamEndpoint is the path the agent used before the sidecar, and its
// body must arrive intact (docs/contrato-proxy.md, guarantee 1): no model
// rewrite and no provider pin. It always uses the generic adapter, as the
// shadow path does (forward.go), even when the upstream is OpenRouter: the
// OpenRouter adapter would pin Provider ("openrouter", not a real provider)
// over the agent's own "provider" member.
func upstreamEndpoint(cfg config.Config, rawModel, id string) providers.Endpoint {
	return providers.Endpoint{
		ID:                id,
		Provider:          cfg.Upstream.Provider,
		Channel:           providers.ChannelDirect,
		BaseURL:           cfg.Upstream.BaseURL,
		ProviderModelName: rawModel,
		APIKey:            cfg.Upstream.APIKey,
	}
}

// candidateEndpoint builds the attempt for c with credentials from cfg only.
// It re-checks what Filter checks, so a policy that was not filtered still
// cannot add a channel, provider, URL or key.
func candidateEndpoint(c Candidate, cfg config.Config) (providers.Endpoint, bool) {
	if !slices.Contains(cfg.Routing.AllowedChannels, string(c.Channel)) ||
		!slices.Contains(cfg.Routing.AllowedProviders, c.Provider) {
		return providers.Endpoint{}, false
	}
	var pc config.ProviderConfig
	var ch providers.Channel
	switch c.Channel {
	case ChannelDirect:
		pc, ch = cfg.Providers[c.Provider], providers.ChannelDirect
	case ChannelOpenRouter:
		pc, ch = cfg.Providers[config.ProviderOpenRouter], providers.ChannelOpenRouter
		if pc.BaseURL == "" {
			pc.BaseURL = config.DefaultOpenRouterBaseURL
		}
	default:
		return providers.Endpoint{}, false
	}
	if pc.APIKey == "" || !cfg.BaseURLAllowed(pc.BaseURL) {
		return providers.Endpoint{}, false
	}
	return providers.Endpoint{
		ID:                c.EndpointID,
		Provider:          c.Provider,
		Channel:           ch,
		BaseURL:           pc.BaseURL,
		ProviderModelName: c.ProviderModelName,
		APIKey:            pc.APIKey,
		AuthHeader:        pc.AuthHeader,
		RewriteModel:      true,
	}, true
}

func toDecision(modelID string, cs []Candidate) []decision.Candidate {
	out := make([]decision.Candidate, len(cs))
	for i, c := range cs {
		out[i] = decision.Candidate{
			EndpointID:   c.EndpointID,
			Model:        modelID,
			Quantization: c.Quantization,
			Caps: decision.Caps{
				Tools:         c.Caps.Tools,
				JSONSchema:    c.Caps.JSONSchema,
				Vision:        c.Caps.Vision,
				Logprobs:      c.Caps.Logprobs,
				MaxN:          c.Caps.N,
				ContextLength: c.ContextLength,
			},
			Stats: decision.Stats{
				Uptime1d: c.Stats.Uptime1d,
				TTFTP90:  time.Duration(c.Stats.P90TTFTMs) * time.Millisecond,
				TPSP50:   c.Stats.TPSP50,
			},
			Price: c.Price.Pricing(c.EndpointID),
		}
	}
	return out
}

// thresholds combines the model's client figures with the YAML limits, which
// can only make the filters stricter.
func thresholds(m Model, r config.Routing) decision.Thresholds {
	th := decision.Thresholds{
		MinUptime1d:   max(decision.DefaultMinUptime1d, r.MinUptime1d),
		ClientTTFTP95: time.Duration(m.ClientTTFTP95Ms) * time.Millisecond,
		ClientTPSP50:  m.ClientTPSP50,
	}
	if r.MaxTTFTP95Ms > 0 {
		limit := time.Duration(r.MaxTTFTP95Ms) * time.Millisecond
		if th.ClientTTFTP95 == 0 || limit < th.ClientTTFTP95 {
			th.ClientTTFTP95 = limit
		}
	}
	return th
}

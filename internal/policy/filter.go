package policy

import (
	"maps"
	"slices"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
)

// Reason says why Filter dropped a candidate.
type Reason string

// Discard reasons, checked in this order; a candidate counts once, under the
// first reason that applies.
const (
	ReasonChannelNotAllowed  Reason = "channel_not_allowed"
	ReasonProviderNotAllowed Reason = "provider_not_allowed"
	ReasonNoCredentials      Reason = "no_credentials"
)

// Discards counts dropped candidates by reason.
type Discards map[Reason]int

// Filter returns a copy of p keeping only the candidates the YAML allows
// (rule 8, docs/politica.md):
//
//   - in every mode, the channel must be in routing.allowed_channels and the
//     provider in routing.allowed_providers;
//   - in route mode only, the channel needs credentials: providers.<provider>
//     with api_key and base_url for direct, providers.openrouter with api_key
//     for openrouter.
//
// Order is kept. Current is copied untouched and never merged into
// Candidates. p is not modified.
func Filter(p Policy, cfg config.Config) (Policy, Discards) {
	out := p
	out.Models = make(map[string]Model, len(p.Models))
	discards := Discards{}
	for id, m := range p.Models {
		fm := m
		fm.Aliases = cloneAliases(m.Aliases)
		fm.Current = slices.Clone(m.Current)
		fm.Candidates = nil
		for _, c := range m.Candidates {
			if r, drop := dropReason(c, cfg); drop {
				discards[r]++
				continue
			}
			fm.Candidates = append(fm.Candidates, c)
		}
		out.Models[id] = fm
	}
	return out, discards
}

func dropReason(c Candidate, cfg config.Config) (Reason, bool) {
	if !slices.Contains(cfg.Routing.AllowedChannels, string(c.Channel)) {
		return ReasonChannelNotAllowed, true
	}
	if !slices.Contains(cfg.Routing.AllowedProviders, c.Provider) {
		return ReasonProviderNotAllowed, true
	}
	if cfg.Mode == config.ModeRoute && !hasCredentials(c, cfg.Providers) {
		return ReasonNoCredentials, true
	}
	return "", false
}

func hasCredentials(c Candidate, ps map[string]config.ProviderConfig) bool {
	switch c.Channel {
	case ChannelDirect:
		pc, ok := ps[c.Provider]
		return ok && pc.APIKey != "" && pc.BaseURL != ""
	case ChannelOpenRouter:
		pc, ok := ps[config.ProviderOpenRouter]
		return ok && pc.APIKey != ""
	}
	return false
}

func cloneAliases(a map[string][]string) map[string][]string {
	if a == nil {
		return nil
	}
	out := make(map[string][]string, len(a))
	for k, v := range maps.All(a) {
		out[k] = slices.Clone(v)
	}
	return out
}

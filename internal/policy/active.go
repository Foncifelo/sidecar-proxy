package policy

import (
	"slices"
	"sync/atomic"
	"time"
)

// Active is a published policy: already filtered against the YAML (rule 8).
// It is immutable once stored in a Holder; its maps and slices may be shared
// with later Active values and must be treated as read-only.
type Active struct {
	// Policy is the filtered document.
	Policy Policy
	// Discards counts the candidates Filter removed.
	Discards Discards
	// ETag is the validator sent back as If-None-Match.
	ETag string
	// ValidatedAt is the last valid answer (200 or 304) from the control
	// plane for this policy; Age is measured from it (docs/politica.md).
	ValidatedAt time.Time

	aliases map[rawAlias]string // (provider, raw model) → model id
	raw     []byte              // unfiltered document, rewritten to the cache on 304
}

type rawAlias struct{ provider, raw string }

// newActive builds an Active from a filtered policy and indexes its aliases.
// When two models claim the same alias, the smallest model id wins so the
// result does not depend on map iteration order.
func newActive(p Policy, d Discards, etag string, validatedAt time.Time) *Active {
	idx := make(map[rawAlias]string)
	for id, m := range p.Models {
		for provider, raws := range m.Aliases {
			for _, raw := range raws {
				k := rawAlias{provider, raw}
				if prev, ok := idx[k]; !ok || id < prev {
					idx[k] = id
				}
			}
		}
	}
	return &Active{Policy: p, Discards: d, ETag: etag, ValidatedAt: validatedAt, aliases: idx}
}

// renewed returns a copy of a validated again at t (a 304 from the control).
func (a *Active) renewed(t time.Time) *Active {
	c := *a
	c.ValidatedAt = t
	return &c
}

// Age is the time since the control plane last validated this policy; never
// negative.
func (a *Active) Age(now time.Time) time.Duration {
	return max(now.Sub(a.ValidatedAt), 0)
}

// Version is the policy version, as reported in events and heartbeats.
func (a *Active) Version() string { return a.Policy.Version }

// Model returns the entry of a canonical model id. A missing model means
// unknown_model.
func (a *Active) Model(id string) (Model, bool) {
	m, ok := a.Policy.Models[id]
	return m, ok
}

// Resolve maps the raw model the agent sent to its upstream provider into
// the canonical model, through Model.Aliases[provider] (docs/politica.md).
func (a *Active) Resolve(provider, rawModel string) (string, Model, bool) {
	id, ok := a.aliases[rawAlias{provider, rawModel}]
	if !ok {
		return "", Model{}, false
	}
	m, ok := a.Policy.Models[id]
	return id, m, ok
}

// CurrentFor returns the reference endpoint of a direct (provider, raw model)
// pair. It is never a routing candidate.
func (m Model) CurrentFor(provider, rawModel string) (Current, bool) {
	i := slices.IndexFunc(m.Current, func(c Current) bool {
		return c.Provider == provider && c.RawModel == rawModel
	})
	if i < 0 {
		return Current{}, false
	}
	return m.Current[i], true
}

// Holder publishes the active policy to the request hot path: Load is a
// single atomic read, with no locks.
type Holder struct {
	p atomic.Pointer[Active]
}

// Load returns the active policy, or nil when there is none.
func (h *Holder) Load() *Active { return h.p.Load() }

// Store publishes a. Callers must not modify a afterwards.
func (h *Holder) Store(a *Active) { h.p.Store(a) }

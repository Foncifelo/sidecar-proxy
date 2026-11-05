package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/logging"
	"github.com/Foncifelo/sidecar-proxy/internal/policy"
)

// Everything in these policies is fictitious: model ids, endpoints,
// providers, prices (microdollars) and latencies are invented for the tests.
const (
	fictModel   = "fict-model"
	fictVersion = "p_fict_shadow"
	curEndpoint = "fictitious-provider/fict-model@fp8/standard/global/direct"
	candEP      = "fict-cheap/fict-model@fp8/standard/global/direct"
	curPriceID  = "100"
	candPriceID = "200"
)

// usageBody answers 1000 input and 500 output tokens: on the current price
// (1 $/M in, 2 $/M out) that is 2000 µ$; on the cheap one (0.5, 1) 1000 µ$.
const usageBody = `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"fictitious"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`

func fictPolicy() policy.Policy {
	return policy.Policy{
		SchemaVersion: policy.SchemaVersion,
		Version:       fictVersion,
		ComputedAt:    time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC),
		Models: map[string]policy.Model{fictModel: {
			Aliases:      map[string][]string{"fictitious-provider": {agentModel}},
			RefUsage:     policy.RefUsage{TokensIn: 1000, TokensOut: 100},
			RouteEnabled: true,
			Current: []policy.Current{{
				Provider: "fictitious-provider", RawModel: agentModel, EndpointID: curEndpoint, Quantization: "fp8",
				Price: policy.Price{PriceID: curPriceID, InputPerM: 1_000_000, OutputPerM: 2_000_000},
			}},
			Candidates: []policy.Candidate{{
				EndpointID: candEP, Provider: "fict-cheap", Channel: policy.ChannelDirect,
				ProviderModelName: "fict/model", Quantization: "fp8",
				Caps:          policy.Caps{Tools: true, N: 1},
				ContextLength: 131072,
				Price:         policy.Price{PriceID: candPriceID, InputPerM: 500_000, OutputPerM: 1_000_000},
				Stats:         policy.Stats{Uptime1d: 0.999, P90TTFTMs: 300, TPSP50: 80, Source: policy.SourceProbe},
			}},
		}},
	}
}

// activePolicy publishes p through the disk cache, as the sidecar does at
// start, without any network.
func activePolicy(t testing.TB, p policy.Policy) *policy.Active {
	t.Helper()
	return activePolicyFor(t, p, config.Routing{
		AllowedChannels: []string{"direct", "openrouter"}, AllowedProviders: []string{"fict-cheap"},
	})
}

// activePolicyFor is activePolicy with the YAML routing the refresher filters
// the policy against.
func activePolicyFor(t testing.TB, p policy.Policy, routing config.Routing) *policy.Active {
	t.Helper()
	doc, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := policy.SaveCache(path, policy.Cached{ETag: `"1:` + p.Version + `"`, ValidatedAt: time.Now(), Document: doc}); err != nil {
		t.Fatal(err)
	}
	r, err := policy.NewRefresher(policy.RefresherConfig{
		BaseURL: "http://127.0.0.1:1", APIKey: logging.Secret("sk_test_fictitious"), CachePath: path,
		Config: config.Config{Mode: config.ModeShadow, Routing: routing},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.LoadCached(); err != nil {
		t.Fatal(err)
	}
	a := r.Current()
	if a == nil {
		t.Fatal("no policy published from the cache")
	}
	return a
}

func withPolicy(a *policy.Active) func(*Options) {
	return func(o *Options) { o.Policy = func() *policy.Active { return a } }
}

// Raw_model resolves to model_id through the aliases of
// upstream.provider and the current endpoint fills endpoint_used.
func TestShadowPolicyResolves(t *testing.T) {
	up := newUpstream(t, jsonServe(http.StatusOK, usageBody))
	hs := newHarness(t, up.URL, withPolicy(activePolicy(t, fictPolicy())))
	resp, _ := hs.do(t, http.MethodPost, chatPath, chatBody(false, false), nil)
	ev := hs.sink.wait(t)
	checks := []struct {
		name string
		ok   bool
	}{
		{"model_id", ev.ModelID == fictModel},
		{"endpoint_used", ev.EndpointUsed == curEndpoint},
		{"policy_version", ev.PolicyVersion == fictVersion},
		{"attempt endpoint", len(ev.Attempts) == 1 && ev.Attempts[0].EndpointID == curEndpoint},
		{"endpoint header", resp.Header.Get(HeaderEndpoint) == curEndpoint},
		{"provider header", resp.Header.Get(HeaderProvider) == "fictitious-provider"},
		{"raw_model kept", ev.RawModel == agentModel},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s: event %+v", c.name, ev)
		}
	}
}

// The decision runs with ref_usage before forwarding, its reason
// is both X-Sidecar-Decision and decision_reason, and the real usage prices
// cost_actual and (through savings.Simulated) cost_best.
func TestShadowPolicyDecision(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*policy.Policy)
		opts       func(*Options)
		reason     string
		best       string
		used       string
		costActual events.Micros
		costBest   events.Micros
		idActual   string
		idBest     string
	}{
		{name: "cheaper candidate", reason: "cheaper_endpoint", best: candEP, used: curEndpoint,
			costActual: 2000, costBest: 1000, idActual: curPriceID, idBest: candPriceID},
		{name: "current is cheapest", mutate: func(p *policy.Policy) {
			m := p.Models[fictModel]
			m.Candidates[0].Price.InputPerM, m.Candidates[0].Price.OutputPerM = 3_000_000, 6_000_000
		}, reason: "current_is_cheapest", best: curEndpoint, used: curEndpoint,
			costActual: 2000, costBest: 2000, idActual: curPriceID, idBest: curPriceID},
		{name: "candidate lacks tools", mutate: func(p *policy.Policy) {
			p.Models[fictModel].Candidates[0].Caps.Tools = false
		}, reason: "no_candidate_passes", best: curEndpoint, used: curEndpoint,
			costActual: 2000, costBest: 2000, idActual: curPriceID, idBest: curPriceID},
		{name: "no current entry", mutate: func(p *policy.Policy) {
			m := p.Models[fictModel]
			m.Current = nil
			p.Models[fictModel] = m
		}, reason: "current_price_error", best: UpstreamEndpointID, used: UpstreamEndpointID},
		{name: "yaml uptime removes candidate", opts: func(o *Options) { o.MinUptime1d = 0.9999 },
			reason: "no_candidate_passes", best: curEndpoint, used: curEndpoint,
			costActual: 2000, costBest: 2000, idActual: curPriceID, idBest: curPriceID},
		{name: "yaml ttft removes candidate", opts: func(o *Options) { o.MaxTTFTP95 = 200 * time.Millisecond },
			reason: "no_candidate_passes", best: curEndpoint, used: curEndpoint,
			costActual: 2000, costBest: 2000, idActual: curPriceID, idBest: curPriceID},
		{name: "client ttft removes candidate", mutate: func(p *policy.Policy) {
			m := p.Models[fictModel]
			m.ClientTTFTP95Ms = 250
			p.Models[fictModel] = m
		}, reason: "no_candidate_passes", best: curEndpoint, used: curEndpoint,
			costActual: 2000, costBest: 2000, idActual: curPriceID, idBest: curPriceID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := fictPolicy()
			if tt.mutate != nil {
				tt.mutate(&p)
			}
			up := newUpstream(t, jsonServe(http.StatusOK, usageBody))
			hs := newHarness(t, up.URL, func(o *Options) {
				withPolicy(activePolicy(t, p))(o)
				if tt.opts != nil {
					tt.opts(o)
				}
			})
			resp, _ := hs.do(t, http.MethodPost, chatPath, chatBody(false, false), nil)
			ev := hs.sink.wait(t)
			if got := resp.Header.Get(HeaderDecision); got != tt.reason || ev.DecisionReason != tt.reason {
				t.Errorf("decision header %q, event %q, want %q", got, ev.DecisionReason, tt.reason)
			}
			if ev.EndpointBest != tt.best || ev.EndpointUsed != tt.used {
				t.Errorf("endpoint best %q used %q, want %q %q", ev.EndpointBest, ev.EndpointUsed, tt.best, tt.used)
			}
			if ev.CostActual != tt.costActual || ev.CostBest != tt.costBest {
				t.Errorf("cost actual %d best %d, want %d %d", ev.CostActual, ev.CostBest, tt.costActual, tt.costBest)
			}
			if ev.PriceIDActual != tt.idActual || ev.PriceIDBest != tt.idBest {
				t.Errorf("price ids %q %q, want %q %q", ev.PriceIDActual, ev.PriceIDBest, tt.idActual, tt.idBest)
			}
			if len(ev.Attempts) != 1 || ev.Attempts[0].CostEst != ev.CostActual {
				t.Errorf("attempts %+v do not add up to cost_actual %d", ev.Attempts, ev.CostActual)
			}
			if ev.ModelID != fictModel || ev.PolicyVersion != fictVersion {
				t.Errorf("model_id %q policy_version %q", ev.ModelID, ev.PolicyVersion)
			}
		})
	}
}

// Shadow costs are capped at events.MaxAttemptCostMicros like route ones, so
// a huge (fictitious) usage never produces an event the control plane rejects:
// cost_actual, cost_best and the synthesized attempt's cost_est stay at or
// below the ceiling. At 1 µ$ per input token (current) and 0.5 (candidate),
// 150M input tokens price above it on the current endpoint only; 300M on both.
func TestShadowCostsAreCapped(t *testing.T) {
	const ceiling = events.MaxAttemptCostMicros
	tests := []struct {
		name       string
		promptTok  int64
		costActual events.Micros
		costBest   events.Micros
	}{
		{"actual above the ceiling", 150_000_000, ceiling, 75_000_500},
		{"actual and best above the ceiling", 300_000_000, ceiling, ceiling},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"fictitious"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":500,"total_tokens":%d}}`, tt.promptTok, tt.promptTok+500)
			up := newUpstream(t, jsonServe(http.StatusOK, body))
			hs := newHarness(t, up.URL, withPolicy(activePolicy(t, fictPolicy())))
			hs.do(t, http.MethodPost, chatPath, chatBody(false, false), nil)
			ev := hs.sink.wait(t)
			if ev.CostActual != tt.costActual || ev.CostBest != tt.costBest {
				t.Errorf("cost actual %d best %d, want %d %d", ev.CostActual, ev.CostBest, tt.costActual, tt.costBest)
			}
			if ev.PriceIDActual != curPriceID {
				t.Errorf("price_id_actual %q, want %q", ev.PriceIDActual, curPriceID)
			}
			if len(ev.Attempts) != 1 || ev.Attempts[0].CostEst != ev.CostActual {
				t.Errorf("attempts %+v do not add up to cost_actual %d", ev.Attempts, ev.CostActual)
			}
		})
	}
}

// Without a policy or with an unknown model the reason is
// no_policy / unknown_model as before, nothing is priced and the body reaches
// the upstream byte for byte.
func TestShadowPolicyFallbacks(t *testing.T) {
	unknown := fictPolicy()
	unknown.Models[fictModel].Aliases["fictitious-provider"] = []string{"fictitious/other-model"}
	tests := []struct {
		name    string
		opts    func(*Options)
		reason  string
		version string
	}{
		{"no policy source", nil, ReasonNoPolicy, ""},
		{"source without policy", func(o *Options) { o.Policy = func() *policy.Active { return nil } }, ReasonNoPolicy, ""},
		{"unknown model", withPolicy(activePolicy(t, unknown)), ReasonUnknownModel, fictVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := newUpstream(t, jsonServe(http.StatusOK, usageBody))
			hs := newHarness(t, up.URL, tt.opts)
			resp, _ := hs.do(t, http.MethodPost, chatPath, rawChatBody, nil)
			ev := hs.sink.wait(t)
			if got := up.last(t).Body; !bytes.Equal(got, []byte(rawChatBody)) {
				t.Errorf("upstream body = %q, want %q", got, rawChatBody)
			}
			if got := resp.Header.Get(HeaderDecision); got != tt.reason || ev.DecisionReason != tt.reason {
				t.Errorf("decision header %q, event %q, want %q", got, ev.DecisionReason, tt.reason)
			}
			if ev.PolicyVersion != tt.version || ev.ModelID != "" || ev.EndpointBest != "" {
				t.Errorf("policy_version %q model_id %q endpoint_best %q", ev.PolicyVersion, ev.ModelID, ev.EndpointBest)
			}
			if ev.EndpointUsed != UpstreamEndpointID || resp.Header.Get(HeaderEndpoint) != UpstreamEndpointID {
				t.Errorf("endpoint_used %q header %q", ev.EndpointUsed, resp.Header.Get(HeaderEndpoint))
			}
			if ev.CostActual != 0 || ev.CostBest != 0 || ev.PriceIDActual != "" || ev.PriceIDBest != "" {
				t.Errorf("unexpected cost: %+v", ev)
			}
		})
	}
}

// With a policy the body sent upstream is still byte for byte the agent's.
func TestShadowPolicyKeepsBody(t *testing.T) {
	up := newUpstream(t, jsonServe(http.StatusOK, usageBody))
	hs := newHarness(t, up.URL, withPolicy(activePolicy(t, fictPolicy())))
	resp, _ := hs.do(t, http.MethodPost, chatPath, rawChatBody, nil)
	_ = hs.sink.wait(t)
	if got := up.last(t).Body; !bytes.Equal(got, []byte(rawChatBody)) {
		t.Errorf("upstream body = %q, want %q", got, rawChatBody)
	}
	if got := resp.Header.Get(HeaderDecision); got != "cheaper_endpoint" {
		t.Errorf("decision = %q", got)
	}
}

// Passthrough mode never decides, even with a policy.
func TestPassthroughIgnoresPolicy(t *testing.T) {
	up := newUpstream(t, jsonServe(http.StatusOK, usageBody))
	hs := newHarness(t, up.URL, func(o *Options) {
		o.Mode = config.ModePassthrough
		withPolicy(activePolicy(t, fictPolicy()))(o)
	})
	resp, _ := hs.do(t, http.MethodPost, chatPath, chatBody(false, false), nil)
	if got := resp.Header.Get(HeaderDecision); got != ReasonPassthrough {
		t.Errorf("decision = %q", got)
	}
	if got := resp.Header.Get(HeaderEndpoint); got != UpstreamEndpointID {
		t.Errorf("endpoint = %q", got)
	}
}

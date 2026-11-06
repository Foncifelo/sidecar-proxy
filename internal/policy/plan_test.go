package policy

import (
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/decision"
	"github.com/Foncifelo/sidecar-proxy/internal/providers"
)

// Every provider, endpoint, URL, key, price and latency in this file is
// fictitious.
const (
	pModel   = "fict/model-a"
	pRaw     = "acme/model-a-raw"
	pCurID   = "acme/model-a@fp8/direct"
	pCheap   = "gamma/model-a@bf16/direct"
	pMid     = "beta/model-a@fp8/openrouter"
	pPricey  = "delta/model-a@fp16/direct"
	pWorseQ  = "gamma/model-a@int4/direct"
	pFP4     = "gamma/model-a@fp4/direct"
	pRogue   = "rogue/model-a@bf16/direct"
	pUpURL   = "https://upstream.acme.example/v1"
	pGammaU  = "https://gamma.example/v1"
	pDeltaU  = "https://delta.example/v1"
	pRogueOR = "rogue-or/model-a@bf16/openrouter"
)

func planConfig() config.Config {
	return config.Config{
		Mode:     config.ModeRoute,
		Upstream: config.Upstream{Provider: "acme", BaseURL: pUpURL, APIKey: "fict-up"},
		Routing: config.Routing{
			AllowedChannels:  []string{"direct", "openrouter"},
			AllowedProviders: []string{"acme", "beta", "gamma", "delta"},
		},
		Providers: map[string]config.ProviderConfig{
			"acme":       {APIKey: "fict-acme", BaseURL: "https://acme.example/v1"},
			"gamma":      {APIKey: "fict-gamma", BaseURL: pGammaU, AuthHeader: "X-Api-Key"},
			"delta":      {APIKey: "fict-delta", BaseURL: pDeltaU},
			"openrouter": {APIKey: "fict-or"},
		},
	}
}

func planCandidate(id, provider string, ch Channel, q decision.Quantization, inPerM int64) Candidate {
	return Candidate{
		EndpointID: id, Provider: provider, Channel: ch, ProviderModelName: provider + "-name",
		Quantization: q, Caps: Caps{Tools: true, N: 1}, ContextLength: 32000,
		Price: Price{PriceID: "1", InputPerM: inPerM * 1000, OutputPerM: inPerM * 1000},
		Stats: Stats{Uptime1d: 0.999, P90TTFTMs: 300, TPSP50: 80, Source: SourceProbe},
	}
}

// planPolicy lists the candidates out of order on purpose: Plan must follow
// Rank, not the document order. The current endpoint is also a candidate.
func planPolicy() Policy {
	return Policy{
		SchemaVersion: SchemaVersion, Version: "v-fict",
		Models: map[string]Model{pModel: {
			Aliases:      map[string][]string{"acme": {pRaw}},
			RefUsage:     RefUsage{TokensIn: 1000, TokensOut: 1000},
			RouteEnabled: true,
			Current: []Current{{
				Provider: "acme", RawModel: pRaw, EndpointID: pCurID, Quantization: decision.QuantFP8,
				Price: Price{PriceID: "9", InputPerM: 900, OutputPerM: 900},
			}},
			Candidates: []Candidate{
				planCandidate(pPricey, "delta", ChannelDirect, decision.QuantFP16, 500),
				planCandidate(pCurID, "acme", ChannelDirect, decision.QuantFP8, 50),
				planCandidate(pMid, "beta", ChannelOpenRouter, decision.QuantFP8, 200),
				planCandidate(pWorseQ, "gamma", ChannelDirect, decision.QuantInt4, 10),
				planCandidate(pCheap, "gamma", ChannelDirect, decision.QuantBF16, 100),
			},
		}},
	}
}

func active(p Policy) *Active { return newActive(p, nil, "", time.Time{}) }

type openSet map[string]bool

func (o openSet) IsOpen(id string) bool { return o[id] }

func planIDs(eps []providers.Endpoint) []string {
	out := make([]string, len(eps))
	for i, e := range eps {
		out[i] = e.ID
	}
	return out
}

func withModel(p Policy, f func(*Model)) Policy {
	m := p.Models[pModel]
	f(&m)
	p.Models = map[string]Model{pModel: m}
	return p
}

func TestPlan(t *testing.T) {
	tools := Request{RawModel: pRaw, Needs: decision.Requirements{Tools: true}}
	tests := []struct {
		name   string
		a      *Active
		req    Request
		cfg    func(*config.Config)
		br     Breakers
		want   []string
		reason PlanReason
	}{
		{
			name: "cheapest first, capped at MaxAttempts with upstream last; current and worse quantization skipped",
			a:    active(planPolicy()), req: tools,
			want: []string{pCheap, pMid, pCurID}, reason: PlanRouted,
		},
		{
			name: "fp4 candidate never replaces an fp8 current",
			a: active(withModel(planPolicy(), func(m *Model) {
				m.Candidates = append(m.Candidates, planCandidate(pFP4, "gamma", ChannelDirect, decision.QuantFP4, 5))
			})),
			req: tools, want: []string{pCheap, pMid, pCurID}, reason: PlanRouted,
		},
		{
			name: "fp4 current may move to cheaper fp4 or int4",
			a: active(withModel(planPolicy(), func(m *Model) {
				m.Current = []Current{{
					Provider: "acme", RawModel: pRaw, EndpointID: pCurID, Quantization: decision.QuantFP4,
					Price: Price{PriceID: "9", InputPerM: 900, OutputPerM: 900},
				}}
				m.Candidates = append(m.Candidates, planCandidate(pFP4, "gamma", ChannelDirect, decision.QuantFP4, 5))
			})),
			req: tools, want: []string{pFP4, pWorseQ, pCurID}, reason: PlanRouted,
		},
		{
			name: "open breaker excludes", a: active(planPolicy()), req: tools, br: openSet{pCheap: true},
			want: []string{pMid, pPricey, pCurID}, reason: PlanRouted,
		},
		{
			name: "open breaker on the upstream does not remove it", a: active(planPolicy()), req: tools,
			br:   openSet{pCheap: true, pMid: true, pPricey: true, pCurID: true},
			want: []string{pCurID}, reason: PlanNoCandidate,
		},
		{
			name: "request needs drop candidates", a: active(planPolicy()),
			req:  Request{RawModel: pRaw, Needs: decision.Requirements{Vision: true}},
			want: []string{pCurID}, reason: PlanNoCandidate,
		},
		{
			name: "YAML min uptime only tightens", a: active(planPolicy()), req: tools,
			cfg:  func(c *config.Config) { c.Routing.MinUptime1d = 0.9999 },
			want: []string{pCurID}, reason: PlanNoCandidate,
		},
		{
			name: "YAML max ttft only tightens", a: active(planPolicy()), req: tools,
			cfg:  func(c *config.Config) { c.Routing.MaxTTFTP95Ms = 200 },
			want: []string{pCurID}, reason: PlanNoCandidate,
		},
		{
			name: "candidate without YAML credentials is skipped", a: active(planPolicy()), req: tools,
			cfg:  func(c *config.Config) { delete(c.Providers, "gamma") },
			want: []string{pMid, pPricey, pCurID}, reason: PlanRouted,
		},
		{
			name: "no policy", a: nil, req: tools,
			want: []string{UpstreamEndpointID}, reason: PlanNoPolicy,
		},
		{
			name: "unknown model", a: active(planPolicy()), req: Request{RawModel: "acme/other"},
			want: []string{UpstreamEndpointID}, reason: PlanUnknownModel,
		},
		{
			name: "route disabled", a: active(withModel(planPolicy(), func(m *Model) { m.RouteEnabled = false })),
			req: tools, want: []string{pCurID}, reason: PlanRouteDisabled,
		},
		{
			name: "no current reference", a: active(withModel(planPolicy(), func(m *Model) { m.Current = nil })),
			req: tools, want: []string{UpstreamEndpointID}, reason: PlanNoCurrent,
		},
		{
			name: "no candidates", a: active(withModel(planPolicy(), func(m *Model) { m.Candidates = nil })),
			req: tools, want: []string{pCurID}, reason: PlanNoCandidate,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := planConfig()
			if tt.cfg != nil {
				tt.cfg(&cfg)
			}
			got, reason := Plan(tt.a, tt.req, cfg, tt.br)
			if !reflect.DeepEqual(planIDs(got), tt.want) || reason != tt.reason {
				t.Fatalf("Plan = %v, %q; want %v, %q", planIDs(got), reason, tt.want, tt.reason)
			}
			if len(got) > MaxAttempts {
				t.Fatalf("plan has %d attempts, max %d", len(got), MaxAttempts)
			}
			assertUpstreamLast(t, got, cfg)
		})
	}
}

func assertUpstreamLast(t *testing.T, got []providers.Endpoint, cfg config.Config) {
	t.Helper()
	up := got[len(got)-1]
	if up.BaseURL != pUpURL || up.APIKey != cfg.Upstream.APIKey || up.RewriteModel ||
		up.ProviderModelName != pRaw && up.ProviderModelName != "acme/other" || up.Channel != providers.ChannelDirect {
		t.Fatalf("last attempt is not the YAML upstream: %+v", up)
	}
}

func TestPlanEndpointsComeFromYAML(t *testing.T) {
	cfg := planConfig()
	got, _ := Plan(active(planPolicy()), Request{RawModel: pRaw}, cfg, nil)
	want := []providers.Endpoint{
		{
			ID: pCheap, Provider: "gamma", Channel: providers.ChannelDirect, BaseURL: pGammaU,
			ProviderModelName: "gamma-name", APIKey: "fict-gamma", AuthHeader: "X-Api-Key", RewriteModel: true,
		},
		{
			ID: pMid, Provider: "beta", Channel: providers.ChannelOpenRouter, BaseURL: config.DefaultOpenRouterBaseURL,
			ProviderModelName: "beta-name", APIKey: "fict-or", RewriteModel: true,
		},
		{
			ID: pCurID, Provider: "acme", Channel: providers.ChannelDirect, BaseURL: pUpURL,
			ProviderModelName: pRaw, APIKey: "fict-up",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Plan =\n%+v\nwant\n%+v", got, want)
	}

	cfg.Providers["openrouter"] = config.ProviderConfig{APIKey: "fict-or", BaseURL: "https://or-proxy.example/v1"}
	got, _ = Plan(active(planPolicy()), Request{RawModel: pRaw}, cfg, nil)
	if got[1].BaseURL != "https://or-proxy.example/v1" {
		t.Fatalf("openrouter base_url = %q, want the YAML one", got[1].BaseURL)
	}
}

func TestPlanOpenRouterUpstream(t *testing.T) {
	cfg := planConfig()
	cfg.Upstream = config.Upstream{Provider: "openrouter", BaseURL: config.DefaultOpenRouterBaseURL, APIKey: "fict-up"}
	got, reason := Plan(nil, Request{RawModel: pRaw}, cfg, nil)
	if reason != PlanNoPolicy || len(got) != 1 {
		t.Fatalf("Plan = %+v, %q", got, reason)
	}
	// The upstream body arrives intact (contract, guarantee 1): the generic
	// adapter, never a pin or a model rewrite.
	up := got[0]
	if up.Channel != providers.ChannelDirect || up.Provider != "openrouter" || up.RewriteModel {
		t.Fatalf("upstream endpoint = %+v", up)
	}
}

// TestPlanHostsOnlyFromYAML is rule 8: whatever the policy says, every host
// in the plan is one the YAML names. The policy here is not filtered and adds
// providers and channels the YAML does not allow or has no key for.
func TestPlanHostsOnlyFromYAML(t *testing.T) {
	p := withModel(planPolicy(), func(m *Model) {
		m.Candidates = append([]Candidate{
			planCandidate(pRogue, "rogue", ChannelDirect, decision.QuantBF16, 1),
			planCandidate(pRogueOR, "rogue", ChannelOpenRouter, decision.QuantBF16, 1),
			planCandidate("acme/x@bf16/smoke", "acme", "smoke-signal", decision.QuantBF16, 1),
		}, m.Candidates...)
	})
	cfgs := map[string]config.Config{"full": planConfig()}
	noOR := planConfig()
	noOR.Routing.AllowedChannels = []string{"direct"}
	cfgs["direct only"] = noOR
	rogueAllowed := planConfig()
	rogueAllowed.Routing.AllowedProviders = append(rogueAllowed.Routing.AllowedProviders, "rogue")
	cfgs["rogue allowed without key"] = rogueAllowed
	badURL := planConfig()
	badURL.Providers["delta"] = config.ProviderConfig{APIKey: "fict-delta", BaseURL: "http://delta.example/v1"}
	cfgs["plain http base_url"] = badURL
	loopOn := planConfig()
	loopOn.LoopbackHTTP = true
	loopOn.Providers["gamma"] = config.ProviderConfig{APIKey: "fict-gamma", BaseURL: "http://127.0.0.1:9001/v1"}
	loopOn.Providers["delta"] = config.ProviderConfig{APIKey: "fict-delta", BaseURL: "http://10.0.0.1:9002/v1"}
	cfgs["ADR 0011 on: loopback http and non-loopback http"] = loopOn
	loopOff := planConfig()
	loopOff.Providers["gamma"] = config.ProviderConfig{APIKey: "fict-gamma", BaseURL: "http://127.0.0.1:9001/v1"}
	cfgs["ADR 0011 off: loopback http"] = loopOff

	for name, cfg := range cfgs {
		t.Run(name, func(t *testing.T) {
			allowed := yamlHosts(t, cfg)
			for _, br := range []Breakers{nil, openSet{pCheap: true}, openSet{pCheap: true, pMid: true}} {
				got, _ := Plan(active(p), Request{RawModel: pRaw}, cfg, br)
				for _, ep := range got {
					u, err := url.Parse(ep.BaseURL)
					if err != nil || !allowed[u.Host] {
						t.Fatalf("endpoint %s has host %q not in the YAML", ep.ID, ep.BaseURL)
					}
					if !slices.Contains(cfg.Routing.AllowedProviders, ep.Provider) ||
						!slices.Contains(cfg.Routing.AllowedChannels, string(ep.Channel)) || ep.ID == pRogue {
						t.Fatalf("plan contains endpoint %s the YAML does not allow", ep.ID)
					}
				}
			}
		})
	}
}

func yamlHosts(t *testing.T, cfg config.Config) map[string]bool {
	t.Helper()
	hosts := map[string]bool{}
	add := func(raw string) {
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		// http only to the literal loopback hosts of ADR 0011, and only when
		// Load switched the exception on. Written out here on purpose rather
		// than calling cfg.BaseURLAllowed, the predicate under test.
		loop := u.Hostname() == "127.0.0.1" || u.Hostname() == "::1" || u.Hostname() == "localhost"
		if u.Scheme == "https" || (u.Scheme == "http" && cfg.LoopbackHTTP && loop) {
			hosts[u.Host] = true
		}
	}
	add(cfg.Upstream.BaseURL)
	add(config.DefaultOpenRouterBaseURL) // the only built-in base_url (ADR 0008)
	for _, pc := range cfg.Providers {
		add(pc.BaseURL)
	}
	return hosts
}

// TestPlanHTTPLoopback is ADR 0011's "Cómo se verifica" at the plan level:
// a candidate whose base_url is http reaches the plan only when its host is a
// literal loopback name and SIDECAR_ENV is exactly dev or staging. The flag is
// derived by config.Load from SIDECAR_ENV, as in the binary. Hosts and ports
// are fictitious.
func TestPlanHTTPLoopback(t *testing.T) {
	envs := []struct {
		name  string
		value *string // nil means SIDECAR_ENV is unset
		on    bool
	}{
		{"unset", nil, false},
		{"empty", strPtr(""), false},
		{"dev", strPtr("dev"), true},
		{"staging", strPtr("staging"), true},
		{"production", strPtr("production"), false},
		{"DEV", strPtr("DEV"), false},
	}
	urls := []struct {
		url            string
		loopback, safe bool // safe: accepted in every environment
	}{
		{"http://127.0.0.1:9001/v1", true, false},
		{"http://[::1]:9001/v1", true, false},
		{"http://localhost:9001/v1", true, false},
		{"http://10.0.0.1:9001/v1", false, false},
		{"http://example.com/v1", false, false},
		{"http://localhost.example.com/v1", false, false},
		{"http://127.0.0.2/v1", false, false},
		{"ftp://127.0.0.1/v1", false, false},
		{"https://gamma.example/v1", false, true},
		{"https://127.0.0.1/v1", false, true},
	}
	for _, e := range envs {
		on := loopbackFromEnv(t, e.value)
		if on != e.on {
			t.Fatalf("SIDECAR_ENV %s: Load set LoopbackHTTP=%v, want %v", e.name, on, e.on)
		}
		for _, u := range urls {
			t.Run(e.name+"/"+u.url, func(t *testing.T) {
				cfg := planConfig()
				cfg.LoopbackHTTP = on
				cfg.Providers["gamma"] = config.ProviderConfig{APIKey: "fict-gamma", BaseURL: u.url}
				got, _ := Plan(active(planPolicy()), Request{RawModel: pRaw}, cfg, nil)
				want := u.safe || (u.loopback && e.on)
				var found bool
				for _, ep := range got {
					if ep.ID == pCheap {
						found = true
						if ep.BaseURL != u.url {
							t.Errorf("base_url %q, want the YAML's %q", ep.BaseURL, u.url)
						}
					}
				}
				if found != want {
					t.Errorf("candidate in plan = %v, want %v (plan %v)", found, want, planIDs(got))
				}
				if found != cfg.BaseURLAllowed(u.url) {
					t.Errorf("plan and config disagree on %q", u.url)
				}
			})
		}
	}
}

// loopbackFromEnv loads a minimal fictitious YAML with SIDECAR_ENV set to
// *v (unset when nil) and returns the LoopbackHTTP that Load derived.
func loopbackFromEnv(t *testing.T, v *string) bool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fict.yaml")
	yamlText := "upstream:\n  provider: acme\n  base_url: " + pUpURL + "\n  api_key: fict-up\n"
	if err := os.WriteFile(path, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (string, bool) {
		if name == config.EnvVarSidecarEnv && v != nil {
			return *v, true
		}
		return "", false
	}
	cfg, err := config.Load(config.Options{Path: path, PathExplicit: true, LookupEnv: lookup})
	if err != nil {
		t.Fatal(err)
	}
	return cfg.LoopbackHTTP
}

func strPtr(s string) *string { return &s }

// TestPlanNeverRoutesCurrentOnly: an endpoint that is only in current never
// appears as a routed attempt (docs/politica.md).
func TestPlanNeverRoutesCurrentOnly(t *testing.T) {
	p := withModel(planPolicy(), func(m *Model) {
		m.Current[0].EndpointID = "acme/only-current@fp8/direct"
	})
	got, _ := Plan(active(p), Request{RawModel: pRaw}, planConfig(), nil)
	for _, ep := range got[:len(got)-1] {
		if ep.ID == "acme/only-current@fp8/direct" {
			t.Fatalf("current-only endpoint routed: %+v", got)
		}
	}
}

func TestPlanDoesNotMutateInputs(t *testing.T) {
	p := planPolicy()
	a := active(p)
	cfg := planConfig()
	req := Request{RawModel: pRaw, Needs: decision.Requirements{Tools: true}}
	wantP, wantCfg, wantReq := planPolicy(), planConfig(), req
	Plan(a, req, cfg, nil)
	if !reflect.DeepEqual(a.Policy, wantP) || !reflect.DeepEqual(cfg, wantCfg) || !reflect.DeepEqual(req, wantReq) {
		t.Fatal("Plan mutated its inputs")
	}
}

func TestThresholds(t *testing.T) {
	tests := []struct {
		name string
		m    Model
		r    config.Routing
		want decision.Thresholds
	}{
		{"defaults", Model{}, config.Routing{}, decision.Thresholds{MinUptime1d: decision.DefaultMinUptime1d}},
		{
			"model figures", Model{ClientTTFTP95Ms: 800, ClientTPSP50: 40}, config.Routing{MinUptime1d: 0.5},
			decision.Thresholds{MinUptime1d: decision.DefaultMinUptime1d, ClientTTFTP95: 800 * time.Millisecond, ClientTPSP50: 40},
		},
		{
			"YAML ttft tighter", Model{ClientTTFTP95Ms: 800}, config.Routing{MaxTTFTP95Ms: 500, MinUptime1d: 0.995},
			decision.Thresholds{MinUptime1d: 0.995, ClientTTFTP95: 500 * time.Millisecond},
		},
		{
			"YAML ttft looser is ignored", Model{ClientTTFTP95Ms: 800}, config.Routing{MaxTTFTP95Ms: 900},
			decision.Thresholds{MinUptime1d: decision.DefaultMinUptime1d, ClientTTFTP95: 800 * time.Millisecond},
		},
		{
			"YAML ttft without model data", Model{}, config.Routing{MaxTTFTP95Ms: 900},
			decision.Thresholds{MinUptime1d: decision.DefaultMinUptime1d, ClientTTFTP95: 900 * time.Millisecond},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := thresholds(tt.m, tt.r); got != tt.want {
				t.Fatalf("thresholds = %+v, want %+v", got, tt.want)
			}
		})
	}
}

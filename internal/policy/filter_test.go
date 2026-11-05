package policy

import (
	"reflect"
	"slices"
	"testing"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
)

// Fictitious providers and keys; see decode_test.go.
const (
	epBetaOR    = "beta/fict-model@fp8/standard/global/openrouter"
	epGammaBF16 = "gamma/fict-model@bf16/standard/global/direct"
	epGammaFP16 = "gamma/fict-model@fp16/standard/global/direct"
	epAcmeCur   = "acme/fict-model@fp8/standard/global/direct"
	fictBaseURL = "https://gamma.example/v1"
)

// testConfig returns a YAML config that allows everything in the example
// document, with full credentials, in the given mode.
func testConfig(t *testing.T, mode string) config.Config {
	t.Helper()
	return config.Config{
		Mode: config.Mode(mode),
		Routing: config.Routing{
			AllowedChannels:  []string{"direct", "openrouter"},
			AllowedProviders: []string{"acme", "beta", "gamma"},
		},
		Providers: map[string]config.ProviderConfig{
			"gamma":      {APIKey: "fict-gamma", BaseURL: fictBaseURL},
			"acme":       {APIKey: "fict-acme", BaseURL: "https://acme.example/v1"},
			"openrouter": {APIKey: "fict-or"},
		},
	}
}

func mustFilter(p Policy, cfg config.Config) Policy {
	out, _ := Filter(p, cfg)
	return out
}

func endpointIDs(cs []Candidate) []string {
	ids := make([]string, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.EndpointID)
	}
	return ids
}

func TestFilter(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		edit   func(c *config.Config)
		want   []string
		counts Discards
	}{
		{"route everything allowed keeps order", "route", nil,
			[]string{epBetaOR, epGammaBF16, epGammaFP16}, Discards{}},
		{"channel not allowed", "route", func(c *config.Config) { c.Routing.AllowedChannels = []string{"direct"} },
			[]string{epGammaBF16, epGammaFP16}, Discards{ReasonChannelNotAllowed: 1}},
		{"channel not allowed in shadow", "shadow", func(c *config.Config) { c.Routing.AllowedChannels = []string{"openrouter"} },
			[]string{epBetaOR}, Discards{ReasonChannelNotAllowed: 2}},
		{"provider not allowed", "route", func(c *config.Config) { c.Routing.AllowedProviders = []string{"beta"} },
			[]string{epBetaOR}, Discards{ReasonProviderNotAllowed: 2}},
		{"provider not allowed in shadow", "shadow", func(c *config.Config) { c.Routing.AllowedProviders = []string{"gamma"} },
			[]string{epGammaBF16, epGammaFP16}, Discards{ReasonProviderNotAllowed: 1}},
		{"empty allowlists drop all", "shadow", func(c *config.Config) { c.Routing = config.Routing{} },
			nil, Discards{ReasonChannelNotAllowed: 3}},
		{"channel counted before provider", "route", func(c *config.Config) {
			c.Routing.AllowedChannels = []string{"direct"}
			c.Routing.AllowedProviders = []string{"acme"}
		}, nil, Discards{ReasonChannelNotAllowed: 1, ReasonProviderNotAllowed: 2}},
		{"direct without key in route", "route", func(c *config.Config) {
			c.Providers["gamma"] = config.ProviderConfig{BaseURL: fictBaseURL}
		}, []string{epBetaOR}, Discards{ReasonNoCredentials: 2}},
		{"direct without base_url in route", "route", func(c *config.Config) {
			c.Providers["gamma"] = config.ProviderConfig{APIKey: "fict-gamma"}
		}, []string{epBetaOR}, Discards{ReasonNoCredentials: 2}},
		{"direct provider absent in route", "route", func(c *config.Config) { delete(c.Providers, "gamma") },
			[]string{epBetaOR}, Discards{ReasonNoCredentials: 2}},
		{"openrouter without openrouter key in route", "route", func(c *config.Config) {
			delete(c.Providers, "openrouter")
			c.Providers["beta"] = config.ProviderConfig{APIKey: "fict-beta", BaseURL: "https://beta.example/v1"}
		}, []string{epGammaBF16, epGammaFP16}, Discards{ReasonNoCredentials: 1}},
		{"openrouter key without base_url is enough", "route", func(c *config.Config) {
			c.Providers["openrouter"] = config.ProviderConfig{APIKey: "fict-or"}
		}, []string{epBetaOR, epGammaBF16, epGammaFP16}, Discards{}},
		{"shadow needs no credentials", "shadow", func(c *config.Config) { c.Providers = nil },
			[]string{epBetaOR, epGammaBF16, epGammaFP16}, Discards{}},
		{"passthrough needs no credentials", "passthrough", func(c *config.Config) { c.Providers = nil },
			[]string{epBetaOR, epGammaBF16, epGammaFP16}, Discards{}},
	}
	in, err := Decode([]byte(exampleDoc))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t, tt.mode)
			if tt.edit != nil {
				tt.edit(&cfg)
			}
			out, discards := Filter(in, cfg)
			got := endpointIDs(out.Models["fict-model"].Candidates)
			if !slices.Equal(got, tt.want) {
				t.Errorf("candidates = %v, want %v", got, tt.want)
			}
			if !reflect.DeepEqual(discards, tt.counts) {
				t.Errorf("discards = %v, want %v", discards, tt.counts)
			}
			if !reflect.DeepEqual(out.Models["fict-model"].Current, in.Models["fict-model"].Current) {
				t.Errorf("current changed")
			}
			if out.Version != in.Version || out.SchemaVersion != in.SchemaVersion {
				t.Errorf("top level changed")
			}
		})
	}
}

// TestFilterNeverPromotesCurrent checks that an endpoint present only in
// current never becomes a routable candidate, whatever the YAML allows.
func TestFilterNeverPromotesCurrent(t *testing.T) {
	in, err := Decode([]byte(exampleDoc))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"route", "shadow", "passthrough"} {
		out := mustFilter(in, testConfig(t, mode))
		for id, m := range out.Models {
			if slices.Contains(endpointIDs(m.Candidates), epAcmeCur) {
				t.Errorf("%s: model %s routes to current-only endpoint %s", mode, id, epAcmeCur)
			}
			for _, c := range m.Candidates {
				if !slices.ContainsFunc(in.Models[id].Candidates, func(x Candidate) bool { return reflect.DeepEqual(x, c) }) {
					t.Errorf("%s: candidate %s not in input candidates", mode, c.EndpointID)
				}
			}
		}
	}
}

func TestFilterDoesNotMutateInput(t *testing.T) {
	in, err := Decode([]byte(exampleDoc))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := Decode([]byte(exampleDoc))
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, "route")
	cfg.Routing.AllowedProviders = []string{"gamma"}
	out := mustFilter(in, cfg)
	m := out.Models["fict-model"]
	m.Aliases["acme"][0] = "changed"
	m.Current[0].EndpointID = "changed"
	if len(m.Candidates) > 0 {
		m.Candidates[0].EndpointID = "changed"
	}
	if !reflect.DeepEqual(in, snapshot) {
		t.Fatal("Filter or edits on its result mutated the input policy")
	}
}

func TestFilterEmptyPolicy(t *testing.T) {
	out, discards := Filter(Policy{SchemaVersion: 1, Version: "p_empty"}, testConfig(t, "route"))
	if len(out.Models) != 0 || len(discards) != 0 {
		t.Fatalf("out = %+v, discards = %v", out, discards)
	}
}

package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// routeYAML is a minimal valid route config. URLs and keys are fictitious.
const routeYAML = "mode: route\n" + minimalYAML + `
control_plane:
  url: https://control.example.com
routing:
  allowed_channels: [direct]
  allowed_providers: [together]
providers:
  together:
    api_key: ${TOGETHER_API_KEY}
    base_url: https://api.together.xyz/v1
`

// routeWith returns a route config whose routing and providers sections are
// given by the caller.
func routeWith(routingAndProviders string) string {
	return "mode: route\n" + minimalYAML + "control_plane:\n  url: https://control.example.com\n" + routingAndProviders
}

func TestProviderValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		yaml    string
		wantErr []string // empty means valid
	}{
		// ADR 0008: base_url and auth_header.
		{"shadow direct provider without base_url", minimalYAML + "providers:\n  together:\n    api_key: k\n", nil},
		{"passthrough direct provider without base_url", "mode: passthrough\n" + minimalYAML + "providers:\n  together:\n    api_key: k\n", nil},
		{"http base_url fails", minimalYAML + "providers:\n  together:\n    base_url: http://api.together.xyz/v1\n", []string{"providers.together.base_url", "https"}},
		{"http loopback base_url fails too", minimalYAML + "providers:\n  vllm:\n    base_url: http://127.0.0.1:8000/v1\n", []string{"providers.vllm.base_url", "https"}},
		{"relative base_url fails", minimalYAML + "providers:\n  together:\n    base_url: /v1\n", []string{"providers.together.base_url", "absolute URL"}},
		{"auth_header valid", minimalYAML + "providers:\n  x:\n    base_url: https://x.example/v1\n    auth_header: X-Api-Key\n", nil},
		{"auth_header with space fails", minimalYAML + "providers:\n  x:\n    auth_header: 'X Api'\n", []string{"providers.x.auth_header"}},
		{"auth_header with colon fails", minimalYAML + "providers:\n  x:\n    auth_header: 'X-Key:'\n", []string{"providers.x.auth_header"}},
		// ADR 0008: http_referer and x_title.
		{"openrouter headers valid", minimalYAML + "providers:\n  openrouter:\n    http_referer: https://app.example.com/x\n    x_title: My Agent\n", nil},
		{"http_referer outside openrouter", minimalYAML + "providers:\n  together:\n    http_referer: https://app.example.com\n", []string{"providers.together.http_referer", "only allowed under providers.openrouter"}},
		{"x_title outside openrouter", minimalYAML + "providers:\n  together:\n    x_title: t\n", []string{"providers.together.x_title", "only allowed under providers.openrouter"}},
		{"http_referer http fails", minimalYAML + "providers:\n  openrouter:\n    http_referer: http://app.example.com\n", []string{"providers.openrouter.http_referer", "https"}},
		{"http_referer relative fails", minimalYAML + "providers:\n  openrouter:\n    http_referer: app.example.com\n", []string{"providers.openrouter.http_referer", "absolute URL"}},
		{"http_referer newline fails", minimalYAML + "providers:\n  openrouter:\n    http_referer: \"https://a.example\\r\\nX-Evil: 1\"\n", []string{"providers.openrouter.http_referer", "control characters"}},
		{"x_title newline fails", minimalYAML + "providers:\n  openrouter:\n    x_title: \"a\\nb\"\n", []string{"providers.openrouter.x_title", "control characters"}},
		{"x_title control char fails", minimalYAML + "providers:\n  openrouter:\n    x_title: \"a\\x00b\"\n", []string{"providers.openrouter.x_title", "control characters"}},
		{"x_title tab fails", minimalYAML + "providers:\n  openrouter:\n    x_title: \"a\\tb\"\n", []string{"providers.openrouter.x_title", "control characters"}},
		// Route mode requirements.
		{"route valid direct", routeYAML, nil},
		{"route valid via openrouter default base_url", routeWith("routing:\n  allowed_channels: [openrouter]\n  allowed_providers: [together]\nproviders:\n  openrouter:\n    api_key: k\n"), nil},
		{"route direct provider with key and no base_url", routeWith("routing:\n  allowed_channels: [openrouter]\n  allowed_providers: [together]\nproviders:\n  openrouter:\n    api_key: k\n  deepinfra:\n    api_key: k\n"), []string{"providers.deepinfra.base_url: required in route mode"}},
		{"route direct provider without key needs no base_url", routeWith("routing:\n  allowed_channels: [openrouter]\n  allowed_providers: [together]\nproviders:\n  openrouter:\n    api_key: k\n  deepinfra: {}\n"), nil},
		{"route without control_plane.url", "mode: route\n" + minimalYAML + "routing:\n  allowed_channels: [direct]\n  allowed_providers: [together]\nproviders:\n  together:\n    api_key: k\n    base_url: https://h/v1\n", []string{"control_plane.url: required"}},
		{"route with relative control_plane.url", "mode: route\n" + minimalYAML + "control_plane:\n  url: control\nrouting:\n  allowed_channels: [direct]\n  allowed_providers: [together]\nproviders:\n  together:\n    api_key: k\n    base_url: https://h/v1\n", []string{"control_plane.url: not an absolute URL"}},
		{"route with only upstream", "mode: route\n" + minimalYAML + "control_plane:\n  url: https://c.example\n", []string{"no provider in routing.allowed_providers is reachable"}},
		{"route provider not allowed", routeWith("routing:\n  allowed_channels: [direct]\n  allowed_providers: [deepinfra]\nproviders:\n  together:\n    api_key: k\n    base_url: https://h/v1\n"), []string{"no provider"}},
		{"route direct channel not allowed", routeWith("routing:\n  allowed_channels: [openrouter]\n  allowed_providers: [together]\nproviders:\n  together:\n    api_key: k\n    base_url: https://h/v1\n"), []string{"no provider"}},
		{"route openrouter without key", routeWith("routing:\n  allowed_channels: [openrouter]\n  allowed_providers: [together]\nproviders:\n  openrouter: {}\n"), []string{"no provider"}},
		{"route openrouter channel but no allowed provider", routeWith("routing:\n  allowed_channels: [openrouter]\nproviders:\n  openrouter:\n    api_key: k\n"), []string{"no provider"}},
		{"route only provider has http base_url", routeWith("routing:\n  allowed_channels: [direct]\n  allowed_providers: [together]\nproviders:\n  together:\n    api_key: k\n    base_url: http://h/v1\n"), []string{"providers.together.base_url", "no provider"}},
		{
			name: "every route error at once",
			yaml: "mode: route\n" + minimalYAML + "routing:\n  allowed_channels: [direct]\n  allowed_providers: [together]\n" +
				"providers:\n  together:\n    api_key: k\n  fireworks:\n    api_key: k\n    base_url: http://f/v1\n    x_title: t\n    auth_header: 'a b'\n  openrouter:\n    http_referer: ftp://r\n    x_title: \"a\\nb\"\n",
			wantErr: []string{
				"control_plane.url: required",
				"providers.together.base_url: required in route mode",
				"providers.fireworks.base_url", "providers.fireworks.x_title", "providers.fireworks.auth_header",
				"providers.openrouter.http_referer", "providers.openrouter.x_title",
				"no provider",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadString(t, tc.yaml, fakeEnv, Overrides{})
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want error, got nil")
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not mention %q:\n%v", w, err)
				}
			}
		})
	}
}

func TestProviderDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		yaml     string
		provider string
		want     string
	}{
		{"openrouter without base_url gets default", minimalYAML + "providers:\n  openrouter:\n    api_key: k\n", "openrouter", DefaultOpenRouterBaseURL},
		{"openrouter keeps explicit base_url", minimalYAML + "providers:\n  openrouter:\n    base_url: https://proxy.example/or/v1\n", "openrouter", "https://proxy.example/or/v1"},
		{"direct provider gets no default", minimalYAML + "providers:\n  deepinfra:\n    api_key: k\n", "deepinfra", ""},
		{"no inheritance from upstream.base_url", minimalYAML + "providers:\n  together:\n    api_key: k\n", "together", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := mustLoad(t, tc.yaml)
			if got := cfg.Providers[tc.provider].BaseURL; got != tc.want {
				t.Errorf("providers.%s.base_url = %q, want %q", tc.provider, got, tc.want)
			}
		})
	}
}

func TestProviderErrorsNeverQuoteValues(t *testing.T) {
	t.Parallel()
	// A value may come from ${VAR}; errors name the key, never the value.
	const marker = "fake-value-marker-9999"
	yamlText := minimalYAML + "providers:\n  together:\n    base_url: http://" + marker + "/v1\n" +
		"    auth_header: '" + marker + " x'\n    x_title: " + marker + "\n"
	_, err := loadString(t, yamlText, fakeEnv, Overrides{})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("error quotes a value:\n%v", err)
	}
}

func TestNewProviderFieldsKeepKeysMasked(t *testing.T) {
	t.Parallel()
	yamlText := minimalYAML + "providers:\n  openrouter:\n    api_key: ${OPENROUTER_API_KEY}\n    http_referer: https://app.example.com\n    x_title: t\n" +
		"  deepinfra:\n    api_key: ${DEEPINFRA_API_KEY}\n    base_url: https://h/v1\n    auth_header: X-Api-Key\n"
	cfg := mustLoad(t, yamlText)
	render := map[string]func(t *testing.T) string{
		"%v":   func(*testing.T) string { return fmt.Sprintf("%v", cfg.Providers) },
		"%+v":  func(*testing.T) string { return fmt.Sprintf("%+v", cfg.Providers) },
		"%#v":  func(*testing.T) string { return fmt.Sprintf("%#v", cfg.Providers) },
		"json": rendered(json.Marshal(cfg)),
		"yaml": rendered(yaml.Marshal(cfg)),
	}
	for name, fn := range render {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := fn(t)
			for _, secret := range []string{fakeEnv["OPENROUTER_API_KEY"], fakeEnv["DEEPINFRA_API_KEY"]} {
				if strings.Contains(out, secret) {
					t.Errorf("%s reveals a key:\n%s", name, out)
				}
			}
			for _, visible := range []string{masked, "https://h/v1", "X-Api-Key"} {
				if !strings.Contains(out, visible) {
					t.Errorf("%s output lacks %q:\n%s", name, visible, out)
				}
			}
		})
	}
}

func mustLoad(t *testing.T, content string) Config {
	t.Helper()
	cfg, err := loadString(t, content, fakeEnv, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

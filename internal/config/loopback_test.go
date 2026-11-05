package config

import (
	"slices"
	"strings"
	"testing"
)

// TestHTTPLoopbackException is the table of ADR 0011, "Cómo se verifica".
// Hosts and ports are fictitious.
func TestHTTPLoopbackException(t *testing.T) {
	t.Parallel()
	envs := []struct {
		name  string
		value *string // nil means SIDECAR_ENV is unset
	}{
		{"unset", nil},
		{"empty", ptr("")},
		{"dev", ptr("dev")},
		{"staging", ptr("staging")},
		{"production", ptr("production")},
		{"DEV", ptr("DEV")},
		{"dev with space", ptr(" dev")},
	}
	allowedEnv := map[string]bool{"dev": true, "staging": true}
	urls := []struct {
		url      string
		loopback bool // accepted over http in dev/staging
		https    bool // accepted always
	}{
		{"http://127.0.0.1:9001/v1", true, false},
		{"http://127.0.0.1/v1", true, false},
		{"http://[::1]:9001/v1", true, false},
		{"http://localhost:9001/v1", true, false},
		{"http://10.0.0.1:9001/v1", false, false},
		{"http://example.com/v1", false, false},
		{"http://localhost.example.com/v1", false, false},
		{"http://127.0.0.2/v1", false, false},
		{"http://LOCALHOST/v1", false, false},
		{"ftp://127.0.0.1/v1", false, false},
		{"https://api.example.com/v1", false, true},
		{"https://127.0.0.1/v1", false, true},
	}
	for _, e := range envs {
		for _, u := range urls {
			t.Run(e.name+"/"+u.url, func(t *testing.T) {
				t.Parallel()
				env := map[string]string{"TOGETHER_API_KEY": "fake-key"}
				if e.value != nil {
					env[EnvVarSidecarEnv] = *e.value
				}
				yamlText := minimalYAML + "providers:\n  demo:\n    base_url: " + u.url + "\n"
				cfg, err := loadString(t, yamlText, env, Overrides{})
				want := u.https || (u.loopback && e.value != nil && allowedEnv[*e.value])
				if want {
					if err != nil {
						t.Fatalf("want accepted, got %v", err)
					}
					if got := cfg.Providers["demo"].BaseURL; got != u.url {
						t.Errorf("base_url = %q, want %q", got, u.url)
					}
					if !cfg.BaseURLAllowed(u.url) {
						t.Errorf("BaseURLAllowed(%q) = false on a config that accepted it", u.url)
					}
					return
				}
				if err == nil {
					t.Fatal("want rejected, got nil error")
				}
				for _, s := range []string{"providers.demo.base_url", "https"} {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("error %q does not mention %q", err, s)
					}
				}
			})
		}
	}
}

// TestHTTPLoopbackRoute checks that a loopback http provider counts as
// reachable in route mode only under the exception.
func TestHTTPLoopbackRoute(t *testing.T) {
	t.Parallel()
	yamlText := routeWith("routing:\n  allowed_channels: [direct]\n  allowed_providers: [fake]\n" +
		"providers:\n  fake:\n    api_key: k\n    base_url: http://127.0.0.1:9001/v1\n")
	cases := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{"dev", sidecarEnv("dev"), false},
		{"staging", sidecarEnv("staging"), false},
		{"unset", map[string]string{"TOGETHER_API_KEY": "fake-key"}, true},
		{"production", sidecarEnv("production"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadString(t, yamlText, tc.env, Overrides{})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestHTTPLoopbackProviders(t *testing.T) {
	t.Parallel()
	yamlText := minimalYAML + "providers:\n" +
		"  zeta:\n    base_url: http://localhost:9002/v1\n" +
		"  alpha:\n    base_url: http://[::1]:9001/v1\n" +
		"  together:\n    base_url: https://api.together.xyz/v1\n" +
		"  openrouter:\n    api_key: k\n"
	cases := []struct {
		name string
		yaml string
		env  map[string]string
		want []string
	}{
		{"exception in use", yamlText, sidecarEnv("staging"), []string{"alpha", "zeta"}},
		{"https only", minimalYAML + "providers:\n  together:\n    base_url: https://h/v1\n", sidecarEnv("dev"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := loadString(t, tc.yaml, tc.env, Overrides{})
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.HTTPLoopbackProviders(); !slices.Equal(got, tc.want) {
				t.Errorf("HTTPLoopbackProviders() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLoadSetsLoopbackHTTP: Load fixes Config.LoopbackHTTP from SIDECAR_ENV
// alone, with and without a file, and BaseURLAllowed follows it.
func TestLoadSetsLoopbackHTTP(t *testing.T) {
	t.Parallel()
	const loop = "http://127.0.0.1:9001/v1"
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"dev", sidecarEnv("dev"), true},
		{"staging", sidecarEnv("staging"), true},
		{"unset", map[string]string{"TOGETHER_API_KEY": "fake-key"}, false},
		{"empty", sidecarEnv(""), false},
		{"production", sidecarEnv("production"), false},
		{"DEV", sidecarEnv("DEV"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := mustLoadString(t, minimalYAML, tc.env, Overrides{})
			mode := ModeShadow
			flagsOnly, err := Load(Options{Path: t.TempDir() + "/missing.yaml", LookupEnv: lookupFrom(tc.env), Overrides: Overrides{Mode: &mode}})
			if err != nil {
				t.Fatal(err)
			}
			for name, c := range map[string]Config{"file": cfg, "flags only": flagsOnly} {
				if c.LoopbackHTTP != tc.want || c.BaseURLAllowed(loop) != tc.want {
					t.Errorf("%s: LoopbackHTTP=%v BaseURLAllowed=%v, want %v", name, c.LoopbackHTTP, c.BaseURLAllowed(loop), tc.want)
				}
				if !c.BaseURLAllowed("https://api.example.com/v1") || c.BaseURLAllowed("http://10.0.0.1/v1") {
					t.Errorf("%s: https must always pass and non-loopback http never", name)
				}
			}
		})
	}
}

// TestLoopbackHTTPNotFromYAML: the exception cannot be switched on from the
// file, only from SIDECAR_ENV.
func TestLoopbackHTTPNotFromYAML(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"loopback_http", "loopbackhttp", "LoopbackHTTP"} {
		_, err := loadString(t, minimalYAML+key+": true\n", map[string]string{"TOGETHER_API_KEY": "fake-key"}, Overrides{})
		if err == nil {
			t.Errorf("%s in the YAML was accepted", key)
		}
	}
	if (Config{}).BaseURLAllowed("http://127.0.0.1:9001/v1") {
		t.Error("a Config built by hand must allow https only")
	}
}

// sidecarEnv is a fictitious environment with SIDECAR_ENV set to v.
func sidecarEnv(v string) map[string]string {
	return map[string]string{"TOGETHER_API_KEY": "fake-key", EnvVarSidecarEnv: v}
}

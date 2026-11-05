package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Foncifelo/sidecar-proxy/internal/logging"
)

// All keys and tokens below are fictitious test values.
var fakeEnv = map[string]string{
	"TOGETHER_API_KEY":      "fake-together-key-1111",
	"SIDECAR_API_KEY":       "sk_live_fake_2222",
	"OPENROUTER_API_KEY":    "fake-openrouter-key-3333",
	"DEEPINFRA_API_KEY":     "fake-deepinfra-key-4444",
	"SIDECAR_INBOUND_TOKEN": "fake-inbound-token-5555",
}

func lookupFrom(env map[string]string) LookupFunc {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

const examplePath = "../../sidecar.example.yaml"

func TestLoadExample(t *testing.T) {
	t.Parallel()
	cfg, err := Load(Options{Path: examplePath, PathExplicit: true, LookupEnv: lookupFrom(fakeEnv)})
	if err != nil {
		t.Fatalf("Load(example): %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"mode", cfg.Mode, ModeShadow},
		{"listen", cfg.Listen, "127.0.0.1:8787"},
		{"metrics_listen", cfg.MetricsListen, "127.0.0.1:9797"},
		{"upstream.provider", cfg.Upstream.Provider, "together"},
		{"upstream.base_url", cfg.Upstream.BaseURL, "https://api.together.xyz/v1"},
		{"upstream.api_key", cfg.Upstream.APIKey.Reveal(), "fake-together-key-1111"},
		{"control_plane.api_key", cfg.ControlPlane.APIKey.Reveal(), "sk_live_fake_2222"},
		{"control_plane.policy_refresh", cfg.ControlPlane.PolicyRefresh.Std(), 60 * time.Second},
		{"control_plane.queue_max_mb", cfg.ControlPlane.QueueMaxMB, 256},
		{"limits.max_body_mb", cfg.Limits.MaxBodyMB, 10},
		{"limits.max_inflight", cfg.Limits.MaxInflight, 256},
		{"limits.first_byte_timeout", cfg.Limits.FirstByteTimeout.Std(), 2 * time.Second},
		{"limits.nonstream_first_byte_timeout", cfg.Limits.NonStreamFirstByteTimeout.Std(), 30 * time.Second},
		{"limits.ttft_p95_slack", cfg.Limits.TTFTP95Slack.Std(), 500 * time.Millisecond},
		{"limits.read_header_timeout", cfg.Limits.ReadHeaderTimeout.Std(), 10 * time.Second},
		{"limits.read_timeout", cfg.Limits.ReadTimeout.Std(), 60 * time.Second},
		{"limits.idle_timeout", cfg.Limits.IdleTimeout.Std(), 120 * time.Second},
		{"limits.stream_write_timeout", cfg.Limits.StreamWriteTimeout.Std(), 30 * time.Second},
		{"limits.stream_idle_timeout", cfg.Limits.StreamIdleTimeout.Std(), 120 * time.Second},
		{"routing.allowed_channels", strings.Join(cfg.Routing.AllowedChannels, ","), "openrouter,direct"},
		{"routing.max_ttft_p95_ms", cfg.Routing.MaxTTFTP95Ms, 1200},
		{"routing.min_uptime_1d", cfg.Routing.MinUptime1d, 0.99},
		{"providers.openrouter", cfg.Providers["openrouter"].APIKey.Reveal(), "fake-openrouter-key-3333"},
		{"providers.deepinfra", cfg.Providers["deepinfra"].APIKey.Reveal(), "fake-deepinfra-key-4444"},
		{"providers.openrouter.base_url (default)", cfg.Providers["openrouter"].BaseURL, DefaultOpenRouterBaseURL},
		{"providers.together.base_url", cfg.Providers["together"].BaseURL, "https://api.together.xyz/v1"},
		{"tags.header", cfg.Tags.Header, "X-Sidecar-Tags"},
		{"tags.allowed_keys", len(cfg.Tags.AllowedKeys), 3},
		{"inbound_token (commented out)", cfg.InboundToken.Reveal(), ""},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

const minimalYAML = `
upstream:
  provider: together
  base_url: https://api.together.xyz/v1
  api_key: ${TOGETHER_API_KEY}
`

func TestDefaults(t *testing.T) {
	t.Parallel()
	cfg := mustLoadString(t, minimalYAML, fakeEnv, Overrides{})
	checks := []struct {
		name      string
		got, want any
	}{
		{"mode", cfg.Mode, DefaultMode},
		{"listen", cfg.Listen, DefaultListen},
		{"metrics_listen", cfg.MetricsListen, DefaultMetricsListen},
		{"max_body_mb", cfg.Limits.MaxBodyMB, 10},
		{"max_inflight", cfg.Limits.MaxInflight, 256},
		{"first_byte_timeout", cfg.Limits.FirstByteTimeout.Std(), 2 * time.Second},
		{"nonstream_first_byte_timeout (owner decision 2026-10-29)", cfg.Limits.NonStreamFirstByteTimeout.Std(), 30 * time.Second},
		{"ttft_p95_slack", cfg.Limits.TTFTP95Slack.Std(), 500 * time.Millisecond},
		{"read_header_timeout", cfg.Limits.ReadHeaderTimeout.Std(), DefaultReadHeaderTimeout},
		{"read_timeout", cfg.Limits.ReadTimeout.Std(), DefaultReadTimeout},
		{"idle_timeout", cfg.Limits.IdleTimeout.Std(), DefaultIdleTimeout},
		{"stream_write_timeout", cfg.Limits.StreamWriteTimeout.Std(), DefaultStreamWriteTimeout},
		{"stream_idle_timeout", cfg.Limits.StreamIdleTimeout.Std(), DefaultStreamIdleTimeout},
		{"policy_refresh", cfg.ControlPlane.PolicyRefresh.Std(), 60 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestExpansion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		yaml    string
		env     map[string]string
		check   func(Config) string // returns a failure message or ""
		wantErr []string
	}{
		{
			name: "string and int from env",
			yaml: minimalYAML + "limits:\n  max_inflight: ${MAX}\n",
			env:  map[string]string{"TOGETHER_API_KEY": "k", "MAX": "32"},
			check: func(c Config) string {
				if c.Limits.MaxInflight != 32 {
					return fmt.Sprintf("max_inflight = %d", c.Limits.MaxInflight)
				}
				return ""
			},
		},
		{
			name: "embedded reference",
			yaml: "upstream:\n  provider: together\n  base_url: https://${HOST}/v1\n",
			env:  map[string]string{"HOST": "api.together.xyz"},
			check: func(c Config) string {
				if c.Upstream.BaseURL != "https://api.together.xyz/v1" {
					return "base_url = " + c.Upstream.BaseURL
				}
				return ""
			},
		},
		{
			name: "secret that looks like yaml keywords stays a string",
			yaml: minimalYAML,
			env:  map[string]string{"TOGETHER_API_KEY": "true"},
			check: func(c Config) string {
				if c.Upstream.APIKey.Reveal() != "true" {
					return "api_key = " + c.Upstream.APIKey.Reveal()
				}
				return ""
			},
		},
		{
			name: "secret with special characters",
			yaml: minimalYAML,
			env:  map[string]string{"TOGETHER_API_KEY": "a: b # c"},
			check: func(c Config) string {
				if c.Upstream.APIKey.Reveal() != "a: b # c" {
					return "api_key = " + c.Upstream.APIKey.Reveal()
				}
				return ""
			},
		},
		{
			name: "set but empty is not missing",
			yaml: minimalYAML,
			env:  map[string]string{"TOGETHER_API_KEY": ""},
		},
		{
			name: "bare $VAR is literal",
			yaml: "upstream:\n  provider: together\n  base_url: https://h/v1\n  api_key: $NOPE\n",
			env:  map[string]string{},
			check: func(c Config) string {
				if c.Upstream.APIKey.Reveal() != "$NOPE" {
					return "api_key = " + c.Upstream.APIKey.Reveal()
				}
				return ""
			},
		},
		{
			name:    "missing variable names the key",
			yaml:    minimalYAML,
			env:     map[string]string{},
			wantErr: []string{"upstream.api_key", "TOGETHER_API_KEY"},
		},
		{
			name:    "all missing variables reported, including in lists and maps",
			yaml:    minimalYAML + "providers:\n  groq:\n    api_key: ${GROQ}\nrouting:\n  allowed_providers:\n    - ${P0}\n",
			env:     map[string]string{},
			wantErr: []string{"upstream.api_key", "providers.groq.api_key", "routing.allowed_providers[0]", "GROQ", "P0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := loadString(t, tc.yaml, tc.env, Overrides{})
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				var mv *MissingVarError
				if !errors.As(err, &mv) {
					t.Errorf("error is not a MissingVarError: %v", err)
				}
				for _, w := range tc.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q does not mention %q", err, w)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				if msg := tc.check(cfg); msg != "" {
					t.Error(msg)
				}
			}
		})
	}
}

func TestMissingVarErrorNeverHasValues(t *testing.T) {
	t.Parallel()
	env := map[string]string{"TOGETHER_API_KEY": "fake-present-secret-9999"}
	_, err := loadString(t, minimalYAML+"control_plane:\n  api_key: ${SIDECAR_API_KEY}\n", env, Overrides{})
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "fake-present-secret-9999") {
		t.Errorf("error leaks a secret: %v", err)
	}
}

func TestValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		yaml    string
		wantErr []string // empty means valid
	}{
		{"minimal is valid", minimalYAML, nil},
		{"route mode", routeYAML, nil},
		{"passthrough mode", "mode: passthrough\n" + minimalYAML, nil},
		{"bad mode", "mode: turbo\n" + minimalYAML, []string{"mode:"}},
		{"off-loopback without token", "listen: 0.0.0.0:8787\n" + minimalYAML, []string{"inbound_token is required"}},
		{"off-loopback with token", "listen: 0.0.0.0:8787\ninbound_token: ${SIDECAR_INBOUND_TOKEN}\n" + minimalYAML, nil},
		{"ipv6 loopback", "listen: '[::1]:8787'\n" + minimalYAML, nil},
		{"listen not host:port", "listen: nonsense\n" + minimalYAML, []string{"listen:"}},
		{"metrics_listen not host:port", "metrics_listen: nonsense\n" + minimalYAML, []string{"metrics_listen:"}},
		{"http to remote upstream", "upstream:\n  provider: together\n  base_url: http://api.together.xyz/v1\n", []string{"upstream.base_url", "https"}},
		{"http to localhost", "upstream:\n  provider: vllm\n  base_url: http://localhost:8000/v1\n", nil},
		{"http to 127.0.0.1", "upstream:\n  provider: vllm\n  base_url: http://127.0.0.1:8000/v1\n", nil},
		{"ftp upstream", "upstream:\n  provider: x\n  base_url: ftp://h/v1\n", []string{"scheme"}},
		{"relative upstream", "upstream:\n  provider: x\n  base_url: /v1\n", []string{"absolute URL"}},
		{"missing upstream", "mode: shadow\n", []string{"upstream.provider", "upstream.base_url: required"}},
		{"missing provider", "upstream:\n  base_url: https://h/v1\n", []string{"upstream.provider"}},
		{"bad duration", minimalYAML + "limits:\n  first_byte_timeout: soon\n", []string{"limits.first_byte_timeout"}},
		{"negative duration", minimalYAML + "limits:\n  ttft_p95_slack: -1s\n", []string{"limits.ttft_p95_slack", "positive"}},
		{"bad policy_refresh", minimalYAML + "control_plane:\n  policy_refresh: 1x\n", []string{"control_plane.policy_refresh"}},
		{"bad read_header_timeout", minimalYAML + "limits:\n  read_header_timeout: soon\n", []string{"limits.read_header_timeout"}},
		{"zero read_timeout", minimalYAML + "limits:\n  read_timeout: 0s\n", []string{"limits.read_timeout", "positive"}},
		{"bad idle_timeout", minimalYAML + "limits:\n  idle_timeout: x\n", []string{"limits.idle_timeout"}},
		{"negative stream_write_timeout", minimalYAML + "limits:\n  stream_write_timeout: -1s\n", []string{"limits.stream_write_timeout", "positive"}},
		{"bad stream_idle_timeout", minimalYAML + "limits:\n  stream_idle_timeout: later\n", []string{"limits.stream_idle_timeout"}},
		{"bad nonstream_first_byte_timeout", minimalYAML + "limits:\n  nonstream_first_byte_timeout: soon\n", []string{"limits.nonstream_first_byte_timeout"}},
		{"zero nonstream_first_byte_timeout", minimalYAML + "limits:\n  nonstream_first_byte_timeout: 0s\n", []string{"limits.nonstream_first_byte_timeout"}},
		{"negative nonstream_first_byte_timeout", minimalYAML + "limits:\n  nonstream_first_byte_timeout: -1s\n", []string{"limits.nonstream_first_byte_timeout"}},
		{"custom nonstream_first_byte_timeout", minimalYAML + "limits:\n  nonstream_first_byte_timeout: 45s\n", nil},
		{"custom server timeouts", minimalYAML + "limits:\n  read_header_timeout: 1s\n  read_timeout: 2s\n  idle_timeout: 3s\n  stream_write_timeout: 4s\n  stream_idle_timeout: 5s\n", nil},
		{"negative limits", minimalYAML + "limits:\n  max_body_mb: -1\n  max_inflight: -2\n", []string{"max_body_mb", "max_inflight"}},
		{
			name: "every error at once",
			yaml: "mode: turbo\nlisten: 0.0.0.0:8787\nupstream:\n  base_url: http://remote/v1\nlimits:\n  first_byte_timeout: x\n  ttft_p95_slack: y\n",
			wantErr: []string{
				"mode:", "inbound_token is required", "upstream.provider", "upstream.base_url",
				"limits.first_byte_timeout", "limits.ttft_p95_slack",
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

func TestParseErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, yaml, want string
	}{
		{"unknown key", minimalYAML + "typo_key: 1\n", "typo_key"},
		{"malformed yaml", "mode: [\n", "parse yaml"},
		{"duration not scalar", minimalYAML + "limits:\n  first_byte_timeout: [1]\n", "duration must be a scalar"},
		{"wrong type", minimalYAML + "limits:\n  max_inflight: lots\n", "decode yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadString(t, tc.yaml, fakeEnv, Overrides{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

// TestTypeErrorsNeverLeakValues expands fictitious secrets into non-string
// fields: the decode error must report the problem without the value or any
// prefix of it (yaml.v3 quotes up to seven characters).
func TestTypeErrorsNeverLeakValues(t *testing.T) {
	t.Parallel()
	const minPrefix = 3
	cases := []struct {
		name, yaml, secret, want string
	}{
		{"long secret into int", "limits:\n  max_inflight: ${TOK}\n", "Qz7-supersecretvalue", "into int"},
		{"short secret into int", "limits:\n  max_body_mb: ${TOK}\n", "Qz7wXk9", "into int"},
		{"secret into float", "routing:\n  min_uptime_1d: ${TOK}\n", "Qz7-floatsecret", "into float64"},
		{"secret into list", "routing:\n  allowed_channels: ${TOK}\n", "Qz7-listsecret", "into []string"},
		{"secret into map", "providers: ${TOK}\n", "Qz7-mapsecret", "into map"},
		{"secret with backquotes", "tags:\n  allowed_keys: ${TOK}\n", "Qz7`a`b`cdefghij", "into []string"},
		{"short secret into queue size", "control_plane:\n  queue_max_mb: ${TOK}\n", "Qz7mb", "into int"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{"TOK": tc.secret}
			for k, v := range fakeEnv {
				env[k] = v
			}
			_, err := loadString(t, minimalYAML+tc.yaml, env, Overrides{})
			if err == nil {
				t.Fatal("expected a decode error")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) || !strings.Contains(msg, "<redacted>") {
				t.Errorf("error = %q, want mention of %q and <redacted>", msg, tc.want)
			}
			for n := minPrefix; n <= len(tc.secret); n++ {
				if strings.Contains(msg, tc.secret[:n]) {
					t.Fatalf("error leaks %d-byte prefix of the secret: %q", n, msg)
				}
			}
		})
	}
}

func TestRedactQuoted(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"line 3: cannot unmarshal !!str `abc` into int", "line 3: cannot unmarshal !!str `<redacted>` into int"},
		{"line 3: cannot unmarshal !!str `a`b`c...` into int", "line 3: cannot unmarshal !!str `<redacted>` into int"},
		{"line 3: cannot unmarshal !!seq into int", "line 3: cannot unmarshal !!seq into int"},
		{"odd ` single", "odd ` single"},
	}
	for _, tc := range cases {
		if got := redactQuoted(tc.in); got != tc.want {
			t.Errorf("redactQuoted(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEmptyFileNeedsUpstream(t *testing.T) {
	t.Parallel()
	_, err := loadString(t, "", fakeEnv, Overrides{})
	if err == nil || !strings.Contains(err.Error(), "upstream.base_url") {
		t.Fatalf("error = %v, want upstream.base_url required", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestOverrides(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		yaml       string
		o          Overrides
		wantMode   Mode
		wantListen string
		wantErr    string
	}{
		{"none", routeYAML, Overrides{}, ModeRoute, DefaultListen, ""},
		{"mode wins", "mode: route\n" + minimalYAML, Overrides{Mode: ptr(ModePassthrough)}, ModePassthrough, DefaultListen, ""},
		{"listen wins", minimalYAML, Overrides{Listen: ptr("127.0.0.1:9000")}, ModeShadow, "127.0.0.1:9000", ""},
		{"listen off-loopback needs token", minimalYAML, Overrides{Listen: ptr("0.0.0.0:8787")}, "", "", "inbound_token is required"},
		{
			"token flag satisfies off-loopback", minimalYAML,
			Overrides{Listen: ptr("0.0.0.0:8787"), InboundToken: ptr(logging.Secret("fake-flag-token"))},
			ModeShadow, "0.0.0.0:8787", "",
		},
		{"bad mode flag is validated", minimalYAML, Overrides{Mode: ptr(Mode("turbo"))}, "", "", "mode:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := loadString(t, tc.yaml, fakeEnv, tc.o)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Mode != tc.wantMode || cfg.Listen != tc.wantListen {
				t.Errorf("mode, listen = %s, %s; want %s, %s", cfg.Mode, cfg.Listen, tc.wantMode, tc.wantListen)
			}
		})
	}
}

func TestMissingFile(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "sidecar.yaml")
	cases := []struct {
		name    string
		o       Options
		wantErr bool
		want    Mode
	}{
		{"default path, no flags", Options{Path: missing}, true, ""},
		{"default path, only listen", Options{Path: missing, Overrides: Overrides{Listen: ptr("127.0.0.1:1")}}, true, ""},
		{"explicit path, with mode", Options{Path: missing, PathExplicit: true, Overrides: Overrides{Mode: ptr(ModeShadow)}}, true, ""},
		{"default path, with mode", Options{Path: missing, Overrides: Overrides{Mode: ptr(ModePassthrough)}}, false, ModePassthrough},
		{
			"flags-only still enforces rule 4",
			Options{Path: missing, Overrides: Overrides{Mode: ptr(ModeShadow), Listen: ptr("0.0.0.0:1")}},
			true, "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.o.LookupEnv = lookupFrom(nil)
			cfg, err := Load(tc.o)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Mode != tc.want || cfg.Listen != DefaultListen {
				t.Errorf("cfg = %+v", cfg)
			}
		})
	}
	_, err := Load(Options{Path: missing})
	if !errors.Is(err, ErrNoConfig) || !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want ErrNoConfig naming the path", err)
	}
}

func TestLoadUsesOSEnvByDefault(t *testing.T) {
	t.Setenv("SIDECAR_CONFIG_TEST_KEY", "fake-os-env-key")
	path := writeTemp(t, "upstream:\n  provider: together\n  base_url: https://h/v1\n  api_key: ${SIDECAR_CONFIG_TEST_KEY}\n")
	cfg, err := Load(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream.APIKey.Reveal() != "fake-os-env-key" {
		t.Errorf("api_key not read from OS env")
	}
}

func TestReadErrorIsNotMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // reading a directory fails with something other than ErrNotExist
	_, err := Load(Options{Path: dir, Overrides: Overrides{Mode: ptr(ModeShadow)}})
	if err == nil || errors.Is(err, ErrNoConfig) {
		t.Fatalf("error = %v, want a read error", err)
	}
}

// TestSecretsNeverPrint covers every way a Config can leave the process as text.
func TestSecretsNeverPrint(t *testing.T) {
	t.Parallel()
	yamlText := "listen: 0.0.0.0:8787\ninbound_token: ${SIDECAR_INBOUND_TOKEN}\n" +
		"upstream:\n  provider: together\n  base_url: https://h/v1\n  api_key: ${TOGETHER_API_KEY}\n" +
		"control_plane:\n  api_key: ${SIDECAR_API_KEY}\n" +
		"providers:\n  openrouter:\n    api_key: ${OPENROUTER_API_KEY}\n  deepinfra:\n    api_key: ${DEEPINFRA_API_KEY}\n"
	cfg := mustLoadString(t, yamlText, fakeEnv, Overrides{})

	render := map[string]func(t *testing.T) string{
		"%v":      func(*testing.T) string { return fmt.Sprintf("%v", cfg) },
		"%+v":     func(*testing.T) string { return fmt.Sprintf("%+v", cfg) },
		"%#v":     func(*testing.T) string { return fmt.Sprintf("%#v", cfg) },
		"%#v ptr": func(*testing.T) string { return fmt.Sprintf("%#v", &cfg) },
		"%#v parts": func(*testing.T) string {
			return fmt.Sprintf("%#v %#v %#v", cfg.Upstream, cfg.ControlPlane, cfg.Providers)
		},
		"%+v parts": func(*testing.T) string {
			return fmt.Sprintf("%+v %+v %+v", cfg.Upstream, cfg.ControlPlane, cfg.Providers)
		},
		"json":       rendered(json.Marshal(cfg)),
		"json parts": rendered(json.Marshal(cfg.Providers)),
		"yaml":       rendered(yaml.Marshal(cfg)),
		"yaml parts": rendered(yaml.Marshal([]any{cfg.Upstream, cfg.ControlPlane, cfg.Providers})),
		"slog": func(*testing.T) string {
			var sb strings.Builder
			logging.New(&sb, logging.Options{Level: slog.LevelInfo, JSON: true}).Info("cfg", "config", cfg)
			return sb.String()
		},
	}
	for name, fn := range render {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := fn(t)
			for env, secret := range fakeEnv {
				if strings.Contains(out, secret) {
					t.Errorf("%s output reveals %s:\n%s", name, env, out)
				}
			}
			if !strings.Contains(out, masked) {
				t.Errorf("%s output has no mask:\n%s", name, out)
			}
		})
	}
	// Redaction works on copies: the original still holds the values.
	if cfg.Upstream.APIKey.Reveal() != fakeEnv["TOGETHER_API_KEY"] {
		t.Error("Redacted mutated the original config")
	}
}

func TestRedactedKeepsUnsetVisible(t *testing.T) {
	t.Parallel()
	r := Config{}.Redacted()
	if r.InboundToken != "" || r.Upstream.APIKey != "" || r.Providers != nil {
		t.Errorf("unset secrets should stay empty: %#v", r)
	}
}

func TestYAMLRoundTripOfDurations(t *testing.T) {
	t.Parallel()
	b, err := yaml.Marshal(Limits{FirstByteTimeout: NewDuration(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "first_byte_timeout: 2s") {
		t.Errorf("yaml = %s", b)
	}
	if NewDuration(time.Second).String() != "1s" {
		t.Error("Duration.String")
	}
}

func TestIsLoopback(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"127.0.0.1:8787": true,
		"localhost:8787": true,
		"[::1]:8787":     true,
		"0.0.0.0:8787":   false,
		"10.0.0.5:8787":  false,
		"nonsense":       false,
	}
	for addr, want := range cases {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sidecar.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadString(t *testing.T, content string, env map[string]string, o Overrides) (Config, error) {
	t.Helper()
	return Load(Options{Path: writeTemp(t, content), PathExplicit: true, LookupEnv: lookupFrom(env), Overrides: o})
}

func mustLoadString(t *testing.T, content string, env map[string]string, o Overrides) Config {
	t.Helper()
	cfg, err := loadString(t, content, env, o)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// rendered turns the result of a json or yaml Marshal call into a render
// entry that fails the subtest on the marshal error.
func rendered(b []byte, err error) func(*testing.T) string {
	return func(t *testing.T) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
}

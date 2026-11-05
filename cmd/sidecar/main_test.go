package main

import (
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
)

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
		if got := isLoopback(addr); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestLoadOptions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "sidecar.yaml")
	// Fictitious values; a valid route config (ADR 0008).
	yamlText := "mode: route\nupstream:\n  provider: together\n  base_url: https://api.together.xyz/v1\n" +
		"control_plane:\n  url: https://control.example.com\n" +
		"routing:\n  allowed_channels: [direct]\n  allowed_providers: [together]\n" +
		"providers:\n  together:\n    api_key: fake-together-key\n    base_url: https://api.together.xyz/v1\n"
	if err := os.WriteFile(cfgPath, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "absent.yaml")

	cases := []struct {
		name       string
		args       []string
		env        map[string]string
		wantErr    string
		wantMode   config.Mode
		wantListen string
		wantToken  string
	}{
		{"file only", []string{"--config", cfgPath}, nil, "", config.ModeRoute, config.DefaultListen, ""},
		{"mode flag overrides", []string{"--config", cfgPath, "--mode", "passthrough"}, nil, "", config.ModePassthrough, config.DefaultListen, ""},
		{"listen flag needs token", []string{"--config", cfgPath, "--listen", "0.0.0.0:8787"}, nil, "inbound_token is required", "", "", ""},
		{
			"token flag", []string{"--config", cfgPath, "--listen", "0.0.0.0:8787", "--inbound-token", "fake-flag-token"}, nil,
			"", config.ModeRoute, "0.0.0.0:8787", "fake-flag-token",
		},
		{
			"token from env", []string{"--config", cfgPath, "--listen", "0.0.0.0:8787"}, map[string]string{"SIDECAR_INBOUND_TOKEN": "fake-env-token"},
			"", config.ModeRoute, "0.0.0.0:8787", "fake-env-token",
		},
		{
			"token flag beats env", []string{"--config", cfgPath, "--listen", "0.0.0.0:8787", "--inbound-token", "fake-flag-token"},
			map[string]string{"SIDECAR_INBOUND_TOKEN": "fake-env-token"}, "", config.ModeRoute, "0.0.0.0:8787", "fake-flag-token",
		},
		{"explicit missing file", []string{"--config", missing, "--mode", "shadow"}, nil, "does not exist", "", "", ""},
		{"default missing file without flags", nil, nil, "does not exist", "", "", ""},
		{"default missing file with mode", []string{"--mode", "shadow", "--listen", "127.0.0.1:9000"}, nil, "", config.ModeShadow, "127.0.0.1:9000", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			// The default path points at a file that does not exist.
			configPath := fs.String("config", missing, "")
			mode := fs.String("mode", "", "")
			listen := fs.String("listen", "", "")
			token := fs.String("inbound-token", "", "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			lookup := func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok }
			v := flagValues{configPath: *configPath, mode: *mode, listen: *listen, inboundToken: *token}
			cfg, err := config.Load(loadOptions(fs, v, lookup))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Mode != tc.wantMode || cfg.Listen != tc.wantListen || cfg.InboundToken.Reveal() != tc.wantToken {
				t.Errorf("got mode=%s listen=%s token-set=%v", cfg.Mode, cfg.Listen, cfg.InboundToken != "")
			}
		})
	}
}

func TestMetricsListenFlag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "sidecar.yaml")
	yamlText := "upstream:\n  provider: together\n  base_url: https://api.together.xyz/v1\n"
	if err := os.WriteFile(cfgPath, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		args    []string
		want    string
		wantErr string
	}{
		{"default", []string{"--config", cfgPath}, config.DefaultMetricsListen, ""},
		{"override", []string{"--config", cfgPath, "--metrics-listen", "127.0.0.1:9999"}, "127.0.0.1:9999", ""},
		{"invalid is validated", []string{"--config", cfgPath, "--metrics-listen", "not-an-addr"}, "", "metrics_listen"},
		{"empty means default", []string{"--config", cfgPath, "--metrics-listen", ""}, config.DefaultMetricsListen, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			configPath := fs.String("config", "", "")
			metrics := fs.String("metrics-listen", "", "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			v := flagValues{configPath: *configPath, metricsListen: *metrics}
			none := func(string) (string, bool) { return "", false }
			cfg, err := config.Load(loadOptions(fs, v, none))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.MetricsListen != tc.want {
				t.Errorf("metrics listen = %q, want %q", cfg.MetricsListen, tc.want)
			}
		})
	}
}

// The http.Server takes ReadHeaderTimeout, ReadTimeout and
// IdleTimeout from limits in sidecar.yaml, with the documented defaults.
func TestNewServersUseLimits(t *testing.T) {
	t.Parallel()
	const upstream = "upstream:\n  provider: together\n  base_url: https://api.together.xyz/v1\n"
	cases := []struct {
		name               string
		yaml               string
		header, read, idle time.Duration
	}{
		{"defaults", upstream, config.DefaultReadHeaderTimeout, config.DefaultReadTimeout, config.DefaultIdleTimeout},
		{
			"from yaml",
			upstream + "limits:\n  read_header_timeout: 3s\n  read_timeout: 45s\n  idle_timeout: 90s\n",
			3 * time.Second, 45 * time.Second, 90 * time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "sidecar.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			none := func(string) (string, bool) { return "", false }
			cfg, err := config.Load(config.Options{Path: path, PathExplicit: true, LookupEnv: none})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			for _, s := range []*http.Server{
				newAPIServer(cfg.Listen, http.NotFoundHandler(), cfg.Limits),
				newMetricsServer(cfg.MetricsListen, http.NotFoundHandler(), cfg.Limits),
			} {
				if s.ReadHeaderTimeout != tc.header || s.ReadTimeout != tc.read || s.IdleTimeout != tc.idle {
					t.Errorf("%s: header=%s read=%s idle=%s, want %s %s %s", s.Addr,
						s.ReadHeaderTimeout, s.ReadTimeout, s.IdleTimeout, tc.header, tc.read, tc.idle)
				}
			}
			if api := newAPIServer(cfg.Listen, http.NotFoundHandler(), cfg.Limits); api.WriteTimeout != 0 {
				t.Errorf("api WriteTimeout = %s, want 0 (per-chunk deadlines in the proxy)", api.WriteTimeout)
			}
		})
	}
}

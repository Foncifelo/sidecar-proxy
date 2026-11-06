package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/logging"
)

// TestLogHTTPLoopbackException checks the ADR 0011 startup log: a single
// line naming the providers by id and never their base_url. Hosts, ports and
// keys are fictitious.
func TestLogHTTPLoopbackException(t *testing.T) {
	t.Parallel()
	const base = "upstream:\n  provider: together\n  base_url: https://api.together.xyz/v1\n  api_key: ${TOGETHER_API_KEY}\n"
	cases := []struct {
		name      string
		yaml      string
		wantLines int
		want      []string
		hidden    []string
	}{
		{
			name: "exception in use",
			yaml: base + "providers:\n  fakea:\n    base_url: http://127.0.0.1:9001/v1\n" +
				"  fakeb:\n    base_url: http://localhost:9002/v1\n  together:\n    base_url: https://api.together.xyz/v1\n",
			wantLines: 1,
			want:      []string{"fakea", "fakeb", "ADR 0011"},
			// Ports are matched with their colon: a bare "9001" can appear in the
			// log timestamp's nanoseconds.
			hidden: []string{"http://", "127.0.0.1", "localhost", ":9001", ":9002", "together"},
		},
		{name: "https only", yaml: base + "providers:\n  together:\n    base_url: https://api.together.xyz/v1\n", wantLines: 0},
	}
	env := map[string]string{config.EnvVarSidecarEnv: "dev", "TOGETHER_API_KEY": "fictitious"}
	lookup := func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "sidecar.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(config.Options{Path: path, PathExplicit: true, LookupEnv: lookup})
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			logHTTPLoopbackException(logging.New(&buf, logging.Options{Level: slog.LevelInfo, JSON: true}), cfg)
			out := buf.String()
			if got := strings.Count(out, "\n"); got != tc.wantLines {
				t.Fatalf("got %d log lines, want %d: %s", got, tc.wantLines, out)
			}
			for _, s := range tc.want {
				if !strings.Contains(out, s) {
					t.Errorf("log %q does not mention %q", out, s)
				}
			}
			for _, s := range tc.hidden {
				if strings.Contains(out, s) {
					t.Errorf("log %q leaks %q", out, s)
				}
			}
		})
	}
}

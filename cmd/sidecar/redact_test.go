package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
)

func TestRedactURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://control.example", "https://control.example"},
		{"https://control.example/base/", "https://control.example/base/"},
		{"https://user:s3cret@control.example/base", "https://control.example/base"},
		{"https://user@control.example:8443/", "https://control.example:8443/"},
		{"https://control.example/?token=t0ken&x=1", "https://control.example/"},
		{"https://control.example/#t0ken", "https://control.example/"},
		{"http://[::1]:8080/v1?key=t0ken", "http://[::1]:8080/v1"},
		{"://bad", "<invalid url>"},
		{"control.example/no-scheme?token=t0ken", "<invalid url>"},
		{"", "<invalid url>"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := redactURL(tt.in); got != tt.want {
				t.Fatalf("redactURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestStartupLogRedactsControlURL: the startup line names
// the control plane without the credentials embedded in its URL.
func TestStartupLogRedactsControlURL(t *testing.T) {
	tests := []struct {
		name, url, want string
		leaks           []string
	}{
		{"userinfo", "https://ctl-user:s3cret-pass@control.example/base", "https://control.example/base", []string{"ctl-user", "s3cret-pass"}},
		{"query token", "https://control.example/?token=t0ken-value&api_key=k3y-value", "https://control.example/", []string{"t0ken-value", "k3y-value"}},
		{"both", "https://ctl-user:s3cret-pass@control.example/?access_token=t0ken-value", "https://control.example/", []string{"ctl-user", "s3cret-pass", "t0ken-value"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log, logs := newTestLogger()
			// The queue stays empty, so nothing is ever sent to the fictitious host.
			p, err := openEventPipeline(config.ControlPlane{URL: tt.url, APIKey: testControlKey, QueueDir: t.TempDir()}, config.ModeShadow, log, nil)
			if err != nil {
				t.Fatalf("openEventPipeline: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			p.Close(ctx)
			out := logs.String()
			if !strings.Contains(out, "sending events to the control plane") || !strings.Contains(out, tt.want) {
				t.Fatalf("startup line missing or URL not %q:\n%s", tt.want, out)
			}
			for _, leak := range append(tt.leaks, testControlKey) {
				if strings.Contains(out, leak) {
					t.Fatalf("log leaks %q:\n%s", leak, out)
				}
			}
		})
	}
}

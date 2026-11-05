package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/metrics"
	"github.com/Foncifelo/sidecar-proxy/internal/policy"
)

// Metrics_listen serves GET /metrics with every series, fed by
// the proxy, the real event queue and the cached policy. Upstream, key and
// policy are fictitious.
func TestMetricsEndpoint(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer up.Close()
	ctl := httptest.NewServer(http.NotFoundHandler())
	ctlURL := ctl.URL
	ctl.Close() // control plane down: the cached policy is used

	cfg := policyConfig(t, ctlURL)
	cfg.Listen = "127.0.0.1:8787"
	cfg.Limits = config.Limits{MaxBodyMB: 1, MaxInflight: 4}
	cfg.Upstream = config.Upstream{Provider: "fictitious", BaseURL: up.URL + "/v1", APIKey: "sk-fictitious"}
	cfg.Routing = config.Routing{AllowedChannels: []string{"direct"}, AllowedProviders: []string{"fict-cheap"}}
	if err := policy.SaveCache(policy.CachePath(cfg.ControlPlane.QueueDir), policy.Cached{
		ETag: `"1:p_fict_metrics"`, ValidatedAt: time.Now().Add(-time.Minute), Document: []byte(fictPolicyDoc),
	}); err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	h, closeFn, err := newV1Handler(cfg, slog.New(slog.DiscardHandler), m)
	if err != nil {
		t.Fatalf("newV1Handler: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	defer closeFn(ctx)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)))

	mux := metricsMux(m)
	tests := []struct {
		name, method, path string
		wantStatus         int
		want               []string
	}{
		{
			"metrics", http.MethodGet, "/metrics", http.StatusOK,
			[]string{
				"\nsidecar_overhead_seconds_count 1\n",
				"\nsidecar_requests_total{kind=\"chat\"} 1\n",
				"\nsidecar_fallbacks_total 0\n",
				"\nsidecar_attempts_total{reason=\"none\"} 0\n",
				"\nsidecar_queue_depth ",
				"\nsidecar_dropped_events_total 0\n",
				"\nsidecar_queue_disk_errors_total 0\n",
				"\nsidecar_policy_age_seconds 6", // about 60 s since ValidatedAt
			},
		},
		{"other path", http.MethodGet, "/", http.StatusNotFound, nil},
		{"wrong method", http.MethodPost, "/metrics", http.StatusMethodNotAllowed, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d", rec.Code, tt.wantStatus)
			}
			for _, w := range tt.want {
				if !strings.Contains(rec.Body.String(), w) {
					t.Errorf("missing %q in\n%s", w, rec.Body.String())
				}
			}
		})
	}
}

func TestMetricsSourcesWithoutQueueOrPolicy(t *testing.T) {
	src := metricsSources(nil, nil, time.Now)
	if src.QueueDepth != nil || src.DroppedEvents != nil || src.PolicyAge != nil {
		t.Errorf("sources without queue or policy should be empty: %+v", src)
	}
}

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/ingest"
	"github.com/Foncifelo/sidecar-proxy/internal/logging"
	"github.com/Foncifelo/sidecar-proxy/internal/policy"
)

// Fictitious, empty policy document.
const emptyPolicyDoc = `{"schema_version":1,"version":"p_fict","computed_at":"2026-10-29T00:00:00Z","flags":{},"models":{}}`

func policyConfig(t *testing.T, controlURL string) config.Config {
	t.Helper()
	return config.Config{
		Mode: config.ModeShadow,
		ControlPlane: config.ControlPlane{
			URL: controlURL, APIKey: logging.Secret(testControlKey),
			QueueDir: filepath.Join(t.TempDir(), "queue"), PolicyRefresh: config.NewDuration(time.Hour),
		},
	}
}

func waitPolicy(t *testing.T, pr *policyRunner) *policy.Active {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a := pr.r.Current(); a != nil {
			return a
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no policy published")
	return nil
}

// Heartbeats carry the real mode, policy version and age.
func TestHeartbeatStateFromPolicy(t *testing.T) {
	ctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/policy" || r.Header.Get("Authorization") != "Bearer "+testControlKey {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(emptyPolicyDoc))
	}))
	t.Cleanup(ctl.Close)
	log, _ := newTestLogger()
	pr, err := startPolicy(policyConfig(t, ctl.URL), log)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Stop()
	a := waitPolicy(t, pr)

	tests := []struct {
		name string
		mode config.Mode
		pr   *policyRunner
		want ingest.State
	}{
		{"with policy", config.ModeShadow, pr, ingest.State{Mode: events.ModeShadow, PolicyVersion: "p_fict", PolicyAge: 90 * time.Second}},
		{"no control plane", config.ModeRoute, nil, ingest.State{Mode: events.ModeRoute}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := func() time.Time { return a.ValidatedAt.Add(90 * time.Second) }
			if got := heartbeatState(tt.mode, tt.pr, now)(); got != tt.want {
				t.Errorf("state = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// With the control plane down the sidecar starts with
// the cached policy (ADR 0001).
func TestStartPolicyUsesCacheWhenControlIsDown(t *testing.T) {
	ctl := httptest.NewServer(http.NotFoundHandler())
	url := ctl.URL
	ctl.Close()
	cfg := policyConfig(t, url)
	validated := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	path := policy.CachePath(cfg.ControlPlane.QueueDir)
	if err := policy.SaveCache(path, policy.Cached{ETag: `"1:p_fict"`, ValidatedAt: validated, Document: []byte(emptyPolicyDoc)}); err != nil {
		t.Fatal(err)
	}
	log, _ := newTestLogger()

	pr, err := startPolicy(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Stop()

	a := pr.r.Current()
	if a == nil || a.Version() != "p_fict" || !a.ValidatedAt.Equal(validated) {
		t.Fatalf("active = %+v, want the cached policy", a)
	}
}

func TestStartPolicyWithoutControlPlane(t *testing.T) {
	log, _ := newTestLogger()
	cfg := policyConfig(t, "")
	pr, err := startPolicy(cfg, log)
	if err != nil || pr != nil {
		t.Fatalf("startPolicy = %v, %v; want nil, nil", pr, err)
	}
	pr.Stop() // safe on nil

	cfg = policyConfig(t, "http://control.example")
	cfg.ControlPlane.APIKey = ""
	if _, err := startPolicy(cfg, log); err == nil {
		t.Error("missing api_key accepted")
	}
}

// heartbeatControl is a fake control plane: GET /v1/policy waits for release
// (so the cached policy stays active while the first heartbeat goes out) and
// POST /v1/ingest records heartbeats and answers 503 to event batches, so
// queued events stay queued. Everything it serves is fictitious.
type heartbeatControl struct {
	srv     *httptest.Server
	release chan struct{}
	beats   chan ingest.Heartbeat
}

func newHeartbeatControl(t *testing.T) *heartbeatControl {
	t.Helper()
	hc := &heartbeatControl{release: make(chan struct{}), beats: make(chan ingest.Heartbeat, 16)}
	hc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testControlKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/policy":
			select {
			case <-hc.release:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte(emptyPolicyDoc))
		case "/v1/ingest":
			var env ingest.Envelope
			if err := json.NewDecoder(r.Body).Decode(&env); err != nil || len(env.Items) == 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			hb := env.Items[0].Heartbeat
			if hb == nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			select {
			case hc.beats <- *hb:
			default:
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprintf(w, `{"accepted_n":%d}`, len(env.Items))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		close(hc.release)
		hc.srv.Close()
	})
	return hc
}

func (hc *heartbeatControl) next(t *testing.T) ingest.Heartbeat {
	t.Helper()
	select {
	case hb := <-hc.beats:
		return hb
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat reached the control plane")
		return ingest.Heartbeat{}
	}
}

// seedSidecarState writes a cached policy validated cachedAge ago and, when
// queued > 0, that many fictitious events into the queue at dir.
func seedSidecarState(t *testing.T, dir string, cachedAge time.Duration, queued int) {
	t.Helper()
	c := policy.Cached{ETag: `"1:p_fict"`, ValidatedAt: time.Now().Add(-cachedAge), Document: []byte(emptyPolicyDoc)}
	if err := policy.SaveCache(policy.CachePath(dir), c); err != nil {
		t.Fatal(err)
	}
	if queued == 0 {
		return
	}
	q, err := events.OpenQueue(events.QueueConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for i := range queued {
		ev := events.Event{ID: fmt.Sprintf("ev-fict-%d", i), Mode: events.ModeShadow, RawModel: "fictitious/model", Attempts: []events.Attempt{}}
		if err := q.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

// The running sidecar sends heartbeats in every mode with
// the real mode, policy version, policy age and queue depth.
func TestSidecarSendsRealHeartbeat(t *testing.T) {
	const cachedAge = 90 * time.Second
	up := newFakeUpstream(t)
	tests := []struct {
		name      string
		mode      config.Mode
		queued    int
		wantMode  events.Mode
		wantDepth int
	}{
		{"shadow with queued events", config.ModeShadow, 2, events.ModeShadow, 2},
		{"route", config.ModeRoute, 0, events.ModeRoute, 0},
		{"passthrough", config.ModePassthrough, 0, events.ModePassthrough, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctl := newHeartbeatControl(t)
			dir := t.TempDir()
			seedSidecarState(t, dir, cachedAge, tt.queued)
			cfg := shadowConfig(t, up.srv.URL, ctl.srv.URL, dir)
			cfg.Mode = tt.mode
			cfg.ControlPlane.PolicyRefresh = config.NewDuration(time.Hour)
			log, _ := newTestLogger()

			// Every mode goes through newV1Handler; route also
			// opens the event queue, as shadow does.
			_, closeFn, err := newV1Handler(cfg, log, nil)
			if err != nil {
				t.Fatal(err)
			}
			stop := func() { closeWithin(t, closeFn, 200*time.Millisecond) }
			defer stop()

			hb := ctl.next(t)
			if hb.Mode != tt.wantMode || hb.PolicyVersion != "p_fict" || hb.QueueDepth != tt.wantDepth {
				t.Errorf("heartbeat mode=%q policy_version=%q queue_depth=%d; want %q, p_fict, %d",
					hb.Mode, hb.PolicyVersion, hb.QueueDepth, tt.wantMode, tt.wantDepth)
			}
			if hb.PolicyAgeS == nil || *hb.PolicyAgeS < int64(cachedAge/time.Second) || *hb.PolicyAgeS > int64(cachedAge/time.Second)+5 {
				t.Errorf("policy_age_s = %v, want about %d", hb.PolicyAgeS, int64(cachedAge/time.Second))
			}
		})
	}
}

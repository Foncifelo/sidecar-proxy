package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/ingest"
	"github.com/Foncifelo/sidecar-proxy/internal/logging"
)

// Every key, model and payload below is fictitious.
const (
	testControlKey  = "sk_test_fictitious_control_key"
	testUpstreamKey = "upstream-key-fictitious"
	testAgentKey    = "agent-key-fictitious"
)

// syncBuffer is a log sink safe for the batcher goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return logging.New(buf, logging.Options{Level: slog.LevelDebug, JSON: true}), buf
}

// fakeControl is a stand-in for POST /v1/ingest.
type fakeControl struct {
	srv    *httptest.Server
	mu     sync.Mutex
	auth   []string
	events []events.Event
}

// newFakeControl answers with status (202 acknowledges every item).
func newFakeControl(t *testing.T, status int) *fakeControl {
	t.Helper()
	fc := &fakeControl{}
	fc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/ingest" {
			http.NotFound(w, r)
			return
		}
		var env ingest.Envelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fc.mu.Lock()
		fc.auth = append(fc.auth, r.Header.Get("Authorization"))
		if status == http.StatusAccepted {
			for _, it := range env.Items {
				if it.Event != nil {
					fc.events = append(fc.events, *it.Event)
				}
			}
		}
		fc.mu.Unlock()
		w.WriteHeader(status)
		if status == http.StatusAccepted {
			_, _ = fmt.Fprintf(w, `{"accepted_n":%d}`, len(env.Items))
		}
	}))
	t.Cleanup(fc.srv.Close)
	return fc
}

func (fc *fakeControl) snapshot() (auth []string, evs []events.Event) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return append([]string(nil), fc.auth...), append([]events.Event(nil), fc.events...)
}

// fakeUpstream records whether the control plane key ever reached it.
type fakeUpstream struct {
	srv       *httptest.Server
	mu        sync.Mutex
	requests  int
	sawCtlKey bool
	auth      string
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	fu := &fakeUpstream{}
	fu.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen := bytes.Contains(body, []byte(testControlKey)) || strings.Contains(r.URL.String(), testControlKey)
		for _, vs := range r.Header {
			for _, v := range vs {
				seen = seen || strings.Contains(v, testControlKey)
			}
		}
		fu.mu.Lock()
		fu.requests++
		fu.sawCtlKey = fu.sawCtlKey || seen
		fu.auth = r.Header.Get("Authorization")
		fu.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"fictitious/model","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	t.Cleanup(fu.srv.Close)
	return fu
}

func shadowConfig(t *testing.T, upstreamURL, controlURL, queueDir string) config.Config {
	t.Helper()
	cfg := config.Config{
		Mode: config.ModeShadow, Listen: "127.0.0.1:8787",
		Limits:   config.Limits{MaxBodyMB: 1, MaxInflight: 4},
		Upstream: config.Upstream{Provider: "fictitious", BaseURL: upstreamURL + "/v1", APIKey: testUpstreamKey},
		ControlPlane: config.ControlPlane{
			URL: controlURL, QueueDir: queueDir,
		},
	}
	if controlURL != "" {
		cfg.ControlPlane.APIKey = logging.Secret(testControlKey)
	}
	return cfg
}

func postCompletion(t *testing.T, h http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"fictitious/model","messages":[{"role":"user","content":"fictitious"}]}`))
	req.Header.Set("Authorization", "Bearer "+testAgentKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	return rec.Header().Get("X-Sidecar-Request-Id")
}

func queueDepth(t *testing.T, dir string) int {
	t.Helper()
	q, err := events.OpenQueue(events.QueueConfig{Dir: dir})
	if err != nil {
		t.Fatalf("reopen queue: %v", err)
	}
	defer func() { _ = q.Close() }()
	return q.Depth()
}

func closeWithin(t *testing.T, closeFn func(context.Context), d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	closeFn(ctx)
}

// The binary's pipeline sends its own batches to the
// control plane with the control key, and that key never reaches the provider
// nor the log.
func TestEventPipelineDelivers(t *testing.T) {
	up, ctl := newFakeUpstream(t), newFakeControl(t, http.StatusAccepted)
	log, logs := newTestLogger()
	dir := t.TempDir()
	h, closeFn, err := newV1Handler(shadowConfig(t, up.srv.URL, ctl.srv.URL, dir), log, nil)
	if err != nil {
		t.Fatalf("newV1Handler: %v", err)
	}
	ids := []string{postCompletion(t, h), postCompletion(t, h)}
	closeWithin(t, closeFn, eventFlushTimeout)

	auth, evs := ctl.snapshot()
	if len(evs) != len(ids) {
		t.Fatalf("control plane got %d events, want %d", len(evs), len(ids))
	}
	for i, id := range ids {
		if evs[i].ID != id {
			t.Errorf("event %d id = %q, want %q", i, evs[i].ID, id)
		}
	}
	for _, a := range auth {
		if a != "Bearer "+testControlKey {
			t.Errorf("control plane Authorization = %q", a)
		}
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.requests != len(ids) || up.sawCtlKey || up.auth != "Bearer "+testUpstreamKey {
		t.Errorf("upstream: requests=%d saw control key=%v auth=%q", up.requests, up.sawCtlKey, up.auth)
	}
	for _, secret := range []string{testControlKey, testUpstreamKey, testAgentKey} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("log contains a key:\n%s", logs)
		}
	}
	if d := queueDepth(t, dir); d != 0 {
		t.Errorf("queue depth after flush = %d, want 0", d)
	}
}

// Without a control plane nothing is sent, the log says so once
// at start and the events stay on disk.
func TestEventPipelineWithoutControlPlane(t *testing.T) {
	up := newFakeUpstream(t)
	log, logs := newTestLogger()
	dir := t.TempDir()
	h, closeFn, err := newV1Handler(shadowConfig(t, up.srv.URL, "", dir), log, nil)
	if err != nil {
		t.Fatalf("newV1Handler: %v", err)
	}
	postCompletion(t, h)
	postCompletion(t, h)
	closeWithin(t, closeFn, eventFlushTimeout)

	if n := strings.Count(logs.String(), "no control plane configured"); n != 1 {
		t.Errorf("notice logged %d times, want 1:\n%s", n, logs)
	}
	if d := queueDepth(t, dir); d != 2 {
		t.Errorf("queue depth = %d, want 2", d)
	}
}

func TestEventPipelineConfigErrors(t *testing.T) {
	const badKey = "not-an-sk-key-fictitious"
	tests := []struct {
		name string
		cp   config.ControlPlane
		want string
	}{
		{"url without key", config.ControlPlane{URL: "http://127.0.0.1:1"}, "control_plane.api_key: required"},
		{"key without sk_ prefix", config.ControlPlane{URL: "http://127.0.0.1:1", APIKey: badKey}, "sk_ key"},
		{"relative url", config.ControlPlane{URL: "control.example", APIKey: testControlKey}, "absolute http(s) URL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.cp.QueueDir = t.TempDir()
			log, logs := newTestLogger()
			p, err := openEventPipeline(tc.cp, config.ModeShadow, log, nil)
			if err == nil {
				p.Close(context.Background())
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
			for _, secret := range []string{badKey, testControlKey} {
				if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
					t.Errorf("error or log leaks the key: %v", err)
				}
			}
		})
	}
}

// On shutdown the pipeline sends what it can within the
// deadline; the rest stays on disk and goes out after the next start.
func TestEventPipelineCloseLeavesRestOnDisk(t *testing.T) {
	const flush = 300 * time.Millisecond
	up := newFakeUpstream(t)
	down := newFakeControl(t, http.StatusServiceUnavailable)
	dir := t.TempDir()
	log, logs := newTestLogger()

	h, closeFn, err := newV1Handler(shadowConfig(t, up.srv.URL, down.srv.URL, dir), log, nil)
	if err != nil {
		t.Fatalf("newV1Handler: %v", err)
	}
	ids := map[string]bool{postCompletion(t, h): true, postCompletion(t, h): true}
	start := time.Now()
	closeWithin(t, closeFn, flush)
	if elapsed := time.Since(start); elapsed > flush+2*time.Second {
		t.Errorf("close took %s with a %s deadline", elapsed, flush)
	}
	if d := queueDepth(t, dir); d != len(ids) {
		t.Fatalf("queue depth after failed flush = %d, want %d", d, len(ids))
	}
	if !strings.Contains(logs.String(), "events left in the local queue") {
		t.Errorf("no warning about events left on disk:\n%s", logs)
	}

	ok := newFakeControl(t, http.StatusAccepted)
	_, closeFn, err = newV1Handler(shadowConfig(t, up.srv.URL, ok.srv.URL, dir), log, nil)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	closeWithin(t, closeFn, eventFlushTimeout)
	_, evs := ok.snapshot()
	if len(evs) != len(ids) {
		t.Fatalf("second start delivered %d events, want %d", len(evs), len(ids))
	}
	for _, ev := range evs {
		if !ids[ev.ID] {
			t.Errorf("unexpected event id %q", ev.ID)
		}
	}
	if d := queueDepth(t, dir); d != 0 {
		t.Errorf("queue depth after second flush = %d, want 0", d)
	}
}

// Shutdown flushes after the servers stop, within eventFlushTimeout.
func TestShutdownFlushesAfterServers(t *testing.T) {
	log, _ := newTestLogger()
	srv := &http.Server{ReadHeaderTimeout: time.Second}
	var got time.Duration
	err := shutdown(log, func(ctx context.Context) {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("flush context has no deadline")
		}
		got = time.Until(dl)
	}, srv)
	if err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got <= 0 || got > eventFlushTimeout {
		t.Errorf("flush deadline in %s, want within %s", got, eventFlushTimeout)
	}
}

package policy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/logging"
)

// Fictitious control-plane key; never a real one.
const testKey logging.Secret = "sk_test_fictitious"

const exampleETag = `"1:p_fictitious"`

var t0 = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// fakeControl serves GET /v1/policy with a scripted answer and records the
// request headers it saw.
type fakeControl struct {
	mu      sync.Mutex
	status  int
	body    []byte
	etag    string
	delay   time.Duration
	headers []http.Header
	srv     *httptest.Server
}

func newFakeControl(t *testing.T, status int, body []byte, etag string) *fakeControl {
	t.Helper()
	f := &fakeControl{status: status, body: body, etag: etag}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeControl) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.headers = append(f.headers, r.Header.Clone())
	status, body, etag, delay := f.status, f.body, f.etag, f.delay
	f.mu.Unlock()
	if r.Method != http.MethodGet || r.URL.Path != "/v1/policy" {
		http.NotFound(w, r)
		return
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(status)
	if status == http.StatusOK {
		_, _ = w.Write(body)
	}
}

func (f *fakeControl) set(status int, body []byte, etag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body, f.etag = status, body, etag
}

func (f *fakeControl) requests() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]http.Header(nil), f.headers...)
}

type refresherOpts struct {
	cfg     config.Config
	timeout time.Duration
	noCache bool
}

func newTestRefresher(t *testing.T, baseURL string, o refresherOpts) *Refresher {
	t.Helper()
	cfg := o.cfg
	if cfg.Mode == "" {
		cfg = testConfig(t, "route")
	}
	rc := RefresherConfig{BaseURL: baseURL, APIKey: testKey, Config: cfg, Interval: time.Minute, Timeout: o.timeout}
	if !o.noCache {
		rc.CachePath = filepath.Join(t.TempDir(), "data", "policy.json")
	}
	r, err := NewRefresher(rc)
	if err != nil {
		t.Fatalf("NewRefresher: %v", err)
	}
	r.now = func() time.Time { return t0 }
	return r
}

// preload seeds the disk cache with exampleDoc validated at t0 and loads it,
// as at start.
func preload(t *testing.T, r *Refresher) {
	t.Helper()
	if err := SaveCache(r.cfg.CachePath, Cached{ETag: exampleETag, ValidatedAt: t0, Document: []byte(exampleDoc)}); err != nil {
		t.Fatal(err)
	}
	if err := r.LoadCached(); err != nil {
		t.Fatalf("LoadCached: %v", err)
	}
	if r.Current() == nil {
		t.Fatal("no policy after LoadCached")
	}
}

// 200, 304, 401, 426 and a control plane that is down; in every
// failure the cached policy stays active (ADR 0001).
func TestRefreshStatuses(t *testing.T) {
	invalid := mutateDoc(t, func(d map[string]any) { d["schema_version"] = SchemaVersion + 1; d["version"] = "p_other" })
	v2 := mutateDoc(t, func(d map[string]any) { d["version"] = "p_fictitious2" })
	later := t0.Add(time.Hour)
	tests := []struct {
		name        string
		preload     bool
		status      int
		body        []byte
		etag        string
		down        bool
		wantErr     error
		wantStatus  int
		wantVersion string
		wantETag    string
		wantAt      time.Time // ValidatedAt in memory and on disk
		wantStopped bool
	}{
		{name: "200 without policy publishes", status: 200, body: []byte(exampleDoc), etag: exampleETag,
			wantVersion: "p_fictitious", wantETag: exampleETag, wantAt: later},
		{name: "200 without ETag header synthesizes it", status: 200, body: []byte(exampleDoc),
			wantVersion: "p_fictitious", wantETag: exampleETag, wantAt: later},
		{name: "200 replaces cached policy", preload: true, status: 200, body: v2, etag: `"1:p_fictitious2"`,
			wantVersion: "p_fictitious2", wantETag: `"1:p_fictitious2"`, wantAt: later},
		{name: "304 renews the age", preload: true, status: 304, etag: exampleETag,
			wantVersion: "p_fictitious", wantETag: exampleETag, wantAt: later},
		{name: "304 for another etag is rejected", preload: true, status: 304, etag: `"1:p_zzz"`, wantErr: ErrInvalid,
			wantVersion: "p_fictitious", wantETag: exampleETag, wantAt: t0},
		{name: "304 without policy", status: 304, etag: exampleETag, wantErr: ErrNotModifiedWithoutPolicy},
		{name: "401 keeps the cache", preload: true, status: 401, wantErr: ErrUnauthorized,
			wantVersion: "p_fictitious", wantETag: exampleETag, wantAt: t0},
		{name: "426 stops and keeps the cache", preload: true, status: 426, wantErr: ErrUpgradeRequired,
			wantVersion: "p_fictitious", wantETag: exampleETag, wantAt: t0, wantStopped: true},
		{name: "500 keeps the cache", preload: true, status: 500, wantStatus: 500,
			wantVersion: "p_fictitious", wantETag: exampleETag, wantAt: t0},
		{name: "control down keeps the cache", preload: true, down: true,
			wantVersion: "p_fictitious", wantETag: exampleETag, wantAt: t0},
		{name: "invalid 200 does not replace the good policy", preload: true, status: 200, body: invalid, etag: `"` + strconv.Itoa(SchemaVersion+1) + `:p_other"`,
			wantErr: ErrUnsupportedSchema, wantVersion: "p_fictitious", wantETag: exampleETag, wantAt: t0},
		{name: "invalid 200 without policy publishes nothing", status: 200, body: []byte(`{"schema_version":1}`), wantErr: ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := newFakeControl(t, tt.status, tt.body, tt.etag)
			r := newTestRefresher(t, fc.srv.URL, refresherOpts{})
			if tt.preload {
				preload(t, r)
			}
			if tt.down {
				fc.srv.Close()
			}
			r.now = func() time.Time { return later }

			err := r.Refresh(context.Background())

			assertRefreshErr(t, err, tt.wantErr, tt.wantStatus, tt.down)
			if got := r.Stopped(); got != tt.wantStopped {
				t.Errorf("Stopped = %v, want %v", got, tt.wantStopped)
			}
			assertActive(t, r, tt.wantVersion, tt.wantETag, tt.wantAt)
		})
	}
}

func assertRefreshErr(t *testing.T, err, want error, wantStatus int, down bool) {
	t.Helper()
	switch {
	case want != nil:
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
	case wantStatus != 0:
		var se *StatusError
		if !errors.As(err, &se) || se.Code != wantStatus {
			t.Fatalf("err = %v, want status %d", err, wantStatus)
		}
	case down:
		if err == nil {
			t.Fatal("err = nil, want a transport error")
		}
	default:
		if err != nil {
			t.Fatalf("err = %v", err)
		}
	}
}

// assertActive checks the policy in memory and the copy on disk agree with
// the expectation; an empty version means no policy anywhere.
func assertActive(t *testing.T, r *Refresher, version, etag string, at time.Time) {
	t.Helper()
	a := r.Current()
	c, cerr := LoadCache(r.cfg.CachePath)
	if version == "" {
		if a != nil {
			t.Errorf("active = %q, want none", a.Version())
		}
		if cerr == nil {
			t.Error("cache written, want none")
		}
		return
	}
	if a == nil {
		t.Fatalf("no active policy, want %q", version)
	}
	if a.Version() != version || a.ETag != etag || !a.ValidatedAt.Equal(at) {
		t.Errorf("active = (%q, %q, %v), want (%q, %q, %v)", a.Version(), a.ETag, a.ValidatedAt, version, etag, at)
	}
	if cerr != nil {
		t.Fatalf("LoadCache: %v", cerr)
	}
	p, err := Decode(c.Document)
	if err != nil {
		t.Fatalf("cached document: %v", err)
	}
	if p.Version != version || c.ETag != etag || !c.ValidatedAt.Equal(at) {
		t.Errorf("cache = (%q, %q, %v), want (%q, %q, %v)", p.Version, c.ETag, c.ValidatedAt, version, etag, at)
	}
}

func TestRefreshAfter426DoesNotCallControl(t *testing.T) {
	fc := newFakeControl(t, http.StatusUpgradeRequired, nil, "")
	r := newTestRefresher(t, fc.srv.URL, refresherOpts{})
	for range 2 {
		if err := r.Refresh(context.Background()); !errors.Is(err, ErrUpgradeRequired) {
			t.Fatalf("err = %v", err)
		}
	}
	if n := len(fc.requests()); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
	r.sleep = func(context.Context, time.Duration) error { t.Error("Run slept after 426"); return nil }
	r.Run(context.Background()) // returns at once
}

// Run sends the key, schema version and If-None-Match, and
// refreshes again after the interval.
func TestRunRefreshesWithIfNoneMatch(t *testing.T) {
	fc := newFakeControl(t, http.StatusOK, []byte(exampleDoc), exampleETag)
	r := newTestRefresher(t, fc.srv.URL, refresherOpts{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ticks atomic.Int32
	var delays []time.Duration
	r.onTick = func() {
		if ticks.Add(1) == 1 {
			fc.set(http.StatusNotModified, nil, exampleETag)
		}
		if ticks.Load() == 3 {
			cancel()
		}
	}
	r.sleep = func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }

	r.Run(ctx)

	reqs := fc.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(reqs))
	}
	for i, h := range reqs {
		if got := h.Get("Authorization"); got != "Bearer "+testKey.Reveal() {
			t.Errorf("req %d Authorization not the control key", i)
		}
		if got := h.Get("X-Sidecar-Schema-Version"); got != strconv.Itoa(SchemaVersion) {
			t.Errorf("req %d schema header = %q", i, got)
		}
	}
	if got := reqs[0].Get("If-None-Match"); got != "" {
		t.Errorf("first If-None-Match = %q, want none", got)
	}
	if got := reqs[1].Get("If-None-Match"); got != exampleETag {
		t.Errorf("second If-None-Match = %q, want %q", got, exampleETag)
	}
	for _, d := range delays {
		if d < 54*time.Second || d > 66*time.Second {
			t.Errorf("delay after success = %v, want 60s ±10%%", d)
		}
	}
}

// Jitter around the interval and capped exponential backoff.
func TestNextDelay(t *testing.T) {
	tests := []struct {
		name     string
		failures int
		rnd      string // "min" or "max"
		want     time.Duration
	}{
		{"success min", 0, "min", 54 * time.Second},
		{"success max", 0, "max", 66 * time.Second},
		{"1 failure min", 1, "min", 2500 * time.Millisecond},
		{"1 failure max", 1, "max", 5 * time.Second},
		{"2 failures max", 2, "max", 10 * time.Second},
		{"4 failures max", 4, "max", 40 * time.Second},
		{"capped at interval", 5, "max", 60 * time.Second},
		{"capped at interval min", 30, "min", 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRefresher(t, "http://control.example", refresherOpts{noCache: true})
			r.randN = func(n time.Duration) time.Duration {
				if tt.rnd == "max" {
					return n - 1
				}
				return 0
			}
			got := r.nextDelay(tt.failures)
			if diff := got - tt.want; diff < -time.Nanosecond || diff > time.Nanosecond {
				t.Errorf("nextDelay(%d) = %v, want %v", tt.failures, got, tt.want)
			}
		})
	}
}

// A control plane that never answers is cut by the timeout.
func TestRefreshTimeout(t *testing.T) {
	fc := newFakeControl(t, http.StatusOK, []byte(exampleDoc), exampleETag)
	r := newTestRefresher(t, fc.srv.URL, refresherOpts{timeout: 50 * time.Millisecond})
	preload(t, r)
	fc.mu.Lock()
	fc.delay = 5 * time.Second
	fc.mu.Unlock()

	start := time.Now()
	err := r.Refresh(context.Background())

	if err == nil {
		t.Fatal("err = nil, want timeout")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("Refresh took %v, want it cut by the timeout", el)
	}
	assertActive(t, r, "p_fictitious", exampleETag, t0)
}

// Filter runs before publishing; the cache keeps the raw
// document so a later YAML can allow more.
func TestRefreshAppliesFilter(t *testing.T) {
	fc := newFakeControl(t, http.StatusOK, []byte(exampleDoc), exampleETag)
	cfg := testConfig(t, "route")
	cfg.Routing.AllowedChannels = []string{"direct"}
	r := newTestRefresher(t, fc.srv.URL, refresherOpts{cfg: cfg})

	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	a := r.Current()
	m, _ := a.Model("fict-model")
	if got := endpointIDs(m.Candidates); len(got) != 2 || got[0] != epGammaBF16 {
		t.Errorf("published candidates = %v, want the direct ones", got)
	}
	if a.Discards[ReasonChannelNotAllowed] != 1 {
		t.Errorf("discards = %v", a.Discards)
	}
	c, err := LoadCache(r.cfg.CachePath)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := Decode(c.Document)
	if n := len(raw.Models["fict-model"].Candidates); n != 3 {
		t.Errorf("cached candidates = %d, want the unfiltered 3", n)
	}
}

// The cache is loaded at start, filtered with the current YAML.
func TestLoadCached(t *testing.T) {
	tests := []struct {
		name        string
		write       func(path string) error
		wantErr     bool
		wantVersion string
	}{
		{"no file", func(string) error { return nil }, false, ""},
		{"good", func(p string) error {
			return SaveCache(p, Cached{ETag: exampleETag, ValidatedAt: t0, Document: []byte(exampleDoc)})
		}, false, "p_fictitious"},
		{"corrupt file", func(p string) error { return writeFile(p, "{not json") }, true, ""},
		{"unknown format", func(p string) error { return writeFile(p, `{"format":9,"document":{}}`) }, true, ""},
		{"invalid document", func(p string) error {
			return SaveCache(p, Cached{ETag: exampleETag, ValidatedAt: t0, Document: []byte(`{"schema_version":1}`)})
		}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t, "shadow")
			cfg.Routing.AllowedProviders = []string{"gamma"}
			r := newTestRefresher(t, "http://control.example", refresherOpts{cfg: cfg})
			if err := tt.write(r.cfg.CachePath); err != nil {
				t.Fatal(err)
			}

			err := r.LoadCached()

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			a := r.Current()
			if tt.wantVersion == "" {
				if a != nil {
					t.Errorf("active = %q, want none", a.Version())
				}
				return
			}
			if a == nil || a.Version() != tt.wantVersion || !a.ValidatedAt.Equal(t0) || a.ETag != exampleETag {
				t.Fatalf("active = %+v", a)
			}
			m, _ := a.Model("fict-model")
			if got := endpointIDs(m.Candidates); len(got) != 2 {
				t.Errorf("candidates = %v, want filtered to gamma", got)
			}
		})
	}
}

// Readers on the hot path see whole policies while Refresh
// publishes new ones (run with -race).
func TestConcurrentReadersDuringRefresh(t *testing.T) {
	v2 := mutateDoc(t, func(d map[string]any) { d["version"] = "p_fictitious2" })
	fc := newFakeControl(t, http.StatusOK, []byte(exampleDoc), "")
	r := newTestRefresher(t, fc.srv.URL, refresherOpts{noCache: true})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if a := r.Current(); a != nil {
					if v := a.Version(); v != "p_fictitious" && v != "p_fictitious2" {
						t.Errorf("version = %q", v)
					}
					_, _, _ = a.Resolve("acme", "acme/fict-model")
				}
			}
		}()
	}
	for i := range 20 {
		body := []byte(exampleDoc)
		if i%2 == 1 {
			body = v2
		}
		fc.set(http.StatusOK, body, "")
		if err := r.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	wg.Wait()
}

func TestNewRefresherValidates(t *testing.T) {
	tests := []struct {
		name string
		url  string
		key  logging.Secret
	}{
		{"relative url", "/v1", testKey},
		{"bad scheme", "ftp://control.example", testKey},
		{"not an sk key", "https://control.example", "pk_fict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewRefresher(RefresherConfig{BaseURL: tt.url, APIKey: tt.key}); err == nil {
				t.Error("err = nil")
			}
		})
	}
}

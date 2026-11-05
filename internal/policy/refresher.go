package policy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/logging"
)

// Defaults for RefresherConfig zero values.
const (
	DefaultTimeout     = 10 * time.Second
	DefaultBaseBackoff = 5 * time.Second
	// MaxDocumentBytes bounds the body of a 200.
	MaxDocumentBytes = 8 << 20
)

const (
	policyPath         = "/v1/policy"
	schemaHeader       = "X-Sidecar-Schema-Version"
	keyPrefix          = "sk_"
	dialTimeout        = 5 * time.Second
	tlsTimeout         = 5 * time.Second
	idleTimeout        = 90 * time.Second
	maxErrBodyBytes    = 64 << 10
	jitterFraction     = 10 // the refresh interval varies by ±1/jitterFraction
	backoffJitterShare = 2  // a backoff wait is in [limit/2, limit)
)

var (
	// ErrUpgradeRequired means the control plane answered 426. Refreshing stops
	// until the sidecar restarts; the active policy is kept.
	ErrUpgradeRequired = errors.New("policy: control plane requires a newer sidecar (426)")
	// ErrUnauthorized means the control plane rejected control_plane.api_key (401).
	ErrUnauthorized = errors.New("policy: control plane rejected the api key (401)")
	// ErrNotModifiedWithoutPolicy means a 304 arrived but there is no policy to renew.
	ErrNotModifiedWithoutPolicy = errors.New("policy: 304 without an active policy")
	// ErrTooLarge means the document exceeds MaxDocumentBytes.
	ErrTooLarge = errors.New("policy: document too large")
)

// StatusError is an unexpected HTTP status from GET /v1/policy.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return "policy: control plane answered " + strconv.Itoa(e.Code) }

// RefresherConfig configures a Refresher.
type RefresherConfig struct {
	BaseURL     string         // control_plane.url; /v1/policy is appended
	APIKey      logging.Secret // control_plane.api_key (sk_…); only sent to BaseURL
	Config      config.Config  // the client's YAML, for Filter (rule 8)
	CachePath   string         // see CachePath; "" disables the disk cache
	Interval    time.Duration  // control_plane.policy_refresh; default config.DefaultPolicyRefresh
	Timeout     time.Duration  // per request when HTTPClient is nil; default DefaultTimeout
	BaseBackoff time.Duration  // first wait after a failure; default DefaultBaseBackoff
	HTTPClient  *http.Client   // default: own client and transport with explicit timeouts
	Logger      *slog.Logger   // default discards; use one from internal/logging
}

// Refresher downloads the policy in the background and publishes the last
// valid one. Requests from the agent only call Current, which never blocks
// and never does I/O (ADR 0001).
type Refresher struct {
	cfg     RefresherConfig
	url     string
	http    *http.Client
	log     *slog.Logger
	holder  Holder
	stopped atomic.Bool

	// Test seams; never nil.
	now    func() time.Time
	randN  func(n time.Duration) time.Duration
	sleep  func(ctx context.Context, d time.Duration) error
	onTick func() // called after each Refresh in Run; tests only
}

// NewRefresher validates cfg and returns a Refresher with no policy.
func NewRefresher(cfg RefresherConfig) (*Refresher, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("policy: BaseURL must be an absolute http(s) URL")
	}
	if !strings.HasPrefix(cfg.APIKey.Reveal(), keyPrefix) {
		return nil, errors.New("policy: APIKey must be an sk_ key")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = config.DefaultPolicyRefresh
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = DefaultBaseBackoff
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = newHTTPClient(cfg.Timeout)
	}
	return &Refresher{
		cfg:    cfg,
		url:    strings.TrimRight(cfg.BaseURL, "/") + policyPath,
		http:   hc,
		log:    cfg.Logger,
		now:    time.Now,
		randN:  randN,
		sleep:  sleepCtx,
		onTick: func() {},
	}, nil
}

func newHTTPClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: timeout,
		IdleConnTimeout:       idleTimeout,
		MaxIdleConnsPerHost:   1,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// Current returns the active policy, or nil without one. Lock-free.
func (r *Refresher) Current() *Active { return r.holder.Load() }

// Holder exposes the publication point, for readers that take a *Holder.
func (r *Refresher) Holder() *Holder { return &r.holder }

// Stopped reports whether the control plane answered 426.
func (r *Refresher) Stopped() bool { return r.stopped.Load() }

// LoadCached publishes the policy cached on disk, if any. A missing file is
// not an error. An unreadable or invalid cache leaves no policy.
func (r *Refresher) LoadCached() error {
	if r.cfg.CachePath == "" {
		return nil
	}
	c, err := LoadCache(r.cfg.CachePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	p, err := Decode(c.Document)
	if err != nil {
		return fmt.Errorf("policy cache: %w", err)
	}
	r.publish(p, c.Document, c.ETag, c.ValidatedAt)
	return nil
}

// Run refreshes now and then every Interval ±10 %, backing off after
// failures, until ctx is done or the control plane answers 426. Run it in
// its own goroutine.
func (r *Refresher) Run(ctx context.Context) {
	failures := 0
	for {
		err := r.Refresh(ctx)
		r.onTick()
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, ErrUpgradeRequired):
			return
		case err != nil:
			failures++
			r.log.WarnContext(ctx, "policy: refresh failed; keeping the active policy",
				"err", err, "failures", failures, "policy_version", r.version())
		default:
			failures = 0
		}
		if r.sleep(ctx, r.nextDelay(failures)) != nil {
			return
		}
	}
}

func (r *Refresher) version() string {
	if a := r.Current(); a != nil {
		return a.Version()
	}
	return ""
}

// nextDelay is the wait before the next refresh: Interval ±10 % after a
// success; after n failures, min(Interval, BaseBackoff·2^(n-1)) with jitter
// in its upper half, so a broken control plane is never hammered.
func (r *Refresher) nextDelay(failures int) time.Duration {
	iv := r.cfg.Interval
	if failures == 0 {
		spread := iv / jitterFraction
		return iv - spread + r.randN(2*spread+1)
	}
	limit := r.cfg.BaseBackoff
	for i := 1; i < failures && limit < iv; i++ {
		limit *= 2
	}
	limit = min(limit, iv)
	half := limit / backoffJitterShare
	return limit - half + r.randN(half+1)
}

// Refresh makes one request and publishes the result. On any error the
// active policy and the disk cache stay as they were.
func (r *Refresher) Refresh(ctx context.Context) error {
	if r.stopped.Load() {
		return ErrUpgradeRequired
	}
	cur := r.Current()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, http.NoBody)
	if err != nil {
		return fmt.Errorf("policy: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.cfg.APIKey.Reveal())
	req.Header.Set(schemaHeader, strconv.Itoa(SchemaVersion))
	req.Header.Set("Accept", "application/json")
	if cur != nil && cur.ETag != "" {
		req.Header.Set("If-None-Match", cur.ETag)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("policy: get: %w", withoutURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return r.accept(resp)
	case http.StatusNotModified:
		return r.renew(cur, resp.Header.Get("ETag"))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrBodyBytes))
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusUpgradeRequired:
		if !r.stopped.Swap(true) {
			r.log.ErrorContext(ctx, "policy: control plane answered 426; refresh stopped until the sidecar is upgraded",
				"schema_version", SchemaVersion, "policy_version", r.version())
		}
		return ErrUpgradeRequired
	}
	return &StatusError{Code: resp.StatusCode}
}

// accept decodes a 200, caches it and publishes it.
func (r *Refresher) accept(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxDocumentBytes+1))
	if err != nil {
		return fmt.Errorf("policy: read body: %w", withoutURL(err))
	}
	if len(body) > MaxDocumentBytes {
		return ErrTooLarge
	}
	p, err := Decode(body)
	if err != nil {
		return err
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		etag = fmt.Sprintf("%q", fmt.Sprintf("%d:%s", p.SchemaVersion, p.Version))
	}
	now := r.now()
	r.save(body, etag, now)
	a := r.publish(p, body, etag, now)
	r.log.Info("policy: published", "policy_version", p.Version, "models", len(p.Models), "discards", map[Reason]int(a.Discards))
	return nil
}

// renew handles a 304: the active policy is valid as of now.
func (r *Refresher) renew(cur *Active, etag string) error {
	if cur == nil {
		return ErrNotModifiedWithoutPolicy
	}
	if etag != "" && etag != cur.ETag {
		return fmt.Errorf("policy: 304 for another etag: %w", ErrInvalid)
	}
	a := cur.renewed(r.now())
	r.save(a.raw, a.ETag, a.ValidatedAt)
	r.holder.Store(a)
	return nil
}

// save writes the cache. A failure is logged and does not block publishing:
// the policy in memory is still valid.
func (r *Refresher) save(raw []byte, etag string, at time.Time) {
	if r.cfg.CachePath == "" {
		return
	}
	if err := SaveCache(r.cfg.CachePath, Cached{ETag: etag, ValidatedAt: at, Document: raw}); err != nil {
		r.log.Warn("policy: cache not saved; the policy stays active in memory", "err", err)
	}
}

// publish filters p against the YAML (rule 8) and makes it the active policy.
func (r *Refresher) publish(p Policy, raw []byte, etag string, at time.Time) *Active {
	fp, d := Filter(p, r.cfg.Config)
	a := newActive(fp, d, etag, at)
	a.raw = raw
	r.holder.Store(a)
	return a
}

// withoutURL drops the request URL from a transport error: *url.Error prints
// it, and the caller logs the error.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

func randN(n time.Duration) time.Duration {
	if n <= 0 {
		return 0
	}
	return rand.N(n) //nolint:gosec // refresh jitter does not need cryptographic randomness
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("policy: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/logging"
	"github.com/Foncifelo/sidecar-proxy/internal/version"
)

// Defaults for Config zero values.
const (
	DefaultTimeout        = 10 * time.Second
	DefaultMaxAttempts    = 5
	DefaultBaseBackoff    = 500 * time.Millisecond
	DefaultMaxBackoff     = 30 * time.Second
	DefaultMaxRetryAfter  = 5 * time.Minute
	DefaultHeartbeatEvery = 60 * time.Second

	ingestPath   = "/v1/ingest"
	keyPrefix    = "sk_"
	maxRespBytes = 64 << 10
	dialTimeout  = 5 * time.Second
	tlsTimeout   = 5 * time.Second
	idleTimeout  = 90 * time.Second

	maxRetryAfterSecs = 24 * 60 * 60 // overflow guard only
)

// Errors returned by Send.
var (
	// ErrUpgradeRequired means the control plane answered 426: this sidecar's
	// schema_version is no longer accepted. The client stops sending for good.
	ErrUpgradeRequired = errors.New("ingest: control plane requires a newer sidecar (426)")
	// ErrIncoherentAck means a 202 whose body has no valid accepted_n.
	ErrIncoherentAck = errors.New("ingest: 202 without a coherent accepted_n")
)

// StatusError is a non-202 answer from the control plane.
type StatusError struct {
	Code       int
	RetryAfter time.Duration // parsed Retry-After, 0 if absent
}

func (e *StatusError) Error() string {
	return "ingest: control plane answered " + strconv.Itoa(e.Code)
}

// QueueStats is what the heartbeat reads from the local queue; *events.Queue implements it.
type QueueStats interface {
	Depth() int
	DroppedEvents() uint64
}

// State is the sidecar state reported in each heartbeat.
type State struct {
	Mode          events.Mode
	PolicyVersion string
	Inflight      int64
	// PolicyAge is the age of the active policy; ignored when PolicyVersion
	// is empty (no policy).
	PolicyAge time.Duration
}

// Config configures a Client. Zero values take the defaults above.
type Config struct {
	BaseURL        string         // control plane base URL; /v1/ingest is appended
	APIKey         logging.Secret // sk_… key; required
	SidecarVersion string         // default version.Version
	HTTPClient     *http.Client   // default: own client and transport with explicit timeouts
	Timeout        time.Duration  // per HTTP attempt when HTTPClient is nil
	MaxAttempts    int            // attempts per Send before giving the batch back
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	MaxRetryAfter  time.Duration // cap applied to Retry-After
	HeartbeatEvery time.Duration
	Queue          QueueStats   // optional; heartbeat queue_depth and dropped_events
	State          func() State // optional; heartbeat mode, policy_version, inflight
	Logger         *slog.Logger // default discards; use one from internal/logging
}

// Client posts envelopes to the control plane. It is safe for concurrent use.
type Client struct {
	cfg      Config
	url      string
	http     *http.Client
	log      *slog.Logger
	ids      *events.IDGen
	started  time.Time
	stopped  atomic.Bool
	stopOnce sync.Once

	// Test seams; never nil.
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func(limit time.Duration) time.Duration
	now    func() time.Time
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("ingest: BaseURL must be an absolute http(s) URL")
	}
	if !strings.HasPrefix(cfg.APIKey.Reveal(), keyPrefix) {
		return nil, errors.New("ingest: APIKey must be an sk_ key")
	}
	cfg = withDefaults(cfg)
	hc := cfg.HTTPClient
	if hc == nil {
		hc = newHTTPClient(cfg.Timeout)
	}
	return &Client{
		cfg:     cfg,
		url:     strings.TrimRight(cfg.BaseURL, "/") + ingestPath,
		http:    hc,
		log:     cfg.Logger,
		ids:     events.NewIDGen(nil),
		started: time.Now(),
		sleep:   sleepCtx,
		jitter:  fullJitter,
		now:     time.Now,
	}, nil
}

func withDefaults(cfg Config) Config {
	if cfg.SidecarVersion == "" {
		cfg.SidecarVersion = version.Version
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = DefaultBaseBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	if cfg.MaxRetryAfter <= 0 {
		cfg.MaxRetryAfter = DefaultMaxRetryAfter
	}
	if cfg.HeartbeatEvery <= 0 {
		cfg.HeartbeatEvery = DefaultHeartbeatEvery
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return cfg
}

func newHTTPClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: timeout,
		IdleConnTimeout:       idleTimeout,
		MaxIdleConnsPerHost:   2,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// Stopped reports whether the control plane answered 426 and sending stopped.
func (c *Client) Stopped() bool { return c.stopped.Load() }

// Send delivers one queue batch. It has the events.Sink signature: a nil error
// means 202 with a coherent accepted_n, and the batcher acknowledges the batch.
func (c *Client) Send(ctx context.Context, b events.Batch) error {
	items := make([]Item, len(b.Events))
	for i := range b.Events {
		items[i] = Item{Type: ItemRequest, Event: &b.Events[i]}
	}
	return c.deliver(ctx, b.ID, items)
}

// deliver sends items under batchID, retrying transient failures and splitting on 413.
func (c *Client) deliver(ctx context.Context, batchID string, items []Item) error {
	for attempt := 0; ; attempt++ {
		if c.stopped.Load() {
			return ErrUpgradeRequired
		}
		err := c.post(ctx, batchID, items)
		if err == nil {
			return nil
		}
		var se *StatusError
		isStatus := errors.As(err, &se)
		switch {
		case isStatus && se.Code == http.StatusRequestEntityTooLarge:
			return c.split(ctx, batchID, items)
		case isStatus && se.Code == http.StatusConflict:
			// batch_id taken by another tenant: no retry can succeed, so the
			// batcher sets the batch aside instead of blocking the queue.
			return fmt.Errorf("%w: %w", events.ErrRejected, err)
		case isStatus && se.Code == http.StatusUpgradeRequired:
			c.stop()
			return ErrUpgradeRequired
		case !retryable(err):
			return err
		case attempt+1 >= c.cfg.MaxAttempts:
			return fmt.Errorf("ingest: giving up after %d attempts: %w", attempt+1, err)
		}
		if serr := c.sleep(ctx, c.wait(attempt, se)); serr != nil {
			return fmt.Errorf("ingest: retry aborted: %w (last error: %w)", serr, err)
		}
	}
}

// split halves a batch rejected with 413. Child IDs are deterministic
// (<id>.0, <id>.1) so a redelivered batch splits into the same idempotency keys.
func (c *Client) split(ctx context.Context, batchID string, items []Item) error {
	if len(items) <= 1 {
		// A single item over the server limit can never succeed; keeping it would
		// block the queue forever, so it is dropped and reported.
		c.log.ErrorContext(ctx, "ingest: dropping item rejected as too large", "batch_id", batchID, "items", len(items))
		return nil
	}
	mid := len(items) / 2
	if err := c.deliver(ctx, batchID+".0", items[:mid]); err != nil {
		return err
	}
	return c.deliver(ctx, batchID+".1", items[mid:])
}

func (c *Client) stop() {
	c.stopped.Store(true)
	c.stopOnce.Do(func() {
		c.log.Error("ingest: control plane answered 426; sending stopped until the sidecar is upgraded",
			"schema_version", events.SchemaVersion, "sidecar_version", c.cfg.SidecarVersion)
	})
}

// retryable reports whether err is transient: network errors, 408, 429 and 5xx.
func retryable(err error) bool {
	if errors.Is(err, ErrIncoherentAck) || errors.Is(err, errEncode) {
		return false
	}
	var se *StatusError
	if !errors.As(err, &se) {
		return true // transport error
	}
	return se.Code == http.StatusRequestTimeout || se.Code == http.StatusTooManyRequests || se.Code >= http.StatusInternalServerError
}

// wait is the delay before retry number attempt+1: Retry-After on 429 when
// present (capped), otherwise exponential backoff with full jitter.
func (c *Client) wait(attempt int, se *StatusError) time.Duration {
	if se != nil && se.Code == http.StatusTooManyRequests && se.RetryAfter > 0 {
		return min(se.RetryAfter, c.cfg.MaxRetryAfter)
	}
	return c.jitter(backoffLimit(c.cfg.BaseBackoff, c.cfg.MaxBackoff, attempt))
}

// backoffLimit is min(maxD, base·2^attempt) without overflow.
func backoffLimit(base, maxD time.Duration, attempt int) time.Duration {
	d := base
	for range attempt {
		if d >= maxD/2 {
			return maxD
		}
		d *= 2
	}
	return min(d, maxD)
}

func fullJitter(limit time.Duration) time.Duration {
	if limit <= 0 {
		return 0
	}
	return rand.N(limit) //nolint:gosec // retry jitter does not need cryptographic randomness
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("ingest: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

var errEncode = errors.New("ingest: encode envelope")

// withoutURL drops the request URL from a transport error: *url.Error prints
// it with user and query, and the caller logs the error.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

// post makes one HTTP attempt. Bodies and headers are never logged (CONTRIBUTING.md rule 2).
func (c *Client) post(ctx context.Context, batchID string, items []Item) error {
	body, err := json.Marshal(Envelope{
		SchemaVersion:  events.SchemaVersion,
		SidecarVersion: c.cfg.SidecarVersion,
		BatchID:        batchID,
		Items:          items,
	})
	if err != nil {
		return fmt.Errorf("%w: %w", errEncode, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ingest: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey.Reveal())
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ingest: post batch: %w", withoutURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBytes))
		return &StatusError{Code: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), c.now())}
	}
	return c.checkAck(ctx, resp.Body, batchID, len(items))
}

// checkAck accepts a 202 only when accepted_n is present and 0 ≤ n ≤ sent.
// n < sent means the server rejected items deterministically; resending would not help.
func (c *Client) checkAck(ctx context.Context, r io.Reader, batchID string, sent int) error {
	var res Response
	if err := json.NewDecoder(io.LimitReader(r, maxRespBytes)).Decode(&res); err != nil {
		return fmt.Errorf("%w: %w", ErrIncoherentAck, err)
	}
	if res.AcceptedN == nil || *res.AcceptedN < 0 || *res.AcceptedN > sent {
		return fmt.Errorf("%w: batch %s sent %d items", ErrIncoherentAck, batchID, sent)
	}
	if n := *res.AcceptedN; n < sent {
		c.log.WarnContext(ctx, "ingest: control plane accepted fewer items than sent", "batch_id", batchID, "sent", sent, "accepted_n", n)
	}
	return nil
}

// parseRetryAfter reads delta-seconds or an HTTP date; 0 when absent or invalid.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseInt(v, 10, 64); err == nil {
		if s <= 0 {
			return 0
		}
		return time.Duration(min(s, maxRetryAfterSecs)) * time.Second // Client.wait applies MaxRetryAfter
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

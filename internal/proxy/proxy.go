package proxy

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/logging"
	"github.com/Foncifelo/sidecar-proxy/internal/metrics"
	"github.com/Foncifelo/sidecar-proxy/internal/policy"
)

// Response headers set on every answer (docs/contrato-proxy.md, guarantee 3).
const (
	HeaderRequestID = "X-Sidecar-Request-Id"
	HeaderMode      = "X-Sidecar-Mode"
	HeaderProvider  = "X-Sidecar-Provider"
	HeaderEndpoint  = "X-Sidecar-Endpoint"
	HeaderDecision  = "X-Sidecar-Decision"
	HeaderFallback  = "X-Sidecar-Fallback"
)

// UpstreamEndpointID identifies the YAML upstream in headers and events. It is
// policy.UpstreamEndpointID, so plans and events use the same id.
const UpstreamEndpointID = policy.UpstreamEndpointID

// Decision reasons written to X-Sidecar-Decision and Event.DecisionReason.
const (
	ReasonNoPolicy     = "no_policy"
	ReasonPassthrough  = "passthrough"
	ReasonUnauthorized = "unauthorized"
	ReasonOverloaded   = "overloaded"
	ReasonBodyTooLarge = "body_too_large"
	ReasonUpstreamErr  = "upstream_error"
	// ReasonUnknownModel: a policy is active but raw_model does not resolve
	// through the aliases of upstream.provider.
	ReasonUnknownModel = "unknown_model"
)

const retryAfterSeconds = "1"

// Sink receives one event per measured request. *events.Queue implements it.
type Sink interface {
	Append(ev events.Event) error
}

// Upstream is the provider the agent used before the sidecar, from the YAML.
type Upstream struct {
	Provider string
	BaseURL  string // OpenAI-compatible base, e.g. https://api.together.xyz/v1
	APIKey   logging.Secret
}

// Options configures a Handler.
type Options struct {
	Mode config.Mode
	// Route is required in route mode and ignored otherwise.
	Route    *Route
	Upstream Upstream
	// Client talks to the upstream. It must not set a global Timeout (streams);
	// timeouts live in its transport.
	Client       *http.Client
	Sink         Sink // required in shadow mode
	IDs          *events.IDGen
	MaxBodyBytes int64
	MaxInflight  int
	// InboundToken, when set, is the Bearer every request must carry.
	InboundToken logging.Secret
	Logger       *slog.Logger
	Version      string
	Now          func() time.Time // nil means time.Now
	// WriteTimeout is the deadline of each write to the agent, renewed per
	// chunk (limits.stream_write_timeout). Zero means no deadline.
	WriteTimeout time.Duration
	// StreamIdleTimeout is the longest the upstream may stay silent while
	// sending a body (limits.stream_idle_timeout); past it the response is cut
	// with event: error and stream_state = truncated. Zero means no limit.
	StreamIdleTimeout time.Duration
	// Policy returns the active policy, already filtered against the YAML,
	// or nil without one. nil means no policy. Read once per request, in
	// shadow mode only; it must not block.
	Policy func() *policy.Active
	// MinUptime1d is routing.min_uptime_1d; the effective threshold is never
	// below decision.DefaultMinUptime1d.
	MinUptime1d float64
	// MaxTTFTP95 is routing.max_ttft_p95_ms; zero means no YAML limit. It
	// tightens the policy's client_ttft_p95_ms, never loosens it.
	MaxTTFTP95 time.Duration
	// Metrics, when set, receives the /metrics series; nil records nothing.
	Metrics *metrics.Sidecar
}

// Handler is the OpenAI-compatible proxy for /v1/*.
type Handler struct {
	o        Options
	inflight chan struct{}
	drops    *dropLog
}

// New validates o and returns a Handler.
func New(o Options) (*Handler, error) {
	switch o.Mode {
	case config.ModeShadow:
		if o.Sink == nil {
			return nil, errors.New("proxy: shadow mode needs an event sink")
		}
	case config.ModePassthrough:
	case config.ModeRoute:
		if o.Sink == nil {
			return nil, errors.New("proxy: route mode needs an event sink")
		}
		if o.Route == nil || o.Route.Transports == nil {
			return nil, errors.New("proxy: route mode needs route options with transports")
		}
		if o.Route.FirstByteTimeout < 0 || o.Route.NonStreamFirstByteTimeout < 0 {
			return nil, errors.New("proxy: first byte timeout must not be negative")
		}
	default:
		return nil, fmt.Errorf("proxy: unknown mode %q", o.Mode)
	}
	switch {
	case o.Client == nil:
		return nil, errors.New("proxy: nil http client")
	case o.Upstream.BaseURL == "" || o.Upstream.Provider == "":
		return nil, errors.New("proxy: upstream provider and base_url are required")
	case o.MaxBodyBytes <= 0 || o.MaxInflight <= 0:
		return nil, errors.New("proxy: max body and max inflight must be positive")
	case o.WriteTimeout < 0 || o.StreamIdleTimeout < 0:
		return nil, errors.New("proxy: write and stream idle timeouts must not be negative")
	}
	if o.IDs == nil {
		o.IDs = events.NewIDGen(nil)
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	o.Upstream.BaseURL = strings.TrimRight(o.Upstream.BaseURL, "/")
	return &Handler{o: o, inflight: make(chan struct{}, o.MaxInflight), drops: &dropLog{}}, nil
}

func (h *Handler) measuring() bool {
	return h.o.Mode == config.ModeShadow || h.o.Mode == config.ModeRoute
}

func (h *Handler) baseReason() string {
	if h.measuring() {
		return ReasonNoPolicy
	}
	return ReasonPassthrough
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := h.o.Now()
	id, err := h.o.IDs.New(start)
	if err != nil {
		h.o.Logger.Error("request id", "err", err)
	}
	h.setHeaders(w, id, h.baseReason())
	h.o.Metrics.Request(string(kindOf(r)))

	if !h.authorized(r) {
		h.setHeaders(w, id, ReasonUnauthorized)
		writeLocalError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid sidecar inbound token")
		return
	}
	req := newRequestState(id, start, r)
	h.setDecision(w, req, h.baseReason())

	select {
	case h.inflight <- struct{}{}:
		defer func() { <-h.inflight }()
	default:
		w.Header().Set("Retry-After", retryAfterSeconds)
		h.localError(w, req, http.StatusServiceUnavailable, ReasonOverloaded, "too many requests in flight")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.o.MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.localError(w, req, http.StatusRequestEntityTooLarge, ReasonBodyTooLarge, "request body exceeds limits.max_body_mb")
			return
		}
		h.localError(w, req, http.StatusBadRequest, "bad_request", "could not read request body")
		return
	}
	req.inspect(body)
	if h.routable(req) {
		h.serveRoute(w, r, req, body)
		return
	}
	if h.o.Mode == config.ModeRoute {
		h.setDecision(w, req, ReasonNotRouted)
	}
	h.decide(w, req)
	h.forward(w, r, req, body)
}

func (h *Handler) authorized(r *http.Request) bool {
	if h.o.InboundToken == "" {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(h.o.InboundToken.Reveal())) == 1
}

func (h *Handler) setHeaders(w http.ResponseWriter, id, reason string) {
	hd := w.Header()
	hd.Set(HeaderRequestID, id)
	hd.Set(HeaderMode, string(h.o.Mode))
	hd.Set(HeaderProvider, h.o.Upstream.Provider)
	hd.Set(HeaderEndpoint, UpstreamEndpointID)
	hd.Set(HeaderDecision, reason)
	hd.Set(HeaderFallback, "0")
}

// setDecision sets X-Sidecar-Decision and the event's decision_reason
// together, so both always carry the same value.
func (h *Handler) setDecision(w http.ResponseWriter, req *requestState, reason string) {
	w.Header().Set(HeaderDecision, reason)
	req.reason = reason
}

// localError answers without contacting the upstream and records the event.
func (h *Handler) localError(w http.ResponseWriter, req *requestState, status int, reason, msg string) {
	h.setDecision(w, req, reason)
	writeLocalError(w, status, reason, msg)
	req.status = status
	req.errorType = reason
	req.local = true
	h.emit(req)
}

func writeLocalError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"message":%q,"type":%q}}`+"\n", msg, typ)
}

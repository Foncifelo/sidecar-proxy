// Command sidecar is the local OpenAI-compatible proxy that sits next to the agent.
//
// It loads sidecar.yaml (--config; --mode, --listen and --inbound-token override
// it), serves /healthz and /version, mounts the proxy (internal/proxy) on /v1/
// and shuts down gracefully. Without an upstream in the YAML /v1/* answers 501.
// metrics_listen serves GET /metrics (internal/metrics) in the Prometheus text
// format.
// In shadow mode events go to the on-disk queue and, when control_plane.url is
// set, are sent in batches to the control plane's POST /v1/ingest. With
// control_plane.url every mode sends heartbeats. With control_plane.url the policy is refreshed from GET
// /v1/policy in the background and cached next to queue_dir. On SIGTERM
// it drains open streams, then sends what it can within eventFlushTimeout; the
// rest stays on disk for the next start.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/logging"
	"github.com/Foncifelo/sidecar-proxy/internal/metrics"
	"github.com/Foncifelo/sidecar-proxy/internal/proxy"
	"github.com/Foncifelo/sidecar-proxy/internal/transport"
	"github.com/Foncifelo/sidecar-proxy/internal/version"
)

const (
	defaultConfigPath = "sidecar.yaml"
	drainTimeout      = 30 * time.Second
	defaultQueueDir   = "./data/queue"
	bytesPerMB        = 1 << 20
	// upstreamHeaderTimeout bounds the wait for response headers in shadow and
	// every mode: non-streamed completions only send headers when generation
	// ends. Route mode falls back earlier with limits.first_byte_timeout.
	upstreamHeaderTimeout = 5 * time.Minute
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sidecar:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("sidecar", flag.ContinueOnError)
	var (
		configPath    = fs.String("config", defaultConfigPath, "path to sidecar.yaml")
		mode          = fs.String("mode", "", "shadow | route | passthrough (overrides the file)")
		listen        = fs.String("listen", "", "address for the OpenAI-compatible API (overrides the file)")
		metricsListen = fs.String("metrics-listen", "", "address for /metrics (overrides the file)")
		inboundToken  = fs.String("inbound-token", "", "required when listen is not loopback (overrides the file; or SIDECAR_INBOUND_TOKEN)")
		showVersion   = fs.Bool("version", false, "print version and exit")
	)
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("parse flags: %w", err)
	}

	if *showVersion {
		fmt.Println(version.String())
		return nil
	}

	cfg, err := config.Load(loadOptions(fs, flagValues{
		configPath:    *configPath,
		mode:          *mode,
		listen:        *listen,
		metricsListen: *metricsListen,
		inboundToken:  *inboundToken,
	}, os.LookupEnv))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := logging.New(os.Stderr, logging.Options{Level: slog.LevelInfo, JSON: true}).
		With("component", "sidecar", "version", version.Version, "mode", string(cfg.Mode))
	slog.SetDefault(log)
	logHTTPLoopbackException(log, cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, version.String()) })
	m := metrics.New()
	v1, closeV1, err := newV1Handler(cfg, log, m)
	if err != nil {
		return err
	}
	mux.Handle("/v1/", v1)

	api := newAPIServer(cfg.Listen, mux, cfg.Limits)
	metricsSrv := newMetricsServer(cfg.MetricsListen, metricsMux(m), cfg.Limits)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	go func() { errCh <- serve(api) }()
	go func() { errCh <- serve(metricsSrv) }()
	log.Info("listening", "api", cfg.Listen, "metrics", cfg.MetricsListen)

	var serveErr error
	select {
	case serveErr = <-errCh:
	case <-ctx.Done():
	}
	return errors.Join(serveErr, shutdown(log, closeV1, api, metricsSrv))
}

// logHTTPLoopbackException says once at startup that ADR 0011 is in use,
// naming the providers by id only: the base_url is never logged.
func logHTTPLoopbackException(log *slog.Logger, cfg config.Config) {
	ids := cfg.HTTPLoopbackProviders()
	if len(ids) == 0 {
		return
	}
	log.Warn("http base_url on loopback allowed by SIDECAR_ENV (ADR 0011)", "providers", ids)
}

// shutdown stops the servers first, so open streams can still write their
// events, and then flushes the event queue within eventFlushTimeout.
func shutdown(log *slog.Logger, closeV1 func(context.Context), servers ...*http.Server) error {
	log.Info("shutting down: draining open streams", "timeout", drainTimeout)
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainTimeout)
	defer cancelDrain()
	errs := make([]error, 0, len(servers))
	for _, s := range servers {
		errs = append(errs, s.Shutdown(drainCtx))
	}

	log.Info("shutting down: flushing queued events", "timeout", eventFlushTimeout)
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), eventFlushTimeout)
	defer cancelFlush()
	closeV1(flushCtx)
	return errors.Join(errs...)
}

// newAPIServer returns the agent-facing server with the limits.* timeouts.
// WriteTimeout stays 0 because SSE streams last as long as generation; the
// proxy handler instead renews a write deadline on every chunk
// (limits.stream_write_timeout) and cuts a silent upstream
// (limits.stream_idle_timeout).
func newAPIServer(addr string, h http.Handler, l config.Limits) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: l.ReadHeaderTimeout.Std(),
		ReadTimeout:       l.ReadTimeout.Std(),
		IdleTimeout:       l.IdleTimeout.Std(),
		WriteTimeout:      0,
	}
}

// metricsMux serves GET /metrics from m; every other path is 404.
func metricsMux(m *metrics.Sidecar) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m)
	return mux
}

// newMetricsServer returns the /metrics server; its answers are short, so the
// write timeout reuses limits.read_timeout.
func newMetricsServer(addr string, h http.Handler, l config.Limits) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: l.ReadHeaderTimeout.Std(),
		ReadTimeout:       l.ReadTimeout.Std(),
		WriteTimeout:      l.ReadTimeout.Std(),
		IdleTimeout:       l.IdleTimeout.Std(),
	}
}

// newV1Handler builds the proxy for /v1/*. With control_plane.url it starts
// the background policy refresh (internal/policy) and, in every mode, ingest
// heartbeats with the real mode and policy; the event queue runs in shadow and route. The returned func stops the
// refresh, flushes and closes the event pipeline within its context and
// closes idle upstream connections. m, which may be nil, receives the
// proxy's series and reads the queue and the policy at scrape time.
func newV1Handler(cfg config.Config, log *slog.Logger, m *metrics.Sidecar) (http.Handler, func(context.Context), error) {
	if cfg.Upstream.BaseURL == "" {
		log.Warn("no upstream in the config: /v1/* answers 501")
		return proxy.NotConfigured(cfg.Mode, nil), func(context.Context) {}, nil
	}
	tset, err := transport.New(transport.Config{Providers: transportProviders(cfg)})
	if err != nil {
		return nil, nil, fmt.Errorf("upstream transport: %w", err)
	}
	rt, _ := tset.Transport(cfg.Upstream.Provider)
	opts := proxy.Options{
		Mode:              cfg.Mode,
		Upstream:          proxy.Upstream{Provider: cfg.Upstream.Provider, BaseURL: cfg.Upstream.BaseURL, APIKey: cfg.Upstream.APIKey},
		Client:            &http.Client{Transport: rt}, // no global Timeout: streams; the transport bounds headers
		MaxBodyBytes:      int64(cfg.Limits.MaxBodyMB) * bytesPerMB,
		MaxInflight:       cfg.Limits.MaxInflight,
		Logger:            log.With("component", "proxy"),
		Version:           version.Version,
		WriteTimeout:      cfg.Limits.StreamWriteTimeout.Std(),
		StreamIdleTimeout: cfg.Limits.StreamIdleTimeout.Std(),
		MinUptime1d:       cfg.Routing.MinUptime1d,
		MaxTTFTP95:        time.Duration(cfg.Routing.MaxTTFTP95Ms) * time.Millisecond,
		Metrics:           m,
	}
	if cfg.Mode == config.ModeRoute {
		opts.Route = &proxy.Route{Config: cfg, Transports: tset, FirstByteTimeout: cfg.Limits.FirstByteTimeout.Std()}
	}
	if !isLoopback(cfg.Listen) {
		opts.InboundToken = cfg.InboundToken
	}
	pol, err := startPolicy(cfg, log)
	if err != nil {
		return nil, nil, err
	}
	if pol != nil {
		opts.Policy = pol.r.Current // lock-free read of the refreshed policy
	}
	p, err := openEventPipeline(cfg.ControlPlane, cfg.Mode, log, heartbeatState(cfg.Mode, pol, time.Now))
	if err != nil {
		pol.Stop()
		return nil, nil, err
	}
	if p.q != nil {
		opts.Sink = p.q
	}
	m.SetSources(metricsSources(p.q, pol, time.Now))
	closeFn := func(ctx context.Context) {
		pol.Stop()
		tset.CloseIdleConnections()
		p.Close(ctx)
	}
	h, err := proxy.New(opts)
	if err != nil {
		// Nothing was served, so nothing new is queued: close without flushing.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		closeFn(ctx)
		return nil, nil, fmt.Errorf("proxy: %w", err)
	}
	return h, closeFn, nil
}

// transportProviders lists one transport per provider the sidecar may call:
// the upstream and, in route mode, every provider in the YAML plus the
// openrouter channel. Non-streamed completions only send headers when
// generation ends, so the header timeout stays long; route mode bounds the
// wait before a fallback with limits.first_byte_timeout in the proxy.
func transportProviders(cfg config.Config) map[string]transport.ProviderConfig {
	pc := transport.ProviderConfig{ResponseHeaderTimeout: upstreamHeaderTimeout}
	out := map[string]transport.ProviderConfig{cfg.Upstream.Provider: pc}
	if cfg.Mode != config.ModeRoute {
		return out
	}
	for name := range cfg.Providers {
		out[name] = pc
	}
	out[config.ProviderOpenRouter] = pc
	return out
}

func serve(s *http.Server) error {
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen %s: %w", s.Addr, err)
	}
	return nil
}

// flagValues are the parsed values of the flags that feed config.Load.
type flagValues struct {
	configPath    string
	mode          string
	listen        string
	metricsListen string
	inboundToken  string
}

// loadOptions turns the flags the user actually set into config overrides, so
// that config.Load validates them like file values. --inbound-token falls back
// to SIDECAR_INBOUND_TOKEN so the token can stay off the command line.
func loadOptions(fs *flag.FlagSet, v flagValues, lookup config.LookupFunc) config.Options {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	o := config.Options{Path: v.configPath, PathExplicit: set["config"], LookupEnv: lookup}
	if set["mode"] {
		m := config.Mode(v.mode)
		o.Overrides.Mode = &m
	}
	if set["listen"] {
		o.Overrides.Listen = &v.listen
	}
	if set["metrics-listen"] {
		o.Overrides.MetricsListen = &v.metricsListen
	}
	token := v.inboundToken
	if !set["inbound-token"] {
		token, _ = lookup("SIDECAR_INBOUND_TOKEN")
	}
	if token != "" {
		s := logging.Secret(token)
		o.Overrides.InboundToken = &s
	}
	return o
}

func isLoopback(addr string) bool { return config.IsLoopback(addr) }

// metricsSources reads the queue and the policy for the /metrics gauges. A
// nil queue reads as empty; a nil runner or no active policy as no policy.
func metricsSources(q *events.Queue, pr *policyRunner, now func() time.Time) metrics.Sources {
	var src metrics.Sources
	if q != nil {
		src.QueueDepth, src.DroppedEvents, src.QueueDiskErrors = q.Depth, q.DroppedEvents, q.DiskErrors
	}
	if pr != nil {
		src.PolicyAge = func() (time.Duration, bool) {
			a := pr.r.Current()
			if a == nil {
				return 0, false
			}
			return a.Age(now()), true
		}
	}
	return src
}

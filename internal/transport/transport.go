package transport

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
)

// Set owns one http.Transport per provider and one Endpoint (bulkhead and
// circuit breaker) per endpoint id. It is safe for concurrent use.
type Set struct {
	transports map[string]*http.Transport
	defaults   EndpointConfig
	overrides  map[string]EndpointConfig
	clock      Clock

	mu        sync.Mutex
	endpoints map[string]*Endpoint
}

// New validates cfg and builds one dedicated http.Transport per provider.
// It never uses or clones http.DefaultTransport.
func New(cfg Config) (*Set, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	clock := cfg.Clock
	if clock == nil {
		clock = systemClock{}
	}
	s := &Set{
		transports: make(map[string]*http.Transport, len(cfg.Providers)),
		defaults:   cfg.EndpointDefaults,
		overrides:  make(map[string]EndpointConfig, len(cfg.Endpoints)),
		clock:      clock,
		endpoints:  make(map[string]*Endpoint),
	}
	for name, p := range cfg.Providers {
		s.transports[name] = newHTTPTransport(p.withDefaults())
	}
	for id, e := range cfg.Endpoints {
		s.overrides[id] = e
	}
	return s, nil
}

func newHTTPTransport(p ProviderConfig) *http.Transport {
	dialer := &net.Dialer{Timeout: p.DialTimeout, KeepAlive: DefaultKeepAlive}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     *p.ForceAttemptHTTP2,
		MaxIdleConns:          p.MaxIdleConnsPerHost,
		MaxIdleConnsPerHost:   p.MaxIdleConnsPerHost,
		MaxConnsPerHost:       p.MaxConnsPerHost,
		ResponseHeaderTimeout: p.ResponseHeaderTimeout,
		IdleConnTimeout:       p.IdleConnTimeout,
		TLSHandshakeTimeout:   p.TLSHandshakeTimeout,
		ExpectContinueTimeout: DefaultExpectContinueTimeout,
	}
}

// Transport returns the dedicated transport of provider.
func (s *Set) Transport(provider string) (*http.Transport, bool) {
	t, ok := s.transports[provider]
	return t, ok
}

// Endpoint returns the bulkhead and breaker of endpoint id, creating them on
// first use from the override for id or the endpoint defaults.
func (s *Set) Endpoint(id string) *Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.endpoints[id]; ok {
		return e
	}
	cfg := s.overrides[id].withDefaults(s.defaults)
	e := &Endpoint{
		ID:       id,
		Bulkhead: NewBulkhead(cfg.MaxInFlight),
		Breaker:  NewBreaker(cfg.FailureThreshold, cfg.Cooldown, s.clock),
	}
	s.endpoints[id] = e
	return e
}

// RoundTripper returns a RoundTripper that sends requests for endpoint id
// through the provider's transport, guarded by the endpoint's breaker and
// bulkhead.
func (s *Set) RoundTripper(provider, id string) (http.RoundTripper, error) {
	t, ok := s.Transport(provider)
	if !ok {
		return nil, fmt.Errorf("transport: unknown provider %q", provider)
	}
	return &guard{next: t, ep: s.Endpoint(id)}, nil
}

// CloseIdleConnections closes idle connections on every provider transport.
func (s *Set) CloseIdleConnections() {
	for _, t := range s.transports {
		t.CloseIdleConnections()
	}
}

// Endpoint groups the per-endpoint bulkhead and circuit breaker.
type Endpoint struct {
	ID       string
	Bulkhead *Bulkhead
	Breaker  *Breaker
}

// IsFailure reports whether a round-trip outcome counts against the breaker:
// transport errors (including the response header timeout), 429 and 5xx.
func IsFailure(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	return resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode >= http.StatusInternalServerError
}

type guard struct {
	next http.RoundTripper
	ep   *Endpoint
}

// RoundTrip asks the breaker, acquires a bulkhead slot, forwards the request
// and records the outcome once headers arrive. The breaker is consulted first
// so an open endpoint rejects without waiting for a slot. The slot is held
// until the response body is closed, so streams count as in flight.
func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	tok, err := g.ep.Breaker.Allow()
	if err != nil {
		return nil, fmt.Errorf("transport: endpoint %q: %w", g.ep.ID, err)
	}
	release, err := g.ep.Bulkhead.Acquire(ctx)
	if err != nil {
		// Never reached the provider; give the probe slot back if we held it.
		g.ep.Breaker.Ignore(tok)
		return nil, fmt.Errorf("transport: endpoint %q: %w", g.ep.ID, err)
	}
	resp, err := g.next.RoundTrip(req)
	switch {
	case err != nil && ctx.Err() != nil:
		// The caller gave up; that says nothing about the provider.
		g.ep.Breaker.Ignore(tok)
	case IsFailure(resp, err):
		g.ep.Breaker.Failure(tok)
	default:
		g.ep.Breaker.Success(tok)
	}
	if err != nil {
		release()
		return nil, fmt.Errorf("transport: endpoint %q: %w", g.ep.ID, err)
	}
	resp.Body = &releasingBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

type releasingBody struct {
	io.ReadCloser
	release func()
}

func (b *releasingBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	if err != nil {
		return errors.Join(errors.New("transport: close body"), err)
	}
	return nil
}

package transport

import (
	"errors"
	"fmt"
	"time"
)

// Default values applied when a field is left at its zero value.
const (
	DefaultMaxIdleConnsPerHost   = 16
	DefaultMaxConnsPerHost       = 64
	DefaultResponseHeaderTimeout = 2 * time.Second
	DefaultIdleConnTimeout       = 90 * time.Second
	DefaultTLSHandshakeTimeout   = 5 * time.Second
	DefaultDialTimeout           = 5 * time.Second
	DefaultKeepAlive             = 30 * time.Second
	DefaultExpectContinueTimeout = 1 * time.Second

	DefaultMaxInFlight      = 32
	DefaultFailureThreshold = 5
	DefaultCooldown         = 30 * time.Second
)

// ProviderConfig tunes the http.Transport dedicated to one provider.
// Zero values take the Default* constants.
type ProviderConfig struct {
	MaxIdleConnsPerHost   int
	MaxConnsPerHost       int
	ResponseHeaderTimeout time.Duration
	IdleConnTimeout       time.Duration
	TLSHandshakeTimeout   time.Duration
	DialTimeout           time.Duration
	// ForceAttemptHTTP2 defaults to true when nil. A custom dialer disables
	// automatic HTTP/2 in net/http, so it has to be requested explicitly.
	ForceAttemptHTTP2 *bool
}

// EndpointConfig tunes the bulkhead and circuit breaker of one endpoint.
// Zero values take the Default* constants.
type EndpointConfig struct {
	MaxInFlight      int
	FailureThreshold int
	Cooldown         time.Duration
}

// Config is the input of New.
type Config struct {
	// Providers maps a provider name to its transport settings. One
	// http.Transport is built per entry.
	Providers map[string]ProviderConfig
	// EndpointDefaults applies to every endpoint without an override.
	EndpointDefaults EndpointConfig
	// Endpoints holds per-endpoint overrides keyed by endpoint id.
	Endpoints map[string]EndpointConfig
	// Clock drives the circuit breakers. Nil means the system clock.
	Clock Clock
}

func (p ProviderConfig) withDefaults() ProviderConfig {
	out := p
	out.MaxIdleConnsPerHost = orInt(p.MaxIdleConnsPerHost, DefaultMaxIdleConnsPerHost)
	out.MaxConnsPerHost = orInt(p.MaxConnsPerHost, DefaultMaxConnsPerHost)
	out.ResponseHeaderTimeout = orDur(p.ResponseHeaderTimeout, DefaultResponseHeaderTimeout)
	out.IdleConnTimeout = orDur(p.IdleConnTimeout, DefaultIdleConnTimeout)
	out.TLSHandshakeTimeout = orDur(p.TLSHandshakeTimeout, DefaultTLSHandshakeTimeout)
	out.DialTimeout = orDur(p.DialTimeout, DefaultDialTimeout)
	if p.ForceAttemptHTTP2 == nil {
		v := true
		out.ForceAttemptHTTP2 = &v
	}
	return out
}

func (p ProviderConfig) validate() error {
	if p.MaxIdleConnsPerHost < 0 || p.MaxConnsPerHost < 0 {
		return errors.New("connection limits must not be negative")
	}
	if p.ResponseHeaderTimeout < 0 || p.IdleConnTimeout < 0 ||
		p.TLSHandshakeTimeout < 0 || p.DialTimeout < 0 {
		return errors.New("timeouts must not be negative")
	}
	return nil
}

func (e EndpointConfig) withDefaults(base EndpointConfig) EndpointConfig {
	return EndpointConfig{
		MaxInFlight:      orInt(e.MaxInFlight, orInt(base.MaxInFlight, DefaultMaxInFlight)),
		FailureThreshold: orInt(e.FailureThreshold, orInt(base.FailureThreshold, DefaultFailureThreshold)),
		Cooldown:         orDur(e.Cooldown, orDur(base.Cooldown, DefaultCooldown)),
	}
}

func (e EndpointConfig) validate() error {
	if e.MaxInFlight < 0 || e.FailureThreshold < 0 || e.Cooldown < 0 {
		return errors.New("endpoint limits must not be negative")
	}
	return nil
}

func (c Config) validate() error {
	if len(c.Providers) == 0 {
		return errors.New("transport: at least one provider is required")
	}
	for name, p := range c.Providers {
		if name == "" {
			return errors.New("transport: provider name must not be empty")
		}
		if err := p.validate(); err != nil {
			return fmt.Errorf("transport: provider %q: %w", name, err)
		}
	}
	if err := c.EndpointDefaults.validate(); err != nil {
		return fmt.Errorf("transport: endpoint defaults: %w", err)
	}
	for id, e := range c.Endpoints {
		if id == "" {
			return errors.New("transport: endpoint id must not be empty")
		}
		if err := e.validate(); err != nil {
			return fmt.Errorf("transport: endpoint %q: %w", id, err)
		}
	}
	return nil
}

func orInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func orDur(v, def time.Duration) time.Duration {
	if v == 0 {
		return def
	}
	return v
}

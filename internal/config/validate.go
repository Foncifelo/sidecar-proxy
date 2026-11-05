package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"
)

// IsLoopback reports whether addr (host:port) binds only to the loopback interface.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return isLoopbackHost(host)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// withDefaults returns a copy of c with every absent value set to its default.
func (c Config) withDefaults() Config {
	out := c
	if out.Mode == "" {
		out.Mode = DefaultMode
	}
	if out.Listen == "" {
		out.Listen = DefaultListen
	}
	if out.MetricsListen == "" {
		out.MetricsListen = DefaultMetricsListen
	}
	if out.Limits.MaxBodyMB == 0 {
		out.Limits.MaxBodyMB = DefaultMaxBodyMB
	}
	if out.Limits.MaxInflight == 0 {
		out.Limits.MaxInflight = DefaultMaxInflight
	}
	out.Limits.FirstByteTimeout = defaultDuration(out.Limits.FirstByteTimeout, DefaultFirstByteTimeout)
	out.Limits.NonStreamFirstByteTimeout = defaultDuration(out.Limits.NonStreamFirstByteTimeout, DefaultNonStreamFirstByteTimeout)
	out.Limits.TTFTP95Slack = defaultDuration(out.Limits.TTFTP95Slack, DefaultTTFTP95Slack)
	out.Limits.ReadHeaderTimeout = defaultDuration(out.Limits.ReadHeaderTimeout, DefaultReadHeaderTimeout)
	out.Limits.ReadTimeout = defaultDuration(out.Limits.ReadTimeout, DefaultReadTimeout)
	out.Limits.IdleTimeout = defaultDuration(out.Limits.IdleTimeout, DefaultIdleTimeout)
	out.Limits.StreamWriteTimeout = defaultDuration(out.Limits.StreamWriteTimeout, DefaultStreamWriteTimeout)
	out.Limits.StreamIdleTimeout = defaultDuration(out.Limits.StreamIdleTimeout, DefaultStreamIdleTimeout)
	out.ControlPlane.PolicyRefresh = defaultDuration(out.ControlPlane.PolicyRefresh, DefaultPolicyRefresh)
	out.Providers = providersWithDefaults(c.Providers)
	return out
}

func defaultDuration(d Duration, def time.Duration) Duration {
	if d.raw == "" {
		return NewDuration(def)
	}
	return d
}

// validateOptions tunes validate for configs that come only from flags.
type validateOptions struct {
	requireUpstream bool
	// allowLoopbackHTTP enables ADR 0011 for providers.<id>.base_url.
	allowLoopbackHTTP bool
}

// validate parses durations and checks every rule, returning a copy with the
// parsed values and all errors joined (never just the first one).
func (c Config) validate(o validateOptions) (Config, error) {
	out := c
	var errs []error

	if !c.Mode.Valid() {
		errs = append(errs, fmt.Errorf("mode: %q is not one of shadow, route, passthrough", c.Mode))
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		errs = append(errs, fmt.Errorf("listen: %q is not host:port", c.Listen))
	} else if !IsLoopback(c.Listen) && c.InboundToken == "" {
		errs = append(errs, errors.New("listen: not loopback, inbound_token is required (CONTRIBUTING.md rule 4)"))
	}
	if _, _, err := net.SplitHostPort(c.MetricsListen); err != nil {
		errs = append(errs, fmt.Errorf("metrics_listen: %q is not host:port", c.MetricsListen))
	}
	errs = append(errs, c.Upstream.validate(o.requireUpstream)...)
	errs = append(errs, validateProviders(c.Providers, o.allowLoopbackHTTP)...)
	if c.Mode == ModeRoute {
		errs = append(errs, c.validateRoute(o.allowLoopbackHTTP)...)
	}

	var err error
	if out.Limits.FirstByteTimeout, err = parseDuration("limits.first_byte_timeout", c.Limits.FirstByteTimeout); err != nil {
		errs = append(errs, err)
	}
	if out.Limits.TTFTP95Slack, err = parseDuration("limits.ttft_p95_slack", c.Limits.TTFTP95Slack); err != nil {
		errs = append(errs, err)
	}
	for _, d := range []struct {
		key string
		dst *Duration
	}{
		{"limits.nonstream_first_byte_timeout", &out.Limits.NonStreamFirstByteTimeout},
		{"limits.read_header_timeout", &out.Limits.ReadHeaderTimeout},
		{"limits.read_timeout", &out.Limits.ReadTimeout},
		{"limits.idle_timeout", &out.Limits.IdleTimeout},
		{"limits.stream_write_timeout", &out.Limits.StreamWriteTimeout},
		{"limits.stream_idle_timeout", &out.Limits.StreamIdleTimeout},
	} {
		if *d.dst, err = parseDuration(d.key, *d.dst); err != nil {
			errs = append(errs, err)
		}
	}
	if out.ControlPlane.PolicyRefresh, err = parseDuration("control_plane.policy_refresh", c.ControlPlane.PolicyRefresh); err != nil {
		errs = append(errs, err)
	}
	if c.Limits.MaxBodyMB < 0 {
		errs = append(errs, fmt.Errorf("limits.max_body_mb: must be positive, got %d", c.Limits.MaxBodyMB))
	}
	if c.Limits.MaxInflight < 0 {
		errs = append(errs, fmt.Errorf("limits.max_inflight: must be positive, got %d", c.Limits.MaxInflight))
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid config:\n%w", err)
	}
	return out, nil
}

func (u Upstream) validate(required bool) []error {
	if u.BaseURL == "" && u.Provider == "" && !required {
		return nil
	}
	var errs []error
	if u.Provider == "" {
		errs = append(errs, errors.New("upstream.provider: required (id of the upstream's provider, e.g. together)"))
	}
	if u.BaseURL == "" {
		return append(errs, errors.New("upstream.base_url: required"))
	}
	parsed, err := url.Parse(u.BaseURL)
	if err != nil || parsed.Host == "" {
		return append(errs, errors.New("upstream.base_url: not an absolute URL"))
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(parsed.Hostname()) {
			errs = append(errs, errors.New("upstream.base_url: http is only allowed for 127.0.0.1 or localhost; use https"))
		}
	default:
		errs = append(errs, fmt.Errorf("upstream.base_url: scheme %q is not https", parsed.Scheme))
	}
	return errs
}

// parseDuration parses d.raw; the value is not secret, so it may be quoted.
func parseDuration(key string, d Duration) (Duration, error) {
	v, err := time.ParseDuration(d.raw)
	if err != nil {
		return Duration{}, fmt.Errorf("%s: %q is not a duration (e.g. 2s, 500ms)", key, d.raw)
	}
	if v <= 0 {
		return Duration{}, fmt.Errorf("%s: must be positive, got %s", key, d.raw)
	}
	return Duration{d: v, raw: d.raw}, nil
}

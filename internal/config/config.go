// Package config loads and validates sidecar.yaml, the client's policy of record.
//
// The YAML is the only source of upstream URLs, providers and keys (CONTRIBUTING.md
// rules 3 and 8). Values may reference environment variables as ${VAR}; keys and
// tokens are held as logging.Secret, and every way of printing or serializing a
// Config (fmt verbs, JSON, YAML) masks them.
package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Foncifelo/sidecar-proxy/internal/logging"
)

// Mode is the operating mode of the sidecar.
type Mode string

// Operating modes (docs/contrato-proxy.md).
const (
	ModeShadow      Mode = "shadow"
	ModeRoute       Mode = "route"
	ModePassthrough Mode = "passthrough"
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool {
	switch m {
	case ModeShadow, ModeRoute, ModePassthrough:
		return true
	default:
		return false
	}
}

// Defaults applied when a key is absent from the YAML.
const (
	DefaultMode             = ModeShadow
	DefaultListen           = "127.0.0.1:8787"
	DefaultMetricsListen    = "127.0.0.1:9797"
	DefaultMaxBodyMB        = 10
	DefaultMaxInflight      = 256
	DefaultFirstByteTimeout = 2 * time.Second
	// DefaultNonStreamFirstByteTimeout bounds headers of a non-streamed
	// routed attempt: without stream they only come with the whole answer.
	DefaultNonStreamFirstByteTimeout = 30 * time.Second
	DefaultTTFTP95Slack              = 500 * time.Millisecond
	DefaultPolicyRefresh             = 60 * time.Second

	// Server and streaming timeouts (limits.*). StreamIdleTimeout is generous
	// on purpose: in shadow mode cutting a legitimate quiet stream is worse
	// than waiting.
	DefaultReadHeaderTimeout  = 10 * time.Second
	DefaultReadTimeout        = 60 * time.Second
	DefaultIdleTimeout        = 120 * time.Second
	DefaultStreamWriteTimeout = 30 * time.Second
	DefaultStreamIdleTimeout  = 120 * time.Second
)

const masked = "***"

// Config mirrors sidecar.example.yaml.
type Config struct {
	Mode          Mode                      `yaml:"mode" json:"mode"`
	Listen        string                    `yaml:"listen" json:"listen"`
	MetricsListen string                    `yaml:"metrics_listen" json:"metrics_listen"`
	InboundToken  logging.Secret            `yaml:"inbound_token" json:"inbound_token"`
	Upstream      Upstream                  `yaml:"upstream" json:"upstream"`
	ControlPlane  ControlPlane              `yaml:"control_plane" json:"control_plane"`
	Limits        Limits                    `yaml:"limits" json:"limits"`
	Routing       Routing                   `yaml:"routing" json:"routing"`
	Providers     map[string]ProviderConfig `yaml:"providers" json:"providers"`
	Tags          Tags                      `yaml:"tags" json:"tags"`

	// LoopbackHTTP enables the ADR 0011 exception (http base_url on literal
	// loopback). Only Load sets it, from SIDECAR_ENV; it cannot come from the
	// YAML or the policy, and a Config built by hand keeps https only.
	LoopbackHTTP bool `yaml:"-" json:"-"`
}

// Upstream is where traffic went before the sidecar existed.
type Upstream struct {
	// Provider is the id of the upstream's provider (e.g. "together").
	Provider string         `yaml:"provider" json:"provider"`
	BaseURL  string         `yaml:"base_url" json:"base_url"`
	APIKey   logging.Secret `yaml:"api_key" json:"api_key"`
}

// ControlPlane configures the connection to the control plane.
type ControlPlane struct {
	URL           string         `yaml:"url" json:"url"`
	APIKey        logging.Secret `yaml:"api_key" json:"api_key"`
	PolicyRefresh Duration       `yaml:"policy_refresh" json:"policy_refresh"`
	QueueDir      string         `yaml:"queue_dir" json:"queue_dir"`
	QueueMaxMB    int            `yaml:"queue_max_mb" json:"queue_max_mb"`
}

// Limits bounds the proxy and sets the fallback triggers.
type Limits struct {
	MaxBodyMB        int      `yaml:"max_body_mb" json:"max_body_mb"`
	MaxInflight      int      `yaml:"max_inflight" json:"max_inflight"`
	FirstByteTimeout Duration `yaml:"first_byte_timeout" json:"first_byte_timeout"`
	// NonStreamFirstByteTimeout replaces FirstByteTimeout for routed
	// requests without stream.
	NonStreamFirstByteTimeout Duration `yaml:"nonstream_first_byte_timeout" json:"nonstream_first_byte_timeout"`
	TTFTP95Slack              Duration `yaml:"ttft_p95_slack" json:"ttft_p95_slack"`
	// ReadHeaderTimeout, ReadTimeout and IdleTimeout feed the sidecar's
	// http.Server. ReadTimeout covers the agent's headers and body only.
	ReadHeaderTimeout Duration `yaml:"read_header_timeout" json:"read_header_timeout"`
	ReadTimeout       Duration `yaml:"read_timeout" json:"read_timeout"`
	IdleTimeout       Duration `yaml:"idle_timeout" json:"idle_timeout"`
	// StreamWriteTimeout is the deadline of each write to the agent, renewed
	// per chunk. StreamIdleTimeout is the longest the upstream may stay silent
	// while sending a response body.
	StreamWriteTimeout Duration `yaml:"stream_write_timeout" json:"stream_write_timeout"`
	StreamIdleTimeout  Duration `yaml:"stream_idle_timeout" json:"stream_idle_timeout"`
}

// Routing restricts what route mode may choose.
type Routing struct {
	AllowedChannels  []string `yaml:"allowed_channels" json:"allowed_channels"`
	AllowedProviders []string `yaml:"allowed_providers" json:"allowed_providers"`
	MaxTTFTP95Ms     int      `yaml:"max_ttft_p95_ms" json:"max_ttft_p95_ms"`
	MinUptime1d      float64  `yaml:"min_uptime_1d" json:"min_uptime_1d"`
}

// ProviderConfig holds the client's credentials and endpoint for one provider
// or channel (ADR 0008).
type ProviderConfig struct {
	APIKey logging.Secret `yaml:"api_key" json:"api_key"`
	// BaseURL is the OpenAI-compatible base, https only (http to literal
	// loopback with SIDECAR_ENV dev or staging, ADR 0011). Required in route
	// mode for every direct provider with a key; only openrouter has a
	// built-in default (DefaultOpenRouterBaseURL).
	BaseURL string `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	// AuthHeader names the header that carries the key, without prefix.
	// Empty means "Authorization: Bearer <key>".
	AuthHeader string `yaml:"auth_header,omitempty" json:"auth_header,omitempty"`
	// HTTPReferer and XTitle are sent as HTTP-Referer and X-Title on the
	// openrouter channel only; they are an error under any other provider.
	HTTPReferer string `yaml:"http_referer,omitempty" json:"http_referer,omitempty"`
	XTitle      string `yaml:"x_title,omitempty" json:"x_title,omitempty"`
}

// Tags is the allowlist of request tags.
type Tags struct {
	AllowedKeys []string `yaml:"allowed_keys" json:"allowed_keys"`
	Header      string   `yaml:"header" json:"header"`
}

// Duration is a time.Duration written as "2s" in YAML. Decoding keeps the raw
// text so that validation can report every malformed duration at once.
type Duration struct {
	d   time.Duration
	raw string
}

// NewDuration returns a Duration holding d.
func NewDuration(d time.Duration) Duration { return Duration{d: d, raw: d.String()} }

// Std returns the parsed duration. It is zero until the Config is validated.
func (d Duration) Std() time.Duration { return d.d }

// String implements fmt.Stringer.
func (d Duration) String() string { return d.d.String() }

// UnmarshalYAML stores the raw scalar; Validate parses it.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: duration must be a scalar like \"2s\"", n.Line)
	}
	d.raw = n.Value
	return nil
}

// MarshalYAML writes the duration as text.
func (d Duration) MarshalYAML() (any, error) { return d.d.String(), nil }

// MarshalJSON writes the duration as a JSON string.
func (d Duration) MarshalJSON() ([]byte, error) { return []byte(`"` + d.d.String() + `"`), nil }

// mask returns the masked form of a secret, keeping "unset" visible.
func mask(s logging.Secret) logging.Secret {
	if s == "" {
		return ""
	}
	return masked
}

// Redacted returns a copy of c in which every secret is replaced by "***".
func (c Config) Redacted() Config {
	out := c
	out.InboundToken = mask(c.InboundToken)
	out.Upstream = c.Upstream.redacted()
	out.ControlPlane = c.ControlPlane.redacted()
	if c.Providers != nil {
		out.Providers = make(map[string]ProviderConfig, len(c.Providers))
		for name, p := range c.Providers {
			out.Providers[name] = p.redacted()
		}
	}
	return out
}

func (u Upstream) redacted() Upstream {
	u.APIKey = mask(u.APIKey)
	return u
}

func (cp ControlPlane) redacted() ControlPlane {
	cp.APIKey = mask(cp.APIKey)
	return cp
}

func (p ProviderConfig) redacted() ProviderConfig {
	p.APIKey = mask(p.APIKey)
	return p
}

// Plain aliases drop the methods below so they do not recurse.
type (
	plainConfig         Config
	plainUpstream       Upstream
	plainControlPlane   ControlPlane
	plainProviderConfig ProviderConfig
)

// GoString implements fmt.GoStringer so %#v never prints a secret.
func (c Config) GoString() string { return fmt.Sprintf("config.Config%+v", plainConfig(c.Redacted())) }

// MarshalYAML implements yaml.Marshaler with secrets masked.
func (c Config) MarshalYAML() (any, error) { return plainConfig(c.Redacted()), nil }

// GoString implements fmt.GoStringer so %#v never prints a secret.
func (u Upstream) GoString() string {
	return fmt.Sprintf("config.Upstream%+v", plainUpstream(u.redacted()))
}

// MarshalYAML implements yaml.Marshaler with secrets masked.
func (u Upstream) MarshalYAML() (any, error) { return plainUpstream(u.redacted()), nil }

// GoString implements fmt.GoStringer so %#v never prints a secret.
func (cp ControlPlane) GoString() string {
	return fmt.Sprintf("config.ControlPlane%+v", plainControlPlane(cp.redacted()))
}

// MarshalYAML implements yaml.Marshaler with secrets masked.
func (cp ControlPlane) MarshalYAML() (any, error) { return plainControlPlane(cp.redacted()), nil }

// GoString implements fmt.GoStringer so %#v never prints a secret.
func (p ProviderConfig) GoString() string {
	return fmt.Sprintf("config.ProviderConfig%+v", plainProviderConfig(p.redacted()))
}

// MarshalYAML implements yaml.Marshaler with secrets masked.
func (p ProviderConfig) MarshalYAML() (any, error) { return plainProviderConfig(p.redacted()), nil }

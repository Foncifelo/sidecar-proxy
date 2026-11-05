package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode"
)

// Provider and channel ids with a special meaning in the YAML (ADR 0008).
const (
	ProviderOpenRouter = "openrouter"
	ChannelOpenRouter  = "openrouter"
	ChannelDirect      = "direct"

	// DefaultOpenRouterBaseURL is the only base_url built into the binary
	// (ADR 0008). No other provider has a default.
	DefaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"

	// EnvVarSidecarEnv names the environment variable that, set to exactly
	// "dev" or "staging", enables the ADR 0011 exception.
	EnvVarSidecarEnv = "SIDECAR_ENV"
)

// providersWithDefaults returns a copy of ps where openrouter, if present and
// without base_url, gets DefaultOpenRouterBaseURL. Nothing else is filled in,
// and base_url is never inherited from upstream.base_url.
func providersWithDefaults(ps map[string]ProviderConfig) map[string]ProviderConfig {
	if ps == nil {
		return nil
	}
	out := make(map[string]ProviderConfig, len(ps))
	for name, p := range ps {
		if name == ProviderOpenRouter && p.BaseURL == "" {
			p.BaseURL = DefaultOpenRouterBaseURL
		}
		out[name] = p
	}
	return out
}

// validateProviders checks the shape of every providers.<id> entry in any
// mode. Values are never quoted: they may come from ${VAR}.
func validateProviders(ps map[string]ProviderConfig, allowLoopbackHTTP bool) []error {
	var errs []error
	for _, name := range sortedNames(ps) {
		p := ps[name]
		key := "providers." + name
		if p.BaseURL != "" {
			if err := checkBaseURL(p.BaseURL, allowLoopbackHTTP); err != nil {
				errs = append(errs, fmt.Errorf("%s.base_url: %w", key, err))
			}
		}
		if p.AuthHeader != "" && !isHeaderName(p.AuthHeader) {
			errs = append(errs, fmt.Errorf("%s.auth_header: not a valid HTTP header name", key))
		}
		errs = append(errs, p.validateOpenRouterHeaders(name, key)...)
	}
	return errs
}

func (p ProviderConfig) validateOpenRouterHeaders(name, key string) []error {
	var errs []error
	if name != ProviderOpenRouter {
		if p.HTTPReferer != "" {
			errs = append(errs, fmt.Errorf("%s.http_referer: only allowed under providers.openrouter", key))
		}
		if p.XTitle != "" {
			errs = append(errs, fmt.Errorf("%s.x_title: only allowed under providers.openrouter", key))
		}
		return errs
	}
	if p.HTTPReferer != "" {
		if hasControl(p.HTTPReferer) {
			errs = append(errs, fmt.Errorf("%s.http_referer: must not contain line breaks or control characters", key))
		} else if err := checkHTTPSURL(p.HTTPReferer); err != nil {
			errs = append(errs, fmt.Errorf("%s.http_referer: %w", key, err))
		}
	}
	if hasControl(p.XTitle) {
		errs = append(errs, fmt.Errorf("%s.x_title: must not contain line breaks or control characters", key))
	}
	return errs
}

// validateRoute checks what route mode needs to act: a control plane, a base_url
// for every direct provider with a key, and at least one allowed provider it
// can actually call (docs/politica.md, "Filtro en el sidecar").
func (c Config) validateRoute(allowLoopbackHTTP bool) []error {
	var errs []error
	if c.ControlPlane.URL == "" {
		errs = append(errs, errors.New("control_plane.url: required in route mode"))
	} else if u, err := url.Parse(c.ControlPlane.URL); err != nil || u.Host == "" {
		errs = append(errs, errors.New("control_plane.url: not an absolute URL"))
	}
	for _, name := range sortedNames(c.Providers) {
		p := c.Providers[name]
		if name != ProviderOpenRouter && p.APIKey != "" && p.BaseURL == "" {
			errs = append(errs, fmt.Errorf("providers.%s.base_url: required in route mode for a provider with api_key (ADR 0008)", name))
		}
	}
	if !c.hasUsableProvider(allowLoopbackHTTP) {
		errs = append(errs, errors.New("route mode: no provider in routing.allowed_providers is reachable; "+
			"need an allowed channel (routing.allowed_channels) with an api_key and base_url in providers"))
	}
	return errs
}

// hasUsableProvider reports whether some provider in routing.allowed_providers
// can be called through an allowed channel with credentials and a base_url.
func (c Config) hasUsableProvider(allowLoopbackHTTP bool) bool {
	direct := slices.Contains(c.Routing.AllowedChannels, ChannelDirect)
	viaOpenRouter := slices.Contains(c.Routing.AllowedChannels, ChannelOpenRouter) &&
		c.Providers[ProviderOpenRouter].reachable(allowLoopbackHTTP)
	for _, name := range c.Routing.AllowedProviders {
		if viaOpenRouter || (direct && c.Providers[name].reachable(allowLoopbackHTTP)) {
			return true
		}
	}
	return false
}

// reachable reports whether p has a key and a base_url that passes validation.
func (p ProviderConfig) reachable(allowLoopbackHTTP bool) bool {
	return p.APIKey != "" && p.BaseURL != "" && checkBaseURL(p.BaseURL, allowLoopbackHTTP) == nil
}

// BaseURLAllowed reports whether raw may be a providers.<id>.base_url under
// c: https always, http only under the ADR 0011 exception (c.LoopbackHTTP).
// It is the single predicate shared by startup validation and the route plan.
func (c Config) BaseURLAllowed(raw string) bool {
	return checkBaseURL(raw, c.LoopbackHTTP) == nil
}

// checkBaseURL applies ADR 0008 (https only) with the single exception of
// ADR 0011: http is accepted when allowLoopbackHTTP is set (SIDECAR_ENV is dev
// or staging) and the host is literally 127.0.0.1, ::1 or localhost. The host
// is compared as written; DNS is never consulted.
func checkBaseURL(raw string, allowLoopbackHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("not an absolute URL")
	}
	if u.Scheme == "http" && allowLoopbackHTTP && isExceptionHost(u.Hostname()) {
		return nil
	}
	if u.Scheme != "https" {
		if allowLoopbackHTTP {
			return errors.New("must use https (http only for 127.0.0.1, ::1 or localhost, ADR 0011)")
		}
		return errors.New("must use https")
	}
	return nil
}

// isExceptionHost reports whether host is one of the three literal loopback
// names ADR 0011 allows over http.
func isExceptionHost(host string) bool {
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// loopbackHTTPAllowed reports whether env, the exact value of SIDECAR_ENV,
// enables the ADR 0011 exception.
func loopbackHTTPAllowed(env string) bool {
	return env == "dev" || env == "staging"
}

// HTTPLoopbackProviders returns, sorted, the ids of providers whose base_url
// uses the ADR 0011 http-on-loopback exception. Only meaningful on a Config
// returned by Load, which has already rejected any other http base_url.
func (c Config) HTTPLoopbackProviders() []string {
	var ids []string
	for _, name := range sortedNames(c.Providers) {
		u, err := url.Parse(c.Providers[name].BaseURL)
		if err == nil && u.Scheme == "http" {
			ids = append(ids, name)
		}
	}
	return ids
}

func checkHTTPSURL(raw string) error {
	return checkBaseURL(raw, false)
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

// isHeaderName reports whether s is an RFC 9110 field-name token.
func isHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", b) >= 0:
		default:
			return false
		}
	}
	return true
}

func sortedNames(ps map[string]ProviderConfig) []string {
	names := make([]string, 0, len(ps))
	for name := range ps {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

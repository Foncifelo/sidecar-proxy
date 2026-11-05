// Package logging is the only logger allowed in this repository (CONTRIBUTING.md rule 2).
//
// It wraps log/slog with a handler that redacts well-known secret keys and any
// attribute whose value is a Secret, so a key can never reach Grafana by accident.
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

// Secret is a string whose value never prints. Use it for API keys, tokens and
// cookies. Call Reveal only at the point of use (an Authorization header).
type Secret string

// String implements fmt.Stringer and masks the value.
func (Secret) String() string { return "***" }

// LogValue implements slog.LogValuer and masks the value.
func (Secret) LogValue() slog.Value { return slog.StringValue("***") }

// MarshalJSON masks the value in any JSON output.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"***"`), nil }

// Reveal returns the underlying value. Grep for it in review.
func (s Secret) Reveal() string { return string(s) }

// redactedKeys are attribute keys whose values are always masked, whatever their type.
var redactedKeys = map[string]struct{}{
	"authorization": {}, "proxy-authorization": {}, "cookie": {}, "set-cookie": {},
	"api_key": {}, "apikey": {}, "token": {}, "secret": {}, "password": {},
	"inbound_token": {}, "x-api-key": {}, "body": {}, "prompt": {}, "messages": {},
}

func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	if _, ok := redactedKeys[strings.ToLower(a.Key)]; ok {
		return slog.String(a.Key, "***")
	}
	if a.Value.Kind() == slog.KindAny {
		if _, ok := a.Value.Any().(Secret); ok {
			return slog.String(a.Key, "***")
		}
	}
	return a
}

// Options configures New.
type Options struct {
	Level slog.Leveler
	JSON  bool
}

// New returns a logger whose handler redacts secrets. w is usually os.Stderr.
func New(w io.Writer, o Options) *slog.Logger {
	ho := &slog.HandlerOptions{Level: o.Level, ReplaceAttr: replaceAttr}
	var h slog.Handler
	if o.JSON {
		h = slog.NewJSONHandler(w, ho)
	} else {
		h = slog.NewTextHandler(w, ho)
	}
	return slog.New(h)
}

type ctxKey struct{}

// WithContext stores the logger in ctx so request-scoped fields travel with it.
func WithContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the logger stored by WithContext, or slog.Default().
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

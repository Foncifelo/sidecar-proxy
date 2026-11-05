package providers

import (
	"context"
	"errors"
	"net/http"

	"github.com/Foncifelo/sidecar-proxy/internal/logging"
)

// Channel is how a call reaches the provider that serves it.
type Channel string

// Known channels.
const (
	ChannelDirect     Channel = "direct"
	ChannelOpenRouter Channel = "openrouter"
)

// Errors returned by BuildRequest.
var (
	ErrInvalidEndpoint = errors.New("providers: invalid endpoint")
	ErrInvalidBody     = errors.New("providers: invalid request body")
)

// Endpoint is everything an adapter needs to reach one endpoint. All values
// come from the client's YAML or the curated catalog, never from the agent.
type Endpoint struct {
	ID       string
	Provider string // provider that serves the call (slug), e.g. "together"
	Channel  Channel
	// BaseURL is the OpenAI-compatible base, e.g. "https://api.together.xyz/v1".
	BaseURL           string
	ProviderModelName string
	APIKey            logging.Secret
	// AuthHeader is the header carrying APIKey; "" means "Authorization".
	AuthHeader string
	// AuthScheme prefixes APIKey; "" with an empty AuthHeader means "Bearer".
	AuthScheme string
	// RewriteModel replaces the body's model with ProviderModelName (route mode only).
	RewriteModel bool
	// HTTPReferer and XTitle are sent as HTTP-Referer and X-Title on the
	// openrouter channel only, and only from the YAML (ADR 0008).
	HTTPReferer string
	XTitle      string
}

// Provider is the port the proxy uses to talk to an upstream.
type Provider interface {
	// BuildRequest returns the outgoing chat completions request for ep. It
	// never copies headers from the agent; see CopyAllowedHeaders.
	BuildRequest(ctx context.Context, ep Endpoint, body []byte) (*http.Request, error)
	// ParseServed returns the provider that actually served resp as reported
	// by the channel, or "" if the channel does not report it. It leaves
	// resp.Body readable from its first byte.
	ParseServed(resp *http.Response) (providerServed string)
}

var (
	_ Provider = OpenAICompatible{}
	_ Provider = OpenRouter{}
)

// For returns the adapter for ch. Unknown channels get the generic adapter.
func For(ch Channel) Provider {
	if ch == ChannelOpenRouter {
		return OpenRouter{}
	}
	return OpenAICompatible{}
}

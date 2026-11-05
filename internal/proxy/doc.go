// Package proxy is the OpenAI-compatible handler: buffered request body, model rewrite per endpoint, fallback strictly before the first byte, X-Sidecar-* headers. Custom handler, never httputil.ReverseProxy (ADR 0003). Only net/http is allowed here.
package proxy

package providers

import "net/http"

// allowedInbound lists the only agent headers that may reach a
// provider. Authorization, cookies, API keys and anything else never do
// (CONTRIBUTING.md rule 3, ADR 0006).
var allowedInbound = [...]string{"Accept", "Content-Type", "User-Agent"}

// AllowedInboundHeaders returns a copy of the inbound header allowlist.
func AllowedInboundHeaders() []string {
	out := make([]string, len(allowedInbound))
	copy(out, allowedInbound[:])
	return out
}

// CopyAllowedHeaders copies allowlisted agent headers from src into dst
// without overriding anything the adapter already set.
func CopyAllowedHeaders(dst *http.Request, src http.Header) {
	for _, k := range allowedInbound {
		if dst.Header.Get(k) != "" {
			continue
		}
		for _, v := range src.Values(k) {
			dst.Header.Add(k, v)
		}
	}
}

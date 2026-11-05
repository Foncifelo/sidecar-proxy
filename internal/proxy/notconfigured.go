package proxy

import (
	"net/http"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
)

// ReasonNotConfigured is the X-Sidecar-Decision of the 501 sent when the YAML
// has no upstream.
const ReasonNotConfigured = "not_configured"

// NoneProviderID fills X-Sidecar-Provider and X-Sidecar-Endpoint when no
// upstream is configured.
const NoneProviderID = "none"

// NotConfigured answers 501 to every /v1/* request, with the X-Sidecar-*
// headers every response carries (docs/contrato-proxy.md, guarantee 3). It
// emits no event: there is no upstream and no sink. ids may be nil.
func NotConfigured(mode config.Mode, ids *events.IDGen) http.Handler {
	if ids == nil {
		ids = events.NewIDGen(nil)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hd := w.Header()
		// An entropy failure leaves the id empty; the 501 is still answered.
		id, _ := ids.New(time.Now())
		hd.Set(HeaderRequestID, id)
		hd.Set(HeaderMode, string(mode))
		hd.Set(HeaderProvider, NoneProviderID)
		hd.Set(HeaderEndpoint, NoneProviderID)
		hd.Set(HeaderDecision, ReasonNotConfigured)
		hd.Set(HeaderFallback, "0")
		writeLocalError(w, http.StatusNotImplemented, ReasonNotConfigured, "no upstream configured")
	})
}

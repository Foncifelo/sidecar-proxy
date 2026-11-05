//go:build e2e

package e2e

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/decision"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/ingest"
)

// Everything below is fictitious: model ids, endpoint ids, providers, prices
// and latencies. Prices are microdollars per million tokens.
const (
	fakeCanonical   = "fictitious/model-canonical"
	fakeCheap       = "fictitious-cheap"
	epCurrent       = "ep-fictitious-current"
	epCheap         = "ep-fictitious-cheap"
	priceIDCurrent  = "101"
	priceIDCheap    = "102"
	policyVersion   = "v-e2e-fictitious-1"
	policyETag      = `"1:` + policyVersion + `"`
	policyRefreshIv = "1s"
	policyTimeout   = 10 * time.Second

	// The current endpoint costs twice the candidate, which passes every
	// filter (caps, quantization, uptime, TTFT, TPS), so the decision is
	// cheaper_endpoint and endpoint_best is the candidate.
	policyDoc = `{
  "schema_version": 1,
  "version": "` + policyVersion + `",
  "computed_at": "2026-10-29T00:00:00Z",
  "flags": {},
  "models": {
    "` + fakeCanonical + `": {
      "aliases": {"` + fakeProvider + `": ["` + fakeModel + `"]},
      "client_ttft_p95_ms": 2000,
      "client_tps_p50": 40,
      "ref_usage": {"tokens_in": 1000, "tokens_out": 200, "tokens_reasoning": 0, "tokens_cached_read": 0, "tokens_cached_write": 0},
      "route_enabled": false,
      "current": [{
        "provider": "` + fakeProvider + `", "raw_model": "` + fakeModel + `", "endpoint_id": "` + epCurrent + `",
        "quantization": "fp8",
        "price": {"price_id": "` + priceIDCurrent + `", "input_per_m": 1000000, "output_per_m": 2000000}
      }],
      "candidates": [{
        "endpoint_id": "` + epCheap + `", "provider": "` + fakeCheap + `", "channel": "direct",
        "provider_model_name": "fictitious/cheap-model", "quantization": "fp8",
        "caps": {"tools": true, "json_schema": true, "vision": false, "logprobs": false, "n": 1},
        "context_length": 131072,
        "price": {"price_id": "` + priceIDCheap + `", "input_per_m": 500000, "output_per_m": 1000000},
        "stats": {"uptime_1d": 0.999, "p90_ttft_ms": 300, "tps_p50": 90, "source": "probe"}
      }]
    }
  }
}`
)

// policyServed counts what the fake GET /v1/policy answered.
type policyServed struct {
	mu          sync.Mutex
	ok, notMod  int
	badRequests int
	got304      chan struct{}
}

func (p *policyServed) counts() (ok, notMod, bad int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ok, p.notMod, p.badRequests
}

// newFakePolicyControl serves POST /v1/ingest and GET /v1/policy. The policy
// answers 200 with policyETag, or 304 when If-None-Match equals it.
func newFakePolicyControl(t *testing.T) (*httptest.Server, <-chan ingest.Envelope, *policyServed) {
	t.Helper()
	return newFakePolicyControlDoc(t, policyDoc)
}

// newFakePolicyControlDoc is newFakePolicyControl serving doc as the policy.
func newFakePolicyControlDoc(t *testing.T, doc string) (*httptest.Server, <-chan ingest.Envelope, *policyServed) {
	t.Helper()
	ch := make(chan ingest.Envelope, 64)
	served := &policyServed{got304: make(chan struct{})}
	ingestH := ingestHandler(ch)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/policy" {
			ingestH(w, r)
			return
		}
		served.mu.Lock()
		defer served.mu.Unlock()
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+controlKey ||
			r.Header.Get("X-Sidecar-Schema-Version") != "1" {
			served.badRequests++
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("ETag", policyETag)
		if r.Header.Get("If-None-Match") == policyETag {
			served.notMod++
			if served.notMod == 1 {
				close(served.got304)
			}
			w.WriteHeader(http.StatusNotModified)
			return
		}
		served.ok++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(doc))
	}))
	t.Cleanup(srv.Close)
	return srv, ch, served
}

func TestEndToEndShadowPolicy(t *testing.T) {
	tmp := t.TempDir()
	sidecarBin := filepath.Join(tmp, "sidecar")
	buildBinary(t, "./cmd/sidecar", sidecarBin)

	upstream, _ := newFakeUpstream(t)
	control, envelopes, served := newFakePolicyControl(t)
	listen, metrics := freeAddr(t), freeAddr(t)
	cfg := writeConfig(t, tmp, listen, metrics, upstream.URL, control.URL, filepath.Join(tmp, "queue"), configExtra{
		Control:  "  policy_refresh: " + policyRefreshIv + "\n",
		TopLevel: "routing:\n  allowed_channels: [direct]\n  allowed_providers: [" + fakeCheap + "]\n",
	})
	p := startSidecar(t, sidecarBin, cfg, listen)

	// Acceptance: a second refresh gets 304. It carries If-None-Match with the
	// ETag of the first 200, so the policy is already active.
	select {
	case <-served.got304:
	case <-time.After(policyTimeout):
		ok, notMod, bad := served.counts()
		t.Fatalf("no 304 within %s: 200=%d 304=%d bad=%d\n%s", policyTimeout, ok, notMod, bad, p.stderr)
	}
	if ok, _, bad := served.counts(); ok != 1 || bad != 0 {
		t.Errorf("policy served 200=%d times (want 1), bad requests=%d", ok, bad)
	}

	hdr, _ := streamRequest(t, listen)
	reqID := hdr.Get("X-Sidecar-Request-Id")
	if got := hdr.Get("X-Sidecar-Decision"); got != string(decision.ReasonCheaper) {
		t.Errorf("X-Sidecar-Decision = %q, want %s", got, decision.ReasonCheaper)
	}
	if got := hdr.Get("X-Sidecar-Endpoint"); got != epCurrent {
		t.Errorf("X-Sidecar-Endpoint = %q, want %s", got, epCurrent)
	}

	p.stop(t)
	ev := waitEvent(t, envelopes, reqID)

	// Acceptance: endpoint_best, cost_best > 0 and policy_version.
	if ev.PolicyVersion != policyVersion || ev.ModelID != fakeCanonical ||
		ev.EndpointUsed != epCurrent || ev.EndpointBest != epCheap || ev.DecisionReason != string(decision.ReasonCheaper) {
		t.Errorf("event: policy_version=%q model_id=%q endpoint_used=%q endpoint_best=%q decision_reason=%q",
			ev.PolicyVersion, ev.ModelID, ev.EndpointUsed, ev.EndpointBest, ev.DecisionReason)
	}
	if ev.CostBest <= 0 || ev.CostActual <= ev.CostBest {
		t.Errorf("event: cost_actual=%d cost_best=%d, want 0 < cost_best < cost_actual", ev.CostActual, ev.CostBest)
	}
	if ev.PriceIDActual != priceIDCurrent || ev.PriceIDBest != priceIDCheap {
		t.Errorf("event: price_id_actual=%q price_id_best=%q", ev.PriceIDActual, ev.PriceIDBest)
	}
}

// waitEvent reads ingest envelopes until the request event with id arrives.
func waitEvent(t *testing.T, envelopes <-chan ingest.Envelope, id string) events.Event {
	t.Helper()
	timeout := time.After(waitTimeout)
	for {
		select {
		case env := <-envelopes:
			for _, it := range env.Items {
				if it.Type == ingest.ItemRequest && it.Event != nil && it.Event.ID == id {
					return *it.Event
				}
			}
		case <-timeout:
			t.Fatalf("no ingested event for request %s within %s", id, waitTimeout)
		}
	}
}

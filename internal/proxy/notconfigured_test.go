package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
)

// The 501 without upstream carries every X-Sidecar-* header.
func TestNotConfigured(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		mode         config.Mode
		method, path string
	}{
		{"shadow chat", config.ModeShadow, http.MethodPost, "/v1/chat/completions"},
		{"shadow models", config.ModeShadow, http.MethodGet, "/v1/models?page=2"},
		{"passthrough embeddings", config.ModePassthrough, http.MethodPost, "/v1/embeddings"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			NotConfigured(tc.mode, nil).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"model":"m"}`)))
			res := rec.Result()
			body, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if res.StatusCode != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501", res.StatusCode)
			}
			want := map[string]string{
				HeaderMode: string(tc.mode), HeaderProvider: NoneProviderID, HeaderEndpoint: NoneProviderID,
				HeaderDecision: ReasonNotConfigured, HeaderFallback: "0", "Content-Type": "application/json",
			}
			for k, v := range want {
				if got := res.Header.Get(k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			if len(res.Header.Get(HeaderRequestID)) != 26 {
				t.Errorf("request id %q is not a ULID", res.Header.Get(HeaderRequestID))
			}
			if !strings.Contains(string(body), `"type":"not_configured"`) {
				t.Errorf("body = %q", body)
			}
		})
	}
}

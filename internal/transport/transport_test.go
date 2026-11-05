package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func boolPtr(v bool) *bool { return &v }

func TestNewBuildsTransportPerProvider(t *testing.T) {
	tests := []struct {
		name  string
		cfg   ProviderConfig
		check func(t *testing.T, tr *http.Transport)
	}{
		{
			name: "defaults",
			cfg:  ProviderConfig{},
			check: func(t *testing.T, tr *http.Transport) {
				if tr.ResponseHeaderTimeout != 2*time.Second {
					t.Errorf("ResponseHeaderTimeout = %v, want 2s", tr.ResponseHeaderTimeout)
				}
				if tr.MaxIdleConnsPerHost != DefaultMaxIdleConnsPerHost ||
					tr.MaxConnsPerHost != DefaultMaxConnsPerHost {
					t.Errorf("conn limits = %d/%d", tr.MaxIdleConnsPerHost, tr.MaxConnsPerHost)
				}
				if tr.IdleConnTimeout != DefaultIdleConnTimeout ||
					tr.TLSHandshakeTimeout != DefaultTLSHandshakeTimeout {
					t.Errorf("timeouts = %v/%v", tr.IdleConnTimeout, tr.TLSHandshakeTimeout)
				}
				if !tr.ForceAttemptHTTP2 {
					t.Error("ForceAttemptHTTP2 should default to true")
				}
			},
		},
		{
			name: "overrides",
			cfg: ProviderConfig{
				MaxIdleConnsPerHost:   3,
				MaxConnsPerHost:       7,
				ResponseHeaderTimeout: 500 * time.Millisecond,
				IdleConnTimeout:       11 * time.Second,
				TLSHandshakeTimeout:   4 * time.Second,
				ForceAttemptHTTP2:     boolPtr(false),
			},
			check: func(t *testing.T, tr *http.Transport) {
				if tr.MaxIdleConnsPerHost != 3 || tr.MaxConnsPerHost != 7 {
					t.Errorf("conn limits = %d/%d, want 3/7", tr.MaxIdleConnsPerHost, tr.MaxConnsPerHost)
				}
				if tr.ResponseHeaderTimeout != 500*time.Millisecond ||
					tr.IdleConnTimeout != 11*time.Second ||
					tr.TLSHandshakeTimeout != 4*time.Second {
					t.Errorf("timeouts = %v/%v/%v", tr.ResponseHeaderTimeout, tr.IdleConnTimeout, tr.TLSHandshakeTimeout)
				}
				if tr.ForceAttemptHTTP2 {
					t.Error("ForceAttemptHTTP2 should be false")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := New(Config{Providers: map[string]ProviderConfig{"a": tt.cfg, "b": {}}})
			if err != nil {
				t.Fatal(err)
			}
			ta, ok := s.Transport("a")
			if !ok {
				t.Fatal("provider a missing")
			}
			tb, _ := s.Transport("b")
			if ta == tb {
				t.Error("providers must not share a transport")
			}
			if http.RoundTripper(ta) == http.DefaultTransport || http.RoundTripper(tb) == http.DefaultTransport {
				t.Error("must never use http.DefaultTransport")
			}
			if _, ok := s.Transport("missing"); ok {
				t.Error("unknown provider should not resolve")
			}
			tt.check(t, ta)
		})
	}
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "no providers", cfg: Config{}},
		{name: "empty provider name", cfg: Config{Providers: map[string]ProviderConfig{"": {}}}},
		{name: "negative conns", cfg: Config{Providers: map[string]ProviderConfig{"a": {MaxConnsPerHost: -1}}}},
		{name: "negative timeout", cfg: Config{Providers: map[string]ProviderConfig{"a": {ResponseHeaderTimeout: -1}}}},
		{name: "negative endpoint default", cfg: Config{
			Providers:        map[string]ProviderConfig{"a": {}},
			EndpointDefaults: EndpointConfig{MaxInFlight: -1},
		}},
		{name: "empty endpoint id", cfg: Config{
			Providers: map[string]ProviderConfig{"a": {}},
			Endpoints: map[string]EndpointConfig{"": {}},
		}},
		{name: "negative endpoint override", cfg: Config{
			Providers: map[string]ProviderConfig{"a": {}},
			Endpoints: map[string]EndpointConfig{"e": {Cooldown: -1}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); err == nil {
				t.Fatal("New() = nil error, want error")
			}
		})
	}
}

func TestEndpointConfigResolution(t *testing.T) {
	s, err := New(Config{
		Providers:        map[string]ProviderConfig{"a": {}},
		EndpointDefaults: EndpointConfig{MaxInFlight: 4},
		Endpoints:        map[string]EndpointConfig{"special": {MaxInFlight: 9}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		id   string
		want int
	}{
		{id: "plain", want: 4},
		{id: "special", want: 9},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			e := s.Endpoint(tt.id)
			if e != s.Endpoint(tt.id) {
				t.Fatal("Endpoint must be memoised")
			}
			if got := e.Bulkhead.Capacity(); got != tt.want {
				t.Fatalf("capacity = %d, want %d", got, tt.want)
			}
		})
	}
	if _, err := s.RoundTripper("missing", "x"); err == nil {
		t.Fatal("RoundTripper with unknown provider should fail")
	}
	s.CloseIdleConnections()
}

// newGuarded wires a Set with one provider and returns a client for endpoint "ep".
func newGuarded(t *testing.T, threshold int, clock Clock, headerTimeout time.Duration) (*http.Client, *Endpoint) {
	t.Helper()
	s, err := New(Config{
		Providers:        map[string]ProviderConfig{"p": {ResponseHeaderTimeout: headerTimeout}},
		EndpointDefaults: EndpointConfig{FailureThreshold: threshold, Cooldown: time.Minute, MaxInFlight: 2},
		Clock:            clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseIdleConnections)
	rt, err := s.RoundTripper("p", "ep")
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: rt, Timeout: 5 * time.Second}, s.Endpoint("ep")
}

func get(t *testing.T, c *http.Client, url string) (int, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

func TestGuardOpensBreaker(t *testing.T) {
	tests := []struct {
		name    string
		handler func(stop <-chan struct{}) http.HandlerFunc
		timeout time.Duration
		wantErr bool
	}{
		{
			name: "429",
			handler: func(<-chan struct{}) http.HandlerFunc {
				return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }
			},
		},
		{
			name: "500",
			handler: func(<-chan struct{}) http.HandlerFunc {
				return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
			},
		},
		{
			name: "503",
			handler: func(<-chan struct{}) http.HandlerFunc {
				return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }
			},
		},
		{
			name:    "response header timeout",
			timeout: 50 * time.Millisecond,
			wantErr: true,
			handler: func(stop <-chan struct{}) http.HandlerFunc {
				return func(_ http.ResponseWriter, r *http.Request) {
					select {
					case <-r.Context().Done():
					case <-stop:
					}
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stop := make(chan struct{})
			srv := httptest.NewServer(tt.handler(stop))
			defer srv.Close()
			defer close(stop)

			const threshold = 2
			client, ep := newGuarded(t, threshold, newFakeClock(), tt.timeout)
			for i := range threshold {
				if ep.Breaker.State() != StateClosed {
					t.Fatalf("call %d: breaker opened early", i)
				}
				_, err := get(t, client, srv.URL)
				if (err != nil) != tt.wantErr {
					t.Fatalf("call %d: err = %v, wantErr %v", i, err, tt.wantErr)
				}
			}
			if got := ep.Breaker.State(); got != StateOpen {
				t.Fatalf("state = %v, want open", got)
			}
			if _, err := get(t, client, srv.URL); !errors.Is(err, ErrBreakerOpen) {
				t.Fatalf("open breaker: err = %v, want ErrBreakerOpen", err)
			}
			if got := ep.Bulkhead.InFlight(); got != 0 {
				t.Fatalf("InFlight() = %d after all calls, want 0", got)
			}
		})
	}
}

func TestGuardHalfOpenSuccessCloses(t *testing.T) {
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	clock := newFakeClock()
	client, ep := newGuarded(t, 1, clock, 0)

	if code, err := get(t, client, srv.URL); err != nil || code != http.StatusBadGateway {
		t.Fatalf("first call = %d, %v", code, err)
	}
	if ep.Breaker.State() != StateOpen {
		t.Fatal("breaker should be open")
	}
	clock.Advance(time.Minute)
	if ep.Breaker.State() != StateHalfOpen {
		t.Fatal("breaker should be half-open after cooldown")
	}
	healthy.Store(true)
	if code, err := get(t, client, srv.URL); err != nil || code != http.StatusOK {
		t.Fatalf("probe = %d, %v", code, err)
	}
	if got := ep.Breaker.State(); got != StateClosed {
		t.Fatalf("state = %v, want closed", got)
	}
}

func TestGuardNonFailureStatusesKeepClosed(t *testing.T) {
	tests := []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound}
	for _, code := range tests {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()
			client, ep := newGuarded(t, 1, newFakeClock(), 0)
			if got, err := get(t, client, srv.URL); err != nil || got != code {
				t.Fatalf("get = %d, %v", got, err)
			}
			if ep.Breaker.State() != StateClosed {
				t.Fatalf("status %d must not open the breaker", code)
			}
		})
	}
}

func TestGuardHoldsSlotUntilBodyClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "data")
	}))
	defer srv.Close()
	client, ep := newGuarded(t, 1, newFakeClock(), 0)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := ep.Bulkhead.InFlight(); got != 1 {
		t.Fatalf("InFlight() with open body = %d, want 1", got)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if got := ep.Bulkhead.InFlight(); got != 0 {
		t.Fatalf("InFlight() after close = %d, want 0", got)
	}
}

func TestGuardCallerCancelInFlightDoesNotCount(t *testing.T) {
	tests := []struct {
		name      string
		halfOpen  bool
		wantState State
	}{
		{name: "closed", wantState: StateClosed},
		{name: "half-open probe", halfOpen: true, wantState: StateHalfOpen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var failing atomic.Bool
			received := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if failing.Load() {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				received <- struct{}{}
				<-r.Context().Done()
			}))
			defer srv.Close()
			clock := newFakeClock()
			client, ep := newGuarded(t, 1, clock, 0)

			if tt.halfOpen {
				failing.Store(true)
				if code, err := get(t, client, srv.URL); err != nil || code != http.StatusInternalServerError {
					t.Fatalf("priming request = %d, %v", code, err)
				}
				failing.Store(false)
				clock.Advance(time.Minute)
				if got := ep.Breaker.State(); got != StateHalfOpen {
					t.Fatalf("state = %v, want half-open", got)
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				<-received
				cancel()
			}()
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				t.Fatal("expected error from request cancelled in flight")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if got := ep.Breaker.State(); got != tt.wantState {
				t.Fatalf("state = %v, want %v: caller cancel must not count", got, tt.wantState)
			}
			if got := ep.Bulkhead.InFlight(); got != 0 {
				t.Fatalf("InFlight() = %d, want 0", got)
			}
			tok, err := ep.Breaker.Allow()
			if err != nil {
				t.Fatalf("next Allow() = %v, want admitted", err)
			}
			if tok.probe != tt.halfOpen {
				t.Fatalf("next token probe = %v, want %v", tok.probe, tt.halfOpen)
			}
			ep.Breaker.Ignore(tok)
		})
	}
}

// fillBulkhead holds every slot of ep and releases them at cleanup.
func fillBulkhead(t *testing.T, ep *Endpoint) {
	t.Helper()
	for range ep.Bulkhead.Capacity() {
		release, err := ep.Bulkhead.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
	}
}

func TestGuardOpenBreakerRejectsWithoutWaitingForSlot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	client, ep := newGuarded(t, 1, newFakeClock(), 0)
	if _, err := get(t, client, srv.URL); err != nil {
		t.Fatal(err)
	}
	if got := ep.Breaker.State(); got != StateOpen {
		t.Fatalf("state = %v, want open", got)
	}
	fillBulkhead(t, ep)

	// With no free slot and no deadline, waiting on the bulkhead would block
	// until the client timeout; the breaker must reject first.
	_, err := get(t, client, srv.URL)
	if !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("err = %v, want ErrBreakerOpen", err)
	}
}

func TestGuardBulkheadFailureReturnsProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	clock := newFakeClock()
	client, ep := newGuarded(t, 1, clock, 0)
	if _, err := get(t, client, srv.URL); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	fillBulkhead(t, ep)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected bulkhead acquire error")
	}
	if errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("err = %v, the probe should have been admitted", err)
	}
	if got := ep.Breaker.State(); got != StateHalfOpen {
		t.Fatalf("state = %v, want half-open", got)
	}
	tok, err := ep.Breaker.Allow()
	if err != nil || !tok.probe {
		t.Fatalf("next Allow() = %+v, %v; want the probe slot back", tok, err)
	}
	ep.Breaker.Ignore(tok)
}

func TestIsFailure(t *testing.T) {
	tests := []struct {
		name string
		resp *http.Response
		err  error
		want bool
	}{
		{name: "error", err: errors.New("boom"), want: true},
		{name: "200", resp: &http.Response{StatusCode: 200}, want: false},
		{name: "404", resp: &http.Response{StatusCode: 404}, want: false},
		{name: "429", resp: &http.Response{StatusCode: 429}, want: true},
		{name: "500", resp: &http.Response{StatusCode: 500}, want: true},
		{name: "599", resp: &http.Response{StatusCode: 599}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsFailure(tt.resp, tt.err); got != tt.want {
				t.Fatalf("IsFailure() = %v, want %v", got, tt.want)
			}
		})
	}
}

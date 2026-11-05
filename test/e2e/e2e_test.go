//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/ingest"
)

// Everything below is fictitious: provider id, model, keys and token counts.
const (
	fakeProvider = "fictitious-provider"
	fakeModel    = "fictitious/model-e2e"
	upstreamKey  = "upstream-key-fictitious"
	agentKey     = "agent-key-fictitious-must-not-leak"
	controlKey   = "sk_test_fictitious"

	chunk1     = `data: {"id":"c1","object":"chat.completion.chunk","model":"fictitious/model-e2e","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}` + "\n\n"
	chunk2     = `data: {"id":"c1","object":"chat.completion.chunk","model":"fictitious/model-e2e","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}` + "\n\n"
	chunkStop  = `data: {"id":"c1","object":"chat.completion.chunk","model":"fictitious/model-e2e","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	chunkUsage = `data: {"id":"c1","object":"chat.completion.chunk","model":"fictitious/model-e2e","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":2,"total_tokens":14}}` + "\n\n"
	chunkDone  = "data: [DONE]\n\n"

	upstreamBody = chunk1 + chunk2 + chunkStop + chunkUsage + chunkDone

	startTimeout = 15 * time.Second
	stopTimeout  = 40 * time.Second // sidecar drainTimeout (30s) + eventFlushTimeout (5s) + margin
	waitTimeout  = 10 * time.Second
	clientTO     = 30 * time.Second
)

// upstreamSeen is what the fake upstream observed in one request.
type upstreamSeen struct {
	auth          string
	acceptEncode  string
	includeUsage  bool
	agentKeyFound bool
	ctlKeyFound   bool
}

func newFakeUpstream(t *testing.T) (*httptest.Server, func() []upstreamSeen) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []upstreamSeen
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		seen = append(seen, upstreamSeen{
			auth:          r.Header.Get("Authorization"),
			acceptEncode:  r.Header.Get("Accept-Encoding"),
			includeUsage:  req.StreamOptions.IncludeUsage,
			agentKeyFound: headerHas(r.Header, agentKey) || bytes.Contains(body, []byte(agentKey)),
			ctlKeyFound:   headerHas(r.Header, controlKey) || bytes.Contains(body, []byte(controlKey)),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, c := range []string{chunk1, chunk2, chunkStop, chunkUsage, chunkDone} {
			_, _ = io.WriteString(w, c)
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []upstreamSeen {
		mu.Lock()
		defer mu.Unlock()
		return append([]upstreamSeen(nil), seen...)
	}
}

// newFakeControl answers POST /v1/ingest with 202 and hands each envelope to the channel.
func newFakeControl(t *testing.T) (*httptest.Server, <-chan ingest.Envelope) {
	t.Helper()
	ch := make(chan ingest.Envelope, 16)
	srv := httptest.NewServer(ingestHandler(ch))
	t.Cleanup(srv.Close)
	return srv, ch
}

// ingestHandler is the fake POST /v1/ingest: 202 and each envelope to ch.
func ingestHandler(ch chan<- ingest.Envelope) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/ingest" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+controlKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var env ingest.Envelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ch <- env
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, `{"accepted_n":%d}`, len(env.Items))
	}
}

// headerHas reports whether any header value contains s (never printed: CONTRIBUTING.md rule 2).
func headerHas(h http.Header, s string) bool {
	for _, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, s) {
				return true
			}
		}
	}
	return false
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func buildBinary(t *testing.T, pkg, out string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = repoRoot(t)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, b)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// configExtra adds YAML to the base config: Control lines go inside
// control_plane (indented by the caller), TopLevel lines at the end.
type configExtra struct {
	Control  string
	TopLevel string
}

// writeConfig writes a shadow sidecar.yaml.
func writeConfig(t *testing.T, dir, listen, metrics, upstreamURL, controlURL, queueDir string, extra configExtra) string {
	t.Helper()
	yaml := fmt.Sprintf(`mode: shadow
listen: %s
metrics_listen: %s
upstream:
  provider: %s
  base_url: %s/v1
  api_key: %s
control_plane:
  url: %s
  api_key: %s
  queue_dir: %s
  queue_max_mb: 16
%slimits:
  max_body_mb: 1
  max_inflight: 8
%s`, listen, metrics, fakeProvider, upstreamURL, upstreamKey, controlURL, controlKey, queueDir, extra.Control, extra.TopLevel)
	path := filepath.Join(dir, "sidecar.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

type sidecarProc struct {
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	done   chan error
}

// startSidecar runs bin with cfg and waits for /healthz; env entries are
// added to the inherited environment.
func startSidecar(t *testing.T, bin, cfg, listen string, env ...string) *sidecarProc {
	t.Helper()
	p := &sidecarProc{stderr: &bytes.Buffer{}, done: make(chan error, 1)}
	p.cmd = exec.Command(bin, "--config", cfg)
	if len(env) > 0 {
		p.cmd.Env = append(os.Environ(), env...)
	}
	p.cmd.Stderr = p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start sidecar: %v", err)
	}
	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(func() {
		if p.cmd.ProcessState == nil {
			_ = p.cmd.Process.Kill()
			t.Logf("sidecar killed at cleanup: %v", <-p.done)
		}
	})

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(startTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-p.done:
			t.Fatalf("sidecar exited early: %v\n%s", err, p.stderr)
		default:
		}
		resp, err := client.Get("http://" + listen + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return p
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("sidecar not healthy within %s\n%s", startTimeout, p.stderr)
	return nil
}

// stop sends SIGTERM and waits for a clean exit, so the event queue is closed.
func (p *sidecarProc) stop(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal sidecar: %v", err)
	}
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatalf("sidecar exit: %v\n%s", err, p.stderr)
		}
	case <-time.After(stopTimeout):
		t.Fatalf("sidecar did not stop within %s", stopTimeout)
	}
}

func streamRequest(t *testing.T, listen string) (http.Header, []byte) {
	t.Helper()
	body := `{"model":"` + fakeModel + `","stream":true,"messages":[{"role":"user","content":"fictitious prompt"}]}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+listen+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+agentKey)
	client := &http.Client{Timeout: clientTO, Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, got)
	}
	return resp.Header, got
}

func runAgent(t *testing.T, bin, listen string) (stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), clientTO)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-url", "http://"+listen, "-model", fakeModel, "-prompt", "fictitious prompt")
	cmd.Env = append(os.Environ(), "AGENT_API_KEY="+agentKey)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("agent: %v\n%s", err, errb.String())
	}
	return out.String(), errb.String()
}

func checkSidecarHeaders(t *testing.T, h http.Header) string {
	t.Helper()
	want := map[string]string{
		"X-Sidecar-Mode":     "shadow",
		"X-Sidecar-Provider": fakeProvider,
		"X-Sidecar-Fallback": "0",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"X-Sidecar-Request-Id", "X-Sidecar-Endpoint", "X-Sidecar-Decision"} {
		if h.Get(k) == "" {
			t.Errorf("%s missing", k)
		}
	}
	return h.Get("X-Sidecar-Request-Id")
}

func TestEndToEndShadow(t *testing.T) {
	tmp := t.TempDir()
	sidecarBin := filepath.Join(tmp, "sidecar")
	agentBin := filepath.Join(tmp, "agent")
	buildBinary(t, "./cmd/sidecar", sidecarBin)
	buildBinary(t, "./examples/agent", agentBin)

	upstream, upstreamRequests := newFakeUpstream(t)
	control, envelopes := newFakeControl(t)
	listen, metrics := freeAddr(t), freeAddr(t)
	queueDir := filepath.Join(tmp, "queue")
	cfg := writeConfig(t, tmp, listen, metrics, upstream.URL, control.URL, queueDir, configExtra{})

	p := startSidecar(t, sidecarBin, cfg, listen)
	// The agent did not ask for usage, so the sidecar strips exactly that chunk.
	agentBody := strings.Replace(upstreamBody, chunkUsage, "", 1)

	// Acceptance: X-Sidecar-* headers and exact upstream bytes (minus the usage chunk).
	hdr, got := streamRequest(t, listen)
	reqID := checkSidecarHeaders(t, hdr)
	if string(got) != agentBody {
		t.Errorf("agent bytes differ from upstream bytes minus usage chunk:\n got: %q\nwant: %q", got, agentBody)
	}

	// Acceptance: the example agent streams through the sidecar with only net/http.
	out, agentErr := runAgent(t, agentBin, listen)
	if out != agentBody {
		t.Errorf("example agent stdout:\n got: %q\nwant: %q", out, agentBody)
	}
	agentReqID := ""
	for _, line := range strings.Split(agentErr, "\n") {
		if v, ok := strings.CutPrefix(line, "X-Sidecar-Request-Id: "); ok {
			agentReqID = v
		}
	}
	if agentReqID == "" || !strings.Contains(agentErr, "X-Sidecar-Mode: shadow") {
		t.Errorf("example agent did not report X-Sidecar-* headers:\n%s", agentErr)
	}

	for i, s := range upstreamRequests() {
		if s.auth != "Bearer "+upstreamKey || s.agentKeyFound || s.ctlKeyFound {
			t.Errorf("upstream request %d: Authorization %q, agent key leaked=%v, control key leaked=%v",
				i, s.auth, s.agentKeyFound, s.ctlKeyFound)
		}
		if !s.includeUsage || s.acceptEncode != "identity" {
			t.Errorf("upstream request %d: include_usage=%v accept-encoding=%q", i, s.includeUsage, s.acceptEncode)
		}
	}

	// SIGTERM: the sidecar itself flushes its queue to the control plane
	// (cmd/sidecar wires events.Batcher and ingest.Client).
	p.stop(t)
	for _, secret := range []string{controlKey, upstreamKey, agentKey} {
		if strings.Contains(p.stderr.String(), secret) {
			t.Error("sidecar log contains a key")
		}
	}

	// Acceptance: an ingest batch with the events reaches the control plane.
	byID := map[string]events.Event{}
	timeout := time.After(waitTimeout)
	for len(byID) < 2 {
		select {
		case env := <-envelopes:
			if env.SchemaVersion != events.SchemaVersion || env.BatchID == "" {
				t.Fatalf("envelope schema_version=%d batch_id=%q", env.SchemaVersion, env.BatchID)
			}
			for _, it := range env.Items {
				if it.Type == ingest.ItemRequest && it.Event != nil {
					byID[it.Event.ID] = *it.Event
				}
			}
		case <-timeout:
			t.Fatalf("control plane got %d events, want 2", len(byID))
		}
	}
	for _, id := range []string{reqID, agentReqID} {
		ev, ok := byID[id]
		if !ok {
			t.Errorf("no ingested event for request %s", id)
			continue
		}
		if ev.Mode != events.Mode("shadow") || ev.RawModel != fakeModel || !ev.Stream ||
			ev.Status != http.StatusOK || ev.TokensIn != 12 || ev.TokensOut != 2 {
			t.Errorf("event %s: mode=%q raw_model=%q stream=%v status=%d tokens=%d/%d",
				id, ev.Mode, ev.RawModel, ev.Stream, ev.Status, ev.TokensIn, ev.TokensOut)
		}
	}
}

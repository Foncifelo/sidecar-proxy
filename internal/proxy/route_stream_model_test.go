package proxy

import (
	"net/http"
	"testing"
)

// Fictitious provider chunks with the provider's model name. agentModel is
// what the agent sent in streamReq.
const (
	provModelChunk   = "data: {\"id\":\"c1\",\"model\":\"fict/model\" ,\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"
	agentModelChunk  = "data: {\"id\":\"c1\",\"model\":\"" + agentModel + "\" ,\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"
	nestedModelChunk = "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"model\":\"fict/model\"}}]}\n\n"
	noModelChunk     = "event: chunk\nid: 2\ndata: {\"id\":\"c1\",\"choices\":[]}\r\n\r\n"
	notJSONChunk     = "data: {\"model\":\"fict/model\"\n\n"
	arrayChunk       = "data: [{\"model\":\"fict/model\"}]\n\n"
)

// End to end: in route only the top-level model of each
// chunk becomes the agent's; chunks without it or not JSON pass intact.
func TestRouteStreamRewritesModel(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"top-level model", []string{provModelChunk, doneChunk}, agentModelChunk + doneChunk},
		{"keepalive kept", []string{pingChunk, provModelChunk, pingChunk, doneChunk}, pingChunk + agentModelChunk + pingChunk + doneChunk},
		{"nested model kept", []string{nestedModelChunk, doneChunk}, nestedModelChunk + doneChunk},
		{"no model not added", []string{provModelChunk, noModelChunk, doneChunk}, agentModelChunk + noModelChunk + doneChunk},
		{"not JSON intact", []string{provModelChunk, notJSONChunk, arrayChunk, doneChunk}, agentModelChunk + notJSONChunk + arrayChunk + doneChunk},
		{"block split across writes", []string{provModelChunk[:20], provModelChunk[20:], doneChunk}, agentModelChunk + doneChunk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			steps := make([]step, len(tt.in))
			for i, b := range tt.in {
				steps[i] = step{block: b}
			}
			env := newStreamEnv(t, sseSteps(0, steps...), upStream, clientP95Ms)
			resp, body := env.post(t, streamReq, nil)
			_ = env.h.sink.wait(t)
			if resp.StatusCode != http.StatusOK || body != tt.want {
				t.Fatalf("status %d body\n got %q\nwant %q", resp.StatusCode, body, tt.want)
			}
			if resp.Header.Get(HeaderEndpoint) != candEP {
				t.Errorf("served by %q, want %q", resp.Header.Get(HeaderEndpoint), candEP)
			}
		})
	}
}

// ADR 0008: shadow never rewrites, the provider's model reaches the agent.
func TestShadowStreamKeepsProviderModel(t *testing.T) {
	blocks := []string{provModelChunk, doneChunk}
	up := newUpstream(t, sseServe(blocks, 0))
	hs := newHarness(t, up.URL, nil)
	resp, body := hs.do(t, http.MethodPost, chatPath, streamBody(true), nil)
	_ = hs.sink.wait(t)
	if resp.StatusCode != http.StatusOK || string(body) != provModelChunk+doneChunk {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
}

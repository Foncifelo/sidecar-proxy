package ingest

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
)

type fakeStats struct{}

func (fakeStats) Depth() int            { return 42 }
func (fakeStats) DroppedEvents() uint64 { return 7 }

func TestHeartbeatItem(t *testing.T) {
	if DefaultHeartbeatEvery != 60*time.Second {
		t.Fatalf("default heartbeat interval = %v, want 60s", DefaultHeartbeatEvery)
	}
	h := newHarness(t, reply{code: http.StatusAccepted})
	c := h.client
	c.cfg.Queue = fakeStats{}
	c.cfg.State = func() State {
		return State{Mode: events.ModeRoute, PolicyVersion: "pv-9", Inflight: 3, PolicyAge: 30 * time.Second}
	}
	start := c.started
	c.now = func() time.Time { return start.Add(90 * time.Second) }
	c.cfg.HeartbeatEvery = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.RunHeartbeat(ctx) }()
	waitFor(t, "three heartbeats", func() bool { return len(h.fake.requests()) >= 3 })
	cancel()
	<-done

	reqs := h.fake.requests()
	seen := map[string]bool{}
	for _, r := range reqs {
		e := r.env
		if e.SchemaVersion != events.SchemaVersion || e.BatchID == "" || seen[e.BatchID] {
			t.Fatalf("bad heartbeat envelope %+v", e)
		}
		seen[e.BatchID] = true
		if len(e.Items) != 1 || e.Items[0].Type != ItemHeartbeat || e.Items[0].Event != nil || e.Items[0].Heartbeat == nil {
			t.Fatalf("items = %+v", e.Items)
		}
		want := Heartbeat{Mode: events.ModeRoute, PolicyVersion: "pv-9", QueueDepth: 42, DroppedEvents: 7, Inflight: 3, UptimeS: 90, PolicyAgeS: int64Ptr(30)}
		if got := *e.Items[0].Heartbeat; !reflect.DeepEqual(got, want) {
			t.Fatalf("heartbeat = %+v, want %+v", got, want)
		}
		item := r.raw["items"].([]any)[0].(map[string]any)["heartbeat"].(map[string]any)
		for _, k := range []string{"mode", "policy_version", "queue_depth", "dropped_events", "inflight", "uptime_s", "policy_age_s"} {
			if _, ok := item[k]; !ok {
				t.Errorf("heartbeat lacks %q", k)
			}
		}
	}
}

func TestHeartbeatFailureIsLogged(t *testing.T) {
	h := newHarness(t, reply{code: http.StatusBadRequest})
	h.client.sendHeartbeat(context.Background())
	if got := len(h.fake.requests()); got != 1 {
		t.Fatalf("requests = %d", got)
	}
	if !strings.Contains(h.logs.String(), "heartbeat not delivered") {
		t.Fatal("failure not logged")
	}
}

func int64Ptr(v int64) *int64 { return &v }

// The heartbeat carries the age of the active policy and
// omits it without a policy (docs/politica.md).
func TestHeartbeatPolicyAge(t *testing.T) {
	tests := []struct {
		name      string
		state     State
		want      *int64
		wantInRaw bool
	}{
		{"with policy", State{PolicyVersion: "pv-1", PolicyAge: 125*time.Second + 900*time.Millisecond}, int64Ptr(125), true},
		{"fresh policy", State{PolicyVersion: "pv-1"}, int64Ptr(0), true},
		{"negative age clamps", State{PolicyVersion: "pv-1", PolicyAge: -time.Second}, int64Ptr(0), true},
		{"no policy", State{PolicyAge: time.Hour}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, reply{code: http.StatusAccepted})
			h.client.cfg.State = func() State { return tt.state }

			h.client.sendHeartbeat(context.Background())

			reqs := h.fake.requests()
			if len(reqs) != 1 {
				t.Fatalf("requests = %d", len(reqs))
			}
			envelope := reqs[0].env
			if got := envelope.Items[0].Heartbeat.PolicyAgeS; !reflect.DeepEqual(got, tt.want) {
				t.Errorf("policy_age_s = %v, want %v", got, tt.want)
			}
			item := reqs[0].raw["items"].([]any)[0].(map[string]any)["heartbeat"].(map[string]any)
			if _, ok := item["policy_age_s"]; ok != tt.wantInRaw {
				t.Errorf("policy_age_s present = %v, want %v", ok, tt.wantInRaw)
			}
		})
	}
}

func TestHeartbeatWithoutSources(t *testing.T) {
	h := newHarness(t, reply{code: http.StatusAccepted})
	if hb := h.client.heartbeat(); hb.QueueDepth != 0 || hb.Mode != "" {
		t.Fatalf("heartbeat = %+v", hb)
	}
}

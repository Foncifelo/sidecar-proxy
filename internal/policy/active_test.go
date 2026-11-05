package policy

import (
	"testing"
	"time"
)

func TestActiveReads(t *testing.T) {
	p, err := Decode([]byte(exampleDoc))
	if err != nil {
		t.Fatal(err)
	}
	a := newActive(p, Discards{}, exampleETag, t0)
	tests := []struct {
		name, provider, raw, wantID string
		ok                          bool
	}{
		{"direct alias", "acme", "acme/fict-model", "fict-model", true},
		{"openrouter alias", "openrouter", "fict/fict-model", "fict-model", true},
		{"alias of another provider", "gamma", "acme/fict-model", "", false},
		{"unknown model", "acme", "acme/other", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, m, ok := a.Resolve(tt.provider, tt.raw)
			if ok != tt.ok || id != tt.wantID {
				t.Fatalf("Resolve = (%q, %v), want (%q, %v)", id, ok, tt.wantID, tt.ok)
			}
			if ok && len(m.Candidates) != 3 {
				t.Errorf("model candidates = %d", len(m.Candidates))
			}
		})
	}
	m, ok := a.Model("fict-model")
	if !ok {
		t.Fatal("Model not found")
	}
	if _, ok := a.Model("nope"); ok {
		t.Error("unknown model found")
	}
	if c, ok := m.CurrentFor("acme", "acme/fict-model"); !ok || c.EndpointID != epAcmeCur {
		t.Errorf("CurrentFor = %+v, %v", c, ok)
	}
	if _, ok := m.CurrentFor("openrouter", "fict/fict-model"); ok {
		t.Error("CurrentFor found a pair that is not in current")
	}
}

func TestActiveAge(t *testing.T) {
	a := &Active{ValidatedAt: t0}
	tests := []struct {
		now  time.Time
		want time.Duration
	}{
		{t0.Add(90 * time.Second), 90 * time.Second},
		{t0, 0},
		{t0.Add(-time.Minute), 0}, // clock went back
	}
	for _, tt := range tests {
		if got := a.Age(tt.now); got != tt.want {
			t.Errorf("Age(%v) = %v, want %v", tt.now, got, tt.want)
		}
	}
}

func TestHolder(t *testing.T) {
	var h Holder
	if h.Load() != nil {
		t.Fatal("zero Holder has a policy")
	}
	a := &Active{ETag: exampleETag}
	h.Store(a)
	if h.Load() != a {
		t.Error("Load did not return the stored policy")
	}
}

func TestAliasConflictIsDeterministic(t *testing.T) {
	p := Policy{Models: map[string]Model{
		"b-model": {Aliases: map[string][]string{"acme": {"shared"}}},
		"a-model": {Aliases: map[string][]string{"acme": {"shared"}}},
	}}
	for range 10 {
		if id, _, _ := newActive(p, nil, "", t0).Resolve("acme", "shared"); id != "a-model" {
			t.Fatalf("Resolve = %q, want a-model", id)
		}
	}
}

package decision

import (
	"reflect"
	"testing"
	"time"
)

// All endpoints, prices and latencies in this file are fictitious.

func rankReq() Requirements {
	r := allReq()
	r.Model, r.Quantization = testModel, QuantFP8
	return r
}

func ids(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.EndpointID
	}
	return out
}

func TestRankOrder(t *testing.T) {
	withFee := cand("b-openrouter", 900_000, 1_800_000) // 2_700_000 * 1.2 = 3_240_000
	withFee.Price.ChannelFeeBps = 2_000
	badPrice := cand("bad-price", 1, 1)
	badPrice.Price.OutputPerM = -1
	dup1 := cand("dup", 500_000, 500_000)
	dup2 := cand("dup", 500_000, 500_000)
	dup2.Price.PriceID = "p-dup-second"

	tests := []struct {
		name  string
		cands []Candidate
		want  []string
	}{
		{"nil input", nil, []string{}},
		{"cheapest first", []Candidate{cand("b", 500_000, 1_000_000), cand("a", 400_000, 800_000), cand("c", 2_000_000, 2_000_000)}, []string{"a", "b", "c"}},
		{"ties by endpoint id", []Candidate{cand("z", 1, 1), cand("m", 1, 1), cand("y", 1, 1)}, []string{"m", "y", "z"}},
		{"channel fee counts", []Candidate{withFee, cand("x", 1_000_000, 2_100_000)}, []string{"x", "b-openrouter"}},
		{"price error dropped", []Candidate{badPrice, cand("ok", 1, 1)}, []string{"ok"}},
		{"current is not special", []Candidate{cur(), cand("a", 1, 1)}, []string{"a", "current"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Rank(rankReq(), tt.cands, th())
			if got == nil {
				t.Fatal("Rank returned nil")
			}
			if !reflect.DeepEqual(ids(got), tt.want) {
				t.Errorf("Rank = %v, want %v", ids(got), tt.want)
			}
		})
	}

	t.Run("duplicate ids keep input order", func(t *testing.T) {
		got := Rank(rankReq(), []Candidate{dup1, dup2}, th())
		if len(got) != 2 || got[0].Price.PriceID != "p-dup" || got[1].Price.PriceID != "p-dup-second" {
			t.Errorf("Rank = %+v", got)
		}
	})
}

// TestRankFilters checks that every Decide filter also drops the candidate
// from Rank, and that Decide reports the same reason for it.
func TestRankFilters(t *testing.T) {
	tests := []struct {
		name   string
		th     func(*Thresholds)
		req    func(*Requirements)
		mutate func(*Candidate)
		want   Reason
	}{
		{"tools", nil, nil, func(c *Candidate) { c.Caps.Tools = false }, ReasonNoTools},
		{"json schema", nil, nil, func(c *Candidate) { c.Caps.JSONSchema = false }, ReasonNoJSONSchema},
		{"vision", nil, nil, func(c *Candidate) { c.Caps.Vision = false }, ReasonNoVision},
		{"logprobs", nil, nil, func(c *Candidate) { c.Caps.Logprobs = false }, ReasonNoLogprobs},
		{"n", nil, nil, func(c *Candidate) { c.Caps.MaxN = 1 }, ReasonNTooLarge},
		{"context", nil, nil, func(c *Candidate) { c.Caps.ContextLength = 0 }, ReasonContextTooBig},
		{"model", nil, nil, func(c *Candidate) { c.Model = "fictitious/other" }, ReasonOtherModel},
		{"quantization", nil, nil, func(c *Candidate) { c.Quantization = QuantInt4 }, ReasonWorseQuant},
		{"unknown quantization", nil, nil, func(c *Candidate) { c.Quantization = QuantUnknown }, ReasonWorseQuant},
		{"empty reference quantization", nil, func(r *Requirements) { r.Quantization = "" }, func(*Candidate) {}, ReasonWorseQuant},
		{"uptime", nil, nil, func(c *Candidate) { c.Stats.Uptime1d = 0.9 }, ReasonLowUptime},
		{"ttft", nil, nil, func(c *Candidate) { c.Stats.TTFTP90 = time.Second }, ReasonSlowTTFT},
		{"ttft no data", nil, nil, func(c *Candidate) { c.Stats.TTFTP90 = 0 }, ReasonSlowTTFT},
		{"tps", nil, nil, func(c *Candidate) { c.Stats.TPSP50 = 10 }, ReasonSlowTPS},
		{"price error", nil, nil, func(c *Candidate) { c.Price.InputPerM = -1 }, ReasonPriceError},
		{"ttft filter disabled", func(t *Thresholds) { t.ClientTTFTP95 = 0 }, nil, func(c *Candidate) { c.Stats.TTFTP90 = time.Hour }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			thr, req := th(), rankReq()
			if tt.th != nil {
				tt.th(&thr)
			}
			if tt.req != nil {
				tt.req(&req)
			}
			c := cand("x", 1, 1)
			tt.mutate(&c)

			got := Rank(req, []Candidate{c}, thr)
			wantLen := 0
			if tt.want == "" {
				wantLen = 1
			}
			if len(got) != wantLen {
				t.Fatalf("Rank len = %d, want %d", len(got), wantLen)
			}
			if tt.want == "" || tt.req != nil {
				return // Decide uses current as reference, not req.
			}
			d := Decide(req, cur(), []Candidate{c}, thr)
			if len(d.Discards) != 1 || d.Discards[0].Reason != tt.want {
				t.Errorf("Decide discards = %+v, want reason %q", d.Discards, tt.want)
			}
		})
	}
}

// TestDecideMatchesRank checks Decide's winner is Rank's first endpoint
// (current excluded) whenever it is strictly cheaper than current.
func TestDecideMatchesRank(t *testing.T) {
	slow := cand("slow", 1, 1)
	slow.Stats.Uptime1d = 0
	sets := [][]Candidate{
		{cand("b", 500_000, 1_000_000), cand("a", 400_000, 800_000), slow},
		{cand("z", 500_000, 500_000), cand("m", 500_000, 500_000)},
		{cand("x", 2_000_000, 2_000_000)},
		{cur(), cand("y", 100, 100)},
		{slow},
		nil,
	}
	for i, cands := range sets {
		current := cur()
		ref := allReq()
		ref.Model, ref.Quantization = current.Model, current.Quantization
		var others []Candidate
		for _, c := range cands {
			if c.EndpointID != current.EndpointID {
				others = append(others, c)
			}
		}
		ranked := Rank(ref, others, th())
		d := Decide(allReq(), current, cands, th())
		wantBest := current.EndpointID
		if len(ranked) > 0 && d.Reason == ReasonCheaper {
			wantBest = ranked[0].EndpointID
		}
		if len(ranked) > 0 && d.Reason != ReasonCheaper && d.Reason != ReasonCurrentCheaper {
			t.Errorf("set %d: Reason = %q with %d ranked", i, d.Reason, len(ranked))
		}
		if len(ranked) == 0 && d.Reason != ReasonNoCandidate {
			t.Errorf("set %d: Reason = %q, want %q", i, d.Reason, ReasonNoCandidate)
		}
		if d.Best.EndpointID != wantBest {
			t.Errorf("set %d: Decide Best = %q, want %q", i, d.Best.EndpointID, wantBest)
		}
	}
}

func TestRankDeterministicAndPure(t *testing.T) {
	cands := []Candidate{cand("c", 600_000, 600_000), cand("a", 600_000, 600_000), cand("b", 700_000, 700_000)}
	snapshot := append([]Candidate(nil), cands...)
	first := Rank(rankReq(), cands, th())
	for i := 0; i < 50; i++ {
		if got := Rank(rankReq(), cands, th()); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs", i)
		}
	}
	if !reflect.DeepEqual(cands, snapshot) {
		t.Errorf("Rank mutated its input")
	}
	if !reflect.DeepEqual(ids(first), []string{"a", "c", "b"}) {
		t.Errorf("Rank = %v", ids(first))
	}
}

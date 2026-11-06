package decision

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/pricing"
	"github.com/Foncifelo/sidecar-proxy/pkg/openai"
)

// All endpoints, prices and latencies in this file are fictitious.

const testModel = "fictitious/llama-70b"

var refUsage = openai.Usage{In: 1_000_000, Out: 1_000_000}

func price(id string, inPerM, outPerM int64, feeBps int32) pricing.Price {
	return pricing.Price{PriceID: "p-" + id, EndpointID: id, InputPerM: inPerM, OutputPerM: outPerM, ChannelFeeBps: feeBps}
}

func fullCaps() Caps {
	return Caps{Tools: true, JSONSchema: true, Vision: true, Logprobs: true, MaxN: 4, ContextLength: 128_000}
}

func goodStats() Stats {
	return Stats{Uptime1d: 0.999, TTFTP90: 300 * time.Millisecond, TPSP50: 80}
}

// cand builds a fictitious candidate that passes every filter against cur().
func cand(id string, inPerM, outPerM int64) Candidate {
	return Candidate{
		EndpointID:   id,
		Model:        testModel,
		Quantization: QuantFP8,
		Caps:         fullCaps(),
		Stats:        goodStats(),
		Price:        price(id, inPerM, outPerM, 0),
	}
}

func cur() Candidate { return cand("current", 1_000_000, 2_000_000) } // costs 3_000_000

func th() Thresholds {
	return Thresholds{MinUptime1d: DefaultMinUptime1d, ClientTTFTP95: 500 * time.Millisecond, ClientTPSP50: 50}
}

func allReq() Requirements {
	return Requirements{Tools: true, JSONSchema: true, Vision: true, Logprobs: true, N: 2, ContextTokens: 100_000, RefUsage: refUsage}
}

func TestDecideDiscardReasons(t *testing.T) {
	tests := []struct {
		name   string
		req    Requirements
		th     Thresholds
		mutate func(*Candidate)
		want   Reason
	}{
		{"same as current", allReq(), th(), func(c *Candidate) { c.EndpointID = "current" }, ReasonSameAsCurrent},
		{"tools", allReq(), th(), func(c *Candidate) { c.Caps.Tools = false }, ReasonNoTools},
		{"json_schema", allReq(), th(), func(c *Candidate) { c.Caps.JSONSchema = false }, ReasonNoJSONSchema},
		{"vision", allReq(), th(), func(c *Candidate) { c.Caps.Vision = false }, ReasonNoVision},
		{"logprobs", allReq(), th(), func(c *Candidate) { c.Caps.Logprobs = false }, ReasonNoLogprobs},
		{"n>1 unsupported", allReq(), th(), func(c *Candidate) { c.Caps.MaxN = 0 }, ReasonNTooLarge},
		{"context too small", allReq(), th(), func(c *Candidate) { c.Caps.ContextLength = 32_000 }, ReasonContextTooBig},
		{"context unknown", allReq(), th(), func(c *Candidate) { c.Caps.ContextLength = 0 }, ReasonContextTooBig},
		{"other model", allReq(), th(), func(c *Candidate) { c.Model = "fictitious/qwen-72b" }, ReasonOtherModel},
		{"worse quant", allReq(), th(), func(c *Candidate) { c.Quantization = QuantInt8 }, ReasonWorseQuant},
		{"unknown quant vs fp8", allReq(), th(), func(c *Candidate) { c.Quantization = QuantUnknown }, ReasonWorseQuant},
		{"low uptime", allReq(), th(), func(c *Candidate) { c.Stats.Uptime1d = 0.98 }, ReasonLowUptime},
		{"slow ttft", allReq(), th(), func(c *Candidate) { c.Stats.TTFTP90 = 600 * time.Millisecond }, ReasonSlowTTFT},
		{"missing ttft", allReq(), th(), func(c *Candidate) { c.Stats.TTFTP90 = 0 }, ReasonSlowTTFT},
		{"slow tps", allReq(), th(), func(c *Candidate) { c.Stats.TPSP50 = 40 }, ReasonSlowTPS},
		{"price error", allReq(), th(), func(c *Candidate) { c.Price.InputPerM = -1 }, ReasonPriceError},
		{"price overflow", allReq(), th(), func(c *Candidate) { c.Price.InputPerM = math.MaxInt64 }, ReasonPriceError},
		// Order: the first failing filter is the one reported.
		{"caps before model", allReq(), th(), func(c *Candidate) { c.Caps.Tools = false; c.Model = "x" }, ReasonNoTools},
		{"model before quant", allReq(), th(), func(c *Candidate) { c.Model = "x"; c.Quantization = QuantInt4 }, ReasonOtherModel},
		{"quant before uptime", allReq(), th(), func(c *Candidate) { c.Quantization = QuantInt4; c.Stats.Uptime1d = 0 }, ReasonWorseQuant},
		{"uptime before ttft", allReq(), th(), func(c *Candidate) { c.Stats.Uptime1d = 0; c.Stats.TTFTP90 = time.Hour }, ReasonLowUptime},
		{"ttft before tps", allReq(), th(), func(c *Candidate) { c.Stats.TTFTP90 = time.Hour; c.Stats.TPSP50 = 0 }, ReasonSlowTTFT},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cand("cheap", 100_000, 100_000)
			tt.mutate(&c)
			d := Decide(tt.req, cur(), []Candidate{c}, tt.th)
			if d.Best.EndpointID != "current" {
				t.Fatalf("Best = %q, want current", d.Best.EndpointID)
			}
			if d.Reason != ReasonNoCandidate {
				t.Errorf("Reason = %q, want %q", d.Reason, ReasonNoCandidate)
			}
			want := []Discard{{EndpointID: c.EndpointID, Reason: tt.want}}
			if !reflect.DeepEqual(d.Discards, want) {
				t.Errorf("Discards = %+v, want %+v", d.Discards, want)
			}
		})
	}
}

func TestDecidePassesWhenNotRequired(t *testing.T) {
	tests := []struct {
		name   string
		req    Requirements
		th     Thresholds
		mutate func(*Candidate)
	}{
		{"no caps required", Requirements{RefUsage: refUsage}, th(), func(c *Candidate) { c.Caps = Caps{} }},
		{"n=1 with MaxN 0", Requirements{N: 1, RefUsage: refUsage}, th(), func(c *Candidate) { c.Caps.MaxN = 0 }},
		{"context exactly fits", Requirements{ContextTokens: 128_000, RefUsage: refUsage}, th(), func(*Candidate) {}},
		{"better quant", allReq(), th(), func(c *Candidate) { c.Quantization = QuantBF16 }},
		{"uptime equal threshold", allReq(), th(), func(c *Candidate) { c.Stats.Uptime1d = DefaultMinUptime1d }},
		{"ttft equal client p95", allReq(), th(), func(c *Candidate) { c.Stats.TTFTP90 = 500 * time.Millisecond }},
		{"tps equal client", allReq(), th(), func(c *Candidate) { c.Stats.TPSP50 = 50 }},
		{"no client latency data", allReq(), Thresholds{MinUptime1d: DefaultMinUptime1d}, func(c *Candidate) { c.Stats.TTFTP90 = 0; c.Stats.TPSP50 = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cand("cheap", 100_000, 100_000)
			tt.mutate(&c)
			d := Decide(tt.req, cur(), []Candidate{c}, tt.th)
			if d.Best.EndpointID != "cheap" || d.Reason != ReasonCheaper {
				t.Fatalf("Best = %q Reason = %q, want cheap/%q; discards %+v", d.Best.EndpointID, d.Reason, ReasonCheaper, d.Discards)
			}
			if len(d.Discards) != 0 {
				t.Errorf("Discards = %+v, want none", d.Discards)
			}
		})
	}
}

func TestQuantAtLeast(t *testing.T) {
	tests := []struct {
		c, cur Quantization
		want   bool
	}{
		{QuantBF16, QuantFP16, true},
		{QuantFP16, QuantBF16, true},
		{QuantFP8, QuantFP16, false},
		{QuantFP16, QuantFP8, true},
		{QuantFP8, QuantFP8, true},
		{QuantInt8, QuantFP8, false},
		{QuantFP8, QuantInt8, true},
		{QuantInt4, QuantInt8, false},
		{QuantInt8, QuantInt4, true},
		{QuantUnknown, QuantUnknown, true},
		{QuantUnknown, QuantInt4, false},
		{QuantBF16, QuantUnknown, false},
		{Quantization("weird"), Quantization("weird"), false},
		{Quantization("weird"), QuantUnknown, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.c)+"_vs_"+string(tt.cur), func(t *testing.T) {
			if got := quantAtLeast(tt.c, tt.cur); got != tt.want {
				t.Errorf("quantAtLeast(%q, %q) = %v, want %v", tt.c, tt.cur, got, tt.want)
			}
		})
	}
}

// TestQuantAtLeastFP4 covers fp4 against every value of the enum in both
// directions: fp4 ties with int4, ranks below int8 and never matches
// unknown.
func TestQuantAtLeastFP4(t *testing.T) {
	tests := []struct {
		other           Quantization
		fp4AtLeastOther bool // quantAtLeast(fp4, other)
		otherAtLeastFP4 bool // quantAtLeast(other, fp4)
	}{
		{QuantBF16, false, true},
		{QuantFP16, false, true},
		{QuantFP8, false, true},
		{QuantInt8, false, true},
		{QuantInt4, true, true},
		{QuantFP4, true, true},
		{QuantUnknown, false, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.other), func(t *testing.T) {
			if got := quantAtLeast(QuantFP4, tt.other); got != tt.fp4AtLeastOther {
				t.Errorf("quantAtLeast(fp4, %q) = %v, want %v", tt.other, got, tt.fp4AtLeastOther)
			}
			if got := quantAtLeast(tt.other, QuantFP4); got != tt.otherAtLeastFP4 {
				t.Errorf("quantAtLeast(%q, fp4) = %v, want %v", tt.other, got, tt.otherAtLeastFP4)
			}
		})
	}
}

// TestUnrecognisedQuantNeverEligible is the fp4 compatibility check: a
// sidecar built before fp4 existed sees "fp4" as an unrecognised value, which
// is what quantFromTheFuture stands for here. Such a candidate is discarded
// for worse_quantization whatever the current quantization is, and a current
// endpoint carrying it accepts no candidate.
func TestUnrecognisedQuantNeverEligible(t *testing.T) {
	const quantFromTheFuture = Quantization("fp3")
	known := []Quantization{QuantBF16, QuantFP16, QuantFP8, QuantInt8, QuantInt4, QuantFP4, QuantUnknown}
	for _, q := range known {
		t.Run("candidate_vs_"+string(q), func(t *testing.T) {
			current := cur()
			current.Quantization = q
			c := cand("cheap", 100_000, 100_000)
			c.Quantization = quantFromTheFuture
			d := Decide(allReq(), current, []Candidate{c}, th())
			if d.Best.EndpointID != "current" || d.Reason != ReasonNoCandidate {
				t.Fatalf("Best = %q Reason = %q, want current/%q", d.Best.EndpointID, d.Reason, ReasonNoCandidate)
			}
			if want := []Discard{{"cheap", ReasonWorseQuant}}; !reflect.DeepEqual(d.Discards, want) {
				t.Errorf("Discards = %+v, want %+v", d.Discards, want)
			}
		})
		t.Run("current_vs_"+string(q), func(t *testing.T) {
			current := cur()
			current.Quantization = quantFromTheFuture
			c := cand("cheap", 100_000, 100_000)
			c.Quantization = q
			d := Decide(allReq(), current, []Candidate{c}, th())
			if d.Best.EndpointID != "current" || d.Reason != ReasonNoCandidate {
				t.Fatalf("Best = %q Reason = %q, want current/%q", d.Best.EndpointID, d.Reason, ReasonNoCandidate)
			}
		})
	}
}

func TestDecideCheapestWins(t *testing.T) {
	withFee := cand("b-openrouter", 900_000, 1_800_000) // 2_700_000 * 1.2 = 3_240_000
	withFee.Price.ChannelFeeBps = 2_000

	tests := []struct {
		name       string
		cands      []Candidate
		wantBest   string
		wantReason Reason
		wantCost   events.Micros
		wantDisc   []Discard
	}{
		{
			name:       "cheapest of several",
			cands:      []Candidate{cand("b", 500_000, 1_000_000), cand("a", 400_000, 800_000), cand("c", 2_000_000, 2_000_000)},
			wantBest:   "a",
			wantReason: ReasonCheaper,
			wantCost:   1_200_000,
			wantDisc:   []Discard{{"b", ReasonNotCheapest}, {"c", ReasonNotCheapest}},
		},
		{
			name:       "tie broken by endpoint id",
			cands:      []Candidate{cand("z", 500_000, 500_000), cand("m", 500_000, 500_000), cand("y", 500_000, 500_000)},
			wantBest:   "m",
			wantReason: ReasonCheaper,
			wantCost:   1_000_000,
			wantDisc:   []Discard{{"z", ReasonNotCheapest}, {"y", ReasonNotCheapest}},
		},
		{
			name:       "channel fee makes it lose",
			cands:      []Candidate{withFee},
			wantBest:   "current",
			wantReason: ReasonCurrentCheaper,
			wantCost:   3_000_000,
			wantDisc:   []Discard{{"b-openrouter", ReasonNotCheapest}},
		},
		{
			name:       "current already cheapest",
			cands:      []Candidate{cand("x", 2_000_000, 2_000_000)},
			wantBest:   "current",
			wantReason: ReasonCurrentCheaper,
			wantCost:   3_000_000,
			wantDisc:   []Discard{{"x", ReasonNotCheapest}},
		},
		{
			name:       "equal cost keeps current",
			cands:      []Candidate{cand("a", 1_000_000, 2_000_000)},
			wantBest:   "current",
			wantReason: ReasonCurrentCheaper,
			wantCost:   3_000_000,
			wantDisc:   []Discard{{"a", ReasonNotCheapest}},
		},
		{
			name:       "no candidates",
			cands:      nil,
			wantBest:   "current",
			wantReason: ReasonNoCandidate,
			wantCost:   3_000_000,
			wantDisc:   []Discard{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(allReq(), cur(), tt.cands, th())
			if d.Best.EndpointID != tt.wantBest || d.Reason != tt.wantReason {
				t.Fatalf("Best = %q Reason = %q, want %q/%q", d.Best.EndpointID, d.Reason, tt.wantBest, tt.wantReason)
			}
			if d.CostCurrent != 3_000_000 || d.CostBest != tt.wantCost {
				t.Errorf("CostCurrent = %d CostBest = %d, want 3000000/%d", d.CostCurrent, d.CostBest, tt.wantCost)
			}
			if !reflect.DeepEqual(d.Discards, tt.wantDisc) {
				t.Errorf("Discards = %+v, want %+v", d.Discards, tt.wantDisc)
			}
		})
	}
}

func TestDecideCurrentPriceError(t *testing.T) {
	c := cur()
	c.Price.OutputPerM = -1
	d := Decide(allReq(), c, []Candidate{cand("a", 1, 1)}, th())
	if d.Best.EndpointID != "current" || d.Reason != ReasonCurrentNoPrice {
		t.Fatalf("Best = %q Reason = %q", d.Best.EndpointID, d.Reason)
	}
	want := []Discard{{"a", ReasonCurrentNoPrice}}
	if !reflect.DeepEqual(d.Discards, want) {
		t.Errorf("Discards = %+v, want %+v", d.Discards, want)
	}
}

func TestDecideDeterministicAndPure(t *testing.T) {
	cands := []Candidate{cand("c", 600_000, 600_000), cand("a", 600_000, 600_000), cand("b", 700_000, 700_000)}
	snapshot := append([]Candidate(nil), cands...)
	first := Decide(allReq(), cur(), cands, th())
	for i := 0; i < 50; i++ {
		if got := Decide(allReq(), cur(), cands, th()); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs: %+v vs %+v", i, got, first)
		}
	}
	if !reflect.DeepEqual(cands, snapshot) {
		t.Errorf("Decide mutated its input")
	}
	if first.Best.EndpointID != "a" {
		t.Errorf("Best = %q, want a", first.Best.EndpointID)
	}
}

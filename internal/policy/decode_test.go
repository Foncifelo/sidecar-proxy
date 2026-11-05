package policy

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/decision"
	"github.com/Foncifelo/sidecar-proxy/internal/logging"
)

// All models, endpoints, prices and latencies below are fictitious: they only
// illustrate the format of docs/politica.md.

const exampleDoc = `{
  "schema_version": 1,
  "version": "p_fictitious",
  "computed_at": "2026-10-29T14:00:00Z",
  "flags": {},
  "models": {
    "fict-model": {
      "aliases": {"acme": ["acme/fict-model"], "openrouter": ["fict/fict-model"]},
      "client_ttft_p95_ms": 900,
      "client_tps_p50": 60.5,
      "ref_usage": {"tokens_in": 1800, "tokens_out": 350, "tokens_reasoning": 0, "tokens_cached_read": 600, "tokens_cached_write": 0},
      "route_enabled": true,
      "current": [
        {"provider": "acme", "raw_model": "acme/fict-model", "endpoint_id": "acme/fict-model@fp8/standard/global/direct",
         "quantization": "fp8",
         "price": {"price_id": "1", "input_per_m": 270000, "output_per_m": 1100000, "cached_in_per_m": 0, "cache_write_per_m": 0, "reasoning_out_per_m": 0, "per_request": 0, "channel_fee_bps": 0}}
      ],
      "candidates": [
        {"endpoint_id": "beta/fict-model@fp8/standard/global/openrouter", "provider": "beta", "channel": "openrouter",
         "provider_model_name": "fict/fict-model", "quantization": "fp8",
         "caps": {"tools": true, "json_schema": true, "vision": false, "logprobs": false, "n": 1},
         "context_length": 131072,
         "price": {"price_id": "2", "input_per_m": 100000, "output_per_m": 400000, "cached_in_per_m": 25000, "cache_write_per_m": 0, "reasoning_out_per_m": 0, "per_request": 0, "channel_fee_bps": 550},
         "stats": {"uptime_1d": 0.998, "p90_ttft_ms": 640, "tps_p50": 72.0, "source": "openrouter"}},
        {"endpoint_id": "gamma/fict-model@bf16/standard/global/direct", "provider": "gamma", "channel": "direct",
         "provider_model_name": "gamma/fict-model", "quantization": "bf16",
         "caps": {"tools": true, "json_schema": false, "vision": false, "logprobs": false, "n": 1},
         "context_length": 65536,
         "price": {"price_id": "3", "input_per_m": 200000, "output_per_m": 800000, "cached_in_per_m": 0, "cache_write_per_m": 0, "reasoning_out_per_m": 0, "per_request": 0, "channel_fee_bps": 0},
         "stats": {"uptime_1d": 0.99, "p90_ttft_ms": 500, "tps_p50": 80.0, "source": "probe"}},
        {"endpoint_id": "gamma/fict-model@fp16/standard/global/direct", "provider": "gamma", "channel": "direct",
         "provider_model_name": "gamma/fict-model-fp16", "quantization": "fp16",
         "caps": {"tools": true, "json_schema": false, "vision": false, "logprobs": false, "n": 1},
         "context_length": 0,
         "price": {"price_id": "4", "input_per_m": 200000, "output_per_m": 800000, "cached_in_per_m": 0, "cache_write_per_m": 0, "reasoning_out_per_m": 0, "per_request": 0, "channel_fee_bps": 0},
         "stats": {"uptime_1d": 1, "p90_ttft_ms": 0, "tps_p50": 0, "source": "probe"}}
      ]
    }
  }
}`

// mutateDoc decodes exampleDoc into a generic tree, applies f and re-encodes.
func mutateDoc(t *testing.T, f func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(exampleDoc), &doc); err != nil {
		t.Fatal(err)
	}
	f(doc)
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func model(doc map[string]any) map[string]any {
	return doc["models"].(map[string]any)["fict-model"].(map[string]any)
}

func cand(doc map[string]any, i int) map[string]any {
	return model(doc)["candidates"].([]any)[i].(map[string]any)
}

func curr(doc map[string]any) map[string]any {
	return model(doc)["current"].([]any)[0].(map[string]any)
}

func TestDecodeExample(t *testing.T) {
	p, err := Decode([]byte(exampleDoc))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	m := p.Models["fict-model"]
	if p.SchemaVersion != 1 || p.Version != "p_fictitious" || !p.ComputedAt.Equal(time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("top level = %+v", p)
	}
	if !m.RouteEnabled || m.ClientTTFTP95Ms != 900 || m.ClientTPSP50 != 60.5 || m.RefUsage.TokensCachedRead != 600 {
		t.Errorf("model = %+v", m)
	}
	if len(m.Candidates) != 3 || len(m.Current) != 1 {
		t.Fatalf("got %d candidates, %d current", len(m.Candidates), len(m.Current))
	}
	c := m.Candidates[0]
	if c.Channel != ChannelOpenRouter || c.Quantization != decision.QuantFP8 || c.Price.ChannelFeeBps != 550 ||
		c.Price.InputPerM != 100000 || c.Stats.Source != SourceOpenRouter || !c.Caps.Tools || c.Caps.N != 1 {
		t.Errorf("candidate = %+v", c)
	}
	if got := m.Aliases["openrouter"]; len(got) != 1 || got[0] != "fict/fict-model" {
		t.Errorf("aliases = %v", m.Aliases)
	}
}

func TestDecode(t *testing.T) {
	tests := []struct {
		name    string
		doc     []byte
		wantErr error // nil means success
	}{
		{"example", []byte(exampleDoc), nil},
		{"empty models", []byte(`{"schema_version":1,"version":"p_empty","computed_at":"2026-10-29T14:00:00Z","flags":{},"models":{}}`), nil},
		{"unknown flag ignored", mutateDoc(t, func(d map[string]any) { d["flags"] = map[string]any{"future": true} }), nil},
		{"current absent", mutateDoc(t, func(d map[string]any) { delete(model(d), "current") }), nil},
		{"schema too new", mutateDoc(t, func(d map[string]any) { d["schema_version"] = SchemaVersion + 1 }), ErrUnsupportedSchema},
		{"schema zero", mutateDoc(t, func(d map[string]any) { d["schema_version"] = 0 }), ErrUnsupportedSchema},
		{"schema missing", mutateDoc(t, func(d map[string]any) { delete(d, "schema_version") }), ErrUnsupportedSchema},
		{"broken json", []byte(`{"schema_version":1,`), ErrInvalid},
		{"amount not integer", mutateDoc(t, func(d map[string]any) { cand(d, 0)["price"].(map[string]any)["input_per_m"] = 1.5 }), ErrInvalid},
		{"amount as string", mutateDoc(t, func(d map[string]any) { cand(d, 0)["price"].(map[string]any)["output_per_m"] = "400000" }), ErrInvalid},
		{"fee not integer", mutateDoc(t, func(d map[string]any) { cand(d, 0)["price"].(map[string]any)["channel_fee_bps"] = 5.5 }), ErrInvalid},
		{"negative amount", mutateDoc(t, func(d map[string]any) { cand(d, 2)["price"].(map[string]any)["per_request"] = -1 }), ErrInvalid},
		{"negative fee", mutateDoc(t, func(d map[string]any) { cand(d, 0)["price"].(map[string]any)["channel_fee_bps"] = -1 }), ErrInvalid},
		{"current amount not integer", mutateDoc(t, func(d map[string]any) { curr(d)["price"].(map[string]any)["input_per_m"] = 0.25 }), ErrInvalid},
		{"empty version", mutateDoc(t, func(d map[string]any) { d["version"] = "" }), ErrInvalid},
		{"empty model id", mutateDoc(t, func(d map[string]any) {
			d["models"].(map[string]any)[""] = map[string]any{"candidates": []any{}}
		}), ErrInvalid},
		{"empty candidate endpoint_id", mutateDoc(t, func(d map[string]any) { cand(d, 0)["endpoint_id"] = "" }), ErrInvalid},
		{"empty current endpoint_id", mutateDoc(t, func(d map[string]any) { curr(d)["endpoint_id"] = "" }), ErrInvalid},
		{"empty current provider", mutateDoc(t, func(d map[string]any) { curr(d)["provider"] = "" }), ErrInvalid},
		{"empty current raw_model", mutateDoc(t, func(d map[string]any) { curr(d)["raw_model"] = "" }), ErrInvalid},
		{"unknown current quantization", mutateDoc(t, func(d map[string]any) { curr(d)["quantization"] = "fp4" }), ErrInvalid},
		{"empty provider", mutateDoc(t, func(d map[string]any) { cand(d, 1)["provider"] = "" }), ErrInvalid},
		{"empty provider_model_name", mutateDoc(t, func(d map[string]any) { cand(d, 1)["provider_model_name"] = "" }), ErrInvalid},
		{"unknown channel", mutateDoc(t, func(d map[string]any) { cand(d, 1)["channel"] = "relay" }), ErrInvalid},
		{"unknown quantization", mutateDoc(t, func(d map[string]any) { cand(d, 1)["quantization"] = "fp4" }), ErrInvalid},
		{"negative n", mutateDoc(t, func(d map[string]any) { cand(d, 1)["caps"].(map[string]any)["n"] = -1 }), ErrInvalid},
		{"negative context", mutateDoc(t, func(d map[string]any) { cand(d, 1)["context_length"] = -1 }), ErrInvalid},
		{"uptime above 1", mutateDoc(t, func(d map[string]any) { cand(d, 1)["stats"].(map[string]any)["uptime_1d"] = 1.01 }), ErrInvalid},
		{"negative p90", mutateDoc(t, func(d map[string]any) { cand(d, 1)["stats"].(map[string]any)["p90_ttft_ms"] = -1 }), ErrInvalid},
		{"negative tps", mutateDoc(t, func(d map[string]any) { cand(d, 1)["stats"].(map[string]any)["tps_p50"] = -1 }), ErrInvalid},
		{"unknown source", mutateDoc(t, func(d map[string]any) { cand(d, 1)["stats"].(map[string]any)["source"] = "guess" }), ErrInvalid},
		{"price_id not decimal", mutateDoc(t, func(d map[string]any) { cand(d, 1)["price"].(map[string]any)["price_id"] = "p3" }), ErrInvalid},
		{"price_id empty", mutateDoc(t, func(d map[string]any) { cand(d, 1)["price"].(map[string]any)["price_id"] = "" }), ErrInvalid},
		{"negative client ttft", mutateDoc(t, func(d map[string]any) { model(d)["client_ttft_p95_ms"] = -1 }), ErrInvalid},
		{"negative client tps", mutateDoc(t, func(d map[string]any) { model(d)["client_tps_p50"] = -0.5 }), ErrInvalid},
		{"negative ref usage", mutateDoc(t, func(d map[string]any) { model(d)["ref_usage"].(map[string]any)["tokens_out"] = -1 }), ErrInvalid},
		{"candidates not ordered by cost", mutateDoc(t, func(d map[string]any) {
			cs := model(d)["candidates"].([]any)
			cs[0], cs[1] = cs[1], cs[0]
		}), ErrInvalid},
		{"tie not ordered by endpoint_id", mutateDoc(t, func(d map[string]any) {
			cs := model(d)["candidates"].([]any)
			cs[1], cs[2] = cs[2], cs[1]
		}), ErrInvalid},
		{"duplicate endpoint_id", mutateDoc(t, func(d map[string]any) { cand(d, 2)["endpoint_id"] = cand(d, 1)["endpoint_id"] }), ErrInvalid},
		{"cost overflow", mutateDoc(t, func(d map[string]any) {
			cand(d, 2)["price"].(map[string]any)["per_request"] = int64(math.MaxInt64)
		}), ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Decode(tt.doc)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Decode: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(p, Policy{}) {
				t.Errorf("got non-zero policy on error: %+v", p)
			}
		})
	}
}

// secretLikeFields are JSON fields a hostile or buggy control plane might add.
var secretLikeFields = map[string]any{
	"url":           "https://evil.example/v1",
	"base_url":      "https://evil.example/v1",
	"api_key":       "sk-fictitious",
	"headers":       map[string]any{"Authorization": "Bearer sk-fictitious"},
	"auth_header":   "X-Api-Key",
	"provider_url":  "https://evil.example",
	"openrouter_id": "x",
}

func addSecretLike(m map[string]any) {
	for k, v := range secretLikeFields {
		m[k] = v
	}
}

func TestDecodeIgnoresSecretLikeFields(t *testing.T) {
	clean, err := Decode([]byte(exampleDoc))
	if err != nil {
		t.Fatal(err)
	}
	dirty, err := Decode(mutateDoc(t, func(d map[string]any) {
		addSecretLike(d)
		addSecretLike(d["flags"].(map[string]any))
		addSecretLike(model(d))
		addSecretLike(curr(d))
		addSecretLike(curr(d)["price"].(map[string]any))
		for i := range model(d)["candidates"].([]any) {
			c := cand(d, i)
			addSecretLike(c)
			addSecretLike(c["price"].(map[string]any))
			addSecretLike(c["stats"].(map[string]any))
			addSecretLike(c["caps"].(map[string]any))
		}
	}))
	if err != nil {
		t.Fatalf("Decode with extra fields: %v", err)
	}
	if !reflect.DeepEqual(clean, dirty) {
		t.Fatalf("extra fields changed the decoded policy:\nclean %+v\ndirty %+v", clean, dirty)
	}
	for _, cfg := range []struct{ name string }{{"route"}, {"shadow"}} {
		c := testConfig(t, cfg.name)
		fc, dc := mustFilter(clean, c), mustFilter(dirty, c)
		if !reflect.DeepEqual(fc, dc) {
			t.Errorf("%s: extra fields changed Filter result", cfg.name)
		}
	}
}

func TestTypesHaveNoURLOrKeyFields(t *testing.T) {
	banned := []string{"url", "uri", "key", "secret", "header", "auth", "password", "token_"}
	secretType := reflect.TypeOf(logging.Secret(""))
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map || typ.Kind() == reflect.Array {
			if typ.Kind() == reflect.Map {
				walk(typ.Key(), path+"[key]")
			}
			typ = typ.Elem()
		}
		if typ == secretType {
			t.Errorf("%s has type logging.Secret", path)
		}
		if typ.Kind() != reflect.Struct || seen[typ] || typ.PkgPath() == "time" {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			f := typ.Field(i)
			tag := strings.ToLower(strings.Split(f.Tag.Get("json"), ",")[0])
			name := strings.ToLower(f.Name)
			for _, b := range banned {
				// token_ catches e.g. "token_value" while allowing the
				// usage counters "tokens_in" and friends.
				if strings.Contains(name, b) || strings.Contains(tag, b) {
					t.Errorf("%s.%s (json %q) looks like a URL, header or key field", path, f.Name, tag)
				}
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	walk(reflect.TypeOf(Policy{}), "Policy")
	if len(seen) < 8 {
		t.Fatalf("walked only %d struct types; reflection walk is broken", len(seen))
	}
}

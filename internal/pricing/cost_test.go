package pricing

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/pkg/openai"
)

// All prices in these tests are fictitious.

func TestCost(t *testing.T) {
	t.Parallel()
	base := Price{PriceID: "1", InputPerM: 1_000_000, OutputPerM: 2_000_000} // $1/M in, $2/M out
	tests := []struct {
		name  string
		price Price
		usage openai.Usage
		want  events.Micros
	}{
		{"zero usage", base, openai.Usage{}, 0},
		{"input only", base, openai.Usage{In: 1_000}, 1_000},
		{"input and output", base, openai.Usage{In: 1_000, Out: 500}, 2_000},
		{"per request only", Price{PerRequest: 42}, openai.Usage{In: 10}, 42},
		{"every bucket with own rates", Price{
			InputPerM: 1_000_000, OutputPerM: 2_000_000, ReasoningOutPerM: 3_000_000,
			CachedInPerM: 100_000, CacheWritePerM: 1_250_000, PerRequest: 7,
		}, openai.Usage{In: 1_000, Out: 1_000, Reasoning: 1_000, CachedRead: 10_000, CachedWrite: 1_000}, 1_000 + 2_000 + 3_000 + 1_000 + 1_250 + 7},
		{"reasoning falls back to output rate", base, openai.Usage{Reasoning: 1_000}, 2_000},
		{"cached read falls back to input rate", base, openai.Usage{CachedRead: 1_000}, 1_000},
		{"cache write falls back to input rate", base, openai.Usage{CachedWrite: 1_000}, 1_000},
		{"fee 0 bps", Price{InputPerM: 1_000_000, PerRequest: 100}, openai.Usage{In: 900}, 1_000},
		{"fee 550 bps", Price{InputPerM: 1_000_000, PerRequest: 100, ChannelFeeBps: 550}, openai.Usage{In: 900}, 1_055},
		{"fee 550 bps rounds half up", Price{PerRequest: 10, ChannelFeeBps: 550}, openai.Usage{}, 11},                    // 10.55 -> 11
		{"fee 550 bps rounds down below half", Price{PerRequest: 2, ChannelFeeBps: 550}, openai.Usage{}, 2},              // 2.11 -> 2
		{"exactly half rounds up", Price{InputPerM: 1}, openai.Usage{In: 500_000}, 1},                                    // 0.5 -> 1
		{"just below half rounds down", Price{InputPerM: 1}, openai.Usage{In: 499_999}, 0},                               // 0.499999 -> 0
		{"just above half rounds up", Price{InputPerM: 1}, openai.Usage{In: 500_001}, 1},                                 // 0.500001 -> 1
		{"single rounding over the sum", Price{InputPerM: 1, OutputPerM: 1}, openai.Usage{In: 400_000, Out: 400_000}, 1}, // 0.4+0.4 -> 1
		{"fee 10000 bps doubles", Price{PerRequest: 3, ChannelFeeBps: 10_000}, openai.Usage{}, 6},
		{"large but representable", Price{InputPerM: 1_000_000_000}, openai.Usage{In: 1_000_000_000_000}, 1_000_000_000_000_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Cost(tt.price, tt.usage)
			if err != nil {
				t.Fatalf("Cost() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Cost() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCostErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		price Price
		usage openai.Usage
		want  error
	}{
		{"overflow tokens times rate", Price{InputPerM: math.MaxInt64}, openai.Usage{In: math.MaxInt64}, ErrOverflow},
		{"overflow per request with fee", Price{PerRequest: math.MaxInt64, ChannelFeeBps: 1}, openai.Usage{}, ErrOverflow},
		{"negative tokens", Price{InputPerM: 1}, openai.Usage{In: -1}, ErrNegative},
		{"negative rate", Price{OutputPerM: -1}, openai.Usage{Out: 1}, ErrNegative},
		{"negative reasoning rate", Price{ReasoningOutPerM: -1}, openai.Usage{Reasoning: 1}, ErrNegative},
		{"negative per request", Price{PerRequest: -1}, openai.Usage{}, ErrNegative},
		{"negative fee", Price{ChannelFeeBps: -1}, openai.Usage{}, ErrNegative},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Cost(tt.price, tt.usage)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Cost() error = %v, want %v", err, tt.want)
			}
			if got != 0 {
				t.Errorf("Cost() = %d on error, want 0", got)
			}
		})
	}
}

func TestCostMaxInt64IsRepresentable(t *testing.T) {
	t.Parallel()
	got, err := Cost(Price{PerRequest: math.MaxInt64}, openai.Usage{})
	if err != nil || got != math.MaxInt64 {
		t.Fatalf("Cost() = %d, %v; want MaxInt64, nil", got, err)
	}
}

// TestNoFloatMoney checks that every money field of Price is an integer.
func TestNoFloatMoney(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[Price]()
	for i := range typ.NumField() {
		k := typ.Field(i).Type.Kind()
		if k == reflect.Float32 || k == reflect.Float64 || k == reflect.Complex64 || k == reflect.Complex128 {
			t.Errorf("Price.%s is a float", typ.Field(i).Name)
		}
	}
	if k := reflect.TypeFor[events.Micros]().Kind(); k != reflect.Int64 {
		t.Errorf("events.Micros kind = %s, want int64", k)
	}
}

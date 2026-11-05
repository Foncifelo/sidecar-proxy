package pricing

import (
	"errors"
	"testing"
	"time"
)

func TestSelect(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(24 * time.Hour)
	t2 := t1.Add(24 * time.Hour)

	listOld := Price{PriceID: "10", EffectiveFrom: t0, EffectiveTo: t1}
	listCur := Price{PriceID: "11", EffectiveFrom: t1}
	custCur := Price{PriceID: "c1", Negotiated: true, EffectiveFrom: t1, EffectiveTo: t2}
	list := []Price{listOld, listCur}

	tests := []struct {
		name     string
		list     []Price
		customer []Price
		at       time.Time
		wantID   string
		wantErr  error
	}{
		{"before any price", list, nil, t0.Add(-time.Nanosecond), "", ErrNoPrice},
		{"at effective_from is inclusive", list, nil, t0, "10", nil},
		{"just before effective_to", list, nil, t1.Add(-time.Nanosecond), "10", nil},
		{"at effective_to is exclusive", list, nil, t1, "11", nil},
		{"open-ended price far in the future", list, nil, t2.Add(1000 * time.Hour), "11", nil},
		{"negotiated wins when valid", list, []Price{custCur}, t1, "c1", nil},
		{"negotiated not yet valid", list, []Price{custCur}, t1.Add(-time.Nanosecond), "10", nil},
		{"negotiated expired at effective_to", list, []Price{custCur}, t2, "11", nil},
		{"only negotiated", nil, []Price{custCur}, t1, "c1", nil},
		{"no prices at all", nil, nil, t1, "", ErrNoPrice},
		{"overlap picks latest effective_from", []Price{{PriceID: "a", EffectiveFrom: t0}, {PriceID: "b", EffectiveFrom: t1}, {PriceID: "c", EffectiveFrom: t0.Add(time.Hour)}}, nil, t2, "b", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Select(tt.list, tt.customer, tt.at)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Select() error = %v, want %v", err, tt.wantErr)
			}
			if got.PriceID != tt.wantID {
				t.Errorf("Select() PriceID = %q, want %q", got.PriceID, tt.wantID)
			}
		})
	}
}

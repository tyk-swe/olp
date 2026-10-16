package usage

import (
	"testing"
	"time"
)

// CostFloor prices the input estimate at the cheapest input-side rate of the
// selected rate set, plus the per-request fee, scaled by window and discount
// and rounded down.
func TestCostFloor(t *testing.T) {
	ptr := func(v string) *string { return &v }
	now := time.Now().UTC()
	price := &RoutingPrice{Price: Price{ProviderKind: "openai", Model: "m", Operation: "generation",
		InputPerMillion: ptr("1"), CachedInputPerMillion: ptr("0.5"),
		PerRequest: ptr("0.01"), Currency: "USD"}}
	floor, ok := price.CostFloor(1000, now, nil)
	if !ok || floor.String() != "0.0105" {
		t.Fatalf("floor = %s %v, want 0.0105", floor, ok)
	}
	// The tier the estimate selects carries its own rates.
	tiered := &RoutingPrice{Price: Price{ProviderKind: "openai", Model: "m", Operation: "generation",
		InputPerMillion: ptr("1"), Currency: "USD",
		Tiers: []PriceTier{{AboveInputTokens: 5000, InputPerMillion: ptr("0.5")}}}}
	floor, ok = tiered.CostFloor(10000, now, nil)
	if !ok || floor.String() != "0.005" {
		t.Fatalf("tiered floor = %s %v, want 0.005", floor, ok)
	}
	// Discount multiplies the floor, fee included: (0.00005 + 0.01) × 0.5.
	discounted := &RoutingDiscount{Multiplier: "0.5"}
	floor, ok = price.CostFloor(100, now, discounted)
	if !ok || floor.String() != "0.005025" {
		t.Fatalf("discounted floor = %s %v", floor, ok)
	}
	if _, ok := (&RoutingPrice{Price: Price{OutputPerMillion: ptr("1")}}).CostFloor(100, now, nil); ok {
		t.Fatal("floored an output-only price")
	}
	if _, ok := (*RoutingPrice)(nil).CostFloor(100, now, nil); ok {
		t.Fatal("floored a nil price")
	}
}

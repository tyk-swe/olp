package usage

import (
	"testing"
	"time"
)

func TestRoutingCardChargeZeroCost(t *testing.T) {
	t.Parallel()
	rev := &RoutingCardRevision{Markup: "2"}
	if charge, ok := rev.Charge(AttemptUsage{}, time.Now(), true, &Cost{}, "kind", "", "model", "generation"); !ok || charge.String() != "0" {
		t.Fatalf("a marked-up zero cost charged %v, %t, want 0, true", charge, ok)
	}
	if charge, ok := rev.Charge(AttemptUsage{}, time.Now(), true, nil, "kind", "", "model", "generation"); ok || !charge.IsZero() {
		t.Fatalf("an unpriced cost charged %v, %t, want 0, false", charge, ok)
	}
}

func TestRoutingCardChargeBoundZero(t *testing.T) {
	t.Parallel()
	rev := &RoutingCardRevision{Markup: "2"}
	if bound, ok := rev.ChargeBound(100, 50, true, time.Now(), Cost{}, "kind", "", "model", "generation"); !ok || bound.String() != "0" {
		t.Fatalf("a marked-up zero bound charged %v, %t, want 0, true", bound, ok)
	}
}

func TestRateCardRevisionReportsUnknownWhenStale(t *testing.T) {
	t.Parallel()
	now := time.Now()
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	inputs := &RoutingInputs{
		RefreshedAt: now,
		RateCards: []RoutingRateCard{{ProjectID: &project, Revisions: []RoutingCardRevision{
			{ID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608", Markup: "1", EffectiveAt: now.Add(-time.Hour)},
		}}},
	}
	if rev, known := inputs.RateCardRevision(&project, now.Add(time.Minute)); !known || rev == nil {
		t.Fatalf("a fresh project card resolved %v, %t, want the revision and known", rev, known)
	}
	stale := *inputs
	if rev, known := stale.RateCardRevision(&project, now.Add(PerformanceMaxAge+time.Second)); known || rev != nil {
		t.Fatalf("stale inputs resolved %v, %t, want nil and unknown", rev, known)
	}
}

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

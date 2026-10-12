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

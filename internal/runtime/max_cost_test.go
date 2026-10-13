package runtime

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/usage"
)

func pricedInputs(ids []string, model string) *usage.RoutingInputs {
	prices := []usage.RoutingPrice{}
	for _, id := range ids {
		prices = append(prices, usage.RoutingPrice{Price: usage.Price{
			ProviderKind: "openai", ProviderID: &id, Model: model, Operation: "generation",
			InputPerMillion: ptr("1"), OutputPerMillion: ptr("10"), Currency: "USD",
		}})
	}
	return &usage.RoutingInputs{Prices: prices, RefreshedAt: time.Now()}
}

func TestMaxCostNarrowsAcrossScopes(t *testing.T) {
	s, slug, ids := planningFixture()
	inputs := pricedInputs(ids, "wire-model")
	price := &inputs.Prices[0]
	bound, ok := price.CostBound(1000, 100, true, time.Now(), nil)
	if !ok {
		t.Fatal("fixture price cannot bound")
	}

	// Without a ceiling the callback never runs.
	options := SelectionOptions{Inputs: inputs, Now: time.Now(), WorstCost: func(Provider, Target, *usage.RoutingPrice, *usage.RoutingDiscount) (usage.Cost, bool) {
		t.Fatal("WorstCost ran with no ceiling")
		return usage.Cost{}, false
	}}
	plan, err := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if err != nil || len(plan.Attempts) == 0 {
		t.Fatalf("plan without ceiling: %v %d", err, len(plan.Attempts))
	}

	// A bound above the ceiling is excluded for the named reason.
	options.Preferences = &Preferences{MaxCost: ptr("0.000000000001")}
	options.WorstCost = func(Provider, Target, *usage.RoutingPrice, *usage.RoutingDiscount) (usage.Cost, bool) {
		return bound, true
	}
	plan, err = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Attempts) != 0 || !slices.ContainsFunc(plan.Decisions, func(d Decision) bool {
		return d.Reason != nil && *d.Reason == "cost_limit_exceeded"
	}) {
		t.Fatalf("no cost_limit_exceeded decision: %+v", plan.Decisions)
	}

	// The narrowest scope wins: a tighter key ceiling beats a wider header.
	s.InstallationPolicy = &Policy{Constraints: Preferences{MaxCost: ptr("9")}}
	options.Preferences = &Preferences{MaxCost: ptr("0.1")}
	options.WorstCost = func(Provider, Target, *usage.RoutingPrice, *usage.RoutingDiscount) (usage.Cost, bool) {
		return bound, true
	}
	plan, err = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if err != nil || len(plan.Attempts) == 0 {
		t.Fatalf("under ceiling: %v %d", err, len(plan.Attempts))
	}

	// A candidate with no bound is ineligible for the same reason.
	options.WorstCost = func(Provider, Target, *usage.RoutingPrice, *usage.RoutingDiscount) (usage.Cost, bool) {
		return usage.Cost{}, false
	}
	plan, err = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Attempts) != 0 || !slices.ContainsFunc(plan.Decisions, func(d Decision) bool {
		return d.Reason != nil && *d.Reason == "cost_limit_exceeded"
	}) {
		t.Fatalf("unbounded target admitted: %+v", plan.Decisions)
	}

	// A committed provider has zero marginal cost, so it stays eligible under max_cost.
	committed := s.Providers[ids[0]]
	committed.Commitment = &Commitment{Amount: "100", Period: "month"}
	s.Providers[ids[0]] = committed
	options.Preferences = &Preferences{MaxCost: ptr("0.000000000001")}
	options.WorstCost = func(Provider, Target, *usage.RoutingPrice, *usage.RoutingDiscount) (usage.Cost, bool) {
		return bound, true
	}
	plan, err = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if err != nil || len(plan.Attempts) == 0 {
		t.Fatalf("committed target excluded by max_cost: %v %+v", err, plan.Decisions)
	}
}

func TestEstimatedCostRecordsBounds(t *testing.T) {
	s, slug, ids := planningFixture()
	inputs := pricedInputs(ids, "wire-model")
	price := &inputs.Prices[0]
	bound, ok := price.CostBound(1000, 100, true, time.Now(), nil)
	if !ok {
		t.Fatal("fixture price cannot bound")
	}
	options := SelectionOptions{Inputs: inputs, Now: time.Now(), EstimateCost: true,
		TokenDemand: &TokenDemand{EstimatedInputTokens: 1000, MaxOutputTokens: ptr(int64(100))},
		WorstCost: func(Provider, Target, *usage.RoutingPrice, *usage.RoutingDiscount) (usage.Cost, bool) {
			return bound, true
		}}
	plan, err := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if err != nil {
		t.Fatal(err)
	}
	var decision *Decision
	for index := range plan.Decisions {
		if plan.Decisions[index].ProviderID == ids[0] {
			decision = &plan.Decisions[index]
		}
	}
	if decision == nil || decision.EstimatedCost == nil {
		t.Fatalf("no estimate: %+v", plan.Decisions)
	}
	if decision.EstimatedCost.Maximum != bound.String() || decision.EstimatedCost.Currency != "USD" {
		t.Fatalf("estimate = %+v", decision.EstimatedCost)
	}
	floor, _ := price.CostFloor(1000, time.Now(), nil)
	if decision.EstimatedCost.Minimum != floor.String() {
		t.Fatalf("floor = %q, want %q", decision.EstimatedCost.Minimum, floor.String())
	}
}

func TestMaxCostValidation(t *testing.T) {
	for _, payload := range []string{`{"max_cost":"1e9"}`, `{"max_cost":"-1"}`, `{"max_cost":"abc"}`} {
		if _, err := ParsePreferences([]byte(payload)); err == nil {
			t.Fatalf("accepted invalid max_cost %s", payload)
		}
	}
	if _, err := ParsePreferences([]byte(`{"max_cost":"0.5"}`)); err != nil {
		t.Fatalf("refused valid max_cost: %v", err)
	}
	_ = uuid.NewString()
}

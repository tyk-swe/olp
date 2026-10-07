package runtime

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/usage"
)

func reasonOf(plan Plan, targetID string) string {
	for _, d := range plan.Decisions {
		if d.TargetID == targetID && d.Reason != nil {
			return *d.Reason
		}
	}
	return ""
}

func attemptTargets(plan Plan) []string {
	out := make([]string, len(plan.Attempts))
	for i, a := range plan.Attempts {
		out[i] = a.TargetID
	}
	return out
}

func TestSelectorsNarrowTargetsDelegateAndExplainTheirTrace(t *testing.T) {
	s, slug, _ := planningFixture()
	route := s.Routes[slug]
	route.Targets[0].Tags = []string{"small"}
	route.Targets[1].Tags = []string{"large"}
	route.Targets[2].Tags = []string{"large"}
	route.Selectors = []Selector{
		{ID: "classified", When: Predicate{Classifier: &ClassifierPredicate{Route: "tei", Labels: []string{"simple"}, TimeoutMS: 100}}, Tags: []string{"small"}},
		{ID: "short", When: Predicate{MaxInputTokens: ptr(int64(1000))}, Tags: []string{"small"}},
		{ID: "reasoning", When: Predicate{ReasoningEffort: []string{"high"}}, Route: "thinker"},
	}
	s.Routes[slug] = route
	thinker := Route{Slug: "thinker", Operations: []string{"generation"}}
	s.Routes["thinker"] = thinker

	evaluated := 0
	options := SelectionOptions{Features: &Features{Operation: "generation", InputTokens: 400}, Evaluate: func(Selector) PredicateResult {
		evaluated++
		return PredicateResult{Label: "complex"}
	}}
	plan, err := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if err != nil {
		t.Fatal(err)
	}
	if evaluated != 1 || len(plan.Selectors) != 2 || plan.Selectors[0].Outcome != "not_matched" || *plan.Selectors[0].Label != "complex" || plan.Selectors[1].Outcome != "matched" {
		t.Fatalf("selector trace: %+v", plan.Selectors)
	}
	if got := attemptTargets(plan); !slices.Equal(got, []string{route.Targets[0].ID}) {
		t.Fatalf("selector kept %v", got)
	}
	if reasonOf(plan, route.Targets[1].ID) != "selector_excluded" || *plan.Decisions[0].Selector != "short" {
		t.Fatalf("decisions: %+v", plan.Decisions)
	}

	// A classifier failure falls through; a forbidden delegate is skipped.
	options.Evaluate = func(Selector) PredicateResult { return PredicateResult{Failure: "classifier_failed"} }
	options.Features = &Features{Operation: "generation", InputTokens: 5000, ReasoningEffort: "high"}
	options.Permitted = func(r Route) bool { return r.Slug != "thinker" }
	plan, _ = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if plan.Selectors[0].Outcome != "classifier_failed" || plan.Selectors[2].Outcome != "selector_route_forbidden" || plan.Delegate != "" || len(plan.Attempts) != 3 {
		t.Fatalf("fall through: %+v", plan)
	}
	options.Permitted = nil
	plan, _ = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if plan.Delegate != "thinker" || len(plan.Attempts) != 0 || len(plan.Decisions) != 0 {
		t.Fatalf("delegation: %+v", plan)
	}

	// Without features, as on surfaces that do not compute them, selectors
	// are not evaluated at all.
	options.Features = nil
	plan, _ = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if plan.Selectors != nil || len(plan.Attempts) != 3 {
		t.Fatalf("selectors without features: %+v", plan)
	}
}

func TestSpendCapsRemoveOwnersAndNameTheBudgetCondition(t *testing.T) {
	s, slug, ids := planningFixture()
	capped := s.Providers[ids[0]]
	capped.Limits = &Limits{Supply: Supply{DailyCostLimit: ptr("10")}}
	s.Providers[capped.ID] = capped
	route := s.Routes[slug]

	queries := 0
	state := &SupplyState{Spend: map[string]Spend{capped.ID: SpendExhausted}}
	options := SelectionOptions{Supply: supplyFunc(func(q SupplyQuery) *SupplyState {
		queries++
		if len(q.Caps) != 1 || q.Caps[0].OwnerID != capped.ID || len(q.Slots) != 0 {
			t.Fatalf("query %+v", q)
		}
		return state
	})}
	plan, _ := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if queries != 1 || len(plan.Attempts) != 2 || reasonOf(plan, route.Targets[0].ID) != "connection_budget_exhausted" {
		t.Fatalf("exhausted connection: %+v", plan)
	}
	// A missing snapshot skips the capped connection, as an unreadable quota does.
	state = &SupplyState{}
	plan, _ = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if reasonOf(plan, route.Targets[0].ID) != "connection_budget_unavailable" {
		t.Fatalf("unknown spend: %+v", plan.Decisions)
	}

	route.Budget = &CostLimits{MonthlyCostLimit: ptr("100")}
	route.ID = uuid.NewString()
	s.Routes[slug] = route
	state = &SupplyState{Spend: map[string]Spend{route.ID: SpendExhausted, capped.ID: SpendAvailable}}
	options.Supply = supplyFunc(func(SupplyQuery) *SupplyState { return state })
	plan, _ = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if len(plan.Attempts) != 0 || !slices.Equal(plan.Conditions(), []string{FallbackBudget}) {
		t.Fatalf("exhausted route: %+v %v", plan, plan.Conditions())
	}

	// Without a cap or the capacity strategy, the planner reads nothing.
	route.Budget = nil
	s.Routes[slug] = route
	capped.Limits = nil
	s.Providers[capped.ID] = capped
	options.Supply = supplyFunc(func(SupplyQuery) *SupplyState { t.Fatal("read supply for an unconfigured route"); return nil })
	if plan, _ = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options); len(plan.Attempts) != 3 {
		t.Fatalf("unconfigured: %+v", plan)
	}
}

func TestCapacityStrategyOrdersSlotsAndTargetsByHeadroom(t *testing.T) {
	s, slug, ids := planningFixture()
	headroom := map[string]float64{}
	for i, id := range ids {
		p := s.Providers[id]
		p.AuthMode = "api_key"
		for j := 0; j < 2; j++ {
			credential := uuid.NewString()
			slot := Slot{ID: uuid.NewString(), Enabled: true, Weight: 1, CredentialID: &credential}
			p.Slots = append(p.Slots, slot)
			if i < 2 {
				headroom[slot.ID] = float64(i*2+j+1) / 10
			}
		}
		s.Providers[id] = p
	}
	route := s.Routes[slug]
	route.MaxAttempts = 6
	s.Routes[slug] = route
	var queried SupplyQuery
	options := SelectionOptions{CheckSlots: true, Preferences: &Preferences{Strategy: ptr("capacity")}, Supply: supplyFunc(func(q SupplyQuery) *SupplyState {
		queried = q
		return &SupplyState{Headroom: headroom}
	})}
	plan, err := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(queried.Slots) != 6 {
		t.Fatalf("capacity read %d slots", len(queried.Slots))
	}
	// Provider 1's slots hold 0.3 and 0.4, provider 0's 0.1 and 0.2, and
	// provider 2's are unknown, so they rank last.
	want := []string{s.Providers[ids[1]].Slots[1].ID, s.Providers[ids[1]].Slots[0].ID, s.Providers[ids[0]].Slots[1].ID, s.Providers[ids[0]].Slots[0].ID}
	for i, id := range want {
		if d := plan.Decisions[i]; d.CredentialSlotID == nil || *d.CredentialSlotID != id || d.Strategy != "capacity" {
			t.Fatalf("decision %d: %+v", i, d)
		}
	}
	if plan.Decisions[4].Headroom != nil || plan.Decisions[4].ProviderID != ids[2] {
		t.Fatalf("unknown headroom ranked early: %+v", plan.Decisions[4])
	}
	// The gateway dispatches each target's slots in the order the plan chose.
	if got := plan.Attempts[0].Slots; !slices.Equal(got, want[:2]) {
		t.Fatalf("first attempt slots %v, want %v", got, want[:2])
	}
	if got := ArrangeSlots(s.Providers[ids[1]].Slots, plan.Attempts[0].Slots); got[0].ID != want[0] {
		t.Fatalf("arranged slots %+v", got)
	}
}

func TestShadowTargetsAreSampledApartFromTheAttemptOrder(t *testing.T) {
	s, slug, _ := planningFixture()
	route := s.Routes[slug]
	route.Targets[2].Shadow = &Shadow{SampleRate: 1}
	s.Routes[slug] = route
	shadow := route.Targets[2].ID

	plan, _ := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{})
	if len(plan.Attempts) != 2 || len(plan.Shadows) != 0 || reasonOf(plan, shadow) != "shadow_not_sampled" {
		t.Fatalf("unseeded shadow: %+v", plan)
	}
	plan, _ = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{Mirror: []byte("request")})
	if len(plan.Attempts) != 2 || len(plan.Shadows) != 1 || plan.Shadows[0].TargetID != shadow || slices.Contains(attemptTargets(plan), shadow) {
		t.Fatalf("sampled shadow: %+v", plan)
	}
	for _, d := range plan.Decisions {
		if d.TargetID == shadow && (!d.Shadow || d.Attempt != nil) {
			t.Fatalf("shadow decision %+v", d)
		}
	}
	// A shadow obeys the request's hard constraints like any target.
	plan, _ = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{Mirror: []byte("request"), Preferences: &Preferences{Ignore: []string{"provider:" + route.Targets[2].ProviderID}}})
	if len(plan.Shadows) != 0 || reasonOf(plan, shadow) != "provider_ignored" {
		t.Fatalf("constrained shadow: %+v", plan)
	}
}

func TestUnhealthyTargetsMoveLastWithoutLeavingThePlan(t *testing.T) {
	s, slug, ids := planningFixture()
	route := s.Routes[slug]
	route.Targets[0].Priority = -1
	s.Routes[slug] = route
	plan, _ := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{Unhealthy: func(id string) bool { return id == ids[0] }})
	if len(plan.Attempts) != 3 || plan.Attempts[2].ProviderID != ids[0] || !plan.Decisions[2].Unhealthy {
		t.Fatalf("unhealthy order: %+v", plan)
	}
}

func TestPlanConditionsNameContextWindowExhaustion(t *testing.T) {
	s, slug, ids := planningFixture()
	for _, id := range ids {
		p := s.Providers[id]
		p.Models = map[string]json.RawMessage{"wire-model": json.RawMessage(`{"context_length":100}`)}
		s.Providers[id] = p
	}
	plan, _ := PlanRequest(&s, slug, "generation", "openai", "unary", nil, SelectionOptions{TokenDemand: &TokenDemand{EstimatedInputTokens: 500}})
	if !slices.Equal(plan.Conditions(), []string{FallbackContextWindow}) {
		t.Fatalf("conditions %v", plan.Conditions())
	}
}

// supplyFunc reads supply from a function, as a test stand-in for the gateway.
type supplyFunc func(SupplyQuery) *SupplyState

func (f supplyFunc) ReadSupply(_ context.Context, query SupplyQuery) *SupplyState { return f(query) }

func TestNarrowingSelectorNamesTheMostExpensiveTargetItAvoided(t *testing.T) {
	s, slug, ids := planningFixture()
	now := time.Now()
	inputs := &usage.RoutingInputs{RefreshedAt: now, Performance: map[string]usage.Performance{}}
	for i, id := range ids {
		amount := []string{"0.000001", "0.000009", "0.000004"}[i]
		inputs.Prices = append(inputs.Prices, usage.RoutingPrice{Price: usage.Price{ProviderID: &id, ProviderKind: "openai", Model: "wire-model", Operation: "generation", InputPerMillion: &amount, OutputPerMillion: &amount, Currency: "USD"}, RevisionID: uuid.NewString(), Revision: 1, EffectiveAt: now.Add(-time.Hour)})
	}
	route := s.Routes[slug]
	route.Targets[0].Tags = []string{"small"}
	route.Selectors = []Selector{{ID: "short", When: Predicate{MaxInputTokens: ptr(int64(1000))}, Tags: []string{"small"}}}
	s.Routes[slug] = route
	options := SelectionOptions{Inputs: inputs, Now: now, Features: &Features{Operation: "generation", InputTokens: 10}}
	plan, err := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if err != nil || len(plan.Attempts) != 1 || plan.Baseline == nil || plan.Baseline.ProviderID != ids[1] || plan.Baseline.UpstreamModel != "wire-model" {
		t.Fatalf("baseline %+v, %v", plan.Baseline, err)
	}
	// A target the request's constraints exclude never sets the baseline.
	options.Preferences = &Preferences{Ignore: []string{"provider:" + ids[1]}}
	if plan, _ = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options); plan.Baseline == nil || plan.Baseline.ProviderID != ids[2] {
		t.Fatalf("constrained baseline %+v", plan.Baseline)
	}
	// Without a matching selector there is nothing to compare.
	options.Features.InputTokens = 5000
	if plan, _ = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options); plan.Baseline != nil {
		t.Fatalf("unnarrowed baseline %+v", plan.Baseline)
	}
}

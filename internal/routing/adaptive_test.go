package routing

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
)

func reasonOf(plan Plan, targetID string) string {
	for _, d := range plan.Decisions {
		if d.TargetID == targetID && d.Reason != nil {
			return *d.Reason
		}
	}
	return ""
}

// A classifier failure falls through; a forbidden delegate is skipped.

// Without features, as on surfaces that do not compute them, selectors
// are not evaluated at all.

func TestSpendCapsRemoveOwnersAndNameTheBudgetCondition(t *testing.T) {
	s, slug, ids := planningFixture()
	capped := s.Providers[ids[0]]
	capped.Limits = &Limits{Supply: Supply{DailyCostLimit: ptr("10")}}
	s.Providers[capped.ID] = capped
	route := s.Routes[slug]

	queries := 0
	state := &SupplyState{Spend: map[string]Spend{capped.ID: SpendExhausted}}
	options := Options{Supply: supplyFunc(func(q SupplyQuery) *SupplyState {
		queries++
		if len(q.Caps) != 1 || q.Caps[0].OwnerID != capped.ID || len(q.Slots) != 0 {
			t.Fatalf("query %+v", q)
		}
		return state
	})}
	plan, _ := Select(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if queries != 1 || len(plan.Attempts) != 2 || reasonOf(plan, route.Targets[0].ID) != "connection_budget_exhausted" {
		t.Fatalf("exhausted connection: %+v", plan)
	}
	// A missing snapshot skips the capped connection, as an unreadable quota does.
	state = &SupplyState{}
	plan, _ = Select(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if reasonOf(plan, route.Targets[0].ID) != "connection_budget_unavailable" {
		t.Fatalf("unknown spend: %+v", plan.Decisions)
	}

	route.Budget = &CostLimits{MonthlyCostLimit: ptr("100")}
	route.ID = uuid.NewString()
	s.Routes[slug] = route
	state = &SupplyState{Spend: map[string]Spend{route.ID: SpendExhausted, capped.ID: SpendAvailable}}
	options.Supply = supplyFunc(func(SupplyQuery) *SupplyState { return state })
	plan, _ = Select(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if len(plan.Attempts) != 0 || !slices.Equal(plan.Conditions(), []string{FallbackBudget}) {
		t.Fatalf("exhausted route: %+v %v", plan, plan.Conditions())
	}

	// Without a cap or the capacity strategy, the planner reads nothing.
	route.Budget = nil
	s.Routes[slug] = route
	capped.Limits = nil
	s.Providers[capped.ID] = capped
	options.Supply = supplyFunc(func(SupplyQuery) *SupplyState { t.Fatal("read supply for an unconfigured route"); return nil })
	if plan, _ = Select(&s, slug, "generation", "openai", "unary", []byte("seed"), options); len(plan.Attempts) != 3 {
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
	options := Options{CheckSlots: true, Preferences: &Preferences{Strategy: ptr("capacity")}, Supply: supplyFunc(func(q SupplyQuery) *SupplyState {
		queried = q
		return &SupplyState{Headroom: headroom}
	})}
	plan, err := Select(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
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

// A shadow obeys the request's hard constraints like any target.

// supplyFunc reads supply from a function, as a test stand-in for the gateway.
type supplyFunc func(SupplyQuery) *SupplyState

func (f supplyFunc) ReadSupply(_ context.Context, query SupplyQuery) *SupplyState { return f(query) }

// A target the request's constraints exclude never sets the baseline.

// Without a matching selector there is nothing to compare.

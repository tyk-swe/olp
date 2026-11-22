package routes

import (
	"cmp"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

func TestExplainKeepsWeightedOrderWhenCallerCostExempt(t *testing.T) {
	t.Parallel()
	cheap, dear := uuid.NewString(), uuid.NewString()
	explain := func(exempt bool, strategy string) []inspectedDecision {
		t.Helper()
		now := time.Now()
		slots := []routing.Slot{{ID: uuid.NewString(), Name: "caller", Enabled: true, Weight: 1, CredentialID: new(uuid.NewString())}}
		capabilities := []routing.Capability{{Model: "fixture-model", Operation: "generation", Surface: "openai", Mode: "unary"}}
		target := func(provider string, weight int64) routing.Target {
			id := uuid.NewString()
			return routing.Target{ID: id, RoutingID: id, ProviderID: provider, ProviderModel: "fixture-model", Weight: weight, Timeout: 1000}
		}
		routeID := uuid.NewString()
		route := routing.Route{ID: routeID, RoutingID: routeID, Slug: "byok", MaxAttempts: 2, OverallTimeout: 3000, Operations: []string{"generation"},
			Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed}, Behavior: routing.Behavior{CallerCostExempt: exempt}, Targets: []routing.Target{target(cheap, 1), target(dear, 1000)}}
		snapshot := &routing.Snapshot{Routes: map[string]routing.Route{route.Slug: route}, Providers: map[string]routing.Provider{
			cheap: {ID: cheap, Kind: "openai", Enabled: true, CredentialSource: "caller", Capabilities: capabilities, Slots: slots},
			dear:  {ID: dear, Kind: "openai", Enabled: true, CredentialSource: "caller", Capabilities: capabilities, Slots: slots},
		}}
		price := func(provider, input, output string) rates.RoutingPrice {
			return *rates.NewRoutingPrice(rates.RoutingPrice{Price: rates.Price{ProviderID: &provider, ProviderKind: "openai", Model: "fixture-model", Operation: "generation",
				InputPerMillion: &input, OutputPerMillion: &output, Currency: "USD"}, RevisionID: uuid.NewString(), Revision: 1, EffectiveAt: now.Add(-time.Hour)})
		}
		inputs := &routinginputs.Inputs{RefreshedAt: now, Prices: []rates.RoutingPrice{price(cheap, "1", "10"), price(dear, "4", "2")}}
		input := simulationInput{Operation: "generation", Surface: "openai", Mode: "unary", Seed: "seed",
			Preferences: &routing.Preferences{Strategy: &strategy}, EstimatedInputTokens: new(int64(10000)), MaxOutputTokens: new(int64(1000))}
		demand, err := tokenDemand(input.EstimatedInputTokens, input.MaxOutputTokens)
		if err != nil {
			t.Fatal(err)
		}
		s := Server{Access: &access.Server{}}
		m := s.newSimulation(t.Context(), snapshot, input, inputs, demand)
		m.key = func(routing.Route) (inspectionKeyContext, error) { return inspectionKeyContext{}, nil }
		m.eligibility = func(routing.Route) (func(string) routing.Eligibility, error) { return nil, nil }
		result, err := s.explain(m, route.Slug)
		if err != nil || len(result.Legs) != 1 {
			t.Fatalf("simulation: %+v %v", result, err)
		}
		for _, decision := range result.Legs[0].Decisions {
			if decision.Attempt == nil {
				t.Fatalf("every target must be attempted: %+v %s", decision.Decision, *decision.Reason)
			}
		}
		return slices.SortedFunc(slices.Values(result.Legs[0].Decisions), func(a, b inspectedDecision) int { return cmp.Compare(*a.Attempt, *b.Attempt) })
	}
	providers := func(decisions []inspectedDecision) []string {
		return []string{decisions[0].ProviderID, decisions[1].ProviderID}
	}
	if ranked := explain(false, "bounded_cost"); !slices.Equal(providers(ranked), []string{cheap, dear}) {
		t.Fatalf("a priced route is not ordered by upper estimate: %v", providers(ranked))
	}
	weighted := providers(explain(true, "weighted"))
	if slices.Equal(weighted, []string{cheap, dear}) {
		t.Fatalf("fixture weights do not separate weighted from cost order: %v", weighted)
	}
	exempt := explain(true, "bounded_cost")
	if !slices.Equal(providers(exempt), weighted) {
		t.Fatalf("exempt route ordered %v, the gateway dispatches %v", providers(exempt), weighted)
	}
	for _, decision := range exempt {
		if decision.BoundedCostFallbackReason == nil || *decision.BoundedCostFallbackReason != "insufficient_cost_bounds" || decision.EstimatedCost == nil || decision.EstimatedCost.Maximum == "0" {
			t.Fatalf("exempt decision must fall back by weight and still report its estimate: %+v", decision.Decision)
		}
	}
}

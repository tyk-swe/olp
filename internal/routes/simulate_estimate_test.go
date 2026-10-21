package routes

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/google/uuid"
)

// estimateSnapshot publishes a route with one target for each model, every one
// with a context window of two hundred tokens.
func estimateSnapshot(models ...string) (*routing.Snapshot, []string) {
	credential := uuid.NewString()
	snapshot := &routing.Snapshot{Providers: map[string]routing.Provider{}, Routes: map[string]routing.Route{}}
	var targets []routing.PublishedTarget
	var ids []string
	for i, model := range models {
		id, modelID := uuid.NewString(), uuid.NewString()
		targets = append(targets, routing.PublishedTarget{ID: uuid.NewString(), ProviderID: id, ProviderModelID: modelID, ProviderModel: model, Priority: i, Weight: 1})
		ids = append(ids, targets[i].ID)
		snapshot.Providers[id] = routing.Provider{
			ID: id, Kind: "openai", Enabled: true, AuthMode: "api_key",
			Capabilities: []routing.Capability{{Model: model, Operation: "generation", Surface: "openai", Mode: "unary"}},
			Models:       map[string]json.RawMessage{model: json.RawMessage(`{"context_length":200}`)},
			Slots:        []routing.Slot{{ID: uuid.NewString(), Enabled: true, Weight: 1, CredentialID: &credential}},
		}
	}
	snapshot.Routes["route"] = simulationRoute(uuid.NewString(), "route", routing.RouteFidelity{Mode: routing.FidelityTransformed}, []string{"generation"}, 3000, 4, targets)
	return snapshot, ids
}

func decisionFor(t *testing.T, decisions []inspectedDecision, model string) inspectedDecision {
	t.Helper()
	for _, d := range decisions {
		if d.UpstreamModel == model {
			return d
		}
	}
	t.Fatalf("no decision for %s in %+v", model, decisions)
	return inspectedDecision{}
}

// strictEstimateSnapshot is estimateSnapshot with a strict route: every target
// is a provider on the compatible chat profile, which prepares the request the
// target is sent from the caller's.
func strictEstimateSnapshot(models ...string) *routing.Snapshot {
	snapshot, _ := estimateSnapshot(models...)
	for id, provider := range snapshot.Providers {
		provider.Kind, provider.ProfileID, provider.ProfileRevision = "openai_compatible", "compatible-chat", "1"
		snapshot.Providers[id] = provider
	}
	route := snapshot.Routes["route"]
	route.Fidelity = routing.RouteFidelity{Mode: routing.FidelityStrict}
	snapshot.Routes["route"] = route
	return snapshot
}

// The request's own five tokens of reply fit; the caller's hundred do not.

// A caller's input estimate stands for every target, with the bound it names.

// fixedSimulation simulates without a selected key and with every credential
// eligible.
func fixedSimulation(snapshot *routing.Snapshot, input simulationInput, demand *routing.TokenDemand) *simulation {
	m := (&Server{}).newSimulation(context.Background(), snapshot, input, nil, demand)
	m.key = func(routing.Route) (inspectionKeyContext, error) { return inspectionKeyContext{}, nil }
	m.eligibility = func(routing.Route) (func(string) routing.Eligibility, error) {
		return func(string) routing.Eligibility { return routing.Eligible }, nil
	}
	return m
}

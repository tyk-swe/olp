package routes

import (
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestSimulationSkipsUnusableSlotsWithoutConsumingAttempts(t *testing.T) {
	credential := uuid.NewString()
	for _, tc := range []struct {
		name string
		slot routing.Slot
	}{
		{"disabled", routing.Slot{CredentialID: &credential}},
		{"missing credential", routing.Slot{Enabled: true}},
		{"model excluded", routing.Slot{Enabled: true, CredentialID: &credential, AllowedModels: []string{"other"}}},
		{"route excluded", routing.Slot{Enabled: true, CredentialID: &credential, AllowedRoutes: []string{"other"}}},
		{"key excluded", routing.Slot{Enabled: true, CredentialID: &credential, AllowedAPIKeys: []string{uuid.NewString()}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := &routing.Snapshot{Providers: map[string]routing.Provider{}, Routes: map[string]routing.Route{}}
			first, second := uuid.NewString(), uuid.NewString()
			targets := []routing.PublishedTarget{}
			for i, id := range []string{first, second} {
				modelID := uuid.NewString()
				targets = append(targets, routing.PublishedTarget{ID: uuid.NewString(), ProviderID: id, ProviderModelID: modelID, ProviderModel: "model", Priority: i, Weight: 1})
				snapshot.Providers[id] = routing.Provider{ID: id, Kind: "openai", Enabled: true, AuthMode: "api_key", Capabilities: []routing.Capability{{Model: "model", Operation: "generation", Surface: "openai", Mode: "unary"}}, Slots: []routing.Slot{{ID: uuid.NewString(), Enabled: true, Weight: 1, CredentialID: &credential}}}
			}
			p := snapshot.Providers[first]
			tc.slot.ID = uuid.NewString()
			p.Slots = []routing.Slot{tc.slot}
			snapshot.Providers[first] = p
			snapshot.Routes["route"] = simulationRoute(uuid.NewString(), "route", routing.RouteFidelity{Mode: routing.FidelityTransformed}, []string{"generation"}, 3000, 1, targets)
			plan, err := routing.Select(snapshot, "route", "generation", "openai", "unary", []byte("seed"), routing.Options{KeyID: uuid.NewString(), CheckSlots: true})
			if err != nil || len(plan.Decisions) != 2 {
				t.Fatal(plan, err)
			}
			order := plan.Decisions
			if order[0].ProviderID != second || !order[0].Eligible || order[0].Attempt == nil || *order[0].Attempt != 1 || order[1].Eligible || order[1].Attempt != nil || order[1].Reason == nil || *order[1].Reason != "no_eligible_credentials" {
				t.Fatalf("ineligible slot consumed attempt budget: %+v", order)
			}
		})
	}
}

func TestTokenDemandValidation(t *testing.T) {
	negative := int64(-1)
	if _, err := tokenDemand(&negative, nil); err == nil {
		t.Error("negative input estimate accepted")
	}
	if _, err := tokenDemand(nil, &negative); err == nil {
		t.Error("negative output bound accepted")
	}
	if demand, err := tokenDemand(nil, nil); demand != nil || err != nil {
		t.Errorf("absent demand: %v %v", demand, err)
	}
	input, output := int64(100), int64(20)
	demand, err := tokenDemand(&input, &output)
	if err != nil || demand.EstimatedInputTokens != 100 || *demand.MaxOutputTokens != 20 {
		t.Errorf("demand %+v %v", demand, err)
	}
}

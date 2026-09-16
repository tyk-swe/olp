package routes

import (
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/runtime"
	"testing"
)

func TestSimulationSkipsUnusableSlotsWithoutConsumingAttempts(t *testing.T) {
	credential := uuid.NewString()
	for _, tc := range []struct {
		name string
		slot runtime.Slot
	}{
		{"disabled", runtime.Slot{CredentialID: &credential}},
		{"missing credential", runtime.Slot{Enabled: true}},
		{"model excluded", runtime.Slot{Enabled: true, CredentialID: &credential, AllowedModels: []string{"other"}}},
		{"route excluded", runtime.Slot{Enabled: true, CredentialID: &credential, AllowedRoutes: []string{"other"}}},
		{"key excluded", runtime.Slot{Enabled: true, CredentialID: &credential, AllowedAPIKeys: []string{uuid.NewString()}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := &runtime.Snapshot{Providers: map[string]runtime.Provider{}, Routes: map[string]runtime.Route{}}
			first, second := uuid.NewString(), uuid.NewString()
			targets := []runtime.PublishedTarget{}
			for i, id := range []string{first, second} {
				modelID := uuid.NewString()
				targets = append(targets, runtime.PublishedTarget{ID: uuid.NewString(), ProviderID: id, ProviderModelID: modelID, ProviderModel: "model", Priority: i, Weight: 1})
				snapshot.Providers[id] = runtime.Provider{ID: id, Kind: "openai", Enabled: true, AuthMode: "api_key", Capabilities: []runtime.Capability{{Model: "model", Operation: "generation", Surface: "openai", Mode: "unary"}}, Slots: []runtime.Slot{{ID: uuid.NewString(), Enabled: true, Weight: 1, CredentialID: &credential}}}
			}
			p := snapshot.Providers[first]
			tc.slot.ID = uuid.NewString()
			p.Slots = []runtime.Slot{tc.slot}
			snapshot.Providers[first] = p
			snapshot.Routes["route"] = simulationRoute(uuid.NewString(), "route", []string{"generation"}, 3000, 1, targets)
			plan, err := runtime.PlanRequest(snapshot, "route", "generation", "openai", "unary", []byte("seed"), runtime.SelectionOptions{KeyID: uuid.NewString(), CheckSlots: true})
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

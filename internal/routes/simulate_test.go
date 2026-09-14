package routes

import (
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/runtime"
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
			first, second := uuid.NewString(), uuid.NewString()
			targets := []runtime.PublishedTarget{{ID: uuid.NewString(), ProviderModelID: first, Weight: 1}, {ID: uuid.NewString(), ProviderModelID: second, Priority: 1, Weight: 1}}
			live := map[string]*resolved{}
			for _, id := range []string{first, second} {
				live[id] = &resolved{ProviderState: "active", ProviderModel: "model", Published: true, Certified: map[string]bool{"generation/openai/unary": true}, AuthMode: "api_key", Slots: []runtime.Slot{{Enabled: true, CredentialID: &credential}}}
			}
			live[first].Slots = []runtime.Slot{tc.slot}
			order := rank(uuid.NewString(), "route", uuid.NewString(), targets, live, "generation", "openai", "unary", []byte("seed"), 1)
			if order[0].target.ProviderModelID != second || !order[0].eligible || order[0].attempt != 1 || order[1].eligible || order[1].attempt != 0 || order[1].reason != "no_eligible_credentials" {
				t.Fatalf("ineligible slot consumed the attempt budget: first=%+v second=%+v", order[0], order[1])
			}
		})
	}
	noAuth := &resolved{AuthMode: "none", Slots: []runtime.Slot{{Enabled: true}}}
	if !noAuth.hasCredential("route", "") {
		t.Fatal("no-auth provider requires a credential")
	}
}

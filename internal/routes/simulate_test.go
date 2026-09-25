package routes

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
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

// Simulation input that planning refuses is the caller's mistake, not an
// unavailable management service.
func TestSimulationRefusalsAreClientErrors(t *testing.T) {
	snapshot := &runtime.Snapshot{Providers: map[string]runtime.Provider{}, Routes: map[string]runtime.Route{}}
	snapshot.Routes["known"] = simulationRoute(uuid.NewString(), "known", []string{"generation"}, 1000, 1, nil)
	two := 2
	for _, tc := range []struct {
		name, slug, operation string
		preferences           *Preferences
		code                  string
	}{
		{"unknown route", "missing", "generation", nil, ""},
		{"unsupported operation", "known", "embeddings", nil, runtime.OperationNotSupported},
		{"attempt budget increase", "known", "generation", &Preferences{MaxAttempts: &two}, runtime.AttemptBudgetIncreaseForbidden},
	} {
		_, err := runtime.PlanRequest(snapshot, tc.slug, tc.operation, "openai", "unary", nil, runtime.SelectionOptions{Preferences: tc.preferences})
		err = selectionProblem(err)
		if tc.code == "" {
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("%s: %v, want the not-found reply of a hidden route", tc.name, err)
			}
			continue
		}
		var problem *access.Problem
		if !errors.As(err, &problem) || problem.Status != 422 || problem.Code != tc.code {
			t.Errorf("%s: %v, want a 422 %s problem", tc.name, err, tc.code)
		}
	}
	other := &runtime.SelectionError{Code: runtime.NoEligibleTargets}
	if err := selectionProblem(other); err != other {
		t.Errorf("an unrelated selection error was rewritten to %v", err)
	}
}

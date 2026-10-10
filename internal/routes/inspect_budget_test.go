package routes

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestInspectionPolicyBudgetBoundsCumulativeTargetScans(t *testing.T) {
	budget := new(inspectionBudget)
	// Four targets scanning 32 KiB against 64 rules exactly fill the budget.
	for range 4 {
		for range 64 {
			if err := budget.reserve(32 << 10); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := budget.reserve(1); err == nil {
		t.Fatal("cumulative target scan budget was not enforced")
	}
	if err := budget.reserve(0); err == nil {
		t.Fatal("empty input reopened the exhausted budget")
	}
	if err := new(inspectionBudget).reserve(maxInspectionPolicyBytes + 1); err == nil {
		t.Fatal("large first match exceeded scan budget")
	}
}

func TestInspectionRejectsRepeatedEffectivePolicyWork(t *testing.T) {
	policy := &contentpolicy.Policy{}
	for i := range 64 {
		policy.Rules = append(policy.Rules, contentpolicy.Rule{ID: fmt.Sprintf("r%d", i), Phase: contentpolicy.PhaseInput, Pattern: "never-matches", Action: contentpolicy.ActionBlock})
	}
	route := runtime.Route{Fidelity: runtime.RouteFidelity{Mode: runtime.FidelityStrict}, ContentPolicy: policy}
	parsed, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"route","messages":[{"role":"user","content":"`+strings.Repeat("a", 96<<10)+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	accept, _, _ := inspectionAccept(route, parsed, interaction.Context{}, nil, new(inspectionBudget))
	provider := runtime.Provider{ID: "p", Kind: "openai", AuthMode: "api_key", ProfileID: "openai-chat", ProfileRevision: "1"}
	target := runtime.Target{ID: "t", ProviderID: "p", ProviderModel: "gpt-4o"}
	if err := accept(provider, target); err != nil {
		t.Fatalf("first bounded inspection: %v", err)
	}
	err = accept(provider, target)
	diagnostic, ok := err.(*inspectionDiagnostic)
	if !ok || diagnostic.code != "inspection_limit" {
		t.Fatalf("second target should exhaust aggregate policy budget: %v", err)
	}
}

func TestInspectionBudgetCoversUnaryAndMediaTargets(t *testing.T) {
	policy := &contentpolicy.Policy{}
	for i := range 64 {
		policy.Rules = append(policy.Rules, contentpolicy.Rule{ID: fmt.Sprintf("r%d", i), Phase: contentpolicy.PhaseInput, Pattern: "never-matches", Action: contentpolicy.ActionBlock})
	}
	route := runtime.Route{Slug: "route", Fidelity: runtime.RouteFidelity{Mode: runtime.FidelityStrict}, ContentPolicy: policy}
	provider := runtime.Provider{ID: "p", Kind: "openai", AuthMode: "api_key", ProfileID: "openai-chat", ProfileRevision: "1"}
	target := runtime.Target{ID: "t", ProviderID: "p", ProviderModel: "upstream"}
	for _, tc := range []struct{ operation, fields string }{
		{"embeddings", `"input":"%s"`},
		{"image_generation", `"prompt":"%s"`},
		{"speech", `"input":"%s","voice":"alloy"`},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			body := json.RawMessage(fmt.Sprintf(`{"model":"route",`+tc.fields+`}`, strings.Repeat("a", 96<<10)))
			_, unary, media, err := inspectorAnyRequest(body, tc.operation, "openai", "unary", "", route.Slug, true)
			if err != nil {
				t.Fatal(err)
			}
			budget := new(inspectionBudget)
			for pass := range 2 {
				var accept func(runtime.Provider, runtime.Target) error
				if unary != nil {
					accept, _, _ = inspectionUnaryAccept(route, *unary, interaction.Context{}, "", nil, budget)
				} else {
					accept, _, _ = inspectionMediaAccept(route, media, "", interaction.Context{}, "", nil, budget)
				}
				err := accept(provider, target)
				if pass == 0 && err != nil {
					t.Fatalf("first bounded target: %v", err)
				}
				if pass == 1 {
					diagnostic, ok := err.(*inspectionDiagnostic)
					if !ok || diagnostic.code != "inspection_limit" {
						t.Fatalf("new target set reopened inspection budget: %v", err)
					}
				}
			}
		})
	}
}

func TestSimulationSharesPolicyBudgetAcrossRouteLegs(t *testing.T) {
	snapshot := explainSnapshot(map[string]int{"first": 1000000, "second": 1000000})
	for slug, route := range snapshot.Routes {
		route.Fidelity.Mode = runtime.FidelityStrict
		route.ContentPolicy = &contentpolicy.Policy{}
		for i := range 64 {
			route.ContentPolicy.Rules = append(route.ContentPolicy.Rules, contentpolicy.Rule{ID: fmt.Sprintf("r%d", i), Phase: contentpolicy.PhaseInput, Pattern: "never-matches", Action: contentpolicy.ActionBlock})
		}
		snapshot.Routes[slug] = route
		for _, target := range route.Targets {
			provider := snapshot.Providers[target.ProviderID]
			provider.ProfileID, provider.ProfileRevision = "openai-chat", "1"
			snapshot.Providers[provider.ID] = provider
		}
	}
	input := simulationInput{Operation: "generation", Surface: "openai", Mode: "unary", Request: json.RawMessage(`{"model":"first","messages":[{"role":"user","content":"` + strings.Repeat("a", 96<<10) + `"}]}`)}
	m := fixedSimulation(snapshot, input, nil)
	m.named = "first"
	s := &Server{Access: &access.Server{}}
	_, first, _, err := s.leg(m, "first", "")
	if err != nil || len(first.Attempts) != 1 {
		t.Fatalf("first leg: plan=%+v error=%v", first, err)
	}
	leg, second, _, err := s.leg(m, "second", "fallback")
	if err != nil || len(second.Attempts) != 0 || len(leg.Decisions) != 1 || leg.Decisions[0].Reason == nil || *leg.Decisions[0].Reason != "inspection_limit" {
		t.Fatalf("later route reset the budget: leg=%+v plan=%+v error=%v", leg, second, err)
	}
}

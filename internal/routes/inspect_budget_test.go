package routes

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/interaction"
)

// Four targets scanning 32 KiB against 64 rules exactly fill the budget.

func TestInspectionBudgetCoversUnaryAndMediaTargets(t *testing.T) {
	policy := &contentpolicy.Policy{}
	for i := range 64 {
		policy.Rules = append(policy.Rules, contentpolicy.Rule{ID: fmt.Sprintf("r%d", i), Phase: contentpolicy.PhaseInput, Pattern: "never-matches", Action: contentpolicy.ActionBlock})
	}
	route := routing.Route{Slug: "route", Fidelity: routing.RouteFidelity{Mode: routing.FidelityStrict}, ContentPolicy: policy}
	provider := routing.Provider{ID: "p", Kind: "openai", AuthMode: "api_key", ProfileID: "openai-chat", ProfileRevision: "1"}
	target := routing.Target{ID: "t", ProviderID: "p", ProviderModel: "upstream"}
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
				var accept func(routing.Provider, routing.Target) error
				if unary != nil {
					accept, _, _ = inspectionUnaryAccept(route, route.Slug, *unary, interaction.Context{}, "", nil, budget)
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

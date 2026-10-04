package routes

import (
	"fmt"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
	"strings"
	"testing"
)

func TestInspectionPolicyBudgetBoundsCumulativeTargetScans(t *testing.T) {
	policy := &contentpolicy.Policy{}
	for range 64 {
		policy.Rules = append(policy.Rules, contentpolicy.Rule{Phase: contentpolicy.PhaseInput})
	}
	budget := inspectionPolicyBudget(policy)
	// A 32 KiB request may be checked against two targets, not all 64.
	for range 2 {
		if err := budget(32 << 10); err != nil {
			t.Fatal(err)
		}
	}
	if err := budget(1); err == nil {
		t.Fatal("cumulative target scan budget was not enforced")
	}
	if err := budget(1); err == nil {
		t.Fatal("exhausted budget reopened")
	}
	if err := inspectionPolicyBudget(policy)(1 << 20); err == nil {
		t.Fatal("large first target exceeded scan budget")
	}
	if err := inspectionPolicyBudget(nil)(1 << 20); err != nil {
		t.Fatal(err)
	}
}

func TestInspectionRejectsRepeatedEffectivePolicyWork(t *testing.T) {
	policy := &contentpolicy.Policy{}
	for i := range 64 {
		policy.Rules = append(policy.Rules, contentpolicy.Rule{ID: fmt.Sprintf("r%d", i), Phase: contentpolicy.PhaseInput, Pattern: "never-matches", Action: contentpolicy.ActionBlock})
	}
	route := runtime.Route{Fidelity: runtime.RouteFidelity{Mode: runtime.FidelityStrict}, ContentPolicy: policy}
	parsed, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"route","messages":[{"role":"user","content":"`+strings.Repeat("a", 32<<10)+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	accept, _, _ := inspectionAccept(route, parsed, interaction.Context{}, nil)
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

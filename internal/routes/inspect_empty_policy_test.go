package routes

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestInspectionPolicyBudgetCountsEmptyTextScans(t *testing.T) {
	policy := &contentpolicy.Policy{}
	for i := range 64 {
		policy.Rules = append(policy.Rules, contentpolicy.Rule{ID: fmt.Sprintf("r%d", i), Phase: contentpolicy.PhaseInput, Pattern: "never-matches", Action: contentpolicy.ActionBlock})
	}
	body, _ := json.Marshal(map[string]any{
		"model":    "route",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "f",
				"parameters": map[string]any{
					"type": "object", "examples": make([]string, 4096),
				},
			},
		}},
	})
	parsed, err := openai.Parse(openai.FamilyChat, body)
	if err != nil {
		t.Fatal(err)
	}
	route := runtime.Route{Fidelity: runtime.RouteFidelity{Mode: runtime.FidelityStrict}, ContentPolicy: policy}
	provider := runtime.Provider{ID: "p", Kind: "openai", AuthMode: "api_key", ProfileID: "openai-chat", ProfileRevision: "1"}
	accept, _, _ := inspectionAccept(route, parsed, interaction.Context{}, nil, new(inspectionBudget))
	// Empty schema strings still cause regexp calls. Length-only metering
	// would admit all 64 targets and perform over 16 million uncharged scans.
	for i := range 64 {
		target := runtime.Target{ID: fmt.Sprintf("t%d", i), ProviderID: "p", ProviderModel: "gpt-4o"}
		if err = accept(provider, target); err != nil {
			diagnostic, ok := err.(*inspectionDiagnostic)
			if !ok || diagnostic.code != "inspection_limit" {
				t.Fatalf("unexpected empty-text inspection failure: %v", err)
			}
			if i == 0 {
				t.Fatal("one bounded target must remain inspectable")
			}
			return
		}
	}
	t.Fatal("empty input strings bypassed the aggregate inspection budget")
}

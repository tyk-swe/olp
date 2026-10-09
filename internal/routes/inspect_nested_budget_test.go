package routes

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestInspectionPolicyBudgetMetersNestedArguments(t *testing.T) {
	policy := &contentpolicy.Policy{}
	for i := range 64 {
		policy.Rules = append(policy.Rules, contentpolicy.Rule{ID: fmt.Sprintf("r%d", i), Phase: contentpolicy.PhaseInput, Pattern: "never-matches", Action: contentpolicy.ActionBlock})
	}
	route := runtime.Route{Fidelity: runtime.RouteFidelity{Mode: runtime.FidelityStrict}, ContentPolicy: policy}
	provider := runtime.Provider{ID: "p", Kind: "openai", AuthMode: "api_key", ProfileID: "openai-chat", ProfileRevision: "1"}
	target := runtime.Target{ID: "t", ProviderID: "p", ProviderModel: "gpt-4o"}
	for _, depth := range []int{0, 6} {
		t.Run(fmt.Sprintf("depth_%d", depth), func(t *testing.T) {
			arguments, _ := json.Marshal(map[string]any{"text": strings.Repeat("a", 32<<10)})
			for range depth {
				arguments, _ = json.Marshal(map[string]any{"arguments": string(arguments)})
			}
			body, _ := json.Marshal(map[string]any{
				"model": "route",
				"messages": []any{map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"id": "call", "type": "function",
						"function": map[string]any{"name": "f", "arguments": string(arguments)},
					}},
				}},
			})
			parsed, err := openai.Parse(openai.FamilyChat, body)
			if err != nil {
				t.Fatal(err)
			}
			accept, _, _ := inspectionAccept(route, parsed, interaction.Context{}, nil, new(inspectionBudget))
			err = accept(provider, target)
			if depth == 0 {
				if err != nil {
					t.Fatalf("ordinary arguments should fit the work budget: %v", err)
				}
				return
			}
			// This 34,000-byte source contains over 16 MiB of cumulative regexp
			// input across 64 rules: decoded descendants and their encoded parent
			// strings are both inspected. A fixed multiple of source bytes is
			// insufficient to account for recursive JSON argument strings.
			diagnostic, ok := err.(*inspectionDiagnostic)
			if !ok || diagnostic.code != "inspection_limit" {
				t.Fatalf("nested argument scans must exhaust the shared budget: %v", err)
			}
		})
	}
}

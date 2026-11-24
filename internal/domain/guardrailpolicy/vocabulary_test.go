package guardrailpolicy

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/management"
)

func TestVocabularyMatchesManagementContract(t *testing.T) {
	var document struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(management.Document, &document); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		schema, property string
		accepted         []string
	}{
		{"GuardrailPolicyEntry", "phase", names(phases[:])},
		{"GuardrailPolicyEntry", "action", names(actions[:])},
		{"CreateGuardrailRequest", "on_error", names(failurePolicies[:])},
		{"GuardrailEvidence", "outcome", names(outcomes[:])},
		{"GuardrailAttachment", "scope", names(scopes[:])},
	} {
		var published struct {
			Enum []string `json:"enum"`
		}
		if err := json.Unmarshal(document.Components.Schemas[tc.schema].Properties[tc.property], &published); err != nil {
			t.Fatalf("%s.%s: %v", tc.schema, tc.property, err)
		}
		slices.Sort(published.Enum)
		if len(published.Enum) == 0 || !slices.Equal(published.Enum, tc.accepted) {
			t.Errorf("%s.%s publishes %v, the vocabulary accepts %v", tc.schema, tc.property, published.Enum, tc.accepted)
		}
	}
}

func names[T ~string](values []T) []string {
	return slices.Sorted(func(yield func(string) bool) {
		for _, value := range values {
			if !yield(string(value)) {
				return
			}
		}
	})
}

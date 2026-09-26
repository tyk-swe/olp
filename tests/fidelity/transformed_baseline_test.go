package fidelity_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/tests/fidelity"
	fixtures "github.com/tyk-swe/olp/tests/fixtures/fidelity"
)

// This test freezes observed transformed-route translation. Five cases
// intentionally expose losses; passing this characterization is NOT strict
// fidelity qualification. Strict public admission tests consume the same
// independent source/native fixtures and require preservation or precise
// zero-dispatch rejection.
func TestTransformedCounterexamplesAndPositiveNativeControls(t *testing.T) {
	data, err := fixtures.Files.ReadFile("v1/counterexamples.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Scenarios []struct {
			ID                string          `json:"id"`
			SourceFamily      string          `json:"source_family"`
			NativeKind        string          `json:"native_kind"`
			NativeVendor      string          `json:"native_vendor"`
			TargetKind        string          `json:"target_kind"`
			TargetVendor      string          `json:"target_vendor"`
			TargetFamily      string          `json:"target_family"`
			Source            json.RawMessage `json:"source"`
			Native            json.RawMessage `json:"native"`
			TransformedTarget json.RawMessage `json:"transformed_target"`
			TransformedError  string          `json:"transformed_error"`
			TransformedLoss   string          `json:"transformed_loss"`
			PreservedTarget   json.RawMessage `json:"preserved_target"`
		}
	}
	if err = json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	family := map[string]openai.Family{"openai_chat": openai.FamilyChat, "anthropic_messages": openai.FamilyAnthropic, "gemini_generate_content": openai.FamilyGemini}
	losses := 0
	for _, scenario := range corpus.Scenarios {
		t.Run(scenario.ID, func(t *testing.T) {
			request, err := protocols.Parse(family[scenario.SourceFamily], scenario.Source, "")
			if err != nil {
				t.Fatal(err)
			}
			native, _, err := protocols.Encode(request, scenario.NativeKind, scenario.NativeVendor, "fixture-model", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = fidelity.Compare(scenario.Native, native); err != nil {
				t.Fatalf("native positive control: %v", err)
			}
			transformed, wire, err := protocols.Encode(request, scenario.TargetKind, scenario.TargetVendor, "fixture-model", nil)
			if scenario.TransformedError != "" {
				var refusal *openai.RequestError
				if !errors.As(err, &refusal) || refusal.Param != scenario.TransformedError {
					t.Fatalf("expected precise rejection at %s: %v", scenario.TransformedError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if wire != family[scenario.TargetFamily] {
				t.Fatal("wrong target dialect", wire)
			}
			if err = fidelity.Compare(scenario.TransformedTarget, transformed); err != nil {
				t.Fatalf("transformed characterization changed: %v", err)
			}
			if scenario.TransformedLoss != "" {
				losses++
				t.Logf("recorded transformed loss (not strict success): %s", scenario.TransformedLoss)
			}
			if len(scenario.PreservedTarget) > 0 {
				err = fidelity.Compare(scenario.PreservedTarget, transformed)
				if scenario.TransformedLoss != "" && err == nil {
					t.Fatal("oracle missed known ordering loss")
				}
				if scenario.TransformedLoss == "" && err != nil {
					t.Fatalf("qualified positive control: %v", err)
				}
			}
		})
	}
	if losses != 5 {
		t.Fatalf("reproduced %d of five recorded transformed losses", losses)
	}
}

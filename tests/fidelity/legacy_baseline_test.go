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

// This test freezes observed legacy behavior. Five cases intentionally expose
// losses; passing this characterization is NOT strict fidelity qualification.
// Strict public admission tests consume the same independent source/native
// fixtures and require preservation or precise zero-dispatch rejection.
func TestLegacyCounterexamplesAndPositiveNativeControls(t *testing.T) {
	data, err := fixtures.Files.ReadFile("v1/counterexamples.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Scenarios []struct {
			ID              string          `json:"id"`
			SourceFamily    string          `json:"source_family"`
			NativeKind      string          `json:"native_kind"`
			NativeVendor    string          `json:"native_vendor"`
			TargetKind      string          `json:"target_kind"`
			TargetVendor    string          `json:"target_vendor"`
			TargetFamily    string          `json:"target_family"`
			Source          json.RawMessage `json:"source"`
			Native          json.RawMessage `json:"native"`
			LegacyTarget    json.RawMessage `json:"legacy_target"`
			LegacyError     string          `json:"legacy_error"`
			LegacyLoss      string          `json:"legacy_loss"`
			PreservedTarget json.RawMessage `json:"preserved_target"`
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
			legacy, wire, err := protocols.Encode(request, scenario.TargetKind, scenario.TargetVendor, "fixture-model", nil)
			if scenario.LegacyError != "" {
				var refusal *openai.RequestError
				if !errors.As(err, &refusal) || refusal.Param != scenario.LegacyError {
					t.Fatalf("expected precise rejection at %s: %v", scenario.LegacyError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if wire != family[scenario.TargetFamily] {
				t.Fatal("wrong target dialect", wire)
			}
			if err = fidelity.Compare(scenario.LegacyTarget, legacy); err != nil {
				t.Fatalf("legacy characterization changed: %v", err)
			}
			if scenario.LegacyLoss != "" {
				losses++
				t.Logf("recorded legacy loss (not strict success): %s", scenario.LegacyLoss)
			}
			if len(scenario.PreservedTarget) > 0 {
				err = fidelity.Compare(scenario.PreservedTarget, legacy)
				if scenario.LegacyLoss != "" && err == nil {
					t.Fatal("oracle missed known ordering loss")
				}
				if scenario.LegacyLoss == "" && err != nil {
					t.Fatalf("qualified positive control: %v", err)
				}
			}
		})
	}
	if losses != 5 {
		t.Fatalf("reproduced %d of five recorded legacy losses", losses)
	}
}

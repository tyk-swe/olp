package configuration

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/guardrails"
)

func TestValidateGuardrailPoliciesAcceptsToolsetAttachment(t *testing.T) {
	doc := &Document{
		Guardrails: []GuardrailEntry{{Name: "keywords", Project: "alpha", Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["private"]}`), TimeoutMS: 1000, OnError: guardrails.FailClosed}},
		GuardrailPolicies: []GuardrailPolicyEntry{
			{Name: "tool safeguards", Project: "alpha", Entries: []GuardrailPolicyRule{{Guardrail: "keywords", Phase: guardrails.Input, Action: guardrails.Block}}},
		},
		GuardrailAttachments: []GuardrailAttachmentEntry{
			{Scope: "toolset", Name: "research tools", Policy: "tool safeguards", PolicyProject: "alpha", Order: 0},
		},
	}
	if err := validateGuardrailPolicies(doc); err != nil {
		t.Fatalf("toolset attachment refused: %v", err)
	}
	doc.GuardrailAttachments[0].Scope = "server"
	if err := validateGuardrailPolicies(doc); err == nil {
		t.Fatal("an unknown attachment scope was accepted")
	}
}

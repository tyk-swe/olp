package configuration

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/guardrails"
)

func TestValidateGuardrailPoliciesAttachmentScopes(t *testing.T) {
	for _, test := range []struct {
		scope string
		valid bool
	}{
		{"installation", true},
		{"organization", true},
		{"project", true},
		{"route", true},
		{"toolset", true},
		{"api-key", true},
		{"server", false},
	} {
		t.Run(test.scope, func(t *testing.T) {
			doc := &Document{
				Guardrails: []GuardrailEntry{{Name: "keywords", Project: "alpha", Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["private"]}`), TimeoutMS: 1000, OnError: guardrails.FailClosed}},
				GuardrailPolicies: []GuardrailPolicyEntry{
					{Name: "tool safeguards", Project: "alpha", Entries: []GuardrailPolicyRule{{Guardrail: "keywords", Phase: guardrails.Input, Action: guardrails.Block}}},
				},
				GuardrailAttachments: []GuardrailAttachmentEntry{
					{Scope: test.scope, Name: "research tools", Policy: "tool safeguards", PolicyProject: "alpha", Order: 0},
				},
			}
			if err := validateGuardrailPolicies(doc); (err == nil) != test.valid {
				t.Fatalf("attachment scope %s validation = %v", test.scope, err)
			}
		})
	}
}

package configuration

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLegacyGuardrailNormalizationPreservesOrderAndLiteralReplacement(t *testing.T) {
	project := "Project"
	d := &Document{APIVersion: LegacyAPIVersion, Routes: []RouteEntry{{Slug: "route", Project: &project, OverallTimeoutMS: 2000, ContentPolicy: json.RawMessage(`{"rules":[{"id":"first","phase":"input","pattern":"secret","action":"redact","replacement":"$1"},{"id":"next","phase":"output","pattern":"blocked","action":"block"}]}`)}}}
	if err := normalizeGuardrailVersion(d); err != nil {
		t.Fatal(err)
	}
	if d.APIVersion != APIVersion || len(d.Guardrails) != 2 || len(d.GuardrailPolicies) != 1 || d.Routes[0].ContentPolicy != nil {
		t.Fatalf("normalization: %+v", d)
	}
	if d.GuardrailPolicies[0].Entries[0].Action != "redact" || d.GuardrailPolicies[0].Entries[1].Phase != "output" || !strings.Contains(string(d.Guardrails[0].Configuration), "$1") {
		t.Fatal("legacy semantics changed")
	}
	if err := normalizeGuardrailVersion(d); err != nil {
		t.Fatal("normalization not idempotent", err)
	}
}
func TestMixedConfigurationGuardrailsRejected(t *testing.T) {
	d := &Document{APIVersion: LegacyAPIVersion, GuardrailPolicies: []GuardrailPolicyEntry{{Name: "mixed"}}}
	if normalizeGuardrailVersion(d) == nil {
		t.Fatal("accepted mixed version 1")
	}
	d = &Document{APIVersion: APIVersion, Routes: []RouteEntry{{ContentPolicy: json.RawMessage(`{"rules":[]}`)}}}
	if normalizeGuardrailVersion(d) == nil {
		t.Fatal("accepted ambiguous version 2")
	}
}

func TestGuardrailConfigurationRejectsStrictTransforms(t *testing.T) {
	project := "Project"
	for _, action := range []string{"redact", "mask"} {
		doc := &Document{Guardrails: []GuardrailEntry{{Name: "private", Project: project, Type: "builtin.regex", Configuration: json.RawMessage(`{"rules":[{"id":"x","pattern":"secret"}]}`), TimeoutMS: 1000, OnError: "fail_closed"}}, GuardrailPolicies: []GuardrailPolicyEntry{{Name: "safeguards", Project: project, Entries: []GuardrailPolicyRule{{Guardrail: "private", Phase: "input", Action: action}}}}, GuardrailAttachments: []GuardrailAttachmentEntry{{Scope: "route", Name: "strict", Policy: "safeguards", PolicyProject: project}}, Routes: []RouteEntry{{Slug: "strict", Project: &project, Fidelity: json.RawMessage(`{"mode":"strict"}`), OverallTimeoutMS: 5000}}}
		if validateGuardrailPolicies(doc) == nil {
			t.Fatalf("strict configuration accepted %s", action)
		}
		doc.Routes[0].Fidelity = json.RawMessage(`{"mode":"transformed"}`)
		if err := validateGuardrailPolicies(doc); err != nil {
			t.Fatalf("transformed configuration rejected %s: %v", action, err)
		}
	}
}

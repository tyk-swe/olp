package guardrails

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNamedPolicyUsesEngineBoundsAndContentFreeFailures(t *testing.T) {
	valid := input{Name: "  Filter  ", Policy: json.RawMessage(`{"rules":[{"id":"account","phase":"input","pattern":"private","action":"redact"}]}`)}
	policy, err := validate(&valid)
	if err != nil || valid.Name != "Filter" || len(policy.Rules) != 1 || policy.Rules[0].Replacement != "[REDACTED]" {
		t.Fatalf("canonical policy: %#v %v", policy, err)
	}
	for _, raw := range []string{`null`, ` null `, `[]`, `{"unexpected":true}`, `{"rules":[{"id":"secret-value","phase":"input","pattern":"[private-value","action":"block"}]}`} {
		_, err := validate(&input{Name: "Filter", Policy: json.RawMessage(raw)})
		if err == nil || strings.Contains(err.Error(), "private-value") || strings.Contains(err.Error(), "secret-value") {
			t.Fatalf("unsafe validation failure for %s: %v", raw, err)
		}
	}
}

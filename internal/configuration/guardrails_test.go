package configuration

import (
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/contentpolicy"
)

func TestPortableGuardrailIdentityUsesProjectAndNameAndCanonicalizesEmptyPolicy(t *testing.T) {
	first := GuardrailEntry{Project: "A/B", Name: "C", Type: "builtin.regex", Policy: &contentpolicy.Policy{}}
	second := GuardrailEntry{Project: "A", Name: "B/C", Type: "builtin.regex", Policy: &contentpolicy.Policy{}}
	doc := &Document{Guardrails: []GuardrailEntry{first, second}}
	if err := validateGuardrails(doc); err != nil {
		t.Fatal(err)
	}
	for _, entry := range doc.Guardrails {
		if entry.Policy.Rules == nil {
			t.Fatal("empty policy does not satisfy the array contract")
		}
	}
	doc.canonicalize()
	reordered := &Document{Guardrails: []GuardrailEntry{second, first}}
	reordered.canonicalize()
	if !reflect.DeepEqual(doc.Guardrails, reordered.Guardrails) {
		t.Fatal("guardrail order changed the canonical artifact")
	}
	doc.Guardrails = append(doc.Guardrails, GuardrailEntry{Project: "a/b", Name: "c", Type: "builtin.regex", Policy: &contentpolicy.Policy{}})
	if validateGuardrails(doc) == nil {
		t.Fatal("case-insensitive duplicate accepted")
	}
}

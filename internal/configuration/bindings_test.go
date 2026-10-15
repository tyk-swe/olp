package configuration

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/secretstore"
)

func TestExternalBindingsAreExclusiveAndFingerprintTheirPinnedVersion(t *testing.T) {
	first := secretstore.Reference{Store: "gcp", SecretID: "projects/project/secrets/provider", Version: "1"}
	input := promotionInput{ExternalBindings: map[string]secretstore.Reference{"provider/primary": first}}
	bindings, err := input.bindings()
	if err != nil {
		t.Fatal(err)
	}
	before := bindingFingerprint(nil, "digest", bindings, nil)
	input.ExternalBindings["provider/primary"] = secretstore.Reference{Store: first.Store, SecretID: first.SecretID, Version: "2"}
	changed, err := input.bindings()
	if err != nil || reflect.DeepEqual(before, bindingFingerprint(nil, "digest", changed, nil)) {
		t.Fatal("fingerprint ignored immutable version")
	}
	encoded, _ := json.Marshal(before)
	if strings.Contains(string(encoded), first.SecretID) {
		t.Fatal("fingerprint exposes reference metadata")
	}
	input.SecretBindings = map[string]string{"provider/primary": "private-secret"}
	if _, err = input.bindings(); err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatal("accepted ambiguous or exposed binding")
	}
}

func TestGuardrailBindingsIncludeEvaluatorAndNetworkCredentials(t *testing.T) {
	doc := &Document{Guardrails: []GuardrailEntry{{Type: "webhook", Configuration: json.RawMessage(`{"endpoint":"https://example.com","credential_ref":"evaluator","content_egress":true,"network":{"credential_id":"network"}}`)}}}
	valid := map[string]string{"evaluator": `{"api_key":"private-key"}`, "network": `{"proxy_username":"user","proxy_password":"password"}`}
	if err := validateBindings(doc, plainBindings(valid)); err != nil {
		t.Fatalf("referenced guardrail bindings rejected: %v", err)
	}
	for _, tc := range []struct{ name, reference, value string }{
		{"empty", "evaluator", ""},
		{"plain", "evaluator", "private-key"},
		{"invalid_json", "evaluator", "{"},
		{"unknown_field", "evaluator", `{"api_key":"private-key","unexpected":true}`},
		{"incomplete_aws", "evaluator", `{"access_key_id":"private-key"}`},
		{"wrong_network_kind", "network", `{"api_key":"private-key"}`},
		{"incomplete_network", "network", `{"proxy_password":"private-key"}`},
		{"unreferenced", "unused", `{"api_key":"private-key"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateBindings(doc, plainBindings(map[string]string{tc.reference: tc.value})); err == nil || strings.Contains(err.Error(), "private-key") {
				t.Fatalf("invalid binding accepted or disclosed: %v", err)
			}
		})
	}
	for _, reference := range []string{"evaluator", "network"} {
		if err := validateBindings(doc, bindingSet{reference: {reference: &secretstore.Reference{Store: "vault", SecretID: "key", Version: "1"}}}); err == nil {
			t.Fatalf("external guardrail binding accepted: %s", reference)
		}
	}
}

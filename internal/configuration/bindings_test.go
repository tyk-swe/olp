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

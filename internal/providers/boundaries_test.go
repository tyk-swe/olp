package providers

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestConfigurationRejectsInvalidDefaults(t *testing.T) {
	for _, raw := range []string{
		`{"previous_response_id":"resp_other"}`, `{"conversation":"conv_other"}`,
		`{"background":true}`, `{"max_tokens":1,"max_completion_tokens":2}`,
		`{"max_output_tokens":"10"}`, `{"stream":true}`, `{"input":"hidden"}`,
	} {
		cfg := Configuration{Kind: KindOpenAICompatible, AuthMode: AuthNone, Endpoint: new("https://example.com/v1")}
		cfg.normalize()
		if err := json.Unmarshal([]byte(raw), &cfg.Options.ParameterDefaults); err != nil {
			t.Fatal(err)
		}
		if err := cfg.validate(&egress.Policy{}); err == nil {
			t.Errorf("accepted defaults %s", raw)
		}
	}
}

func TestModelDiscoveryRequiresAModelList(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"error":{"message":"unauthorized"}}`, `{"data":null}`, `{"data":{}}`} {
		if _, err := decodeModels([]byte(raw)); err == nil {
			t.Errorf("accepted model response %s", raw)
		}
	}
	if models, err := decodeModels([]byte(`{"data":[]}`)); err != nil || len(models) != 0 {
		t.Fatalf("valid empty list: %v %v", models, err)
	}
	models, err := decodeModels([]byte(`{"data":[{"id":"good"},{"id":"bad\u0000model"},{"id":"bad\nmodel"},{"id":"good"}]}`))
	if err != nil || !reflect.DeepEqual(models, []string{"good"}) {
		t.Fatalf("model identifiers: %q %v", models, err)
	}
}

func TestSlotUUIDsAreCanonical(t *testing.T) {
	const id = "abcdef01-2345-4678-9abc-def012345678"
	for _, value := range []string{strings.ToUpper(id), strings.ReplaceAll(id, "-", ""), "urn:uuid:" + id} {
		in := slotInput{ID: value, Name: "slot", CredentialVersionID: &value, AllowedAPIKeys: []string{value}}
		if err := validSlot(&in, id); err != nil {
			t.Fatal(err)
		}
		if in.ID != id || *in.CredentialVersionID != id || in.AllowedAPIKeys[0] != id {
			t.Fatalf("noncanonical UUIDs retained: %+v", in)
		}
	}
}

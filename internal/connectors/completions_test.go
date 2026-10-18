package connectors

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestLegacyCompletionProfilesAreVendorBound(t *testing.T) {
	for _, vendor := range []string{"openai", "vllm", "tgi", "fireworks", "together"} {
		kind := "openai_compatible"
		if vendor == "openai" {
			kind = "openai"
		}
		c := Config{Kind: kind, VendorID: vendor, ProfileID: vendor + "-completions", ProfileRevision: "1", Endpoint: "https://provider.example/v1", AuthMode: "api_key"}
		if err := c.ValidateProfile(); err != nil {
			t.Fatalf("%s: %v", vendor, err)
		}
		for _, stream := range []bool{false, true} {
			endpoint, err := c.URL(wire.Completions, "native-model", stream)
			if err != nil || !strings.HasSuffix(endpoint, "/v1/completions") {
				t.Fatalf("%s: %s %v", vendor, endpoint, err)
			}
		}
		if _, err := c.URL(wire.Chat, "native-model", false); err == nil {
			t.Fatalf("%s accepted a different native dialect", vendor)
		}
		c.VendorID = "unreviewed"
		if c.ValidateProfile() == nil {
			t.Fatalf("%s can be impersonated", vendor)
		}
	}
}

func TestLegacyCompletionDefaultsRespectNativeTypes(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"logprobs", "3", true}, {"logprobs", "true", false}, {"echo", "true", true}, {"echo", `"true"`, false}, {"suffix", `"text"`, false}, {"prompt", `"text"`, false},
	} {
		err := validateDialectDefaultValue("openai-completions", "generation", tc.name, json.RawMessage(tc.value))
		if (err == nil) != tc.valid {
			t.Errorf("%s=%s: %v", tc.name, tc.value, err)
		}
	}
}

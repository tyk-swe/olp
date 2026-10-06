package connectors

import (
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// TestNativeGenerationProfilesServeOnlyTheirDialect covers the Mistral
// fill-in-the-middle and Cohere chat profiles: each addresses its vendor's
// endpoint and serves generation on the native surface only.
func TestNativeGenerationProfilesServeOnlyTheirDialect(t *testing.T) {
	for _, test := range []struct {
		config Config
		family openai.Family
		url    string
	}{
		{Config{Kind: "openai_compatible", AuthMode: "api_key", ProfileID: "mistral-fim", ProfileRevision: ProfileRevision, VendorID: "mistral", Endpoint: "https://api.mistral.ai/v1"},
			openai.FamilyMistralFIM, "https://api.mistral.ai/v1/fim/completions"},
		{Config{Kind: "openai_compatible", AuthMode: "api_key", ProfileID: "cohere-v2", ProfileRevision: ProfileRevision, VendorID: "cohere-native-v2", Endpoint: "https://api.cohere.ai/v2"},
			openai.FamilyCohereChat, "https://api.cohere.ai/v2/chat"},
	} {
		c := test.config
		if err := c.Validate(&egress.Policy{}); err != nil {
			t.Fatal(err)
		}
		if wire, err := c.TargetFamily(openai.FamilyChat); err != nil || wire != test.family {
			t.Fatalf("%s generates in %s, %v", c.ProfileID, wire, err)
		}
		for _, stream := range []bool{false, true} {
			if got, err := c.URL(test.family, "model", stream); err != nil || got != test.url {
				t.Fatalf("%s URL = %s, %v", c.ProfileID, got, err)
			}
		}
		if !c.Supports("generation", "native", "unary") || !c.Supports("generation", "native", "streaming") {
			t.Fatalf("%s does not serve its native dialect", c.ProfileID)
		}
		for _, surface := range []string{"openai", "anthropic", "gemini"} {
			if c.Supports("generation", surface, "unary") {
				t.Fatalf("%s translates generation to the %s surface", c.ProfileID, surface)
			}
		}
	}
	// Cohere's native vendor generates only through its profile: an automatic
	// provider of it has no Chat Completions to send.
	if Supports("openai_compatible", "cohere-native-v2", "generation", "openai", "unary") {
		t.Fatal("an automatic Cohere native provider serves Chat Completions")
	}
	plain := Config{Kind: "openai_compatible", AuthMode: "api_key", ProfileID: "compatible-chat", ProfileRevision: ProfileRevision, VendorID: "mistral", Endpoint: "https://api.mistral.ai/v1"}
	if plain.Supports("generation", "native", "unary") {
		t.Fatal("a Chat Completions profile serves the native surface")
	}
}

package connectors

import (
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func vertexOpenAI() Config {
	return Config{Kind: "vertex_ai", AuthMode: "adc", ProfileID: "vertex-openai", ProfileRevision: ProfileRevision,
		CloudRegion: "us-central1", CloudProject: "acme-prod", VendorID: "google-vertex",
		Endpoint: DefaultProfileEndpoint("vertex_ai", "vertex-openai", "us-central1", "acme-prod")}
}

// TestVertexOpenAIAddressesPublisherModels covers Vertex's OpenAI-compatible
// endpoint: chat completions under endpoints/openapi, with models named
// publisher/model in the body.
func TestVertexOpenAIAddressesPublisherModels(t *testing.T) {
	c := vertexOpenAI()
	if err := c.Validate(&egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	got, err := c.URL(openai.FamilyChat, "meta/llama-4-maverick-17b-128e-instruct-maas", true)
	if want := "https://us-central1-aiplatform.googleapis.com/v1/projects/acme-prod/locations/us-central1/endpoints/openapi/chat/completions"; err != nil || got != want {
		t.Fatalf("URL = %s, %v", got, err)
	}
	for model, valid := range map[string]bool{
		"google/gemini-2.5-flash": true, "meta/llama-3.3-70b-instruct-maas": true, "deepseek-ai/deepseek-r1-0528-maas": true,
		"gemini-2.5-flash": false, "google/../secrets": false, "/gemini": false, "google/": false,
	} {
		if c.ValidModel(model) != valid {
			t.Fatalf("ValidModel(%q) = %v", model, !valid)
		}
	}
	if !c.Supports("generation", "openai", "streaming") || c.Supports("token_count", "openai", "unary") || c.Supports("embeddings", "openai", "unary") {
		t.Fatal("the Vertex OpenAI profile serves chat generation only")
	}
	if global := DefaultProfileEndpoint("vertex_ai", "vertex-openai", "global", "acme-prod"); global != "https://aiplatform.googleapis.com/v1/projects/acme-prod/locations/global/endpoints/openapi" {
		t.Fatalf("global endpoint = %s", global)
	}
}

func TestVertexOpenAIRefusesOtherAddresses(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"publisher path": func(c *Config) {
			c.Endpoint = "https://us-central1-aiplatform.googleapis.com/v1/projects/acme-prod/locations/us-central1/publishers/google"
		},
		"other project": func(c *Config) {
			c.Endpoint = "https://us-central1-aiplatform.googleapis.com/v1/projects/other/locations/us-central1/endpoints/openapi"
		},
		"dedicated endpoint": func(c *Config) {
			c.Endpoint = "https://us-central1-aiplatform.googleapis.com/v1/projects/acme-prod/locations/us-central1/endpoints/1234"
		},
	} {
		c := vertexOpenAI()
		mutate(&c)
		if err := c.Validate(&egress.Policy{}); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestVertexOpenAIBindingsSeparateAliasesFromModels(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alias string
		model string
		valid bool
	}{
		{"logical alias", "logical-model", "google/gemini-2.5-flash", true},
		{"empty alias", "", "google/gemini-2.5-flash", false},
		{"invalid alias", "logical\nmodel", "google/gemini-2.5-flash", false},
		{"missing publisher", "logical-model", "gemini-2.5-flash", false},
		{"invalid upstream path", "logical-model", "google/../secrets", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := vertexOpenAI()
			c.Bindings = map[string]Binding{tc.alias: {Model: tc.model}}
			if err := c.Validate(&egress.Policy{}); (err == nil) != tc.valid {
				t.Fatalf("Validate binding %q -> %q: %v, want valid=%v", tc.alias, tc.model, err, tc.valid)
			}
			if tc.valid {
				if got := c.Model(tc.alias); got != tc.model {
					t.Fatalf("Model(%q) = %q, want %q", tc.alias, got, tc.model)
				}
				if _, err := c.URL(openai.FamilyChat, tc.alias, false); err != nil {
					t.Fatalf("bound model URL: %v", err)
				}
			}
		})
	}
}

package connectors

import (
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/operationregistry"
)

func TestCohereNativeV2ProfilesCannotUseCompatibilityEndpoint(t *testing.T) {
	for _, test := range []struct{ profile, operation, path string }{
		{"cohere-embed-v2", "embeddings", "embed"},
		{"cohere-rerank-v2", "rerank", "rerank"},
	} {
		profile, err := LookupProfile(test.profile, "1")
		if err != nil {
			t.Fatal(err)
		}
		if profile.DialectRevision != "v2" || profile.OperationDialect(test.operation) != test.profile || !slices.Equal(profile.Authentication, []string{"api_key"}) {
			t.Fatalf("native profile composition: %+v", profile)
		}
		codec, ok := operationregistry.Lookup(test.profile)
		if !ok {
			t.Fatal("native Cohere codec missing")
		}
		config := Config{Kind: "openai_compatible", AuthMode: "api_key", ProfileID: test.profile, ProfileRevision: "1", Endpoint: "https://api.cohere.ai/v2"}
		if err := config.Validate(&egress.Policy{}); err != nil {
			t.Fatal(err)
		}
		endpoint, err := config.OperationURL(codec, "embed-v4.0")
		if err != nil || endpoint != "https://api.cohere.ai/v2/"+test.path {
			t.Fatalf("native endpoint %s: %v", endpoint, err)
		}
		config.Endpoint = coherePresetEndpoint
		if err := config.Validate(&egress.Policy{}); err == nil || !strings.Contains(err.Error(), "/v2 endpoint") {
			t.Fatalf("compatibility endpoint silently accepted by native v2 profile: %v", err)
		}
	}
	compatible := Config{Kind: "openai_compatible", AuthMode: "api_key", ProfileID: "compatible-chat", ProfileRevision: "1", Endpoint: coherePresetEndpoint, VendorID: "cohere"}
	if err := compatible.Validate(&egress.Policy{}); err != nil {
		t.Fatalf("existing Cohere compatibility path changed: %v", err)
	}
}

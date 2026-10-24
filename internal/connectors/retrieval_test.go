package connectors

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestRetrievalDedicatedProfilesAndAddressing(t *testing.T) {
	tests := []struct{ kind, profile, region, project, endpoint, resource, want string }{
		{"bedrock", "bedrock-knowledge-bases", "us-east-1", "", "https://bedrock-agent-runtime.us-east-1.amazonaws.com", "ABCDEFGHIJ", "https://bedrock-agent-runtime.us-east-1.amazonaws.com/knowledgebases/ABCDEFGHIJ/retrieve"},
		{"vertex_ai", "vertex-rag-engine", "us-central1", "test-project", "https://us-central1-aiplatform.googleapis.com/v1/projects/test-project/locations/us-central1", "projects/test-project/locations/us-central1/ragCorpora/123", "https://us-central1-aiplatform.googleapis.com/v1/projects/test-project/locations/us-central1:retrieveContexts"},
		{KindAzureAISearch, "azure-ai-search", "", "", "https://test.search.windows.net", "docs", "https://test.search.windows.net/indexes/docs/docs/search?api-version=2025-09-01"},
	}
	for _, tc := range tests {
		t.Run(tc.profile, func(t *testing.T) {
			c := Config{Kind: tc.kind, ProfileID: tc.profile, ProfileRevision: ProfileRevision, Endpoint: tc.endpoint, CloudProject: tc.project, CloudRegion: tc.region, APIVersion: "2025-09-01"}
			if !c.Supports("retrieval", "openai", "unary") || c.Supports("generation", "openai", "unary") || c.Supports("retrieval", "openai", "streaming") {
				t.Fatal("dedicated retrieval capabilities leaked")
			}
			got, err := c.RetrievalURL(tc.resource)
			if err != nil || got != tc.want {
				t.Fatalf("address %q %v", got, err)
			}
			if _, err = c.RetrievalURL("../unbound"); err == nil {
				t.Fatal("unsafe resource admitted")
			}
		})
	}
	c := Config{Kind: "vertex_ai", ProfileID: "vertex-gemini", ProfileRevision: ProfileRevision}
	if c.Supports("retrieval", "openai", "unary") {
		t.Fatal("generation profile gained cloud retrieval")
	}
}
func TestRetrievalAzureCredentialPlacementAndScope(t *testing.T) {
	c := Config{Kind: KindAzureAISearch, ProfileID: "azure-ai-search", ProfileRevision: ProfileRevision, AuthMode: "api_key", Endpoint: "https://test.search.windows.net", APIVersion: "2025-09-01"}
	req, _ := http.NewRequest(http.MethodPost, c.Endpoint+"/indexes/docs/docs/search?api-version=2025-09-01", strings.NewReader(`{}`))
	auth := NewAuth(&egress.Policy{})
	if _, err := auth.Apply(context.Background(), req, c, []byte("secret"), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Api-Key") != "secret" || req.Header.Get("Authorization") != "" || c.AzureScope() != "https://search.azure.com/.default" {
		t.Fatal("search credentials use the wrong authority")
	}
	c.OperationDefaults = map[string]DefaultSet{"retrieval": {}}
	if c.ValidateProfile() == nil {
		t.Fatal("ignored retrieval defaults were accepted")
	}
}

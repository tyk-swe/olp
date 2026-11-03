package connectors

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/retrieval"
)

func TestRetrievalResourceAcceptanceAgreesWithConnectorAddressing(t *testing.T) {
	tests := []struct {
		config    Config
		resources []string
		fields    retrieval.Fields
	}{
		{Config{Kind: "bedrock", ProfileID: "bedrock-knowledge-bases", ProfileRevision: ProfileRevision}, []string{"ABCDEFGHIJ", "0123456789", "ABCDEFGHI", "ABCDEFGHIJK", "../unbound", ""}, retrieval.Fields{}},
		{Config{Kind: "vertex_ai", ProfileID: "vertex-rag-engine", ProfileRevision: ProfileRevision, CloudProject: "project", CloudRegion: "region"}, []string{"projects/project/locations/region/ragCorpora/123", "projects/project/locations/region/ragCorpora/", "projects/project/locations/region/ragCorpora/../other", "../unbound", ""}, retrieval.Fields{}},
		{Config{Kind: KindAzureAISearch, ProfileID: "azure-ai-search", ProfileRevision: ProfileRevision}, []string{"docs", "a-b_c", "UPPER", "../unbound", ""}, retrieval.Fields{Text: "text", ID: "id"}},
	}
	for _, tc := range tests {
		t.Run(tc.config.ProfileID, func(t *testing.T) {
			for _, resource := range tc.resources {
				_, addressErr := tc.config.RetrievalURL(resource)
				_, encodeErr := retrieval.Encode(tc.config.RetrievalBackend(), resource, tc.fields, []byte(`{"query":"test"}`))
				if (addressErr == nil) != (encodeErr == nil) {
					t.Errorf("resource %q, address error %v, encode error %v", resource, addressErr, encodeErr)
				}
			}
		})
	}
	vertex := tests[1].config
	for _, resource := range []string{"projects/other/locations/region/ragCorpora/123", "projects/project/locations/other/ragCorpora/123"} {
		if _, err := retrieval.Encode(vertex.RetrievalBackend(), resource, retrieval.Fields{}, []byte(`{"query":"test"}`)); err != nil {
			t.Fatalf("resource %q should be valid for another connector: %v", resource, err)
		}
		if _, err := vertex.RetrievalURL(resource); err == nil {
			t.Errorf("connector admitted resource outside its project or region: %q", resource)
		}
	}
}

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
func TestRetrievalKindsAgreeWithRetainedSupports(t *testing.T) {
	declared := map[string]bool{}
	kinds := map[string]bool{}
	for _, profile := range Profiles() {
		kinds[profile.Kind] = true
		if slices.Contains(profile.Operations, "retrieval") {
			declared[profile.Kind] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("no connector profile declares retrieval")
	}
	for kind := range kinds {
		if got := retainedapi.Supports(kind, "retrieval", "openai", "unary"); got != declared[kind] {
			t.Errorf("kind %s profiles declare retrieval %v, retained supports says %v", kind, declared[kind], got)
		}
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

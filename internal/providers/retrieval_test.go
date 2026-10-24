package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestRetrievalMetadataCertificationIsReadOnlyAndIdentityBound(t *testing.T) {
	for _, name := range []string{"documents", "another-index"} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/indexes/documents" || r.URL.Query().Get("api-version") != "2025-09-01" || r.Header.Get("Api-Key") != "key" {
					t.Errorf("unsafe probe: %s %s", r.Method, r.URL)
				}
				fmt.Fprintf(w, `{"name":%q,"fields":[]}`, name)
			}))
			defer server.Close()
			cfg := Configuration{ProviderID: "provider", Kind: KindAzureAISearch, ProfileID: "azure-ai-search", ProfileRevision: "1", AuthMode: "api_key", Endpoint: new(server.URL), APIVersion: new("2025-09-01")}
			cfg.Normalize()
			policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
			err := New(nil, policy, nil).certifyTuple(context.Background(), &cfg, []byte("key"), "documents", CapabilityInput{"retrieval", "openai", "unary"}, 4096)
			if (err == nil) != (name == "documents") {
				t.Fatalf("identity %s: %v", name, err)
			}
		})
	}
}

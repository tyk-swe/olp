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

func TestRetainedCapabilitiesAreValidAndCertifiable(t *testing.T) {
	for _, tuple := range []CapabilityInput{
		{"vector_store", "openai", "unary"}, {"skill", "anthropic", "unary"}, {"context_cache", "gemini", "unary"}, {"fine_tuning", "native", "unary"},
	} {
		if _, err := ValidCapabilities([]CapabilityInput{tuple}); err != nil {
			t.Fatalf("retained tuple rejected: %v %v", tuple, err)
		}
	}
	if !certifiable(KindOpenAI, "openai", CapabilityInput{"vector_store", "openai", "unary"}) {
		t.Fatal("openai vector_store tuple not certifiable")
	}
	if certifiable(KindBedrock, "amazon-bedrock", CapabilityInput{"retrieval", "openai", "unary"}) {
		t.Fatal("bedrock retrieval tuple certified without a native path")
	}
	// Certification is one read-only collection request.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/vector_stores" {
			t.Errorf("unsafe probe: %s %s", r.Method, r.URL)
		}
		fmt.Fprint(w, `{"data":[{"id":"vs_native"}]}`)
	}))
	defer server.Close()
	cfg := Configuration{ProviderID: "provider", Kind: KindOpenAI, AuthMode: AuthAPIKey, Endpoint: new(server.URL + "/v1")}
	cfg.Normalize()
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	if err := New(nil, policy, nil).certifyTuple(context.Background(), &cfg, []byte("secret"), "model", CapabilityInput{"vector_store", "openai", "unary"}, 4096); err != nil {
		t.Fatalf("retained certification: %v", err)
	}
}

func TestRetainedAgentCertificationDoesNotCreateResources(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		valid  bool
	}{
		{200, `{"agents":[{"id":"native-id"}]}`, true},
		{200, `{}`, true},
		{200, `{"agents":"invalid"}`, false},
		{200, `{"agents":[{"description":"missing-id"}]}`, false},
		{404, `{"error":"unavailable"}`, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1beta/agents" || r.URL.Query().Get("page_size") != "1" {
					t.Errorf("unsafe probe: %s %s", r.Method, r.URL)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			cfg := Configuration{ProviderID: "provider", Kind: KindGemini, ProfileID: "gemini-agents", ProfileRevision: "1", AuthMode: AuthAPIKey, Endpoint: new(server.URL + "/v1beta")}
			cfg.Normalize()
			policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
			err := New(nil, policy, nil).certifyAgentResources(context.Background(), &cfg, []byte("secret"), "model")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
		})
	}
}

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

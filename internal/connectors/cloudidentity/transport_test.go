package cloudidentity

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestIdentityRequestsReachOnlyTheEndpointsTheEnvironmentNames(t *testing.T) {
	t.Parallel()
	loopback := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(identity.Close)
	transports := map[string]struct {
		variable  string
		transport func(getenv func(string) string) http.RoundTripper
	}{
		"a Microsoft managed identity endpoint": {"IDENTITY_ENDPOINT", func(getenv func(string) string) http.RoundTripper {
			return azureIdentity(loopback, nil, getenv)
		}},
		"an AWS container credentials endpoint": {"AWS_CONTAINER_CREDENTIALS_FULL_URI", func(getenv func(string) string) http.RoundTripper {
			return Billing(loopback, http.DefaultTransport, getenv).Metadata
		}},
	}
	for name, c := range transports {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			unnamed := func(string) string { return "" }
			if response, err := client(c.transport(unnamed)).Get(identity.URL); err == nil {
				response.Body.Close()
				t.Fatal("an endpoint the environment does not name was reached")
			}
			named := func(variable string) string { return map[string]string{c.variable: identity.URL}[variable] }
			response, err := client(c.transport(named)).Get(identity.URL)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("status %d from the named endpoint", response.StatusCode)
			}
		})
	}
}

package media

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

func testTransport(t *testing.T, handler http.Handler) (*Transport, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	policy := egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
	return &Transport{
		Client:           policy.Client(30 * time.Second),
		Auth:             connectors.NewAuth(&policy),
		Egress:           &policy,
		Spool:            testSpool(t, MinCapacityBytes),
		MaxResponseBytes: 1 << 20,
	}, server
}

func testTarget(endpoint string) Target {
	return Target{
		Config: connectors.Config{Kind: "openai", AuthMode: "none", Endpoint: endpoint},
		Model:  "upstream-model",
	}
}

// A server that accepts the request then stalls past the client deadline.

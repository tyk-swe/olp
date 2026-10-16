package media

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
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

func TestTransportVideoListQuery(t *testing.T) {
	transport, server := testTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/videos" || r.Method != "GET" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		if got := r.URL.Query().Get("limit"); got != "20" {
			t.Errorf("limit: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"v1","object":"video","status":"completed"}],"has_more":false}`))
	}))
	query := url.Values{"limit": {"20"}}
	call := &UpstreamCall{Method: "GET", Path: "videos", Query: query, Kind: ResponseVideoList}
	result, failure := transport.Do(context.Background(), testTarget(server.URL+"/v1"), call, nil)
	if failure != nil {
		t.Fatal(failure)
	}
	if result.List == nil || len(result.List.Jobs) != 1 {
		t.Fatalf("list: %+v", result.List)
	}
}

package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// carrierFunc stands in for the unconfined plugins that carry their profiles'
// traffic.
type carrierFunc func(ctx context.Context, provider abi.Provider, req *http.Request) (*http.Response, error)

func (f carrierFunc) Carry(ctx context.Context, _ string, provider abi.Provider, req *http.Request, _ []string) (*http.Response, error) {
	return f(ctx, provider, req)
}

// forward carries a request to the upstream as a plugin would, marking it.
func forward(ctx context.Context, req *http.Request) (*http.Response, error) {
	carried := req.Clone(ctx)
	carried.RequestURI = ""
	carried.Header.Set("X-Carried-By", "acme")
	return http.DefaultClient.Do(carried)
}

// newCarryingHarness serves provider a from an unconfined plugin's profile
// whose traffic carrier carries; provider b stays its failover.
func newCarryingHarness(t *testing.T, carrier connectors.Carrier) *harness {
	t.Helper()
	h := newHarness(t, Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096, Carrier: carrier, UnconfinedPlugins: true})
	manifest := abi.Manifest{Name: "acme", Version: "1.0.0", Profiles: []abi.Profile{{
		ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat", CarriesTraffic: true,
		Hosting: abi.Hosting{Address: h.upstream.URL + "/a/v1", Headers: map[string]string{"Authorization": "Bearer {credential}"}},
	}}}
	plugin, err := connectors.NewUnconfinedPluginProfile(strings.Repeat("cd", 32), manifest, "acme-chat")
	if err != nil {
		t.Fatal(err)
	}
	h.pinPlugin(plugin, nil)
	return h
}

// A plugin that keeps failing after it sent a request never fails over, but
// its failures count towards its provider's health, so the provider's circuit
// opens and later requests go to the route's other target.
func TestCarriedFailuresOpenTheCircuit(t *testing.T) {
	h := newCarryingHarness(t, carrierFunc(func(ctx context.Context, _ abi.Provider, req *http.Request) (*http.Response, error) {
		resp, err := forward(ctx, req)
		if err == nil {
			resp.Body.Close()
		}
		return nil, errors.New("the plugin lost the response")
	}))
	for range circuitFailures {
		if resp, body := h.chat(fullKey, nil); resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "ambiguous_upstream_result" {
			t.Fatalf("status %d body %v", resp.StatusCode, body)
		}
	}
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK || h.mock.count("a") != circuitFailures || h.mock.count("b") != 1 {
		t.Fatalf("status %d a=%d b=%d", resp.StatusCode, h.mock.count("a"), h.mock.count("b"))
	}
}

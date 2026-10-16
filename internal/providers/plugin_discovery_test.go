package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// pluginConfiguration is a plugin provider pinning a profile with the hosting
// adaptation at server, which places the credential as a token.
func pluginConfiguration(t *testing.T, server *httptest.Server, hosting abi.Hosting) *Configuration {
	t.Helper()
	hosting.Address = server.URL + "/v1"
	hosting.Headers = map[string]string{"Authorization": "Token {credential}"}
	cfg := &Configuration{ProviderID: "provider-acme", Kind: KindPlugin, AuthMode: connectors.AuthStaticCredential, ProfileID: "acme-chat", ProfileRevision: strings.Repeat("ab", 32)}
	cfg.Normalize()
	pinAcme(t, cfg, server.URL, abi.Profile{ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat", Hosting: hosting})
	return cfg
}

// pinAcme pins cfg to profile, as the installed acme plugin declares it with
// origin as its only origin, and gives cfg the address the profile places it
// at, as Pin does.
func pinAcme(t *testing.T, cfg *Configuration, origin string, profile abi.Profile) {
	t.Helper()
	installed, err := json.Marshal(connectors.InstalledPlugin{Manifest: abi.Manifest{Name: "acme", Version: "1.0.0", Origins: []string{origin}, Profiles: []abi.Profile{profile}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.pinned(installed); err != nil {
		t.Fatal(err)
	}
	cfg.Endpoint = new(cfg.plugin.Address(cfg.Options.PluginOptions))
}

func loopbackPolicy() *egress.Policy {
	return &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
}

func TestPluginDiscoveryRefusesBrokenContinuations(t *testing.T) {
	for name, tc := range map[string]struct{ page, more string }{
		"a repeated cursor":             {page: `{"items":[{"slug":"first"}],"next":"same"}`},
		"another page without a cursor": {page: `{"items":[{"slug":"first"}],"more":true}`, more: "more"},
		"no model array":                {page: `{"models":[]}`},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.page) }))
			defer server.Close()
			pagination := &abi.Pagination{Parameter: "page", Cursor: "next", More: tc.more}
			cfg := pluginConfiguration(t, server, abi.Hosting{Discovery: &abi.Discovery{Path: "/models", Models: "items", ID: "slug", Pagination: pagination}})
			_, err := New(nil, loopbackPolicy(), nil).listModelFacts(context.Background(), cfg, []byte("secret"))
			if refusal, ok := errors.AsType[*probeError](err); !ok || refusal.Code != "provider_protocol_error" {
				t.Fatalf("listed through %s: %v", name, err)
			}
		})
	}
}

// failingSigner is a signing hook that fails as told.
type failingSigner struct{ err error }

func (s failingSigner) Sign(context.Context, string, abi.Provider, abi.SignRequest, []string) (abi.SignResult, error) {
	return abi.SignResult{}, s.err
}

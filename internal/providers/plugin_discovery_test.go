package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
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
	manifest, err := json.Marshal(connectors.InstalledPlugin{Manifest: abi.Manifest{Name: "acme", Version: "1.0.0", Origins: []string{server.URL}, Profiles: []abi.Profile{{
		ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat", Hosting: hosting,
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Configuration{ProviderID: "provider-acme", Kind: KindPlugin, AuthMode: connectors.AuthStaticCredential, ProfileID: "acme-chat", ProfileRevision: strings.Repeat("ab", 32), Endpoint: new(hosting.Address)}
	cfg.Normalize()
	if err = cfg.pinned(manifest); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func loopbackPolicy() *egress.Policy {
	return &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
}

func TestPluginDiscoveryFollowsTheDeclaredListing(t *testing.T) {
	for name, tc := range map[string]struct {
		pagination *abi.Pagination
		pages      []string
	}{
		"cursor until a page has none": {&abi.Pagination{Parameter: "page_token", Cursor: "next"}, []string{
			`{"items":[{"slug":"first"},{"slug":"models/second"}],"next":"2"}`,
			`{"items":[{"slug":"third"},{"slug":"first"},{"slug":7}],"next":""}`,
		}},
		"cursor while more pages follow": {&abi.Pagination{Parameter: "after", Cursor: "last", More: "more"}, []string{
			`{"items":[{"slug":"first"},{"slug":"second"}],"more":true,"last":"second"}`,
			`{"items":[{"slug":"third"}],"more":false,"last":"third"}`,
		}},
	} {
		t.Run(name, func(t *testing.T) {
			var cursors []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/catalog/models" || r.Header.Get("Authorization") != "Token secret" {
					t.Errorf("listed with %s %s %v", r.Method, r.URL, r.Header)
				}
				cursor := r.URL.Query().Get(tc.pagination.Parameter)
				cursors = append(cursors, cursor)
				page := 0
				if cursor != "" {
					page = 1
				}
				fmt.Fprint(w, tc.pages[page])
			}))
			defer server.Close()
			cfg := pluginConfiguration(t, server, abi.Hosting{Discovery: &abi.Discovery{Path: "/catalog/models", Models: "items", ID: "slug", Pagination: tc.pagination}})
			models, err := New(nil, loopbackPolicy(), nil).listModelFacts(context.Background(), cfg, []byte("secret"))
			if err != nil {
				t.Fatal(err)
			}
			names := []string{}
			for _, model := range models {
				names = append(names, model.Name+"/"+model.Display)
			}
			if !slices.Equal(names, []string{"first/first", "second/second", "third/third"}) || len(cursors) != 2 || cursors[0] != "" {
				t.Fatalf("listed %v with cursors %q", names, cursors)
			}
		})
	}
}

func TestPluginDiscoveryRefusesBrokenContinuations(t *testing.T) {
	for name, page := range map[string]string{
		"a repeated cursor":             `{"items":[{"slug":"first"}],"next":"same"}`,
		"another page without a cursor": `{"items":[{"slug":"first"}],"more":true}`,
		"no model array":                `{"models":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, page) }))
			defer server.Close()
			pagination := &abi.Pagination{Parameter: "page", Cursor: "next"}
			if strings.Contains(page, "more") {
				pagination.More = "more"
			}
			cfg := pluginConfiguration(t, server, abi.Hosting{Discovery: &abi.Discovery{Path: "/models", Models: "items", ID: "slug", Pagination: pagination}})
			_, err := New(nil, loopbackPolicy(), nil).listModelFacts(context.Background(), cfg, []byte("secret"))
			if refusal, ok := errors.AsType[*probeError](err); !ok || refusal.Code != "provider_protocol_error" {
				t.Fatalf("listed through %s: %v", name, err)
			}
		})
	}
}

func TestPluginDiscoveryPreservesEscapedAddressOptions(t *testing.T) {
	for _, account := range []string{"team/prod", "team?prod", "team%2Fprod"} {
		t.Run(account, func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.EscapedPath())
				if r.URL.Query().Get("page") == "" {
					fmt.Fprint(w, `{"items":[{"slug":"first"}],"next":"second/page"}`)
				} else {
					if r.URL.Query().Get("page") != "second/page" {
						t.Errorf("discovery cursor: %s", r.URL)
					}
					fmt.Fprint(w, `{"items":[{"slug":"second"}]}`)
				}
			}))
			defer server.Close()
			plugin, err := connectors.NewPluginProfile(strings.Repeat("ab", 32), abi.Manifest{Name: "acme", Version: "1.0.0", Origins: []string{server.URL}, Profiles: []abi.Profile{{
				ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat", Options: []abi.Option{{Name: "account", Label: "Account"}},
				Hosting: abi.Hosting{
					Address: server.URL + "/accounts/{options.account}/v1", Headers: map[string]string{"Authorization": "Token {credential}"},
					Discovery: &abi.Discovery{Path: "/models", Models: "items", ID: "slug", Pagination: &abi.Pagination{Parameter: "page", Cursor: "next"}},
				},
			}}}, "acme-chat")
			if err != nil {
				t.Fatal(err)
			}
			options := map[string]string{"account": account}
			endpoint := plugin.Address(options)
			cfg := &Configuration{ProviderID: "provider-acme", Kind: KindPlugin, AuthMode: connectors.AuthStaticCredential, ProfileID: "acme-chat", ProfileRevision: strings.Repeat("ab", 32), Endpoint: &endpoint, Options: Options{PluginOptions: options}, plugin: plugin}
			cfg.Normalize()
			models, err := New(nil, loopbackPolicy(), nil).listModelFacts(t.Context(), cfg, []byte("secret"))
			want := strings.TrimPrefix(endpoint, server.URL) + "/models"
			if err != nil || len(models) != 2 || !slices.Equal(paths, []string{want, want}) {
				t.Fatalf("discovery: models=%v error=%v paths=%q want=%q", models, err, paths, want)
			}
		})
	}
}

// Without a declared listing, operators declare a plugin provider's models,
// and discovery certifies each.
func TestPluginProvidersWithoutDiscoveryCertifyDeclaredModels(t *testing.T) {
	var certified []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("called %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		var body struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		certified = append(certified, body.Model)
		fmt.Fprintf(w, `{"id":"c","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`, body.Model)
	}))
	defer server.Close()
	cfg := pluginConfiguration(t, server, abi.Hosting{})
	probes := New(nil, loopbackPolicy(), nil)
	if _, err := probes.listModelFacts(context.Background(), cfg, []byte("secret")); err == nil || classify(err).Code != "model_required" {
		t.Fatalf("probed without a declared model: %v", err)
	}
	cfg.ProbeModels = []string{"acme-large"}
	models, err := probes.listModelFacts(context.Background(), cfg, []byte("secret"))
	if err != nil || len(models) != 1 || models[0].Name != "acme-large" || !slices.Equal(certified, []string{"acme-large"}) {
		t.Fatalf("declared %+v certified %v: %v", models, certified, err)
	}
}

// A probe classifies an upstream rejection as the plugin profile declares,
// and reports only local text.
func TestPluginProbesClassifyAsDeclared(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":{"message":"quota spent for %s","type":"invalid_request_error","code":%q}}`, r.Header.Get("Authorization"), r.URL.Query().Get("code"))
	}))
	defer server.Close()
	cfg := pluginConfiguration(t, server, abi.Hosting{Classification: []abi.FailureRule{{Status: 400, Code: "insufficient_quota", Class: abi.ClassRateLimited}}})
	for code, want := range map[string]probeError{
		"insufficient_quota": {Code: "upstream_rate_limit", Detail: "The upstream is rate limiting (HTTP 400)."},
		"invalid_value":      {Code: "upstream_rejected", Detail: "The upstream answered HTTP 400."},
	} {
		status, body, err := New(nil, loopbackPolicy(), nil).call(context.Background(), cfg, []byte("secret"), http.MethodGet, "/models?code="+code, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := statusError(cfg, status, body); *got != want {
			t.Errorf("%s: %+v, want %+v", code, *got, want)
		}
	}
}

// failingSigner is a signing hook that fails as told.
type failingSigner struct{ err error }

func (s failingSigner) Sign(context.Context, string, abi.Provider, abi.SignRequest, []string) (abi.SignResult, error) {
	return abi.SignResult{}, s.err
}

// A probe blames the credential for a signing failure only when the plugin
// reports one; a hook that can't run leaves the upstream out of reach.
func TestPluginProbesBlameTheCredentialOnlyForReportedSigningFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("a request whose signing failed was sent") }))
	defer server.Close()
	manifest, err := json.Marshal(connectors.InstalledPlugin{Manifest: abi.Manifest{Name: "acme", Version: "1.0.0", Origins: []string{server.URL}, Profiles: []abi.Profile{{
		ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat", Hosting: abi.Hosting{Address: server.URL + "/v1"}, Signing: true,
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Configuration{ProviderID: "provider-acme", Kind: KindPlugin, AuthMode: connectors.AuthStaticCredential, ProfileID: "acme-chat", ProfileRevision: strings.Repeat("ab", 32), Endpoint: new(server.URL + "/v1")}
	cfg.Normalize()
	if err = cfg.pinned(manifest); err != nil {
		t.Fatal(err)
	}
	for failure, want := range map[error]string{
		&abi.Error{Code: "credential_expired", Message: "The key expired."}:     "credential_invalid",
		errors.New("plugin_timed_out: The plugin exceeded its 10s time limit."): "upstream_unavailable",
	} {
		_, _, err := New(nil, loopbackPolicy(), failingSigner{failure}).call(context.Background(), cfg, []byte("secret"), http.MethodGet, "/models", nil)
		if probe, ok := errors.AsType[*probeError](err); !ok || probe.Code != want {
			t.Errorf("signing failure %v probed as %v, want %s", failure, err, want)
		}
	}
}

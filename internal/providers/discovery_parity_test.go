package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestNativeDiscoveryFollowsBoundedPagesAndPreservesFacts(t *testing.T) {
	for _, kind := range []string{KindAnthropic, KindGemini} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if kind == KindAnthropic {
					if r.Header.Get("X-Api-Key") != "secret" {
						t.Error("missing native credential")
					}
					if r.URL.Query().Get("after_id") == "" {
						fmt.Fprint(w, `{"data":[{"id":"first","display_name":"First model"}],"has_more":true,"last_id":"first"}`)
					} else {
						fmt.Fprint(w, `{"data":[{"id":"second"}],"has_more":false}`)
					}
				} else {
					if r.Header.Get("X-Goog-Api-Key") != "secret" {
						t.Error("missing native credential")
					}
					if r.URL.Query().Get("pageToken") == "" {
						fmt.Fprint(w, `{"models":[{"name":"models/first","displayName":"First model","inputTokenLimit":8192,"outputTokenLimit":1024}],"nextPageToken":"page two"}`)
					} else {
						fmt.Fprint(w, `{"models":[{"name":"models/second"}]}`)
					}
				}
			}))
			defer server.Close()
			policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
			cfg := Configuration{Kind: kind, AuthMode: AuthAPIKey, Endpoint: new(server.URL)}
			cfg.Normalize()
			models, e := New(nil, policy).listModelFacts(context.Background(), &cfg, []byte("secret"))
			if e != nil || len(models) != 2 || calls != 2 {
				t.Fatalf("discovery: %+v %v calls=%d", models, e, calls)
			}
			if models[0].Display != "First model" || models[1].Name != "second" {
				t.Fatalf("lost model identity: %+v", models)
			}
			if kind == KindGemini {
				b, _ := json.Marshal(models[0].Metadata)
				if validateMetadata(b) != nil || string(models[0].Metadata["context_length"]) != "8192" {
					t.Fatalf("lost facts: %s", b)
				}
			}
		})
	}
}

// Without a model-list API the probe proves a declared model; an Azure v1
// connection without a deployment has none to prove.
func TestAzureProbeWithoutDeploymentRequiresAModel(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	cfg := Configuration{ProfileID: "azure-v1-chat", ProfileRevision: "1", Kind: KindAzure, AuthMode: AuthAPIKey, Endpoint: new(server.URL)}
	cfg.Normalize()
	if err := cfg.Validate(policy); err != nil {
		t.Fatal(err)
	}
	models, err := New(nil, policy).listModelFacts(context.Background(), &cfg, []byte("secret"))
	if classify(err).Code != "model_required" || calls.Load() != 0 {
		t.Fatalf("probe without a deployment: %+v %v calls=%d", models, err, calls.Load())
	}
}

// Operation-only profiles serve no generation tuple; their connection probe
// uses the profile's own registered operation.
func TestOperationProfileProbeUsesItsServedOperation(t *testing.T) {
	for _, test := range []struct{ profile, vendor, base, path, response string }{
		{"cohere-embed-v2", "cohere-native-v2", "/v2", "/v2/embed", `{"id":"embed-probe","embeddings":{"float":[[1,-2]]},"texts":["embedding probe"],"meta":{"billed_units":{"input_tokens":2}}}`},
		{"cohere-rerank-v2", "cohere-native-v2", "/v2", "/v2/rerank", `{"id":"rank-probe","results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.1}],"meta":{"billed_units":{"search_units":1}}}`},
		{"voyage-rerank", "voyage", "/v1", "/v1/rerank", `{"object":"list","data":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.1}],"model":"fixture-model","usage":{"total_tokens":4}}`},
	} {
		t.Run(test.profile, func(t *testing.T) {
			var mu sync.Mutex
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, test.response)
			}))
			defer server.Close()
			policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
			cfg := Configuration{ProviderID: "abcdef01-2345-4678-9abc-def012345678", ProfileID: test.profile, ProfileRevision: "1", Kind: KindOpenAICompatible, AuthMode: AuthAPIKey, Endpoint: new(server.URL + test.base), Options: Options{VendorID: new(test.vendor)}}
			cfg.Normalize()
			if err := cfg.Validate(policy); err != nil {
				t.Fatal(err)
			}
			cfg.ProbeModels = []string{"fixture-model"}
			models, err := New(nil, policy).listModelFacts(context.Background(), &cfg, []byte("secret"))
			mu.Lock()
			defer mu.Unlock()
			if err != nil || len(models) != 1 || models[0].Name != "fixture-model" || len(paths) != 1 || paths[0] != test.path {
				t.Fatalf("probe: %+v %v paths=%v", models, err, paths)
			}
		})
	}
}

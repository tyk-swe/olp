package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
			models, e := New(nil, policy, nil).listModelFacts(context.Background(), &cfg, []byte("secret"))
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

// TestWatsonxDiscoveryListsChatFoundationModels covers watsonx's listing: the
// foundation model specifications that serve chat, by model ID and label.
func TestWatsonxDiscoveryListsChatFoundationModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.URL.Path != "/ml/v1/foundation_model_specs" || query.Get("version") != "2025-10-25" || query.Get("filters") != "function_text_chat" || query.Get("limit") != "200" {
			t.Errorf("listing request = %s", r.URL)
		}
		fmt.Fprint(w, `{"total_count":2,"limit":200,"first":{"href":"x"},"resources":[{"model_id":"ibm/granite-3-8b-instruct","label":"granite-3-8b-instruct","provider":"IBM"},{"model_id":"meta-llama/llama-3-3-70b-instruct","label":"llama-3-3-70b-instruct"}]}`)
	}))
	defer server.Close()
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	cfg := Configuration{Kind: KindWatsonx, AuthMode: AuthNone, Endpoint: new(server.URL), CloudRegion: new("us-south"), CloudProject: new("8f3b2c1d-1234-4abc-9def-0123456789ab")}
	cfg.Normalize()
	models, err := New(nil, policy, nil).listModelFacts(context.Background(), &cfg, nil)
	if err != nil || len(models) != 2 || models[0].Name != "ibm/granite-3-8b-instruct" || models[0].Display != "granite-3-8b-instruct" || models[1].Name != "meta-llama/llama-3-3-70b-instruct" {
		t.Fatalf("discovery = %+v, %v", models, err)
	}
}

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

//go:build integration

package gateway

import (
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestIntegrationSemanticCacheRoutes(t *testing.T) {
	h, _ := cacheHarness(t)
	h.gateway.Admission = NewAdmission(mediaLimiter(t), nil, h.gateway.log)
	url := os.Getenv("OLP_TEST_CACHE_VALKEY_URL")
	if url == "" {
		t.Fatal("OLP_TEST_CACHE_VALKEY_URL is required")
	}
	cfg, err := coordination.Configuration(url, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client, err := coordination.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	h.gateway.Cache.Client = client
	h.gateway.Cache.Installation = uuid.NewString()
	h.republish(func(s *routing.Snapshot, a, b routing.Provider) {
		b.Capabilities = append(b.Capabilities, routing.Capability{Model: modelB, Operation: "embeddings", Surface: "openai", Mode: "unary"})
		s.Providers[b.ID] = b
		main := s.Routes[routeSlug]
		main.Targets = main.Targets[:1]
		main.Cache = &cache.Policy{Mode: "semantic", EmbeddingRoute: "cache-embedding", AllowUnsealedVectors: true}
		s.Routes[routeSlug] = main
		s.Routes["cache-embedding"] = routing.Route{ID: uuid.NewString(), Slug: "cache-embedding", RevisionID: uuid.NewString(), RoutingID: uuid.NewString(), Operations: []string{"embeddings"}, OverallTimeout: 5000, MaxAttempts: 1, Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed}, Targets: []routing.Target{{ID: uuid.NewString(), ProviderID: b.ID, ProviderModel: modelB, Weight: 1, Timeout: 3000, RoutingID: uuid.NewString()}}}
	})
	h.rt.mu.Lock()
	key := h.rt.keys[fullKey]
	key.LookupID = "cache_test_lookup"
	key.Policy.AllowedRoutes = []string{routeSlug, "cache-embedding"}
	key.Policy.MaxConcurrency = perMinute(1)
	h.rt.keys[fullKey] = key
	h.rt.mu.Unlock()
	h.mock.set("b", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","model":"model-b","data":[{"object":"embedding","index":0,"embedding":[1,0,0]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`)
	})
	request := func(text string) string {
		response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"`+text+`"}]}`), nil)
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != 200 {
			t.Fatalf("%d %s", response.StatusCode, body)
		}
		return response.Header.Get("X-OLP-Cache")
	}
	if got := request("hello"); got != "miss" {
		t.Fatal(got)
	}
	if got := request("greetings"); got != "semantic-hit" {
		t.Fatal(got)
	}
	for _, env := range h.sink.await(t, 4) {
		if env.Origin == usage.OriginCacheEmbedding {
			event := accountingEvent(env)
			if event == nil || event.ParentRequestID == nil {
				t.Fatal("embedding lost parent accounting")
			}
			if _, err := usage.Validate(event); err != nil {
				t.Fatalf("invalid embedding accounting: %v", err)
			}
		}
	}
	if h.mock.count("a") != 1 || h.mock.count("b") != 2 {
		t.Fatalf("generation/embedding attempts = %d/%d", h.mock.count("a"), h.mock.count("b"))
	}
	client.Close()
	if got := request("hello"); got != "bypass" {
		t.Fatalf("closed cache connection did not bypass: %s", got)
	}
}

func BenchmarkIntegrationResponseCache(b *testing.B) {
	url := os.Getenv("OLP_TEST_CACHE_VALKEY_URL")
	if url == "" {
		b.Fatal("OLP_TEST_CACHE_VALKEY_URL is required")
	}
	cfg, err := coordination.Configuration(url, "", time.Second)
	if err != nil {
		b.Fatal(err)
	}
	client, err := coordination.Open(b.Context(), cfg)
	if err != nil {
		b.Fatal(err)
	}
	defer client.Close()
	benchmarkResponseCache(b, client)
}

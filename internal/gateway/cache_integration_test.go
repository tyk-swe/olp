//go:build integration

package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
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

func TestIntegrationSemanticCacheEmbeddingModels(t *testing.T) {
	for _, fidelity := range []string{routing.FidelityTransformed, routing.FidelityStrict} {
		t.Run(fidelity, func(t *testing.T) {
			h, _ := cacheHarness(t)
			cfg, err := coordination.Configuration(os.Getenv("OLP_TEST_CACHE_VALKEY_URL"), "", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			client, err := coordination.Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.Close)
			h.gateway.Cache.Client = client
			h.gateway.Cache.Installation = uuid.NewString()
			var primaryID string
			h.republish(func(s *routing.Snapshot, _, b routing.Provider) {
				primaryID = b.ID
				b.Kind, b.ProfileID, b.ProfileRevision = "openai", "openai-embeddings", connectors.ProfileRevision
				b.Capabilities = []routing.Capability{
					{Model: modelB, Operation: "embeddings", Surface: "openai", Mode: "unary"},
					{Model: "model-c", Operation: "embeddings", Surface: "openai", Mode: "unary"},
				}
				s.Providers[b.ID] = b
				secondaryID := b.ID
				if fidelity == routing.FidelityStrict {
					secondary := b
					secondary.ID, secondary.RevisionID = uuid.NewString(), uuid.NewString()
					secondary.Slots = append([]routing.Slot(nil), b.Slots...)
					secondary.Slots[0].ID = uuid.NewString()
					s.Providers[secondary.ID] = secondary
					secondaryID = secondary.ID
				}
				main := s.Routes[routeSlug]
				main.Targets = main.Targets[:1]
				main.Cache = &cache.Policy{Mode: "semantic", EmbeddingRoute: "cache-embedding", AllowUnsealedVectors: true}
				s.Routes[routeSlug] = main
				s.Routes["cache-embedding"] = routing.Route{
					ID: uuid.NewString(), Slug: "cache-embedding", RevisionID: uuid.NewString(), RoutingID: uuid.NewString(),
					Operations: []string{"embeddings"}, OverallTimeout: 5000, MaxAttempts: 2,
					Fidelity: routing.RouteFidelity{Mode: fidelity}, Targets: []routing.Target{
						{ID: uuid.NewString(), ProviderID: b.ID, ProviderModel: modelB, Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()},
						{ID: uuid.NewString(), ProviderID: secondaryID, ProviderModel: "model-c", Priority: 1, Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()},
					},
				}
			})
			var failPrimary atomic.Bool
			h.mock.set("b", func(w http.ResponseWriter, r *http.Request) {
				var body struct{ Model string }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Model == modelB && failPrimary.Load() {
					status(http.StatusServiceUnavailable, `{"error":{"message":"unavailable","type":"server_error"}}`)(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				// Identical vectors make any cross-model comparison a false hit.
				io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1,0,0]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`)
			})
			requests := 0
			request := func(text, want string, headers map[string]string) {
				t.Helper()
				response, body := h.chat(fullKey, headers, `,"messages":[{"role":"user","content":"`+text+`"}]`)
				if response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != want {
					t.Fatalf("%s: status=%d cache=%s want=%s body=%v", text, response.StatusCode, response.Header.Get("X-OLP-Cache"), want, body)
				}
				requests++
				// Wait for the parent to finish storing before the next lookup.
				for deadline := time.Now().Add(time.Second); ; {
					h.sink.mu.Lock()
					parents := 0
					for _, env := range h.sink.envs {
						if env.Origin != usage.OriginCacheEmbedding {
							parents++
						}
					}
					h.sink.mu.Unlock()
					if parents >= requests {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("cache request did not finish")
					}
					time.Sleep(time.Millisecond)
				}
			}
			request("first", "miss", nil)
			failPrimary.Store(true)
			if fidelity == routing.FidelityStrict {
				// Strict requests pin a serving identity on dispatch. An open
				// circuit selects the alternate model before that binding.
				h.gateway.health.mu.Lock()
				h.gateway.health.provider(primaryID).openUntil = time.Now().Add(time.Hour)
				h.gateway.health.mu.Unlock()
			}
			request("second", "miss", nil)
			request("third", "semantic-hit", nil)
			failPrimary.Store(false)
			if fidelity == routing.FidelityStrict {
				h.gateway.health.mu.Lock()
				h.gateway.health.provider(primaryID).openUntil = time.Time{}
				h.gateway.health.mu.Unlock()
			}
			request("fourth", "semantic-hit", nil)
			embeddings := h.mock.count("b")
			request("first", "hit", nil)
			if h.mock.count("b") != embeddings || h.mock.count("a") != 2 {
				t.Fatal("exact hit embedded again or different models reused a response")
			}
			request("refresh", "miss", map[string]string{"X-OLP-Cache": "no-cache"})
			request("refresh", "hit", nil)
		})
	}
}

func TestIntegrationCacheHitExplicitChargeAdmission(t *testing.T) {
	h, _ := cacheHarness(t)
	limiter := mediaLimiter(t)
	h.gateway.Admission = NewAdmission(limiter, nil, h.gateway.log)
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		route := s.Routes[routeSlug]
		route.Cache.CountHitsAgainst = []string{"requests", "budgets"}
		s.Routes[routeSlug] = route
	})
	h.rt.inputs = &usage.RoutingInputs{RefreshedAt: time.Now(), RateCards: []usage.RoutingRateCard{{Revisions: []usage.RoutingCardRevision{{
		ID: uuid.NewString(), EffectiveAt: time.Now().Add(-time.Hour), Markup: "1", Prices: []usage.RoutingPrice{{Price: usage.Price{
			ProviderKind: "openai_compatible", Model: modelA, Operation: "generation", Currency: "USD",
			InputPerMillion: costText("100"), OutputPerMillion: costText("0"),
		}}},
	}}}}}
	if response, body := h.chat(fullKey, nil); response.StatusCode != http.StatusOK {
		t.Fatalf("warm cache: %d %v", response.StatusCode, body)
	}
	h.sink.nth(t, 1)
	h.rt.mu.Lock()
	key := h.rt.keys[fullKey]
	key.LookupID = "cache_charge_admission"
	key.Policy.CostBasis = "charge"
	key.Policy.DailyCostLimit = costText("0.0001")
	key.Policy.RequestsPerMinute = perMinute(1)
	h.rt.keys[fullKey] = key
	h.rt.mu.Unlock()
	windows := limits.BudgetWindows(time.Now())
	if _, _, err := limiter.ApplyCostSnapshot(t.Context(), limits.CostSnapshot{
		Basis: limits.BasisCharge, CostOwnerID: key.ID,
		DailyWindowID: windows.DailyID, MonthlyWindowID: windows.MonthlyID,
		DailyAccrued: "0", MonthlyAccrued: "0",
	}); err != nil {
		t.Fatal(err)
	}
	if response, body := h.chat(fullKey, nil); response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("cache hit exceeded charge budget: status=%d body=%v", response.StatusCode, body)
	}
	h.sink.nth(t, 2)
	h.rt.mu.Lock()
	key.Policy.DailyCostLimit = costText("0.0003")
	h.rt.keys[fullKey] = key
	h.rt.mu.Unlock()
	if response, body := h.chat(fullKey, nil); response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != "hit" {
		t.Fatalf("rejected hit retained its RPM debit: status=%d cache=%s body=%v", response.StatusCode, response.Header.Get("X-OLP-Cache"), body)
	}
	if got := h.sink.nth(t, 3).Attempts[0].Cache; got == nil || got.BudgetCost != "0" || got.BudgetCharge != "0.0002" {
		t.Fatalf("cache debit = %+v", got)
	}
	if h.mock.count("a") != 1 {
		t.Fatal("cache hit dispatched a provider request")
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

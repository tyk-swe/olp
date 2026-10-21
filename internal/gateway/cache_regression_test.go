package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestResponseCacheNativeDialectIsolation(t *testing.T) {
	h, _ := enableTestCache(t, strictHarness(t, func(s *routing.Snapshot) {
		for id, p := range s.Providers {
			p.ProfileID = "voyage-rerank"
			p.Capabilities = []routing.Capability{{Model: p.Capabilities[0].Model, Operation: "rerank", Surface: "native", Mode: "unary"}}
			s.Providers[id] = p
		}
		route := s.Routes[routeSlug]
		route.Operations = []string{"rerank"}
		s.Routes[routeSlug] = route
	}))
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		route := s.Routes[routeSlug]
		route.Cache.Operations = []string{"rerank"}
		s.Routes[routeSlug] = route
	})
	h.mock.set("a", status(http.StatusOK, `{"data":[{"index":0,"relevance_score":0.9}],"usage":{"total_tokens":2}}`))
	for _, request := range []struct{ dialect, field, cache string }{
		{"rerank", "results", "miss"}, {"voyage-rerank", "data", "miss"},
		{"rerank", "results", "hit"}, {"voyage-rerank", "data", "hit"},
	} {
		response := h.do(t.Context(), http.MethodPost, "/native/"+request.dialect+"/models/"+routeSlug, fullKey,
			[]byte(`{"model":"`+routeSlug+`","query":"q","documents":["a"]}`), nil)
		var body map[string]json.RawMessage
		err := json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != request.cache || body[request.field] == nil {
			t.Fatalf("%s: status=%d cache=%s body=%s error=%v", request.dialect, response.StatusCode, response.Header.Get("X-OLP-Cache"), body, err)
		}
	}
	if h.mock.count("a") != 2 {
		t.Fatalf("provider calls=%d, want two distinct response contracts", h.mock.count("a"))
	}
}

func TestResponseCacheCommitmentBudgetCharges(t *testing.T) {
	for _, card := range []string{"none", "markup", "explicit"} {
		t.Run(card, func(t *testing.T) {
			h, _ := cacheHarness(t)
			h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
				for id, p := range s.Providers {
					p.Commitment = &routing.Commitment{Amount: "300", Period: "day"}
					s.Providers[id] = p
				}
				route := s.Routes[routeSlug]
				route.Cache.CountHitsAgainst = []string{"requests", "budgets"}
				s.Routes[routeSlug] = route
			})
			h.rt.inputs = priceInputs()
			if card != "none" {
				rev := usage.RoutingCardRevision{ID: uuid.NewString(), EffectiveAt: time.Now().Add(-time.Hour), Markup: "2"}
				if card == "explicit" {
					rev.Prices = []usage.RoutingPrice{{Price: usage.Price{ProviderKind: "openai_compatible", Model: modelA, Operation: "generation", Currency: "USD", InputPerMillion: costText("100"), OutputPerMillion: costText("0")}}}
				}
				h.rt.inputs.RateCards = []usage.RoutingRateCard{{Revisions: []usage.RoutingCardRevision{rev}}}
			}
			for i, want := range []string{"miss", "hit"} {
				response, body := h.chat(fullKey, nil)
				if response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != want {
					t.Fatalf("status=%d cache=%s want=%s body=%v", response.StatusCode, response.Header.Get("X-OLP-Cache"), want, body)
				}
				h.sink.nth(t, i+1)
			}
			wantCharge := "0"
			if card == "explicit" {
				wantCharge = "0.0002"
			}
			evidence := h.sink.nth(t, 2).Attempts[0].Cache
			if evidence == nil || evidence.BudgetCost != "0" || evidence.BudgetCharge != wantCharge {
				t.Fatalf("committed cache charge=%+v, want cost=0 charge=%s", evidence, wantCharge)
			}
		})
	}
}

func TestSemanticCacheEmbeddingRetryDeadline(t *testing.T) {
	for _, fidelity := range []string{routing.FidelityStrict, routing.FidelityTransformed} {
		t.Run(fidelity, func(t *testing.T) {
			h, _ := cacheHarness(t)
			h.republish(func(s *routing.Snapshot, _, b routing.Provider) {
				b.Kind, b.ProfileID, b.ProfileRevision = "openai", "openai-embeddings", connectors.ProfileRevision
				b.Capabilities = []routing.Capability{{Model: modelB, Operation: "embeddings", Surface: "openai", Mode: "unary"}}
				s.Providers[b.ID] = b
				route := s.Routes[routeSlug]
				route.Targets = route.Targets[:1]
				route.Cache = &cache.Policy{Mode: "semantic", EmbeddingRoute: "cache-embedding", AllowUnsealedVectors: true}
				s.Routes[routeSlug] = route
				s.Routes["cache-embedding"] = routing.Route{
					ID: uuid.NewString(), Slug: "cache-embedding", RevisionID: uuid.NewString(), RoutingID: uuid.NewString(),
					Operations: []string{"embeddings"}, OverallTimeout: 100, MaxAttempts: 2,
					Fidelity: routing.RouteFidelity{Mode: fidelity},
					Behavior: routing.Behavior{Retry: routing.Retry{classRateLimit: {MaxRetries: 1, BaseBackoffMS: 1, MaxBackoffMS: 1, RespectRetryAfter: true}}},
					Targets:  []routing.Target{{ID: uuid.NewString(), ProviderID: b.ID, ProviderModel: modelB, Weight: 1, Timeout: 3000, RoutingID: uuid.NewString()}},
				}
			})
			h.mock.set("b", func(w http.ResponseWriter, r *http.Request) {
				if h.mock.count("b") == 1 {
					w.Header().Set("Retry-After", "1")
					status(http.StatusTooManyRequests, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)(w, r)
					return
				}
				status(http.StatusOK, `{"object":"list","model":"model-b","data":[{"object":"embedding","index":0,"embedding":[1,0]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`)(w, r)
			})
			started := time.Now()
			response, body := h.chat(fullKey, nil)
			elapsed := time.Since(started)
			if response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != "bypass" {
				t.Fatalf("embedding deadline did not bypass cache: status=%d cache=%s elapsed=%s body=%v", response.StatusCode, response.Header.Get("X-OLP-Cache"), elapsed, body)
			}
			if elapsed >= time.Second || h.mock.count("b") != 1 || h.mock.count("a") != 1 {
				t.Fatalf("retry exceeded embedding deadline: elapsed=%s embedding calls=%d generation calls=%d", elapsed, h.mock.count("b"), h.mock.count("a"))
			}
			for _, env := range h.sink.await(t, 2) {
				if env.Origin == usage.OriginCacheEmbedding && (env.Status != http.StatusTooManyRequests || len(env.Attempts) != 1 || env.Attempts[0].Class != classRateLimit) {
					t.Fatalf("embedding retried beyond its deadline: %+v", env)
				}
			}
		})
	}
}

func TestSemanticCacheEmbeddingRequestDebitOnAdmissionFailure(t *testing.T) {
	for _, fidelity := range []string{routing.FidelityStrict, routing.FidelityTransformed} {
		for _, tc := range []struct {
			name         string
			status       int
			body         string
			bodyLimit    *int64
			wantRequests int64
		}{
			{"success", http.StatusOK, `{"object":"list","model":"model-b","data":[{"object":"embedding","index":0,"embedding":[1,0]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`, nil, 1},
			{"invalid vector", http.StatusOK, `{"object":"list","model":"model-b","data":[{"object":"embedding","index":0,"embedding":[]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`, nil, 1},
			{"provider failure", http.StatusServiceUnavailable, `{"error":{"message":"unavailable","type":"server_error"}}`, nil, 1},
			{"rejected before dispatch", http.StatusOK, "", int64ptr(1), 0},
		} {
			t.Run(fidelity+"/"+tc.name, func(t *testing.T) {
				finished := make(chan struct{}, 1)
				h, _ := enableTestCache(t, newHarnessWrapped(t, Config{}, func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						next.ServeHTTP(w, r)
						finished <- struct{}{}
					})
				}))
				h.republish(func(s *routing.Snapshot, _, b routing.Provider) {
					b.Kind, b.ProfileID, b.ProfileRevision = "openai", "openai-embeddings", connectors.ProfileRevision
					b.Capabilities = []routing.Capability{{Model: modelB, Operation: "embeddings", Surface: "openai", Mode: "unary"}}
					s.Providers[b.ID] = b
					route := s.Routes[routeSlug]
					route.Targets = route.Targets[:1]
					route.Cache = &cache.Policy{Mode: "semantic", EmbeddingRoute: "cache-embedding", AllowUnsealedVectors: true}
					s.Routes[routeSlug] = route
					s.Routes["cache-embedding"] = routing.Route{
						ID: uuid.NewString(), Slug: "cache-embedding", RevisionID: uuid.NewString(), RoutingID: uuid.NewString(),
						Operations: []string{"embeddings"}, OverallTimeout: 5000, MaxAttempts: 1,
						Behavior: routing.Behavior{MaxBodyBytes: tc.bodyLimit},
						Fidelity: routing.RouteFidelity{Mode: fidelity},
						Targets:  []routing.Target{{ID: uuid.NewString(), ProviderID: b.ID, ProviderModel: modelB, Weight: 1, Timeout: 3000, RoutingID: uuid.NewString()}},
					}
				})
				window := admitThrough(t, h)
				h.rt.mu.Lock()
				key := h.rt.keys[fullKey]
				key.LookupID = "cache_embedding_admission"
				key.Policy.RequestsPerMinute, key.Policy.TokensPerMinute = perMinute(1), perMinute(100)
				h.rt.keys[fullKey] = key
				h.rt.mu.Unlock()
				h.mock.set("b", status(tc.status, tc.body))
				for request := range 2 {
					response, body := h.chat(fullKey, nil, `,"max_tokens":1000`)
					select {
					case <-finished:
					case <-time.After(time.Second):
						t.Fatal("request did not finish settling")
					}
					wantStatus := http.StatusBadRequest
					if request > 0 && tc.wantRequests > 0 {
						wantStatus = http.StatusTooManyRequests
					}
					if response.StatusCode != wantStatus {
						t.Fatalf("request %d: status=%d want=%d body=%v", request+1, response.StatusCode, wantStatus, body)
					}
					window.mu.Lock()
					requests := window.of("olp:test:{cache_embedding_admission}:rate").requests
					window.mu.Unlock()
					if requests != tc.wantRequests || h.mock.count("b") != int(tc.wantRequests) || h.mock.count("a") != 0 {
						t.Fatalf("request %d: debit=%d want=%d embedding calls=%d generation calls=%d", request+1, requests, tc.wantRequests, h.mock.count("b"), h.mock.count("a"))
					}
				}
			})
		}
	}
}

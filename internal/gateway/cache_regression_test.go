package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestResponseCacheStreamUsageIsolation(t *testing.T) {
	for _, fidelity := range []string{routing.FidelityStrict, routing.FidelityTransformed} {
		for _, firstUsage := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/first-usage=%t", fidelity, firstUsage), func(t *testing.T) {
				h, _ := enableTestCache(t, strictHarness(t, func(s *routing.Snapshot) {
					route := s.Routes[routeSlug]
					route.Fidelity.Mode = fidelity
					s.Routes[routeSlug] = route
				}))
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: "+`{"id":"c","object":"chat.completion.chunk","model":"model-a","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}]}`+"\n\n")
					io.WriteString(w, "data: "+`{"id":"c","object":"chat.completion.chunk","model":"model-a","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`+"\n\ndata: [DONE]\n\n")
				})
				for i, includeUsage := range []bool{firstUsage, !firstUsage, firstUsage, !firstUsage} {
					body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":%t}}`, routeSlug, includeUsage)
					response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(body), nil)
					raw, err := io.ReadAll(response.Body)
					response.Body.Close()
					wantCache := "miss"
					if i >= 2 {
						wantCache = "hit"
					}
					if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != wantCache {
						t.Fatalf("request %d: status=%d cache=%s want=%s error=%v body=%s", i, response.StatusCode, response.Header.Get("X-OLP-Cache"), wantCache, err, raw)
					}
					if strings.Contains(string(raw), `"usage":`) != includeUsage || !strings.Contains(string(raw), `"content":"hello"`) || !strings.HasSuffix(string(raw), "data: [DONE]\n\n") {
						t.Fatalf("include_usage=%t: incorrect stream: %s", includeUsage, raw)
					}
					h.sink.nth(t, i+1)
				}
				if h.mock.count("a") != 2 {
					t.Fatalf("provider calls=%d, want one for each usage setting", h.mock.count("a"))
				}
			})
		}
	}
}

func TestResponseCacheDelegatedRoutePurge(t *testing.T) {
	for _, scope := range []string{"key", "project", "route"} {
		t.Run(scope, func(t *testing.T) {
			h, _ := cacheHarness(t)
			h.republish(func(s *routing.Snapshot, _, b routing.Provider) {
				splitRoutes(s, b, routing.Behavior{Selectors: []routing.Selector{{ID: "delegate", Route: backupSlug}}})
				named := s.Routes[routeSlug]
				named.Cache = nil
				s.Routes[routeSlug] = named
				serving := s.Routes[backupSlug]
				serving.Cache.Scope = scope
				s.Routes[backupSlug] = serving
			})
			requests := 0
			request := func(model, wantCache string) {
				t.Helper()
				response, body := h.chat(fullKey, nil, `,"model":"`+model+`"`)
				if response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != wantCache || body["model"] != model {
					t.Fatalf("%s: status=%d cache=%s want=%s body=%v", model, response.StatusCode, response.Header.Get("X-OLP-Cache"), wantCache, body)
				}
				requests++
				h.sink.nth(t, requests)
			}
			for _, model := range []string{routeSlug, backupSlug} {
				request(model, "miss")
				request(model, "hit")
			}
			purge := func(model string) {
				t.Helper()
				if err := h.gateway.Cache.Purge(t.Context(), "route", h.rt.Release().Snapshot.Routes[model].ID); err != nil {
					t.Fatal(err)
				}
			}
			purge(routeSlug)
			request(backupSlug, "hit")
			request(routeSlug, "miss")
			request(routeSlug, "hit")
			purge(backupSlug)
			for _, model := range []string{routeSlug, backupSlug} {
				request(model, "miss")
				request(model, "hit")
			}
			if h.mock.count("b") != 5 {
				t.Fatalf("provider calls=%d, want five after both route purges", h.mock.count("b"))
			}
		})
	}
}

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

func TestResponseCacheDelegationPreservesNamedModel(t *testing.T) {
	h, _ := enableTestCache(t, strictHarness(t, strictEmbeddings))
	h.republish(func(s *routing.Snapshot, _, b routing.Provider) {
		route := s.Routes[routeSlug]
		route.Cache.Operations = []string{"embeddings"}
		s.Routes[routeSlug] = route
		splitRoutes(s, b, routing.Behavior{Selectors: []routing.Selector{{ID: "delegate", Route: backupSlug}}})
	})
	h.mock.set("b", status(http.StatusOK, `{"object":"list","model":"model-b","data":[{"object":"embedding","index":0,"embedding":[1,0]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`))
	for _, request := range []struct{ model, cache string }{
		{routeSlug, "miss"}, {backupSlug, "miss"}, {routeSlug, "hit"}, {backupSlug, "hit"},
	} {
		response := h.do(t.Context(), http.MethodPost, "/v1/embeddings", fullKey, []byte(`{"model":"`+request.model+`","input":"hi"}`), nil)
		var body map[string]any
		err := json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != request.cache || body["model"] != request.model {
			t.Fatalf("%s: status=%d cache=%s body=%v error=%v", request.model, response.StatusCode, response.Header.Get("X-OLP-Cache"), body, err)
		}
	}
	if h.mock.count("b") != 2 {
		t.Fatalf("provider calls=%d, want two named models", h.mock.count("b"))
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
					Retry:    routing.Retry{classRateLimit: {MaxRetries: 1, BaseBackoffMS: 1, MaxBackoffMS: 1, RespectRetryAfter: true}},
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

func TestSemanticCacheEmbeddingBodyLimit(t *testing.T) {
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
				limit := int64(64)
				s.Routes["cache-embedding"] = routing.Route{ID: uuid.NewString(), Slug: "cache-embedding", RevisionID: uuid.NewString(), RoutingID: uuid.NewString(), Operations: []string{"embeddings"}, OverallTimeout: 5000, MaxAttempts: 1, MaxBodyBytes: &limit, Fidelity: routing.RouteFidelity{Mode: fidelity}, Targets: []routing.Target{{ID: uuid.NewString(), ProviderID: b.ID, ProviderModel: modelB, Weight: 1, Timeout: 3000, RoutingID: uuid.NewString()}}}
			})
			h.mock.set("b", status(http.StatusOK, `{"object":"list","model":"model-b","data":[{"object":"embedding","index":0,"embedding":[1,0]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`))
			response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"`+strings.Repeat("x", 128)+`"}]}`), nil)
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != "bypass" || h.mock.count("b") != 0 {
				t.Fatalf("oversized embedding: status=%d cache=%s calls=%d body=%s", response.StatusCode, response.Header.Get("X-OLP-Cache"), h.mock.count("b"), body)
			}
			for _, env := range h.sink.await(t, 2) {
				if env.Origin == usage.OriginCacheEmbedding && (env.Status != http.StatusRequestEntityTooLarge || len(env.Attempts) != 0) {
					t.Fatalf("oversized embedding was dispatched or billed: %+v", env)
				}
			}
		})
	}
}

func TestResponseCachePreparedRetentionDefaults(t *testing.T) {
	h, memory := cacheHarness(t)
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		for id, p := range s.Providers {
			p.ParameterDefaults = map[string]json.RawMessage{"store": json.RawMessage(`true`)}
			s.Providers[id] = p
		}
	})
	for range 2 {
		response, body := h.chat(fullKey, nil)
		if response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != "bypass" {
			t.Fatalf("retained request: status=%d cache=%s body=%v", response.StatusCode, response.Header.Get("X-OLP-Cache"), body)
		}
	}
	memory.mu.Lock()
	commands := memory.commands
	memory.mu.Unlock()
	if commands != 0 || h.mock.count("a") != 2 {
		t.Fatalf("retained requests used cache: commands=%d provider calls=%d", commands, h.mock.count("a"))
	}
	for i, want := range []string{"miss", "hit"} {
		response, body := h.chat(fullKey, nil, `,"store":false`)
		if response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != want {
			t.Fatalf("stateless override: status=%d cache=%s want=%s body=%v", response.StatusCode, response.Header.Get("X-OLP-Cache"), want, body)
		}
		h.sink.nth(t, i+3)
	}
}

func TestResponseCacheExplicitChargeWithoutProviderCost(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(fmt.Sprintf("incomplete-price=%t", incomplete), func(t *testing.T) {
			h, _ := cacheHarness(t)
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
			if incomplete {
				h.rt.inputs.Prices = []usage.RoutingPrice{{Price: usage.Price{
					ProviderKind: "openai_compatible", Model: modelA, Operation: "generation", OutputPerMillion: costText("1"),
				}}}
			}
			for i, want := range []string{"miss", "hit"} {
				response, body := h.chat(fullKey, nil)
				if response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != want {
					t.Fatalf("status=%d cache=%s want=%s body=%v", response.StatusCode, response.Header.Get("X-OLP-Cache"), want, body)
				}
				h.sink.nth(t, i+1)
			}
			event := h.sink.nth(t, 2)
			if len(event.Attempts) != 1 || event.Attempts[0].Cache == nil {
				t.Fatalf("missing cache evidence: %+v", event.Attempts)
			}
			evidence := event.Attempts[0].Cache
			if evidence.BudgetCost != "0" || evidence.BudgetCharge != "0.0002" {
				t.Fatalf("unpriced provider lost explicit charge: %+v", evidence)
			}
		})
	}
}

func TestResponseCacheAdmissionFailureRefundsRequest(t *testing.T) {
	h, _ := cacheHarness(t)
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		route := s.Routes[routeSlug]
		route.Cache.CountHitsAgainst = []string{"requests", "tokens"}
		s.Routes[routeSlug] = route
	})
	if response, body := h.chat(fullKey, nil); response.StatusCode != http.StatusOK {
		t.Fatalf("warm cache: %d %v", response.StatusCode, body)
	}
	h.sink.nth(t, 1)
	window := admitThrough(t, h)
	h.rt.mu.Lock()
	key := h.rt.keys[fullKey]
	key.LookupID = "cache_refund"
	key.Policy.RequestsPerMinute, key.Policy.TokensPerMinute = perMinute(1), perMinute(1)
	h.rt.keys[fullKey] = key
	h.rt.mu.Unlock()
	awaitRequests := func(want int64) {
		t.Helper()
		var got int64
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
			window.mu.Lock()
			got = 0
			for _, counter := range window.counters {
				got += counter.requests
			}
			window.mu.Unlock()
			if got == want {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("request debit=%d, want %d", got, want)
	}
	for range 2 {
		response, body := h.chat(fullKey, nil)
		if response.StatusCode != http.StatusBadRequest || strings.Contains(response.Header.Get("X-OLP-Cache"), "hit") {
			t.Fatalf("inadmissible replay: status=%d cache=%s body=%v", response.StatusCode, response.Header.Get("X-OLP-Cache"), body)
		}
		awaitRequests(0)
	}
	h.rt.mu.Lock()
	key.Policy.TokensPerMinute = perMinute(100)
	h.rt.keys[fullKey] = key
	h.rt.mu.Unlock()
	if response, body := h.chat(fullKey, nil); response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != "hit" {
		t.Fatalf("admitted replay: status=%d cache=%s body=%v", response.StatusCode, response.Header.Get("X-OLP-Cache"), body)
	}
	awaitRequests(1)
	if h.mock.count("a") != 1 {
		t.Fatal("rejected or replayed cache hits reached the provider")
	}
}

func TestResponseCacheZeroTokenHits(t *testing.T) {
	for _, fidelity := range []string{routing.FidelityStrict, routing.FidelityTransformed} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", fidelity, stream), func(t *testing.T) {
				h, _ := enableTestCache(t, strictHarness(t, func(s *routing.Snapshot) {
					route := s.Routes[routeSlug]
					route.Fidelity.Mode = fidelity
					s.Routes[routeSlug] = route
				}))
				h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
					route := s.Routes[routeSlug]
					route.Cache.CountHitsAgainst = []string{"requests", "tokens"}
					s.Routes[routeSlug] = route
				})
				reply := `{"id":"c","object":"chat.completion","model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`
				if stream {
					reply = "data: " + `{"id":"c","object":"chat.completion.chunk","model":"model-a","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}` + "\n\ndata: [DONE]\n\n"
				}
				h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
					} else {
						w.Header().Set("Content-Type", "application/json")
					}
					io.WriteString(w, reply)
				})
				body := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":%t}`, routeSlug, stream))
				request := func(wantStatus int, wantCache string) {
					t.Helper()
					response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, body, nil)
					raw, err := io.ReadAll(response.Body)
					response.Body.Close()
					if err != nil || response.StatusCode != wantStatus || response.Header.Get("X-OLP-Cache") != wantCache {
						t.Fatalf("status=%d cache=%s error=%v body=%s", response.StatusCode, response.Header.Get("X-OLP-Cache"), err, raw)
					}
				}
				request(http.StatusOK, "miss")
				h.sink.nth(t, 1)
				window := admitThrough(t, h)
				h.rt.mu.Lock()
				key := h.rt.keys[fullKey]
				key.LookupID = "cache_zero_tokens"
				key.Policy.RequestsPerMinute, key.Policy.TokensPerMinute = perMinute(2), perMinute(1)
				h.rt.keys[fullKey] = key
				h.rt.mu.Unlock()
				for i := range 2 {
					request(http.StatusOK, "hit")
					fact := h.sink.nth(t, i+2).Attempts[0]
					if fact.Usage == nil || fact.Usage.InputTokens != 0 || fact.Usage.OutputTokens != 0 || !fact.UsageComplete {
						t.Fatalf("zero-token replay lost usage: %+v", fact)
					}
				}
				request(http.StatusTooManyRequests, "")
				window.mu.Lock()
				defer window.mu.Unlock()
				var requests, tokens int64
				for _, counter := range window.counters {
					requests += counter.requests
					tokens += counter.tokens
				}
				if requests != 2 || tokens != 0 || h.mock.count("a") != 1 {
					t.Fatalf("requests=%d tokens=%d upstream calls=%d", requests, tokens, h.mock.count("a"))
				}
			})
		}
	}
}

func TestResponseCacheReportedTotalCountsAgainstTokenLimit(t *testing.T) {
	h, _ := cacheHarness(t)
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		route := s.Routes[routeSlug]
		route.Cache.CountHitsAgainst = []string{"requests", "tokens"}
		s.Routes[routeSlug] = route
	})
	h.mock.set("a", status(http.StatusOK, `{"id":"c","object":"chat.completion","model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":3}}`))
	if response, body := h.chat(fullKey, nil); response.StatusCode != http.StatusOK {
		t.Fatalf("warm cache: %d %v", response.StatusCode, body)
	}
	h.sink.nth(t, 1)
	window := admitThrough(t, h)
	h.rt.mu.Lock()
	key := h.rt.keys[fullKey]
	key.LookupID = "cache_reported_total"
	key.Policy.TokensPerMinute = perMinute(2)
	h.rt.keys[fullKey] = key
	h.rt.mu.Unlock()
	if response, body := h.chat(fullKey, nil); response.StatusCode != http.StatusBadRequest || errorCode(t, body) != "request_exceeds_token_limit" {
		t.Fatalf("reported total did not enforce the token limit: %d %v", response.StatusCode, body)
	}
	h.rt.mu.Lock()
	key.Policy.TokensPerMinute = perMinute(10)
	h.rt.keys[fullKey] = key
	h.rt.mu.Unlock()
	if response, body := h.chat(fullKey, nil); response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != "hit" {
		t.Fatalf("admitted replay: status=%d cache=%s body=%v", response.StatusCode, response.Header.Get("X-OLP-Cache"), body)
	}
	if window.lastReserved() != 3 || h.mock.count("a") != 1 {
		t.Fatalf("reserved=%d upstream calls=%d", window.lastReserved(), h.mock.count("a"))
	}
}

func TestResponseCacheUnmeteredModeration(t *testing.T) {
	for _, fidelity := range []string{routing.FidelityStrict, routing.FidelityTransformed} {
		t.Run(fidelity, func(t *testing.T) {
			h, _ := enableTestCache(t, strictHarness(t, func(s *routing.Snapshot) {
				for id, provider := range s.Providers {
					provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "moderation", Surface: "openai", Mode: "unary"}}
					s.Providers[id] = provider
				}
				route := s.Routes[routeSlug]
				route.Operations, route.Fidelity.Mode = []string{"moderation"}, fidelity
				s.Routes[routeSlug] = route
			}))
			h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
				route := s.Routes[routeSlug]
				route.Cache.Operations = []string{"moderation"}
				route.Cache.CountHitsAgainst = []string{"requests", "tokens"}
				s.Routes[routeSlug] = route
			})
			h.mock.set("a", status(http.StatusOK, `{"id":"mod_1","model":"model-a","results":[{"flagged":false,"categories":{"violence":false},"category_scores":{"violence":0.01}}]}`))
			var first string
			for i, want := range []string{"miss", "hit", "hit"} {
				response := h.do(t.Context(), http.MethodPost, "/v1/moderations", fullKey, []byte(`{"model":"team-chat","input":"hello"}`), nil)
				raw, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != want {
					t.Fatalf("request %d: status=%d cache=%s want=%s error=%v body=%s", i, response.StatusCode, response.Header.Get("X-OLP-Cache"), want, err, raw)
				}
				if i == 0 {
					first = string(raw)
				} else if string(raw) != first {
					t.Fatalf("replay changed moderation response: %s / %s", first, raw)
				}
				env := h.sink.nth(t, i+1)
				fact := env.Attempts[0]
				if fact.Usage != nil || fact.UsageObserved || !fact.UsageComplete || fact.BillingUncertain {
					t.Fatalf("unmetered moderation has incorrect accounting: %+v", fact)
				}
				if _, err := usage.Validate(accountingEvent(env)); err != nil {
					t.Fatalf("invalid moderation accounting: %v", err)
				}
				if i == 0 {
					admitThrough(t, h)
					h.rt.mu.Lock()
					key := h.rt.keys[fullKey]
					key.LookupID = "cache_moderation"
					key.Policy.TokensPerMinute = perMinute(1)
					h.rt.keys[fullKey] = key
					h.rt.mu.Unlock()
				}
			}
			if h.mock.count("a") != 1 {
				t.Fatalf("provider calls=%d, want one", h.mock.count("a"))
			}
		})
	}
}

func TestResponseCacheUnmeteredGenerationStaysMiss(t *testing.T) {
	for _, fidelity := range []string{routing.FidelityStrict, routing.FidelityTransformed} {
		t.Run(fidelity, func(t *testing.T) {
			h, _ := enableTestCache(t, strictHarness(t, func(s *routing.Snapshot) {
				route := s.Routes[routeSlug]
				route.Fidelity.Mode = fidelity
				s.Routes[routeSlug] = route
			}))
			h.mock.set("a", evidenceUnmetered(modelA))
			for i := range 2 {
				response, body := h.chat(fullKey, nil)
				if response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != "miss" {
					t.Fatalf("incomplete usage was cached: status=%d cache=%s body=%v", response.StatusCode, response.Header.Get("X-OLP-Cache"), body)
				}
				fact := h.sink.nth(t, i+1).Attempts[0]
				if fact.UsageComplete || !fact.BillingUncertain {
					t.Fatalf("incomplete usage was treated as complete: %+v", fact)
				}
			}
			if h.mock.count("a") != 2 {
				t.Fatalf("provider calls=%d, want two", h.mock.count("a"))
			}
		})
	}
}

func TestResponseCacheSharingScopesRemainInsideProject(t *testing.T) {
	for _, scope := range []string{"key", "project", "route"} {
		t.Run(scope, func(t *testing.T) {
			h, _ := cacheHarness(t)
			project, otherProject := uuid.NewString(), uuid.NewString()
			h.republish(func(snapshot *routing.Snapshot, _, _ routing.Provider) {
				route := snapshot.Routes[routeSlug]
				route.ProjectID, route.Cache.Scope = &project, scope
				snapshot.Routes[routeSlug] = route
			})
			h.rt.mu.Lock()
			for _, secret := range []string{fullKey, otherKey} {
				key := h.rt.keys[secret]
				key.ProjectID = &project
				key.Policy.AllowedRoutes = []string{routeSlug}
				h.rt.keys[secret] = key
			}
			h.rt.mu.Unlock()
			request := func(secret string) (int, string) {
				response := h.do(t.Context(), "POST", "/v1/chat/completions", secret, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"hello"}]}`), nil)
				_, _ = io.Copy(io.Discard, response.Body)
				response.Body.Close()
				return response.StatusCode, response.Header.Get("X-OLP-Cache")
			}
			if status, result := request(fullKey); status != 200 || result != "miss" {
				t.Fatal(status, result)
			}
			want := "hit"
			if scope == "key" {
				want = "miss"
			}
			if status, result := request(otherKey); status != 200 || result != want {
				t.Fatal(status, result, "want", want)
			}
			h.rt.mu.Lock()
			key := h.rt.keys[otherKey]
			key.ProjectID = &otherProject
			h.rt.keys[otherKey] = key
			h.rt.mu.Unlock()
			if status, result := request(otherKey); status != 403 || strings.Contains(result, "hit") {
				t.Fatal("shared cache crossed a project boundary", status, result)
			}
		})
	}
}

func TestResponseCacheTransformedEmbeddingEncoding(t *testing.T) {
	for _, tc := range []struct{ name, kind, reply string }{
		{"gemini", "gemini", `{"embedding":{"values":[1,0.5]},"usageMetadata":{"promptTokenCount":1}}`},
		{"vertex", "vertex_ai", `{"predictions":[{"embeddings":{"values":[1,0.5],"statistics":{"token_count":1}}}]}`},
		{"bedrock", "bedrock", `{"embedding":[1,0.5],"inputTextTokenCount":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := cacheHarness(t)
			if tc.kind == "bedrock" {
				useBedrockProvider(h)
			}
			h.republish(func(snapshot *routing.Snapshot, _, _ routing.Provider) {
				for id, provider := range snapshot.Providers {
					provider.Kind = tc.kind
					if tc.kind == "vertex_ai" {
						provider.CloudRegion = "us-central1"
						provider.Endpoint = "https://us-central1-aiplatform.googleapis.com/" + provider.Name + "/v1"
					}
					model := provider.Capabilities[0].Model
					if tc.kind == "bedrock" {
						model = "amazon.titan-embed-text-v2:0"
					}
					provider.Capabilities = []routing.Capability{{Model: model, Operation: "embeddings", Surface: "openai", Mode: "unary"}}
					snapshot.Providers[id] = provider
				}
				route := snapshot.Routes[routeSlug]
				route.Operations = []string{"embeddings"}
				if tc.kind == "bedrock" {
					for i := range route.Targets {
						route.Targets[i].ProviderModel = "amazon.titan-embed-text-v2:0"
					}
				}
				snapshot.Routes[routeSlug] = route
			})
			if tc.kind == "vertex_ai" {
				// Vertex authenticates only its Google host. Serve that host
				// with an in-memory transport so this stays a local fixture.
				h.gateway.client = &http.Client{Transport: pinnedTransportFunc(func(r *http.Request) (*http.Response, error) {
					w := httptest.NewRecorder()
					h.mock.ServeHTTP(w, r)
					return w.Result(), nil
				})}
			}
			h.mock.set("a", status(http.StatusOK, tc.reply))
			for _, request := range []struct{ format, want string }{
				{"float", "miss"}, {"base64", "miss"}, {"float", "hit"}, {"base64", "hit"}, {"", "hit"},
			} {
				body := `{"model":"team-chat","input":"hello"`
				if request.format != "" {
					body += `,"encoding_format":"` + request.format + `"`
				}
				response := h.do(t.Context(), http.MethodPost, "/v1/embeddings", fullKey, []byte(body+`}`), nil)
				raw, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if response.StatusCode != http.StatusOK || response.Header.Get("X-OLP-Cache") != request.want {
					t.Fatalf("encoding=%s: status=%d cache=%s body=%s", request.format, response.StatusCode, response.Header.Get("X-OLP-Cache"), raw)
				}
				var decoded struct {
					Data []struct{ Embedding any } `json:"data"`
				}
				if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Data) != 1 {
					t.Fatalf("invalid embedding response: %s (%v)", raw, err)
				}
				if request.format == "base64" {
					if decoded.Data[0].Embedding != "AACAPwAAAD8=" {
						t.Fatalf("base64 embedding=%v", decoded.Data[0].Embedding)
					}
				} else if embedding, ok := decoded.Data[0].Embedding.([]any); !ok || len(embedding) != 2 || embedding[0] != float64(1) || embedding[1] != 0.5 {
					t.Fatalf("float embedding=%v", decoded.Data[0].Embedding)
				}
			}
			if h.mock.count("a") != 2 || h.mock.count("b") != 0 {
				t.Fatalf("provider calls=%d/%d", h.mock.count("a"), h.mock.count("b"))
			}
		})
	}
}

func TestResponseCacheGuardrailsAndRevision(t *testing.T) {
	h, _ := cacheHarness(t)
	installTestGuardrail(t, h, guardrails.Input, guardrails.Block, "builtin.keywords", `{"words":["blocked"]}`)
	request := func() (int, string) {
		r := h.do(t.Context(), "POST", "/v1/chat/completions", fullKey, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"hello"}]}`), nil)
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		return r.StatusCode, r.Header.Get("X-OLP-Cache")
	}
	if _, result := request(); result != "miss" {
		t.Fatal(result)
	}
	if _, result := request(); result != "hit" {
		t.Fatal(result)
	}
	registry := h.rt.release.Snapshot.Guardrails
	definition := registry.Definitions["revision"]
	definition.ID = "new-revision"
	registry.Definitions = map[string]guardrails.Revision{definition.ID: definition}
	policy := registry.Policies["policy-revision"]
	policy.Entries[0].RevisionID = definition.ID
	registry.Policies[policy.ID] = policy
	if err := h.rt.release.Snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	if _, result := request(); result != "miss" {
		t.Fatal("old guardrail revision was served", result)
	}
	registry = h.rt.release.Snapshot.Guardrails
	definition.Configuration = json.RawMessage(`{"words":["hello"]}`)
	registry.Definitions[definition.ID] = definition
	if err := h.rt.release.Snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	calls := h.mock.count("a")
	if status, result := request(); status != 400 || result == "hit" || h.mock.count("a") != calls {
		t.Fatal("blocked input reached the cache or provider", status, result)
	}
}

func TestResponseCacheParallelGuardRunsBeforeLookup(t *testing.T) {
	h, memory := cacheHarness(t)
	installTestGuardrail(t, h, guardrails.InputParallel, guardrails.Block, "builtin.keywords", `{"words":["blocked"]}`)
	response := h.do(t.Context(), "POST", "/v1/chat/completions", fullKey, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"blocked"}]}`), nil)
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	memory.mu.Lock()
	commands := memory.commands
	memory.mu.Unlock()
	if response.StatusCode != 400 || commands != 0 || h.mock.count("a")+h.mock.count("b") != 0 {
		t.Fatalf("blocked request read the cache or dispatched: status=%d commands=%d", response.StatusCode, commands)
	}
}

func TestResponseCacheExcludesPartialStreamsAndBoundedEntries(t *testing.T) {
	for _, mode := range []string{"partial", "oversize", "zero-ttl"} {
		t.Run(mode, func(t *testing.T) {
			h, _ := cacheHarness(t)
			stream := mode == "partial"
			if stream {
				h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"id\":\"partial\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"incomplete\"},\"finish_reason\":null}]}\n\n")
				})
			}
			if mode == "oversize" {
				h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
					route := s.Routes[routeSlug]
					route.Cache.MaxEntryBytes = 64
					s.Routes[routeSlug] = route
				})
			}
			headers := map[string]string{}
			if mode == "zero-ttl" {
				headers["X-OLP-Cache"] = "ttl=0"
			}
			for range 2 {
				r := h.do(t.Context(), "POST", "/v1/chat/completions", fullKey, []byte(fmt.Sprintf(`{"model":"team-chat","messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream)), headers)
				_, _ = io.Copy(io.Discard, r.Body)
				r.Body.Close()
				if strings.Contains(r.Header.Get("X-OLP-Cache"), "hit") {
					t.Fatal("excluded response was cached")
				}
			}
			if h.mock.count("a") < 2 {
				t.Fatal("excluded response bypassed the provider")
			}
		})
	}
}

func TestPromptCacheMultiTurnFixture(t *testing.T) {
	for _, mode := range []string{"disabled", "automatic", "strict"} {
		t.Run(mode, func(t *testing.T) {
			h := anthropicTransformedHarness(t)
			if mode == "strict" {
				h = anthropicStrictHarness(t)
			}
			if mode == "automatic" {
				h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
					route := s.Routes[routeSlug]
					route.PromptCache = &cache.PromptPolicy{Positions: []string{"system"}, TTL: "5m"}
					s.Routes[routeSlug] = route
				})
			}
			warm := false
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				marked := strings.Contains(string(body), `"cache_control"`)
				read := 0
				if marked && warm {
					read = 100
				}
				warm = marked
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"id":"msg","type":"message","role":"assistant","model":"model-a","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1,"cache_read_input_tokens":%d}}`, read)
			})
			for turn := range 2 {
				messages := `[{"role":"user","content":"hello"}]`
				if turn == 1 {
					messages = `[{"role":"user","content":"hello"},{"role":"assistant","content":"hello"},{"role":"user","content":"again"}]`
				}
				body := []byte(`{"model":"team-chat","max_tokens":16,"system":"shared system prefix","messages":` + messages + `}`)
				response := h.do(t.Context(), "POST", messagesPath, fullKey, body, map[string]string{"Anthropic-Version": "2023-06-01"})
				body, _ = io.ReadAll(response.Body)
				response.Body.Close()
				if response.StatusCode != 200 {
					t.Fatalf("%d %s", response.StatusCode, body)
				}
			}
			u := h.sink.nth(t, 2).Usage
			var reads int64
			if u != nil && u.CachedInputTokens != nil {
				reads = *u.CachedInputTokens
			}
			if mode == "automatic" && reads != 100 || mode != "automatic" && reads != 0 {
				t.Fatalf("mode=%s cached input tokens=%d", mode, reads)
			}
		})
	}
}

func TestSemanticInputRetainsNonQuerySettings(t *testing.T) {
	context := func(ttl, text string) string {
		doc, err := oif.ParseJSON([]byte(fmt.Sprintf(`{"messages":[{"role":"system","content":"fixed"},{"role":"user","content":[{"type":"text","text":%q,"cache_control":{"ttl":%q}}]}]}`, text, ttl)), oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		canonical, query, ok := semanticInput(doc, false)
		if !ok || query != text {
			t.Fatal("single-turn text with a system prefix was not eligible")
		}
		return string(canonical)
	}
	if context("5m", "hello") != context("5m", "greetings") {
		t.Fatal("query text remained in the exact semantic bucket")
	}
	if context("5m", "hello") == context("1h", "hello") {
		t.Fatal("non-query block settings were removed from the bucket")
	}
}

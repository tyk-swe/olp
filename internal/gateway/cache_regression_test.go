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

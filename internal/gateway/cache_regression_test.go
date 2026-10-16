package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/routing"
)

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

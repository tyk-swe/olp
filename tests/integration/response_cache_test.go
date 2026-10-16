//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/export"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestResponseCacheStreamCapture(t *testing.T) {
	h := newAccessHarness(t)
	installResponseCache(t, h)
	fixture := newOpenAIFixture(t, "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions") {
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			if strings.Contains(string(raw), `"stream":true`) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"id\":\"chat\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n", vendorModel)
				return
			}
		}
		fixture.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	owner, _, slug, secret := provisionOpenAIWith(t, h, upstream.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}},
		[]string{"generation"}, map[string]any{"cache": map[string]any{"mode": "exact"}})
	detail := h.want(owner, "GET", "/api/v1/observability/capture", nil, nil, 200)
	h.want(owner, "PATCH", "/api/v1/observability/capture", map[string]any{"enabled": true}, etagHeader(detail), 200)
	receiver, server := newExportReceiver(t, "", 204)
	sink := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "cached-stream-capture", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
	}, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/observability/capture-policies", map[string]any{
		"sink": sink["export_sink_id"], "sample_ratio": "1", "include": []string{"output", "tool_calls"},
		"max_bytes": 1048576, "route_slug": slug,
	}, idem(uuid.NewString()), 201)
	manager := &export.Manager{Pool: h.Pool, Keys: h.Ring2(), Installation: h.Server.Installation, Sender: export.NewSender(loopbackPolicy()), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h.Gateway.Capture = manager
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { manager.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.After(5 * time.Second)
	for {
		if collector := manager.Resolve("", slug, "", "", uuid.NewString()); collector != nil {
			collector.Close()
			break
		}
		select {
		case <-deadline:
			t.Fatal("capture policy was not loaded")
		case <-time.After(10 * time.Millisecond):
		}
	}
	for _, want := range []string{"miss", "hit"} {
		status, raw, headers := h.gatewayRaw(http.MethodPost, "/v1/chat/completions", secret,
			strings.NewReader(`{"model":"`+slug+`","messages":[{"role":"user","content":"capture cached stream"}],"stream":true}`), map[string]string{"Content-Type": "application/json"})
		if status != http.StatusOK || headers.Get("X-OLP-Cache") != want || !strings.Contains(string(raw), "lookup") {
			t.Fatalf("stream %s: status=%d cache=%s body=%s", want, status, headers.Get("X-OLP-Cache"), raw)
		}
	}
	for _, record := range awaitEnvelope(t, receiver, 2) {
		data, _ := record["data"].(map[string]any)
		tools, _ := data["tool_calls"].([]any)
		if data["output"] != "OK" || len(tools) != 1 {
			t.Fatalf("stream capture lost delivered output or tools: %v", data)
		}
	}
	var captured int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.requests WHERE payload_captured AND cache_result IN ('miss','hit')`).Scan(&captured); err != nil || captured != 2 {
		t.Fatalf("captured requests=%d, want 2: %v", captured, err)
	}
}

func installResponseCache(t *testing.T, h *accessHarness) {
	t.Helper()
	c := client(t, required(t, "OLP_TEST_CACHE_VALKEY_URL"), time.Second)
	h.Gateway.Cache = &cache.Store{Client: c, Keys: h.Server.Keys, Auth: h.Server.Auth, Installation: h.Server.Installation}
	h.Gateway.Sink = &gateway.PersistingSink{Persist: func(ctx context.Context, e *usage.Event, payload []byte) error {
		_, err := usage.PersistEvent(ctx, h.Pool, e, payload)
		if err != nil {
			t.Errorf("persist cache event: %v", err)
		}
		return err
	}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestResponseCachePublicLifecycleAndAccounting(t *testing.T) {
	h := newAccessHarness(t)
	installResponseCache(t, h)
	fixture := newOpenAIFixture(t, "")
	owner, _, slug, key := provisionOpenAIWith(t, h, fixture.URL, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}}, []string{"generation"}, map[string]any{"cache": map[string]any{"mode": "exact"}})
	request := func(secret string, stream bool, headers map[string]string) (string, string) {
		t.Helper()
		raw := `{"model":"` + slug + `","messages":[{"role":"user","content":"private query"}],"stream":false}`
		if stream {
			raw = strings.Replace(raw, `"stream":false`, `"stream":true`, 1)
		}
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = "application/json"
		status, body, resp := h.gatewayRaw("POST", "/v1/chat/completions", secret, strings.NewReader(raw), headers)
		if status != 200 {
			t.Fatalf("cache request %d %s", status, body)
		}
		return resp.Get("X-OLP-Cache"), string(body)
	}
	for _, stream := range []bool{false, true} {
		a, first := request(key, stream, nil)
		b, second := request(key, stream, nil)
		if a != "miss" || b != "hit" || first != second {
			t.Fatalf("replay streaming=%v %s/%s", stream, a, b)
		}
	}
	other := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Other cache key", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem(uuid.NewString()), 201)
	h.refresh()
	if result, _ := request(other["secret"].(string), false, nil); result != "miss" {
		t.Fatal("response crossed key boundary")
	}
	if result, _ := request(key, false, map[string]string{"X-OLP-Cache": "no-store"}); result != "bypass" {
		t.Fatal(result)
	}
	routes := h.want(owner, "GET", "/api/v1/routes", nil, nil, 200)["items"].([]any)
	routeID := ""
	for _, item := range routes {
		r := item.(map[string]any)
		if r["slug"] == slug {
			routeID = r["id"].(string)
		}
	}
	h.want(owner, "POST", "/api/v1/cache/purge", map[string]any{"route_id": strings.ToUpper(routeID)}, nil, 200)
	if result, _ := request(key, false, nil); result != "miss" {
		t.Fatal("purge did not invalidate")
	}
	var cost, charge string
	var hits, billable int
	if err := h.Pool.QueryRow(context.Background(), `SELECT count(*),COALESCE(sum(estimated_cost),0)::text,COALESCE(sum(charge),0)::text,count(*) FILTER (WHERE estimated_cost <> 0 OR charge <> 0) FROM olp.attempt_usage_facts WHERE response_cache`).Scan(&hits, &cost, &charge, &billable); err != nil {
		t.Fatal(err)
	}
	if hits < 2 || billable != 0 || cost != "0" && cost != "0.000000000000" || charge != "0" && charge != "0.000000000000" {
		t.Fatalf("cache charged provider spend: hits=%d cost=%s charge=%s", hits, cost, charge)
	}
	stats := h.want(owner, "GET", "/api/v1/cache/statistics?route="+slug, nil, nil, 200)
	if stats["hits"].(float64) < 2 {
		t.Fatalf("missing hit report: %+v", stats)
	}
	doc := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	found := false
	for _, r := range doc["routes"].([]any) {
		v := r.(map[string]any)
		if v["slug"] == slug {
			found = v["cache"] != nil
		}
	}
	if !found {
		t.Fatal("cache policy missing from configuration export")
	}
}

func TestResponseCachePurgeCanonicalScopeIDs(t *testing.T) {
	h := newAccessHarness(t)
	installResponseCache(t, h)
	fixture := newOpenAIFixture(t, "")
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"}, nil)
	project := createProject(h, owner, "Purge namespace")
	var route string
	if err := h.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.routes WHERE slug=$1", slug).Scan(&route); err != nil {
		t.Fatal(err)
	}
	space := cache.Space{Project: project, Route: route, Scope: "key", Fingerprint: "revision"}
	for kind, id := range map[string]string{"project": project, "route": route} {
		for _, spelling := range []string{strings.ToUpper(id), strings.ReplaceAll(id, "-", ""), "urn:uuid:" + id} {
			t.Run(kind+"/"+spelling, func(t *testing.T) {
				store := h.Gateway.Cache
				ticket, err := store.Begin(t.Context(), space, []byte("request"))
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Put(t.Context(), ticket, cache.Entry{Body: []byte("cached response")}, cache.Policy{Mode: "exact"}, cache.Controls{}, nil); err != nil {
					t.Fatal(err)
				}
				h.want(owner, "POST", "/api/v1/cache/purge", map[string]any{kind + "_id": spelling}, nil, 200)
				next, entry, err := store.Lookup(t.Context(), space, []byte("request"), cache.Controls{})
				if err != nil || entry != nil || next.Key == ticket.Key {
					t.Fatalf("purge left the canonical namespace reachable: entry=%v error=%v", entry, err)
				}
			})
		}
	}
}

func TestResponseCacheHitsRespectRequestsAndExemptTokens(t *testing.T) {
	f := glSeedIn(t, "cache-admission", glPrice{})
	installResponseCache(t, f.h)
	draft := f.h.want(f.owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": routeSlug, "overall_timeout_ms": 10000, "max_attempts": 1,
		"fidelity": map[string]any{"mode": "transformed"}, "cache": map[string]any{"mode": "exact"},
		"targets": []any{map[string]any{"provider_id": f.provider, "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 8000}},
	}, idem(uuid.NewString()), 201)
	f.h.want(f.owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	id, key := f.key("cache-limited", map[string]any{"requests_per_minute": 3})
	if status, code, headers := f.chat(key); status != 200 || headers.Get("X-OLP-Cache") != "miss" {
		t.Fatalf("warm %d %s", status, code)
	}
	current := f.h.want(f.owner, "GET", "/api/v1/api-keys/"+id, nil, nil, 200)
	f.h.want(f.owner, "PATCH", "/api/v1/api-keys/"+id, map[string]any{"tokens_per_minute": 1}, etagHeader(current), 200)
	f.h.refresh()
	for range 2 {
		status, code, headers := f.chat(key)
		if status != 200 || headers.Get("X-OLP-Cache") != "hit" {
			t.Fatalf("token-exempt hit %d %s %v", status, code, headers)
		}
	}
	if status, code, _ := f.chat(key); status != 429 || code != "rate_limit_exceeded" {
		t.Fatalf("request quota bypassed: %d %s", status, code)
	}
}

func TestResponseCacheHitsConsumeAggregateBudget(t *testing.T) {
	f := glSeedIn(t, "cache-budget", glPrice{input: "1000000", output: "1000000"})
	installResponseCache(t, f.h)
	draft := f.h.want(f.owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": routeSlug, "overall_timeout_ms": 10000, "max_attempts": 1,
		"fidelity": map[string]any{"mode": "transformed"},
		"cache":    map[string]any{"mode": "exact", "count_hits_against": []string{"requests", "budgets"}},
		"targets":  []any{map[string]any{"provider_id": f.provider, "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 8000}},
	}, idem(uuid.NewString()), 201)
	f.h.want(f.owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	_, key := f.key("cache-budget", nil)
	await := endUserAccounting(t, f.h, f.limiter)
	if status, code, headers := f.chat(key); status != 200 || headers.Get("X-OLP-Cache") != "miss" {
		t.Fatalf("warm: %d %s %v", status, code, headers)
	}
	await()
	path := "/api/v1/budgets/installation"
	before := f.h.want(f.owner, "GET", path, nil, nil, 200)
	f.h.want(f.owner, "PUT", path, map[string]any{"policy": map[string]any{"daily_cost_limit": "20"}}, etagHeader(before), 200)
	f.h.refresh()
	// The first observation installs this new account's historical balance.
	if status, code, _ := f.chat(key); status != 503 {
		t.Fatalf("uninitialized budget: %d %s", status, code)
	}
	await()
	if status, code, headers := f.chat(key); status != 200 || headers.Get("X-OLP-Cache") != "hit" {
		t.Fatalf("counted cache hit: %d %s %v", status, code, headers)
	}
	await()
	usage := f.h.want(f.owner, "GET", path, nil, nil, 200)["usage"].(map[string]any)
	if got := usage["daily"].(map[string]any)["accrued"]; got != "20.000000000000" {
		t.Fatalf("cache debit missing from aggregate usage: %v", usage)
	}
	if status, code, _ := f.chat(key); status != 429 || code != "budget_exhausted" {
		t.Fatalf("cache hit bypassed exhausted aggregate budget: %d %s", status, code)
	}
	await()
}

func TestResponseCacheSemanticPublicationAndChildAccounting(t *testing.T) {
	h := newAccessHarness(t)
	installResponseCache(t, h)
	fixture := newOpenAIFixture(t, "")
	owner, provider, embeddingRoute, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "embeddings", "surface": "openai", "mode": "unary"}},
		[]string{"embeddings"}, nil)
	slug := "semantic-" + uuid.NewString()[:8]
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": slug, "operations": []string{"generation"}, "fidelity": map[string]any{"mode": "transformed"},
		"overall_timeout_ms": 10000, "max_attempts": 1,
		"cache":   map[string]any{"mode": "semantic", "embedding_route": embeddingRoute, "allow_unsealed_vectors": true},
		"targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
	}, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "semantic caller", "scopes": []string{"inference"}, "allowed_routes": []string{slug, embeddingRoute}}, idem(uuid.NewString()), 201)
	h.refresh()
	for i, text := range []string{"hello", "greetings"} {
		status, body, header := h.gatewayRaw("POST", "/v1/chat/completions", key["secret"].(string), strings.NewReader(`{"model":"`+slug+`","messages":[{"role":"user","content":"`+text+`"}]}`), map[string]string{"Content-Type": "application/json"})
		want := "miss"
		if i == 1 {
			want = "semantic-hit"
		}
		if status != 200 || header.Get("X-OLP-Cache") != want {
			t.Fatalf("semantic result %d: status=%d cache=%s body=%s", i, status, header.Get("X-OLP-Cache"), body)
		}
	}
	var children, acknowledgements int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.requests WHERE origin='cache_embedding' AND parent_request_id IS NOT NULL`).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.audit WHERE action='cache.semantic_acknowledge' AND outcome='success'`).Scan(&acknowledgements); err != nil {
		t.Fatal(err)
	}
	if children != 2 || acknowledgements != 1 {
		t.Fatalf("embedding children=%d acknowledgements=%d", children, acknowledgements)
	}
}

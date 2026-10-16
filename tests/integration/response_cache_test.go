//go:build integration

package integration_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/usage"
)

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
	h.want(owner, "POST", "/api/v1/cache/purge", map[string]any{"route_id": routeID}, nil, 200)
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

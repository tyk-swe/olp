//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/gateway"
)

func TestCallerPaidRoutesKeepUsageAndRateLimitsWithoutCostAdmission(t *testing.T) {
	h := newAccessHarness(t)
	upstream := newOpenAIFixture(t, "")
	owner, provider, slug, _ := provisionOpenAIWith(t, h, upstream.URL, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"}, map[string]any{"caller_cost_exempt": true, "budget": map[string]any{"daily_cost_limit": "1"}}, map[string]any{"credential_source": "caller"})
	h.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": time.Now().UTC().Format(time.RFC3339Nano), "prices": []any{map[string]any{"provider_kind": "azure_openai", "provider_id": provider["id"], "model": vendorModel, "operation": "generation", "currency": "USD", "input_per_million": "1000000", "output_per_million": "1000000"}}}, idem("caller-price"), 201)
	group := h.want(owner, "POST", "/api/v1/budget-groups", map[string]any{"name": "Caller group", "requests_per_minute": 2, "daily_cost_limit": "1"}, idem("caller-group"), 201)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Caller key", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "budget_group_id": group["id"], "daily_cost_limit": "1", "end_user_source": "header", "end_user_policy": map[string]any{"defaults": map[string]any{"daily_cost_limit": "1"}}, "route_limits": map[string]any{slug: map[string]any{"daily_cost_limit": "1"}}}, idem("caller-key"), 201)
	budget := h.want(owner, "GET", "/api/v1/budgets/installation", nil, nil, 200)
	h.want(owner, "PUT", "/api/v1/budgets/installation", map[string]any{"policy": map[string]any{"daily_cost_limit": "1"}}, etagHeader(budget), 200)
	c := limClient(t)
	limiter := limLimiter(t, c, limNamespace(t, c, "caller-paid"))
	h.Gateway.Admission = gateway.NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	await := endUserAccounting(t, h, limiter)
	glEventually(t, "caller price", func() bool { h.refresh(); return len(h.Runtime.RoutingInputs().Prices) > 0 })
	limSettleInMinute(t, c, 5*time.Second)
	for _, want := range []int{200, 200, 429} {
		body, _ := json.Marshal(map[string]any{"model": slug, "messages": []any{map[string]string{"role": "user", "content": "hello"}}, "max_tokens": 16})
		r, _ := http.NewRequest("POST", h.HTTP.URL+"/v1/chat/completions", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+key["secret"].(string))
		r.Header.Set("X-OLP-End-User", "customer")
		r.Header.Set("X-OLP-Provider-Credential", "request-only-key")
		resp, err := h.HTTP.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("inference %d want %d: %s", resp.StatusCode, want, raw)
		}
		await()
	}
	var cost string
	var exempt int
	if err := h.Pool.QueryRow(t.Context(), "SELECT sum(estimated_cost)::text,count(*) FILTER(WHERE budget_exempt) FROM olp.attempt_usage_facts WHERE api_key_id=$1", key["id"]).Scan(&cost, &exempt); err != nil {
		t.Fatal(err)
	}
	if cost != "4.000000000000" || exempt != 2 {
		t.Fatalf("priced caller evidence: %s/%d", cost, exempt)
	}
	detail := h.want(owner, "GET", "/api/v1/api-keys/"+key["id"].(string), nil, nil, 200)
	if detail["budget"].(map[string]any)["daily"].(map[string]any)["accrued"] != "0" {
		t.Fatalf("caller cost charged: %v", detail["budget"])
	}
	doc := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	if doc["routes"].([]any)[0].(map[string]any)["caller_cost_exempt"] != true {
		t.Fatal("exemption not exported")
	}
	if doc["providers"].([]any)[0].(map[string]any)["configuration"].(map[string]any)["credential_source"] != "caller" {
		t.Fatal("caller source not exported")
	}
	h.want(owner, "POST", "/api/v1/configuration/plan", map[string]any{"document": doc}, nil, 200)
	h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": doc}, idem("caller-promote"), 200)
}

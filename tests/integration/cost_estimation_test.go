//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/usage"
)

// maxCostChat calls the OpenAI surface with an X-OLP-Routing cost ceiling.
func maxCostChat(t *testing.T, h *accessHarness, secret, slug, ceiling string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": slug, "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "max_tokens": 16})
	req, err := http.NewRequestWithContext(t.Context(), "POST", h.HTTP.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	if ceiling != "" {
		req.Header.Set("X-OLP-Routing", `{"max_cost":"`+ceiling+`"}`)
	}
	reply, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	data, _ := io.ReadAll(reply.Body)
	var parsed map[string]any
	_ = json.Unmarshal(data, &parsed)
	return reply.StatusCode, parsed
}

func pricedGenerationFixture(t *testing.T) (*glFixture, string) {
	t.Helper()
	client := limClient(t)
	namespace := limNamespace(t, client, "max-cost")
	f := glSeedPriced(t, limLimiter(t, client, namespace), limits.FailClosed, glPrice{input: "1000000", output: "1000000"})
	_, secret := f.key("estimate", map[string]any{})
	return f, secret
}

// Anthropic surface renders the same refusal in its own envelope.

// No price published: the bound is unknowable, so the ceiling refuses.

// A committed provider's bound is zero and passes any ceiling.

// The pricing quote prices one usage the way accounting records it: discount,
// commitment and charge all match a persisted fact for the same usage.
func TestPricingQuoteMatchesAccounting(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	up := newVendor(t)
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Quote vendor", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"},
	}, idem("quote-provider"), 201)
	providerID := created["id"].(string)
	h.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": time.Now().UTC().Format(time.RFC3339Nano), "prices": []any{map[string]any{
		"provider_kind": "openai_compatible", "provider_id": providerID, "model": vendorModel,
		"operation": "generation", "currency": "USD", "input_per_million": "1", "output_per_million": "10"}}}, idem("quote-price"), 201)
	// An installation card at 2x doubles the charge over cost.
	card := h.want(owner, "POST", "/api/v1/rate-cards", map[string]any{"owner": map[string]any{"kind": "installation"}}, idem("quote-card"), 201)
	h.want(owner, "POST", "/api/v1/rate-cards/"+card["id"].(string)+"/revisions", map[string]any{"markup": "2"}, idem("quote-card-revision"), 201)

	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "quote", "scopes": []string{"inference"}}, idem("quote-key"), 201)
	usageBody := map[string]any{"input_tokens": 1000, "output_tokens": 100}
	quote := h.want(owner, "POST", "/api/v1/pricing/quote", map[string]any{
		"provider_id": providerID, "model": vendorModel, "operation": "generation", "usage": usageBody,
	}, nil, 200)
	if quote["cost"] == nil || quote["charge"] == nil || quote["currency"] != "USD" || quote["complete"] != true {
		t.Fatalf("quote: %v", quote)
	}
	cost, _ := strconv.ParseFloat(quote["cost"].(string), 64)
	charge, _ := strconv.ParseFloat(quote["charge"].(string), 64)
	if charge != cost*2 {
		t.Fatalf("charge %v is not cost %v under the 2x card", charge, cost)
	}
	// Persist a real event for the same provider and usage, and read the fact
	// back: the quote and the fact must price identically.
	fixture := acctFixture{Pool: h.Pool, Key: key["id"].(string)}
	event := acctEvent(t, fixture, acctEventOptions{Attempts: []usage.Attempt{
		acctAttempt(t, providerID, 1, vendorModel, 200, acctObserved(1000, 100, nil, nil)),
	}})
	acctPersist(t, fixture, event)
	var storedCost, storedCharge *string
	if err := h.Pool.QueryRow(t.Context(), `SELECT estimated_cost::text, charge::text FROM olp.attempt_usage_facts WHERE request_id = $1::uuid`, event.RequestID).Scan(&storedCost, &storedCharge); err != nil {
		t.Fatalf("fact: %v", err)
	}
	if storedCost == nil || storedCharge == nil ||
		quote["cost"].(string) != *storedCost || quote["charge"].(string) != *storedCharge {
		t.Fatalf("quote %v/%v disagrees with accounting %v/%v", quote["cost"], quote["charge"], storedCost, storedCharge)
	}
}

// glSeedCommitted is glSeedPriced with the provider carrying a day commitment.
func glSeedCommitted(t *testing.T, limiter *limits.Limiter) (f struct {
	h      *accessHarness
	secret string
}) {
	t.Helper()
	h := newAccessHarness(t)
	owner := h.owner()
	up := newVendor(t)
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Committed vendor", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1",
			"options": map[string]any{"commitment": map[string]any{"amount": "300", "period": "day"}}},
	}, idem("committed-provider"), 201)
	path := "/api/v1/providers/" + created["id"].(string)
	if probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(created), 200); probe["succeeded"] != true {
		t.Fatalf("probe failed: %v", probe)
	}
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true,
		"capabilities": []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}},
		etagHeader(h.want(owner, "GET", path, nil, nil, 200)), 200)
	h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil,
		etagHeader(h.want(owner, "GET", path, nil, nil, 200)), 200)
	h.want(owner, "POST", path+"/activate", nil,
		withMatch(h.want(owner, "GET", path, nil, nil, 200), idem("committed-activate")), 200)
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": routeSlug, "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"targets": []any{map[string]any{"provider_id": created["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
	}, idem("committed-draft"), 201)
	validated := h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(validated, idem("committed-route")), 200)
	h.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": time.Now().UTC().Format(time.RFC3339Nano), "prices": []any{map[string]any{
		"provider_kind": "openai_compatible", "provider_id": created["id"], "model": vendorModel,
		"operation": "generation", "currency": "USD", "input_per_million": "1000000", "output_per_million": "1000000"}}}, idem("committed-price"), 201)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "committed", "scopes": []string{"inference"}, "allowed_routes": []string{routeSlug}}, idem("committed-key"), 201)
	namespace := limNamespace(t, limClient(t), "max-cost-committed")
	_ = namespace
	h.refresh()
	glEventually(t, "the committed provider to publish", func() bool {
		h.refresh()
		snapshot := h.Runtime.Release().Snapshot
		p := snapshot.Providers[created["id"].(string)]
		return p.Commitment != nil
	})
	f.h, f.secret = h, key["secret"].(string)
	return f
}

// The fallback route's target is priced high; the primary is priced low
// enough to plan but will fail in dispatch.

// Cheap on the primary, expensive on the fallback target.

//go:build integration

package integration_test

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
)

func TestAttributionBudgetsShareHistoryAcrossKeysAndGateways(t *testing.T) {
	for _, window := range []string{"daily_cost_limit", "weekly_cost_limit", "monthly_cost_limit"} {
		t.Run(window, func(t *testing.T) { testAttributionBudgetWindow(t, window) })
	}
}
func testAttributionBudgetWindow(t *testing.T, window string) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Allocation budgets"}, idem("project"), 201)
	projectID := project["id"].(string)
	slug := "allocated"
	fixture := newOpenAIFixture(t, "")
	provider := activeAzureProviderInProject(t, h, owner, "Allocation provider", fixture.URL, &projectID, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"project_id": projectID, "slug": slug, "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("draft"), 201)
	path := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", path+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(validated, idem("activate")), 200)
	keys := []string{}
	for i, team := range []string{"core", "core", "edge"} {
		key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": fmt.Sprintf("allocation-%d", i), "project_id": projectID, "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "attribution_defaults": map[string]string{"team": team, "workload": "shared"}}, idem(fmt.Sprintf("key-%d", i)), 201)
		keys = append(keys, key["secret"].(string))
	}
	h.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": time.Now().UTC().Format(time.RFC3339Nano), "prices": []any{map[string]any{"provider_kind": "azure_openai", "provider_id": provider["id"], "model": vendorModel, "operation": "generation", "currency": "USD", "input_per_million": "1000000", "output_per_million": "1000000"}}}, idem("price"), 201)
	client := limClient(t)
	limiter := limLimiter(t, client, limNamespace(t, client, "allocated"))
	h.Gateway.Admission = gateway.NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	await := endUserAccounting(t, h, limiter)
	replica := newAccessHarnessOn(t, h.Pool, h.DBURL)
	replica.Gateway.Admission = h.Gateway.Admission
	replica.Gateway.Sink = h.Gateway.Sink
	glEventually(t, "price", func() bool {
		h.refresh()
		replica.refresh()
		return len(h.Runtime.RoutingInputs().Prices) > 0 && len(replica.Runtime.RoutingInputs().Prices) > 0
	})
	call := func(server *accessHarness, key string, want int) {
		t.Helper()
		status, err := endUserChat(t.Context(), server, key, slug, "unused")
		if err != nil || status != want {
			t.Fatalf("allocation request: %d %v, want %d", status, err, want)
		}
		await()
	}
	call(h, keys[0], 200) // History predates the cap.
	path = "/api/v1/projects/" + projectID + "/attribution-budgets"
	set := func(budgets map[string]any) {
		t.Helper()
		detail := h.want(owner, "GET", path, nil, nil, 200)
		h.want(owner, "PUT", path, map[string]any{"budgets": budgets}, etagHeader(detail), 200)
		h.refresh()
		replica.refresh()
	}
	caps := func(team, work string) map[string]any {
		m := map[string]any{}
		if team != "" {
			m["team"] = map[string]any{"core": map[string]any{window: team}}
		}
		if work != "" {
			m["workload"] = map[string]any{"shared": map[string]any{window: work}}
		}
		return m
	}
	set(caps("1000", "1000"))
	call(replica, keys[1], 503)
	call(replica, keys[1], 200)
	detail := h.want(owner, "GET", path, nil, nil, 200)
	for _, pair := range [][2]string{{"team", "core"}, {"workload", "shared"}} {
		usage := detail["usage"].(map[string]any)[pair[0]].(map[string]any)[pair[1]].(map[string]any)
		if usage["daily"].(map[string]any)["accrued"] != "4.000000000000" {
			t.Fatalf("history: %+v", usage)
		}
	}
	set(caps("1000", "4"))
	call(h, keys[0], 429)
	pending, err := limiter.Reserved(t.Context(), limits.AttributionBudgetID(projectID, "team", "core"))
	if err != nil || pending != "0" {
		t.Fatalf("prior allocation hold leaked: %s %v", pending, err)
	}
	set(caps("4", ""))
	call(replica, keys[1], 429)
	call(replica, keys[2], 200)
	set(map[string]any{})
	call(h, keys[0], 200)
	set(caps("4", ""))
	call(replica, keys[1], 429)
	// The same pair in another project has its own accounting identity.
	other := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Other allocation"}, idem("other"), 201)
	otherPath := "/api/v1/projects/" + other["id"].(string) + "/attribution-budgets"
	otherDetail := h.want(owner, "GET", otherPath, nil, nil, 200)
	otherDetail = h.want(owner, "PUT", otherPath, map[string]any{"budgets": caps("4", "")}, etagHeader(otherDetail), 200)
	if otherDetail["usage"].(map[string]any)["team"].(map[string]any)["core"].(map[string]any)["daily"].(map[string]any)["accrued"] != "0" {
		t.Fatalf("project spend leaked: %+v", otherDetail)
	}
}

func TestAttributionBudgetPromotionAndValidation(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Promoted allocations"}, idem("project"), 201)
	path := "/api/v1/projects/" + project["id"].(string) + "/attribution-budgets"
	detail := h.want(owner, "GET", path, nil, nil, 200)
	for _, body := range []any{map[string]any{}, map[string]any{"budgets": nil}, map[string]any{"budgets": map[string]any{"team": map[string]any{"core": map[string]any{}}}}} {
		h.want(owner, "PUT", path, body, etagHeader(detail), 422)
	}
	h.want(owner, "PUT", path, map[string]any{"budgets": map[string]any{"team": map[string]any{"core": map[string]any{"weekly_cost_limit": "12.34"}}}}, etagHeader(detail), 200)
	document := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	entry := document["projects"].([]any)[0].(map[string]any)
	if entry["attribution_budgets"].(map[string]any)["team"].(map[string]any)["core"].(map[string]any)["weekly_cost_limit"] != "12.34" {
		t.Fatal("export lost cap")
	}
	entry["attribution_budgets"] = map[string]any{"team": map[string]any{"core": map[string]any{"weekly_cost_limit": "50"}}}
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "configure only", "scopes": []string{"configure"}, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}, idem("token"), 201)["secret"].(string)
	body := map[string]any{"document": document}
	h.machineWant(token, "POST", "/api/v1/configuration/apply", body, idem("deny"), 403)
	h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 200)
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("apply"), 200)
	h.machineWant(token, "POST", "/api/v1/configuration/apply", body, idem("noop"), 200)
	detail = h.want(owner, "GET", path, nil, nil, 200)
	if detail["budgets"].(map[string]any)["team"].(map[string]any)["core"].(map[string]any)["weekly_cost_limit"] != "50" {
		t.Fatal("promotion lost cap")
	}
	delete(entry, "attribution_budgets")
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("clear"), 200)
	if len(h.want(owner, "GET", path, nil, nil, 200)["budgets"].(map[string]any)) != 0 {
		t.Fatal("omission did not clear")
	}
}

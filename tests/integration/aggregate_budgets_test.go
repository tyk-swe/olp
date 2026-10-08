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

func TestAggregateInstallationBudgetPreservesHistoricalSpendAcrossKeys(t *testing.T) {
	f := glSeedIn(t, "aggregate-installation", glPrice{input: "1000000", output: "1000000"})
	_, first := f.key("first", nil)
	_, second := f.key("second", nil)
	await := endUserAccounting(t, f.h, f.limiter)
	call := func(h *accessHarness, key string, want int) {
		t.Helper()
		status, err := endUserChat(t.Context(), h, key, routeSlug, "unused")
		if err != nil || status != want {
			t.Fatalf("inference: %d %v, want %d", status, err, want)
		}
		await()
	}
	call(f.h, first, 200)
	path := "/api/v1/budgets/installation"
	policy := f.h.want(f.owner, "GET", path, nil, nil, 200)
	for _, body := range []map[string]any{{}, {"policy": map[string]any{"daily_cost_limit": "0"}}, {"policy": map[string]any{"unknown": true}}} {
		f.h.want(f.owner, "PUT", path, body, etagHeader(policy), 422)
	}
	policy = f.h.want(f.owner, "PUT", path, map[string]any{"policy": map[string]any{"daily_cost_limit": "1000"}}, etagHeader(policy), 200)
	f.h.refresh()
	replica := newAccessHarnessOn(t, f.h.Pool, f.h.DBURL)
	replica.Gateway.Admission = gateway.NewAdmission(limLimiter(t, limClient(t), f.namespace), nil, slog.New(slog.DiscardHandler))
	replica.Gateway.Sink = f.h.Gateway.Sink
	replica.refresh()
	call(replica, second, 503) // A cap cannot assume that its pre-existing spend was zero.
	call(replica, second, 200)
	detail := f.h.want(f.owner, "GET", path, nil, nil, 200)
	if detail["usage"].(map[string]any)["daily"].(map[string]any)["accrued"] != "20.000000000000" {
		t.Fatalf("aggregate spend: %v", detail["usage"])
	}
	policy = f.h.want(f.owner, "PUT", path, map[string]any{"policy": map[string]any{"daily_cost_limit": "20"}}, etagHeader(detail), 200)
	f.h.refresh()
	replica.refresh()
	call(f.h, first, 429)
	call(replica, second, 429)
	// The Playground spends against the same caps, without a key of its own.
	f.h.want(f.owner, "POST", "/api/v1/playground", map[string]any{"model": routeSlug, "input": "hi"}, nil, 429)
	f.h.want(f.owner, "POST", "/api/v1/playground/stream", map[string]any{"model": routeSlug, "input": "hi", "stream": true}, nil, 429)
	policy = f.h.want(f.owner, "PUT", path, map[string]any{"policy": nil}, etagHeader(policy), 200)
	f.h.refresh()
	call(f.h, first, 200)
	f.h.want(f.owner, "PUT", path, map[string]any{"policy": map[string]any{"daily_cost_limit": "20"}}, etagHeader(policy), 200)
	f.h.refresh()
	call(f.h, first, 429)
}

func TestAggregateBudgetsPromoteWithConditionalPermissions(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Budget project"}, idem("project"), 201)
	path := "/api/v1/projects/" + project["id"].(string) + "/budget"
	before := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "PUT", path, map[string]any{"policy": map[string]any{"daily_cost_limit": "12.34"}}, etagHeader(before), 200)
	exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	document := exported["document"].(map[string]any)
	projects := document["projects"].([]any)
	if projects[0].(map[string]any)["budget"].(map[string]any)["daily_cost_limit"] != "12.34" {
		t.Fatal("project budget missing from export")
	}
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "configure only", "scopes": []string{"configure"}, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}, idem("token"), 201)["secret"].(string)
	document["installation_budget"] = map[string]any{"monthly_cost_limit": "100"}
	body := map[string]any{"document": document}
	h.machineWant(token, "POST", "/api/v1/configuration/apply", body, idem("installation-denied"), 403)
	h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 200)
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("installation-apply"), 200)
	h.machineWant(token, "POST", "/api/v1/configuration/apply", body, idem("installation-noop"), 200)
	projects[0].(map[string]any)["budget"] = map[string]any{"daily_cost_limit": "50"}
	h.machineWant(token, "POST", "/api/v1/configuration/apply", body, idem("project-denied"), 403)
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("project-apply"), 200)
	saved := h.want(owner, "GET", path, nil, nil, 200)
	if saved["policy"].(map[string]any)["daily_cost_limit"] != "50" {
		t.Fatal("project budget not applied")
	}
	projects[0].(map[string]any)["budget"] = nil
	document["installation_budget"] = map[string]any{}
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("clear"), 200)
	if h.want(owner, "GET", path, nil, nil, 200)["policy"] != nil || h.want(owner, "GET", "/api/v1/budgets/installation", nil, nil, 200)["policy"] != nil {
		t.Fatal("empty policies did not clear limits")
	}
}

func TestAggregateProjectAndInstallationCapsBothApply(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	organization := h.want(owner, "POST", "/api/v1/organizations", map[string]any{"name": "Aggregate organization"}, idem("organization"), 201)
	organizationPath := "/api/v1/organizations/" + organization["id"].(string) + "/budget"
	project := h.want(owner, "POST", "/api/v1/organizations/"+organization["id"].(string)+"/projects", map[string]any{"name": "Aggregate project"}, idem("project"), 201)
	projectID := project["id"].(string)
	slug := "aggregate-project"
	fixture := newOpenAIFixture(t, "")
	provider := activeAzureProviderInProject(t, h, owner, "Aggregate provider", fixture.URL, &projectID, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"project_id": projectID, "slug": slug, "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("draft"), 201)
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, idem("activate")), 200)
	keys := []string{}
	for i := range 2 {
		key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": fmt.Sprintf("project-%d", i), "project_id": projectID, "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem(fmt.Sprintf("key-%d", i)), 201)
		keys = append(keys, key["secret"].(string))
	}
	h.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": time.Now().UTC().Format(time.RFC3339Nano), "prices": []any{map[string]any{"provider_kind": "azure_openai", "provider_id": provider["id"], "model": vendorModel, "operation": "generation", "currency": "USD", "input_per_million": "1000000", "output_per_million": "1000000"}}}, idem("price"), 201)
	client := limClient(t)
	namespace := limNamespace(t, client, "aggregate-project")
	limiter := limLimiter(t, client, namespace)
	h.Gateway.Admission = gateway.NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	await := endUserAccounting(t, h, limiter)
	installationPath := "/api/v1/budgets/installation"
	projectPath := "/api/v1/projects/" + projectID + "/budget"
	set := func(path, amount string) {
		t.Helper()
		detail := h.want(owner, "GET", path, nil, nil, 200)
		h.want(owner, "PUT", path, map[string]any{"policy": map[string]any{"daily_cost_limit": amount}}, etagHeader(detail), 200)
		h.refresh()
	}
	set(installationPath, "1000")
	set(projectPath, "1000")
	set(organizationPath, "1000")
	glEventually(t, "prices", func() bool { h.refresh(); return len(h.Runtime.RoutingInputs().Prices) > 0 })
	call := func(key string, want int) {
		t.Helper()
		status, err := endUserChat(t.Context(), h, key, slug, "unused")
		if err != nil || status != want {
			t.Fatalf("request: %d %v, want %d", status, err, want)
		}
		await()
	}
	call(keys[0], 503)
	call(keys[0], 200)
	call(keys[1], 200)
	for index, path := range []string{projectPath, organizationPath, installationPath} {
		detail := h.want(owner, "GET", path, nil, nil, 200)
		if detail["usage"].(map[string]any)["daily"].(map[string]any)["accrued"] != "4.000000000000" {
			t.Fatalf("%s spend: %v", path, detail["usage"])
		}
		set(path, "4")
		call(keys[1], 429)
		var requestID string
		if err := h.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.requests WHERE status_code=429 ORDER BY completed_at DESC,id DESC LIMIT 1").Scan(&requestID); err != nil {
			t.Fatal(err)
		}
		reported := h.want(owner, "GET", "/api/v1/requests/"+requestID, nil, nil, 200)
		if reported["budget_boundary"] != []string{"project", "organization", "installation"}[index] {
			t.Fatalf("lost exhausted hierarchy level: %v", reported["budget_boundary"])
		}
		set(path, "1000")
	}
	// Refusals at the project level must refund the preceding installation hold.
	call(keys[0], 200)
	var id string
	if err := h.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.installation WHERE singleton").Scan(&id); err != nil {
		t.Fatal(err)
	}
	pending, err := limiter.Reserved(t.Context(), limits.AggregateBudgetID("installation", id))
	if err != nil || pending != "0" {
		t.Fatalf("leaked installation reservation: %s %v", pending, err)
	}
}

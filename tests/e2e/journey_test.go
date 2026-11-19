//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

// TestPublishedRouteServesAndSettlesUsage takes one request from publication
// through the gateway to durable usage, across the Valkey stream and the
// consumer that writes PostgreSQL.
func TestPublishedRouteServesAndSettlesUsage(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "journey"}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	provider := s.activeProvider(owner, "journey-provider", newVendor(t).URL, project)
	slug := uniqueName("journey")
	s.publishedRoute(owner, slug, provider, project)
	testSelectorPluginPins(t, s, owner, provider, project)
	key := s.Want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "journey", "scopes": []string{"inference"}, "project_id": project}, harness.Idempotent(), http.StatusCreated)
	consumer := &harness.Caller{Name: "journey key", Token: key["secret"].(string)}
	request := map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}

	s.Eventually("the published route serving a request", 20*time.Second, func() (bool, string) {
		status, raw := s.Do(consumer, "POST", "/v1/chat/completions", request, nil)
		if status != http.StatusOK {
			return false, describe(status, raw)
		}
		var completion struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &completion); err != nil {
			t.Fatalf("invalid completion: %v: %s", err, raw)
		}
		if len(completion.Choices) != 1 || !strings.Contains(completion.Choices[0].Message.Content, vendor.Answer) {
			t.Fatalf("unexpected completion: %s", raw)
		}
		return true, ""
	})

	s.Eventually("usage reaching durable storage", 20*time.Second, func() (bool, string) {
		var attempts, input, output int64
		err := s.Pool.QueryRow(t.Context(), `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0) FROM olp.attempt_usage_facts`).Scan(&attempts, &input, &output)
		if err != nil {
			t.Fatal(err)
		}
		return attempts == 1 && input == vendor.InputTokens && output == vendor.OutputTokens,
			fmt.Sprintf("attempts=%d input=%d output=%d", attempts, input, output)
	})

	t.Run("simulation_requires_all_predicates", func(t *testing.T) {
		classifier := &routing.ClassifierPredicate{Route: slug, Labels: []string{"complex"}, TimeoutMS: 500}
		plugin := &routing.PluginPredicate{Digest: strings.Repeat("a", 64)}
		draft := s.Want(owner, "POST", "/api/v1/route-drafts", map[string]any{
			"slug": uniqueName("simulation"), "operations": []string{"generation"}, "overall_timeout_ms": 5000, "max_attempts": 1,
			"fidelity": map[string]any{"mode": "transformed"}, "project_id": project,
			"targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendor.Model, "priority": 0, "weight": 1, "timeout_ms": 2000, "tags": []string{"selected"}}},
			"selectors": []routing.Selector{
				{ID: "both", Tags: []string{"selected"}, When: routing.Predicate{Classifier: classifier, Plugin: plugin}},
				{ID: "plugin", Tags: []string{"selected"}, When: routing.Predicate{Plugin: plugin}},
				{ID: "classifier", Tags: []string{"selected"}, When: routing.Predicate{Classifier: classifier}},
			},
		}, harness.Idempotent(), http.StatusCreated)
		result := s.Want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/simulate", map[string]any{
			"operation": "generation", "surface": "openai", "mode": "unary", "seed": "predicates",
			"classifier_labels": map[string]string{"both": "complex", "classifier": "complex"},
		}, nil, http.StatusOK)
		selectors, ok := result["selectors"].([]any)
		if !ok || len(selectors) != 3 {
			t.Fatalf("simulation selectors = %v, want both plugin predicates skipped before classifier", result["selectors"])
		}
		for i, id := range []string{"both", "plugin", "classifier"} {
			selector := selectors[i].(map[string]any)
			want := "plugin_not_simulated"
			if id == "classifier" {
				want = "matched"
			}
			if selector["id"] != id || selector["matched"] != (id == "classifier") || selector["outcome"] != want {
				t.Fatalf("selector %s = %v, want %s", id, selector, want)
			}
		}
	})
}

func TestInstallationNotificationDeleteRequiresSettings(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	ownerID := s.Want(owner, "GET", "/api/v1/profile", nil, nil, http.StatusOK)["id"].(string)
	destination := uuid.NewString()
	if _, err := s.Pool.Exec(t.Context(), `INSERT INTO olp.notification_destinations(id,name,url,etag,created_by)
 VALUES($1,'installation',$2,$1,$3)`, destination, "https://hooks.example.com/olp", ownerID); err != nil {
		t.Fatal(err)
	}
	token := s.Want(owner, "POST", "/api/v1/management-tokens", map[string]any{
		"name": "keys only", "scopes": []string{"keys"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, harness.Idempotent(), http.StatusCreated)
	path := "/api/v1/notifications/destinations/" + destination
	match := map[string]string{"If-Match": `"` + destination + `"`}
	for _, c := range []*harness.Caller{s.Invite(owner, "developer@example.com", authz.RoleDeveloper), {Name: "keys token", Token: token["secret"].(string)}} {
		if status, raw := s.Do(c, "DELETE", path, nil, harness.Idempotent(match)); status != http.StatusForbidden {
			t.Errorf("%s deleted an installation destination: %s", c.Name, describe(status, raw))
		}
	}
	s.Want(owner, "DELETE", path, nil, harness.Idempotent(match), http.StatusNoContent)
}

func testSelectorPluginPins(t *testing.T, s *service, owner *harness.Caller, provider map[string]any, project string) {
	t.Helper()
	const digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := s.Pool.Exec(t.Context(), `INSERT INTO olp.plugins(digest, abi_version, manifest, module, size_bytes, etag, installed_by, approved_by, approved_at)
 SELECT $1, 1, '{"name":"selector-fixture","version":"0.1.0","origins":[],"profiles":[]}', '\x0061736d01000000'::bytea, 8, gen_random_uuid(), id, id, now() FROM olp.users WHERE email='owner@example.com'`, digest); err != nil {
		t.Fatal(err)
	}
	pluginPath := "/api/v1/plugins/" + digest
	installed := s.Want(owner, "GET", pluginPath, nil, nil, http.StatusOK)
	draft := s.Want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": uniqueName("selector-pin"), "project_id": project, "operations": []string{"generation"}, "overall_timeout_ms": 5000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"targets":   []any{map[string]any{"provider_id": provider["id"], "provider_model": vendor.Model, "priority": 0, "weight": 1, "timeout_ms": 2000, "tags": []string{"selected"}}},
		"selectors": []routing.Selector{{ID: "module", Tags: []string{"selected"}, When: routing.Predicate{Plugin: &routing.PluginPredicate{Digest: digest}}}},
	}, harness.Idempotent(), http.StatusCreated)
	requirePinned := func() {
		t.Helper()
		problem := s.Want(owner, "DELETE", pluginPath, nil, harness.Idempotent(harness.ETag(installed)), http.StatusConflict)
		if !strings.HasSuffix(problem["type"].(string), "/plugin_pinned") {
			t.Fatalf("selector pin uninstall = %v", problem)
		}
	}
	requirePinned()
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	activated := s.Want(owner, "POST", draftPath+"/activate", nil, harness.Idempotent(harness.ETag(draft)), http.StatusOK)
	current := s.Want(owner, "GET", draftPath, nil, nil, http.StatusOK)
	s.Want(owner, "DELETE", draftPath, nil, harness.Idempotent(harness.ETag(current)), http.StatusNoContent)
	requirePinned()
	routePath := "/api/v1/routes/" + activated["route_id"].(string)
	route := s.Want(owner, "GET", routePath, nil, nil, http.StatusOK)
	s.Want(owner, "POST", routePath+"/retire", nil, harness.Idempotent(harness.ETag(route)), http.StatusOK)
	requirePinned()
}

func TestNotificationRuleRejectsNullEnabled(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	ownerID := s.Want(owner, "GET", "/api/v1/profile", nil, nil, http.StatusOK)["id"].(string)
	destination := uuid.NewString()
	if _, err := s.Pool.Exec(t.Context(), `INSERT INTO olp.notification_destinations(id,name,url,etag,created_by)
 VALUES($1,'notification validation','https://hooks.example.com/olp',$1,$2)`, destination, ownerID); err != nil {
		t.Fatal(err)
	}
	rule := s.Want(owner, "POST", "/api/v1/notifications/rules", map[string]any{
		"name": "disabled", "event": "provider.grant.lapsed", "destination_id": destination, "enabled": false,
	}, harness.Idempotent(), http.StatusCreated)
	path := "/api/v1/notifications/rules/" + rule["id"].(string)
	current := s.Want(owner, "GET", path, nil, nil, http.StatusOK)
	status, raw := s.Do(owner, "PATCH", path, map[string]any{"enabled": nil}, harness.Idempotent(map[string]string{"If-Match": `"` + current["etag"].(string) + `"`}))
	if status != http.StatusUnprocessableEntity {
		t.Errorf("PATCH enabled=null: %s", describe(status, raw))
	}
	current = s.Want(owner, "GET", path, nil, nil, http.StatusOK)
	if current["enabled"] != false {
		t.Errorf("disabled rule after rejected null patch: enabled=%v", current["enabled"])
	}
	for _, enabled := range []bool{true, false} {
		s.Want(owner, "PATCH", path, map[string]any{"enabled": enabled}, harness.Idempotent(map[string]string{"If-Match": `"` + current["etag"].(string) + `"`}), http.StatusOK)
		current = s.Want(owner, "GET", path, nil, nil, http.StatusOK)
		if current["enabled"] != enabled {
			t.Fatalf("enabled=%v, want %v", current["enabled"], enabled)
		}
	}
}

func TestFirstOwnerSetupWaitsForAMountedBootstrapToken(t *testing.T) {
	t.Parallel()
	pool, databaseURL := harness.Database(t)
	env := harness.NewEnvironment(t, databaseURL, "Setup test")
	harness.RunBinary(t, env, "migrate")
	tokenFile := env.Values["OLP_BOOTSTRAP_TOKEN_FILE"]
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}
	s := harness.StartProcess(t, pool, env)
	status := func() any {
		return s.Want(nil, "GET", "/api/v1/setup/status", nil, nil, http.StatusOK)["bootstrap_token_configured"]
	}
	if configured := status(); configured != false {
		t.Fatalf("bootstrap_token_configured=%v without a token file", configured)
	}
	s.Want(nil, "POST", "/api/v1/setup", map[string]any{"email": "owner@example.com", "display_name": "Owner", "password": "a long service test password", "installation_name": "Setup test"}, map[string]string{"X-OLP-Setup-Token": env.Bootstrap}, http.StatusForbidden)
	if err := os.WriteFile(tokenFile, []byte(env.Bootstrap), 0o600); err != nil {
		t.Fatal(err)
	}
	if configured := status(); configured != true {
		t.Fatalf("bootstrap_token_configured=%v after mounting the token", configured)
	}
	s.Owner()
}

func TestSinkWritesReplayGuardRetainedHistoryAndAuditOnce(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	sink := func(name string, extra map[string]any) map[string]any {
		body := map[string]any{"name": name, "destination": "https://sinks.example.com/export", "streams": []string{"requests"}, "enabled": true}
		maps.Copy(body, extra)
		return body
	}
	key := harness.Idempotent()
	draft := sink("warehouse", map[string]any{"type": "https", "credential": "signing-secret"})
	created := s.Want(owner, "POST", "/api/v1/sinks", draft, key, http.StatusCreated)
	if replayed := s.Want(owner, "POST", "/api/v1/sinks", draft, key, http.StatusCreated); replayed["id"] != created["id"] {
		t.Fatalf("replayed create answered sink %v, want %v", replayed["id"], created["id"])
	}
	id := created["id"].(string)
	path := "/api/v1/sinks/" + id
	stale := map[string]string{"If-Match": `"` + uuid.NewString() + `"`}
	if status, raw := s.Do(owner, "PUT", path, sink("lake", nil), harness.Idempotent(stale)); status != http.StatusPreconditionFailed {
		t.Fatalf("stale update: %s", describe(status, raw))
	}
	if status, raw := s.Do(owner, "PUT", path, sink("lake", map[string]any{"type": "https"}), harness.Idempotent(harness.ETag(created))); status != http.StatusUnprocessableEntity {
		t.Fatalf("update that restates the immutable type: %s", describe(status, raw))
	}
	updated := s.Want(owner, "PUT", path, sink("lake", nil), harness.Idempotent(harness.ETag(created)), http.StatusOK)
	if updated["name"] != "lake" || updated["has_credential"] != true || updated["etag"] == created["etag"] {
		t.Fatalf("updated sink = %v, want the new name, the kept credential and a new etag", updated)
	}
	s.Want(owner, "DELETE", path, nil, harness.Idempotent(harness.ETag(updated)), http.StatusNoContent)
	var etag string
	if err := s.Pool.QueryRow(t.Context(), "SELECT etag::text FROM olp.managed_export_sinks WHERE id=$1 AND retired_at IS NOT NULL AND credential_id IS NULL", id).Scan(&etag); err != nil {
		t.Fatalf("retired sink kept its credential or stayed active: %v", err)
	}
	if status, raw := s.Do(owner, "PUT", path, sink("lake", nil), harness.Idempotent(harness.ETag(map[string]any{"etag": etag}))); status != http.StatusConflict {
		t.Fatalf("update of a retired sink: %s", describe(status, raw))
	}
	if actions, want := auditActions(t, s, id), []string{"sink.create", "sink.retire", "sink.update"}; !slices.Equal(actions, want) {
		t.Fatalf("audit actions = %v, want %v", actions, want)
	}
}

func auditActions(t *testing.T, s *service, resource string) []string {
	t.Helper()
	rows, err := s.Pool.Query(t.Context(), `SELECT action COLLATE "C" FROM olp.audit WHERE resource_id::text=$1 ORDER BY 1`, resource)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return actions
}

func TestCachePurgeRequiresOneScopeAndAgreesWithStatistics(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "cached"}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	for name, scope := range map[string]map[string]any{"no scope": {}, "two scopes": {"project_id": project, "route_id": uuid.NewString()}} {
		if status, raw := s.Do(owner, "POST", "/api/v1/cache/purge", scope, nil); status != http.StatusUnprocessableEntity {
			t.Fatalf("purge with %s: %s", name, describe(status, raw))
		}
	}
	statistics := s.Want(owner, "GET", "/api/v1/cache/statistics", nil, nil, http.StatusOK)
	if statistics["requests"] != float64(0) {
		t.Fatalf("statistics = %v, want no cached requests on a new installation", statistics)
	}
	status, raw := s.Do(owner, "POST", "/api/v1/cache/purge", map[string]any{"project_id": project}, nil)
	if purged, configured := status == http.StatusOK, statistics["configured"] == true; purged != configured || !purged && status != http.StatusServiceUnavailable {
		t.Fatalf("purge with configured=%v: %s", configured, describe(status, raw))
	}
}

func TestSettingAndBrandingWritesValidateGuardStaleAndAudit(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	stale := map[string]string{"If-Match": `"` + uuid.NewString() + `"`}

	const key = "retention.audit_days"
	setting := s.Want(owner, "GET", "/api/v1/settings/"+key, nil, nil, http.StatusOK)
	write := func(value string, headers map[string]string, want int) {
		t.Helper()
		if status, raw := s.Do(owner, "PUT", "/api/v1/settings/"+key, map[string]any{"value": value}, headers); status != want {
			t.Fatalf("setting %q: %s", value, describe(status, raw))
		}
	}
	write("0", harness.ETag(setting), http.StatusUnprocessableEntity)
	write("30", stale, http.StatusPreconditionFailed)
	write("30", harness.ETag(setting), http.StatusOK)
	write("31", harness.ETag(setting), http.StatusPreconditionFailed)
	if stored := s.Want(owner, "GET", "/api/v1/settings/"+key, nil, nil, http.StatusOK); stored["value"] != "30" {
		t.Fatalf("setting = %v", stored["value"])
	}
	if status, raw := s.Do(owner, "PUT", "/api/v1/settings/unknown", map[string]any{"value": "1"}, harness.ETag(setting)); status != http.StatusNotFound {
		t.Fatalf("unknown setting: %s", describe(status, raw))
	}
	if actions, want := auditActions(t, s, key), []string{"setting.update"}; !slices.Equal(actions, want) {
		t.Fatalf("setting audit = %v, want %v", actions, want)
	}

	brand := s.Want(owner, "GET", "/api/v1/branding", nil, nil, http.StatusOK)
	rebrand := func(body map[string]any, headers map[string]string, want int) {
		t.Helper()
		if status, raw := s.Do(owner, "PUT", "/api/v1/branding", body, headers); status != want {
			t.Fatalf("branding %v: %s", body, describe(status, raw))
		}
	}
	renamed := map[string]any{"name": "Renamed", "logo": "", "accent": ""}
	rebrand(map[string]any{"name": "Renamed", "accent": ""}, harness.ETag(brand), http.StatusUnprocessableEntity)
	rebrand(renamed, stale, http.StatusPreconditionFailed)
	rebrand(renamed, harness.ETag(brand), http.StatusOK)
	rebrand(renamed, harness.ETag(brand), http.StatusPreconditionFailed)
	if stored := s.Want(owner, "GET", "/api/v1/branding", nil, nil, http.StatusOK); stored["name"] != "Renamed" {
		t.Fatalf("branding name = %v", stored["name"])
	}
	var updates int
	if err := s.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.audit WHERE action='branding.update'`).Scan(&updates); err != nil || updates != 1 {
		t.Fatalf("branding audit rows = %d, %v", updates, err)
	}
}

func TestBudgetGroupAndIncreaseWritesReplayGuardStaleAndAudit(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "budgets"}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	stale := map[string]string{"If-Match": `"` + uuid.NewString() + `"`}
	expect := func(method, path string, body any, headers map[string]string, want int) {
		t.Helper()
		if status, raw := s.Do(owner, method, path, body, headers); status != want {
			t.Fatalf("%s %s: %s", method, path, describe(status, raw))
		}
	}

	create := harness.Idempotent()
	budget := map[string]any{"name": "team", "monthly_cost_limit": "5.00", "project_id": project}
	group := s.Want(owner, "POST", "/api/v1/budget-groups", budget, create, http.StatusCreated)
	if replayed := s.Want(owner, "POST", "/api/v1/budget-groups", budget, create, http.StatusCreated); replayed["id"] != group["id"] {
		t.Fatalf("replayed group = %v, want %v", replayed["id"], group["id"])
	}
	id := group["id"].(string)
	path := "/api/v1/budget-groups/" + id
	rename := map[string]any{"name": "renamed"}
	expect("PATCH", path, map[string]any{"owner": "x"}, harness.Idempotent(harness.ETag(group)), http.StatusUnprocessableEntity)
	expect("PATCH", path, rename, harness.Idempotent(stale), http.StatusPreconditionFailed)
	updated := s.Want(owner, "PATCH", path, rename, harness.Idempotent(harness.ETag(group)), http.StatusOK)
	if stored := s.Want(owner, "GET", path, nil, nil, http.StatusOK); stored["name"] != "renamed" || stored["etag"] != updated["etag"] {
		t.Fatalf("stored group = %v", stored)
	}

	raise := func(window string) map[string]any {
		return map[string]any{"target": map[string]any{"kind": "budget_group", "id": id}, "window": window, "amount": "1", "reason": "launch week"}
	}
	grant := harness.Idempotent()
	increase := s.Want(owner, "POST", "/api/v1/budget-increases", raise("month"), grant, http.StatusCreated)
	if replayed := s.Want(owner, "POST", "/api/v1/budget-increases", raise("month"), grant, http.StatusCreated); replayed["id"] != increase["id"] {
		t.Fatalf("replayed increase = %v, want %v", replayed["id"], increase["id"])
	}
	expect("POST", "/api/v1/budget-increases", raise("day"), harness.Idempotent(), http.StatusUnprocessableEntity)
	increasePath := "/api/v1/budget-increases/" + increase["id"].(string)
	expect("DELETE", increasePath, nil, harness.Idempotent(stale), http.StatusPreconditionFailed)
	expect("DELETE", increasePath, nil, harness.Idempotent(harness.ETag(increase)), http.StatusNoContent)

	expect("DELETE", path, nil, harness.Idempotent(stale), http.StatusPreconditionFailed)
	remove := harness.Idempotent(harness.ETag(updated))
	expect("DELETE", path, nil, remove, http.StatusNoContent)
	expect("DELETE", path, nil, remove, http.StatusNoContent)
	expect("GET", path, nil, nil, http.StatusNotFound)

	if actions, want := auditActions(t, s, id), []string{"budget_group.create", "budget_group.delete", "budget_group.update"}; !slices.Equal(actions, want) {
		t.Fatalf("group audit = %v, want %v", actions, want)
	}
	if actions, want := auditActions(t, s, increase["id"].(string)), []string{"budget.increase.create", "budget.increase.revoke"}; !slices.Equal(actions, want) {
		t.Fatalf("increase audit = %v, want %v", actions, want)
	}
}

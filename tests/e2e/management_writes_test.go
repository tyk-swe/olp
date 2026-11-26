//go:build e2e

package e2e_test

import (
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/tests/harness"
)

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
	status, raw := s.Do(owner, "PATCH", path, map[string]any{"enabled": nil}, harness.Idempotent(harness.ETag(current)))
	if status != http.StatusUnprocessableEntity {
		t.Errorf("PATCH enabled=null: %s", describe(status, raw))
	}
	current = s.Want(owner, "GET", path, nil, nil, http.StatusOK)
	if current["enabled"] != false {
		t.Errorf("disabled rule after rejected null patch: enabled=%v", current["enabled"])
	}
	for _, enabled := range []bool{true, false} {
		s.Want(owner, "PATCH", path, map[string]any{"enabled": enabled}, harness.Idempotent(harness.ETag(current)), http.StatusOK)
		current = s.Want(owner, "GET", path, nil, nil, http.StatusOK)
		if current["enabled"] != enabled {
			t.Fatalf("enabled=%v, want %v", current["enabled"], enabled)
		}
	}
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
	s.expectStatus(owner, "PUT", path, sink("lake", nil), harness.Idempotent(staleETag()), http.StatusPreconditionFailed)
	s.expectStatus(owner, "PUT", path, sink("lake", map[string]any{"type": "https"}), harness.Idempotent(harness.ETag(created)), http.StatusUnprocessableEntity)
	updated := s.Want(owner, "PUT", path, sink("lake", nil), harness.Idempotent(harness.ETag(created)), http.StatusOK)
	if updated["name"] != "lake" || updated["has_credential"] != true || updated["etag"] == created["etag"] {
		t.Fatalf("updated sink = %v, want the new name, the kept credential and a new etag", updated)
	}
	s.Want(owner, "DELETE", path, nil, harness.Idempotent(harness.ETag(updated)), http.StatusNoContent)
	var etag string
	if err := s.Pool.QueryRow(t.Context(), "SELECT etag::text FROM olp.managed_export_sinks WHERE id=$1 AND retired_at IS NOT NULL AND credential_id IS NULL", id).Scan(&etag); err != nil {
		t.Fatalf("retired sink kept its credential or stayed active: %v", err)
	}
	s.expectStatus(owner, "PUT", path, sink("lake", nil), harness.Idempotent(harness.ETag(map[string]any{"etag": etag})), http.StatusConflict)
	if actions, want := auditActions(t, s, id), []string{"sink.create", "sink.retire", "sink.update"}; !slices.Equal(actions, want) {
		t.Fatalf("audit actions = %v, want %v", actions, want)
	}
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
	stale := staleETag()

	const key = "retention.audit_days"
	setting := s.Want(owner, "GET", "/api/v1/settings/"+key, nil, nil, http.StatusOK)
	s.expectStatus(owner, "PUT", "/api/v1/settings/"+key, map[string]any{"value": "0"}, harness.ETag(setting), http.StatusUnprocessableEntity)
	s.expectStatus(owner, "PUT", "/api/v1/settings/"+key, map[string]any{"value": "30"}, stale, http.StatusPreconditionFailed)
	s.expectStatus(owner, "PUT", "/api/v1/settings/"+key, map[string]any{"value": "30"}, harness.ETag(setting), http.StatusOK)
	s.expectStatus(owner, "PUT", "/api/v1/settings/"+key, map[string]any{"value": "31"}, harness.ETag(setting), http.StatusPreconditionFailed)
	if stored := s.Want(owner, "GET", "/api/v1/settings/"+key, nil, nil, http.StatusOK); stored["value"] != "30" {
		t.Fatalf("setting = %v", stored["value"])
	}
	s.expectStatus(owner, "PUT", "/api/v1/settings/unknown", map[string]any{"value": "1"}, harness.ETag(setting), http.StatusNotFound)
	if actions, want := auditActions(t, s, key), []string{"setting.update"}; !slices.Equal(actions, want) {
		t.Fatalf("setting audit = %v, want %v", actions, want)
	}

	brand := s.Want(owner, "GET", "/api/v1/branding", nil, nil, http.StatusOK)
	renamed := map[string]any{"name": "Renamed", "logo": "", "accent": ""}
	s.expectStatus(owner, "PUT", "/api/v1/branding", map[string]any{"name": "Renamed", "accent": ""}, harness.ETag(brand), http.StatusUnprocessableEntity)
	s.expectStatus(owner, "PUT", "/api/v1/branding", renamed, stale, http.StatusPreconditionFailed)
	s.expectStatus(owner, "PUT", "/api/v1/branding", renamed, harness.ETag(brand), http.StatusOK)
	s.expectStatus(owner, "PUT", "/api/v1/branding", renamed, harness.ETag(brand), http.StatusPreconditionFailed)
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
	stale := staleETag()

	create := harness.Idempotent()
	budget := map[string]any{"name": "team", "monthly_cost_limit": "5.00", "project_id": project}
	group := s.Want(owner, "POST", "/api/v1/budget-groups", budget, create, http.StatusCreated)
	if replayed := s.Want(owner, "POST", "/api/v1/budget-groups", budget, create, http.StatusCreated); replayed["id"] != group["id"] {
		t.Fatalf("replayed group = %v, want %v", replayed["id"], group["id"])
	}
	id := group["id"].(string)
	path := "/api/v1/budget-groups/" + id
	rename := map[string]any{"name": "renamed"}
	s.expectStatus(owner, "PATCH", path, map[string]any{"owner": "x"}, harness.Idempotent(harness.ETag(group)), http.StatusUnprocessableEntity)
	s.expectStatus(owner, "PATCH", path, rename, harness.Idempotent(stale), http.StatusPreconditionFailed)
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
	s.expectStatus(owner, "POST", "/api/v1/budget-increases", raise("day"), harness.Idempotent(), http.StatusUnprocessableEntity)
	increasePath := "/api/v1/budget-increases/" + increase["id"].(string)
	s.expectStatus(owner, "DELETE", increasePath, nil, harness.Idempotent(stale), http.StatusPreconditionFailed)
	s.expectStatus(owner, "DELETE", increasePath, nil, harness.Idempotent(harness.ETag(increase)), http.StatusNoContent)

	s.expectStatus(owner, "DELETE", path, nil, harness.Idempotent(stale), http.StatusPreconditionFailed)
	remove := harness.Idempotent(harness.ETag(updated))
	s.expectStatus(owner, "DELETE", path, nil, remove, http.StatusNoContent)
	s.expectStatus(owner, "DELETE", path, nil, remove, http.StatusNoContent)
	s.expectStatus(owner, "GET", path, nil, nil, http.StatusNotFound)

	if actions, want := auditActions(t, s, id), []string{"budget_group.create", "budget_group.delete", "budget_group.update"}; !slices.Equal(actions, want) {
		t.Fatalf("group audit = %v, want %v", actions, want)
	}
	if actions, want := auditActions(t, s, increase["id"].(string)), []string{"budget.increase.create", "budget.increase.revoke"}; !slices.Equal(actions, want) {
		t.Fatalf("increase audit = %v, want %v", actions, want)
	}
}

func TestAReplayedBudgetGroupDeleteIsRefusedOnceTheCallerNoLongerManagesItsProject(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "handed-over"}, harness.Idempotent(), http.StatusCreated)
	projectID := project["id"].(string)
	const email = "former-manager@example.com"
	manager, userID := s.assignProjectMember(owner, email, authz.RoleOperator, projectID, "manager")
	membership := "/api/v1/projects/" + projectID + "/members/" + userID

	group := s.Want(manager, "POST", "/api/v1/budget-groups", map[string]any{"name": "team", "monthly_cost_limit": "5.00", "project_id": projectID}, harness.Idempotent(), http.StatusCreated)
	path := "/api/v1/budget-groups/" + group["id"].(string)
	remove := harness.Idempotent(harness.ETag(group))
	s.Want(manager, "DELETE", path, nil, remove, http.StatusNoContent)
	s.Want(manager, "DELETE", path, nil, remove, http.StatusNoContent)

	managed := s.Want(owner, "GET", "/api/v1/projects/"+projectID, nil, nil, http.StatusOK)
	s.Want(owner, "PUT", membership, map[string]any{"role": "viewer"}, harness.ETag(managed), http.StatusOK)
	if status, raw := s.Do(s.Login(email), "DELETE", path, nil, remove); status != http.StatusForbidden {
		t.Fatalf("replayed delete after losing the project: %s", describe(status, raw))
	}
}

func TestAPIKeyWritesReplayGuardStaleRotateRevokeAndAudit(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "keys"}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	stale := staleETag()

	create := harness.Idempotent()
	input := map[string]any{"name": "service", "scopes": []string{"inference"}, "project_id": project}
	key := s.Want(owner, "POST", "/api/v1/api-keys", input, create, http.StatusCreated)
	if replayed := s.Want(owner, "POST", "/api/v1/api-keys", input, create, http.StatusCreated); replayed["id"] != key["id"] || replayed["secret"] != key["secret"] {
		t.Fatalf("replayed create returned another key")
	}
	id := key["id"].(string)
	path := "/api/v1/api-keys/" + id
	stored := s.Want(owner, "GET", path, nil, nil, http.StatusOK)

	rename := map[string]any{"name": "renamed"}
	s.expectStatus(owner, "PATCH", path, map[string]any{"secret": "x"}, harness.Idempotent(harness.ETag(stored)), http.StatusUnprocessableEntity)
	s.expectStatus(owner, "PATCH", path, rename, harness.Idempotent(stale), http.StatusPreconditionFailed)
	updated := s.Want(owner, "PATCH", path, rename, harness.Idempotent(harness.ETag(stored)), http.StatusOK)

	overlap := map[string]any{"overlap_seconds": 60}
	s.expectStatus(owner, "POST", path+"/rotate", map[string]any{"name": "x"}, harness.Idempotent(harness.ETag(updated)), http.StatusUnprocessableEntity)
	s.expectStatus(owner, "POST", path+"/rotate", overlap, harness.Idempotent(stale), http.StatusPreconditionFailed)
	rotate := harness.Idempotent(harness.ETag(updated))
	rotated := s.Want(owner, "POST", path+"/rotate", overlap, rotate, http.StatusOK)
	if rotated["secret"] == key["secret"] || rotated["overlap_expires_at"] == nil {
		t.Fatalf("rotation = %v", rotated)
	}
	if replayed := s.Want(owner, "POST", path+"/rotate", overlap, rotate, http.StatusOK); replayed["secret"] != rotated["secret"] {
		t.Fatalf("replayed rotation returned another secret")
	}
	if current := s.Want(owner, "GET", path, nil, nil, http.StatusOK); current["name"] != "renamed" || current["etag"] != rotated["etag"] {
		t.Fatalf("stored key = %v", current)
	}

	s.expectStatus(owner, "POST", path+"/revoke", nil, harness.Idempotent(stale), http.StatusPreconditionFailed)
	revoke := harness.Idempotent(harness.ETag(rotated))
	s.expectStatus(owner, "POST", path+"/revoke", nil, revoke, http.StatusOK)
	s.expectStatus(owner, "POST", path+"/revoke", nil, revoke, http.StatusOK)
	revoked := s.Want(owner, "GET", path, nil, nil, http.StatusOK)
	s.expectStatus(owner, "POST", path+"/revoke", nil, harness.Idempotent(harness.ETag(revoked)), http.StatusConflict)
	s.expectStatus(owner, "POST", path+"/rotate", overlap, harness.Idempotent(harness.ETag(revoked)), http.StatusConflict)
	s.expectStatus(owner, "PATCH", path, rename, harness.Idempotent(harness.ETag(revoked)), http.StatusConflict)

	if actions, want := auditActions(t, s, id), []string{"api_key.create", "api_key.revoke", "api_key.rotate", "api_key.update"}; !slices.Equal(actions, want) {
		t.Fatalf("key audit = %v, want %v", actions, want)
	}
}

func TestWorkloadIssuerWritesReplayGuardStaleKeepIssuerAndAudit(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "workloads"}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	issuer := func(name, url string) map[string]any {
		return map[string]any{
			"name": name, "issuer": url, "jwks_url": url + "/keys", "enabled": false,
			"audiences": []string{"olp"}, "algorithms": []string{"RS256"}, "max_lifetime_seconds": 600, "disabled_key_ids": []string{},
			"mappings": []map[string]any{{
				"name": "deploy", "match": map[string]string{"/sub": "repository"}, "project_id": project,
				"limit_template": "standard", "route_groups": []string{"main"}, "scopes": []string{"inference"},
			}},
		}
	}

	create := harness.Idempotent()
	input := issuer("pipeline", "https://issuer.example")
	created := s.Want(owner, "POST", "/api/v1/workload-issuers", input, create, http.StatusCreated)
	if replayed := s.Want(owner, "POST", "/api/v1/workload-issuers", input, create, http.StatusCreated); replayed["id"] != created["id"] {
		t.Fatalf("replayed create returned another issuer")
	}
	s.expectStatus(owner, "POST", "/api/v1/workload-issuers", issuer("", "https://other.example"), harness.Idempotent(), http.StatusUnprocessableEntity)
	id := created["id"].(string)
	path := "/api/v1/workload-issuers/" + id

	renamed := issuer("renamed", "https://issuer.example")
	s.expectStatus(owner, "PUT", path, renamed, harness.Idempotent(staleETag()), http.StatusPreconditionFailed)
	s.expectStatus(owner, "PUT", path, issuer("renamed", "https://moved.example"), harness.Idempotent(harness.ETag(created)), http.StatusUnprocessableEntity)
	updated := s.Want(owner, "PUT", path, renamed, harness.Idempotent(harness.ETag(created)), http.StatusOK)
	if stored := s.Want(owner, "GET", path, nil, nil, http.StatusOK); stored["name"] != "renamed" || stored["etag"] != updated["etag"] || stored["etag"] == created["etag"] {
		t.Fatalf("stored issuer = %v", stored)
	}

	if actions, want := auditActions(t, s, id), []string{"workload_issuer.update", "workload_issuer.update"}; !slices.Equal(actions, want) {
		t.Fatalf("issuer audit = %v, want %v", actions, want)
	}
}

func TestAuditLogFiltersByResourceAndRejectsBadFilters(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "audited"}, harness.Idempotent(), http.StatusCreated)["id"].(string)

	events := items(s.Want(owner, "GET", "/api/v1/audit?action=project.create&resource_id="+project, nil, nil, http.StatusOK)["items"])
	if len(events) != 1 || events[0]["resource_type"] != "project" || events[0]["actor_type"] != "user" || events[0]["outcome"] != "success" {
		t.Fatalf("audit events = %v", events)
	}
	if failed := items(s.Want(owner, "GET", "/api/v1/audit?outcome=failure&resource_id="+project, nil, nil, http.StatusOK)["items"]); len(failed) != 0 {
		t.Fatalf("failure filter returned %v", failed)
	}
	for _, query := range []string{
		"outcome=maybe",
		"actor_user_id=nobody",
		"occurred_after=yesterday",
		"occurred_after=2026-01-02T00:00:00Z&occurred_before=2026-01-01T00:00:00Z",
	} {
		if status, raw := s.Do(owner, "GET", "/api/v1/audit?"+query, nil, nil); status != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %s", query, describe(status, raw))
		}
	}
}

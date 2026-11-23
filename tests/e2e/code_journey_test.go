//go:build e2e

package e2e_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/tests/harness"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/codemode/codeadapter"
)

func TestCodeAccountEndpoints(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	var user string
	if err := s.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp.users WHERE email='owner@example.com'`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "allowance-read"}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	seed := func(profile, principal string) string {
		t.Helper()
		provider, credential, account := uuid.NewString(), uuid.NewString(), uuid.NewString()
		configuration := `{"kind":"plugin","auth_mode":"grant","profile_id":"` + profile + `","profile_revision":"` + strings.Repeat("0", 64) + `"}`
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO olp.providers(id, project_id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, $2, $7, 'plugin', 'active', $3, $4, $5, $6)`, []any{provider, project, configuration, uuid.NewString(), uuid.NewString(), user, principal}},
			{`INSERT INTO olp.provider_credentials(id, provider_id, version, principal, plugin_digest, profile_id, grant_facts) VALUES($1, $2, 1, $3, repeat('0',64), $4, '{}')`, []any{credential, provider, principal, profile}},
			{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{credential}},
			{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, etag, created_by) VALUES($1, $2, $3, $4, $5, $6, '["model"]', $7, $8)`, []any{account, project, provider, credential, principal, principal, uuid.NewString(), user}},
		} {
			if _, err := s.Pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
		return account
	}
	codex := seed("codex-subscription", "codex-principal")
	minimax := seed("minimax-plan", "minimax-principal")
	claude := seed("claude-subscription", "claude-principal")

	if status, raw := s.Do(owner, "POST", "/api/v1/code/accounts/"+codex+"/allowance-reads", nil, nil); status != http.StatusBadRequest {
		t.Fatalf("missing Idempotency-Key: status %d: %s", status, raw)
	}
	for _, account := range []string{codex, minimax} {
		if status, raw := s.Do(owner, "POST", "/api/v1/code/accounts/"+account+"/allowance-reads", nil, harness.Idempotent()); status != http.StatusUnprocessableEntity {
			t.Fatalf("unqualified allowance read: status %d: %s", status, raw)
		}
	}
	if status, raw := s.Do(owner, "POST", "/api/v1/code/accounts/"+claude+"/allowance-reads", nil, harness.Idempotent()); status != http.StatusUnprocessableEntity {
		t.Fatalf("unqualified allowance read: status %d: %s", status, raw)
	}
	s.Eventually("code allowance worker checkpoint", 30*time.Second, func() (bool, string) {
		var checks int
		if err := s.Pool.QueryRow(t.Context(), `SELECT successes_total+failures_total FROM olp.worker_task_health WHERE task='code_allowance_read'`).Scan(&checks); err != nil {
			return false, err.Error()
		}
		return checks > 0, "no checkpoint yet"
	})

	if _, err := s.Pool.Exec(t.Context(), `UPDATE olp.code_accounts SET models='["model","other"]' WHERE id=$1`, minimax); err != nil {
		t.Fatal(err)
	}
	store := codesessions.New(s.Pool)
	retryAt := time.Now().Add(time.Hour)
	for _, account := range []string{codex, minimax} {
		if err := store.ObserveCooldownClass(t.Context(), account, "model", codeadapter.Classification{Class: codeadapter.CooldownCapacity, Scope: codeadapter.ScopeModel, RetryAt: retryAt}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	eligible := map[string]bool{}
	for _, item := range s.Want(owner, "GET", "/api/v1/code/accounts?project_id="+project, nil, nil, http.StatusOK)["items"].([]any) {
		account := item.(map[string]any)
		eligible[account["id"].(string)] = account["eligible"].(bool)
	}
	if want := map[string]bool{codex: false, minimax: true, claude: true}; !maps.Equal(eligible, want) {
		t.Fatalf("eligible after model cooldowns = %v, want %v", eligible, want)
	}
	codeAccountConcurrencyWrites(t, s, owner, project, codex)
	codeAccountDrainWrites(t, s, owner, project, claude)
}

func codeAccountDrainWrites(t *testing.T, s *service, owner *harness.Caller, project, id string) {
	t.Helper()
	find := func() map[string]any {
		t.Helper()
		for _, item := range s.Want(owner, "GET", "/api/v1/code/accounts?project_id="+project, nil, nil, http.StatusOK)["items"].([]any) {
			account := item.(map[string]any)
			if account["id"] == id {
				return account
			}
		}
		t.Fatal("enrolled account not found")
		return nil
	}
	initial := find()
	if initial["draining"] != false || initial["remaining_pins"] != float64(0) || initial["last_activity_at"] != nil {
		t.Fatalf("unexpected drain progress on an unused account: %v", initial)
	}
	const viewerEmail = "code-viewer@example.com"
	viewer := s.Invite(owner, viewerEmail, authz.RoleViewer)
	profile := s.Want(viewer, "GET", "/api/v1/profile", nil, nil, http.StatusOK)
	user := profile["id"].(string)
	s.Want(owner, "PATCH", "/api/v1/users/"+user, map[string]any{"access_scope": "assigned"}, harness.ETag(profile), http.StatusOK)
	projectPath := "/api/v1/projects/" + project
	membership := projectPath + "/members/" + user
	currentProject := func() map[string]any {
		t.Helper()
		return s.Want(owner, "GET", projectPath, nil, nil, http.StatusOK)
	}
	grantAccess := func() {
		t.Helper()
		s.Want(owner, "PUT", membership, map[string]any{"role": "viewer"}, harness.ETag(currentProject()), http.StatusOK)
	}
	grantAccess()
	binding := seedCodeAccountPin(t, s, project, id, user, initial["provider_id"].(string))
	path := "/api/v1/code/accounts/" + id + "/drain"
	start, stop := map[string]bool{"draining": true}, map[string]bool{"draining": false}
	s.Want(owner, "PUT", path, start, nil, http.StatusPreconditionRequired)
	s.Want(owner, "PUT", path, map[string]bool{}, harness.ETag(initial), http.StatusUnprocessableEntity)
	drained := s.Want(owner, "PUT", path, start, harness.ETag(initial), http.StatusOK)
	progress := find()
	if drained["draining"] != true || drained["eligible"] != false || drained["enabled"] != true ||
		progress["remaining_pins"] != float64(1) || progress["last_activity_at"] == nil {
		t.Fatalf("draining account lost its live pin or activity: %v", progress)
	}
	s.Want(owner, "PUT", path, stop, harness.ETag(initial), http.StatusPreconditionFailed)
	s.Want(s.Login(viewerEmail), "PUT", path, stop, harness.ETag(drained), http.StatusForbidden)
	s.Want(owner, "DELETE", membership, nil, harness.ETag(currentProject()), http.StatusNoContent)
	if missingAccess := find(); missingAccess["remaining_pins"] != float64(0) {
		t.Fatalf("inaccessible pin counted after project membership removal: %v", missingAccess)
	}
	grantAccess()
	if restoredAccess := find(); restoredAccess["remaining_pins"] != float64(1) {
		t.Fatalf("live pin missing after project membership restoration: %v", restoredAccess)
	}
	if _, err := s.Pool.Exec(t.Context(), `UPDATE olp.code_bindings SET retired_at=now() WHERE id=$1`, binding); err != nil {
		t.Fatal(err)
	}
	resumed := s.Want(owner, "PUT", path, stop, harness.ETag(drained), http.StatusOK)
	if resumed["draining"] != false || resumed["eligible"] != true || resumed["remaining_pins"] != float64(0) ||
		resumed["last_activity_at"] != progress["last_activity_at"] {
		t.Fatalf("unexpected drain progress after retiring the tree and resuming: %v", resumed)
	}
	if actions, want := auditActions(t, s, id), []string{"code_account.drain", "code_account.resume"}; !slices.Equal(actions, want) {
		t.Fatalf("audit actions = %v, want %v", actions, want)
	}
}

func seedCodeAccountPin(t *testing.T, s *service, project, account, user, provider string) string {
	t.Helper()
	key := uuid.NewString()
	route := codemode.Route{ID: uuid.NewString(), ProjectID: project, Slug: "drain-journey", PoolID: uuid.NewString(), Models: []string{"model"}, Enabled: true, RevisionID: uuid.NewString(), Revision: 1}
	document, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag,project_id) VALUES($1,$1::uuid::text,'\x00','drain',$2,'{"scopes":["inference"]}',$3,$4)`, []any{key, user, uuid.NewString(), project}},
		{`INSERT INTO olp.code_pools(id,project_id,name,kind,etag,created_by) VALUES($1,$2,'drain','shared',$3,$4)`, []any{route.PoolID, project, uuid.NewString(), user}},
		{`INSERT INTO olp.code_pool_accounts(pool_id,account_id,weight) VALUES($1,$2,1)`, []any{route.PoolID, account}},
		{`INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2)`, []any{route.PoolID, key}},
		{`INSERT INTO olp.code_routes(id,project_id,slug,draft,etag,created_by) VALUES($1,$2,$3,'{}',$4,$5)`, []any{route.ID, project, route.Slug, uuid.NewString(), user}},
		{`INSERT INTO olp.code_route_revisions(id,route_id,revision,document,connections,created_by)
			SELECT $1,$2,1,$3,jsonb_build_object(id::text,configuration),$4 FROM olp.providers WHERE id=$5`, []any{route.RevisionID, route.ID, document, user, provider}},
		{`UPDATE olp.code_routes SET latest_revision_id=$2 WHERE id=$1`, []any{route.ID, route.RevisionID}},
	} {
		if _, err := s.Pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	store := codesessions.New(s.Pool)
	permit, err := store.Admit(t.Context(), codesessions.Admission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: "messages", Model: "model", Identity: codemode.Identity{Conversation: "draining-pin"}}, Providers: []string{provider}})
	if err != nil {
		t.Fatal(err)
	}
	return permit.Binding.ID
}

func codeAccountConcurrencyWrites(t *testing.T, s *service, owner *harness.Caller, project, id string) {
	t.Helper()
	list := "/api/v1/code/accounts?project_id=" + project
	find := func() map[string]any {
		for _, item := range s.Want(owner, "GET", list, nil, nil, http.StatusOK)["items"].([]any) {
			account := item.(map[string]any)
			if account["id"] == id {
				return account
			}
		}
		t.Fatal("enrolled account not found")
		return nil
	}
	initial := find()
	body := map[string]any{
		"project_id": project, "provider_id": initial["provider_id"], "credential_id": initial["credential_id"],
		"name": initial["name"], "models": initial["models"], "enabled": true, "max_concurrency": 0,
	}
	path := "/api/v1/code/accounts/" + id
	s.Want(owner, "PUT", path, body, harness.ETag(initial), http.StatusUnprocessableEntity)
	body["max_concurrency"] = 2
	limited := s.Want(owner, "PUT", path, body, harness.ETag(initial), http.StatusOK)
	if limited["max_concurrency"] != float64(2) || find()["max_concurrency"] != float64(2) {
		t.Fatalf("updated account = %v, want limit persisted and returned", limited["max_concurrency"])
	}
	body["max_concurrency"] = nil
	cleared := s.Want(owner, "PUT", path, body, harness.ETag(limited), http.StatusOK)
	if cleared["max_concurrency"] != nil || find()["max_concurrency"] != nil {
		t.Fatal("clearing the limit left an account cap configured")
	}
}

func TestCodeBudgetWritesReplayGuardStaleAndAuditEachWrite(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := func(name string) string {
		return s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": name}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	}
	home, other := project("code-budget-home"), project("code-budget-other")
	budget := func(project string, daily int) map[string]any {
		return map[string]any{"project_id": project, "daily_tokens": daily, "enabled": true}
	}
	key := harness.Idempotent()
	created := s.Want(owner, "POST", "/api/v1/code/budgets", budget(home, 100), key, http.StatusCreated)
	if replayed := s.Want(owner, "POST", "/api/v1/code/budgets", budget(home, 100), key, http.StatusCreated); replayed["id"] != created["id"] {
		t.Fatalf("replayed create answered budget %v, want %v", replayed["id"], created["id"])
	}
	id := created["id"].(string)
	path := "/api/v1/code/budgets/" + id
	stale := map[string]string{"If-Match": `"` + uuid.NewString() + `"`}
	if status, raw := s.Do(owner, "PUT", path, budget(home, 200), harness.Idempotent(stale)); status != http.StatusPreconditionFailed {
		t.Fatalf("stale update: %s", describe(status, raw))
	}
	if status, raw := s.Do(owner, "PUT", path, budget(other, 200), harness.Idempotent(harness.ETag(created))); status != http.StatusUnprocessableEntity {
		t.Fatalf("update that moves the budget to another project: %s", describe(status, raw))
	}
	updated := s.Want(owner, "PUT", path, budget(home, 200), harness.Idempotent(harness.ETag(created)), http.StatusOK)
	if updated["daily_tokens"] != float64(200) || updated["etag"] == created["etag"] {
		t.Fatalf("updated budget = %v, want the new cap and a new etag", updated)
	}
	if actions, want := auditActions(t, s, id), []string{"code_budget.write", "code_budget.write"}; !slices.Equal(actions, want) {
		t.Fatalf("audit actions = %v, want %v", actions, want)
	}
}

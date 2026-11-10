//go:build e2e

package e2e_test

import (
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codesessions"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/codeadapter"
)

func TestCodeAccountEndpoints(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.owner()
	var user string
	if err := s.pool.QueryRow(t.Context(), `SELECT id::text FROM olp.users WHERE email='owner@example.com'`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	project := s.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "allowance-read"}, idempotent(), http.StatusCreated)["id"].(string)
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
			if _, err := s.pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
		return account
	}
	codex := seed("codex-subscription", "codex-principal")
	minimax := seed("minimax-plan", "minimax-principal")
	claude := seed("claude-subscription", "claude-principal")

	if status, raw := s.do(owner, "POST", "/api/v1/code/accounts/"+codex+"/allowance-reads", nil, nil); status != http.StatusBadRequest {
		t.Fatalf("missing Idempotency-Key: status %d: %s", status, raw)
	}
	for _, account := range []string{codex, minimax} {
		if status, raw := s.do(owner, "POST", "/api/v1/code/accounts/"+account+"/allowance-reads", nil, idempotent()); status != http.StatusUnprocessableEntity {
			t.Fatalf("unqualified allowance read: status %d: %s", status, raw)
		}
	}
	if status, raw := s.do(owner, "POST", "/api/v1/code/accounts/"+claude+"/allowance-reads", nil, idempotent()); status != http.StatusUnprocessableEntity {
		t.Fatalf("unqualified allowance read: status %d: %s", status, raw)
	}
	s.eventually("code allowance worker checkpoint", 30*time.Second, func() (bool, string) {
		var checks int
		if err := s.pool.QueryRow(t.Context(), `SELECT successes_total+failures_total FROM olp.worker_task_health WHERE task='code_allowance_read'`).Scan(&checks); err != nil {
			return false, err.Error()
		}
		return checks > 0, "no checkpoint yet"
	})

	if _, err := s.pool.Exec(t.Context(), `UPDATE olp.code_accounts SET models='["model","other"]' WHERE id=$1`, minimax); err != nil {
		t.Fatal(err)
	}
	store := codesessions.New(s.pool)
	retryAt := time.Now().Add(time.Hour)
	for _, account := range []string{codex, minimax} {
		if err := store.ObserveCooldownClass(t.Context(), account, "model", codeadapter.Classification{Class: codeadapter.CooldownCapacity, Scope: codeadapter.ScopeModel, RetryAt: retryAt}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	eligible := map[string]bool{}
	for _, item := range s.want(owner, "GET", "/api/v1/code/accounts?project_id="+project, nil, nil, http.StatusOK)["items"].([]any) {
		account := item.(map[string]any)
		eligible[account["id"].(string)] = account["eligible"].(bool)
	}
	if want := map[string]bool{codex: false, minimax: true, claude: true}; !maps.Equal(eligible, want) {
		t.Fatalf("eligible after model cooldowns = %v, want %v", eligible, want)
	}
}

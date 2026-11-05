//go:build service

package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/database"
)

func TestAllowanceObservationMigrationBackfillsLegacyWindows(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	user, project, provider, credential, account := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id, email, display_name, role, etag) VALUES($1, 'allowance@example.invalid', 'Allowance', 'owner', $2)`, []any{user, uuid.NewString()}},
		{`INSERT INTO olp.projects(id, name, etag, created_by) VALUES($1, 'allowance-migration', $2, $3)`, []any{project, uuid.NewString(), user}},
		{`INSERT INTO olp.providers(id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, 'allowance-migration', 'plugin', 'active', '{}', $2, $3, $4)`, []any{provider, uuid.NewString(), uuid.NewString(), user}},
		{`INSERT INTO olp.provider_credentials(id, provider_id, version) VALUES($1, $2, 1)`, []any{credential, provider}},
		{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, allowance, etag, created_by) VALUES($1, $2, $3, $4, 'principal', 'account', '["model"]', $5, $6, $7)`, []any{account, project, provider, credential, `{"windows":[{"limit_id":"codex","window":"primary","used_percent":25,"remaining_percent":75,"window_minutes":300,"resets_at":"2036-11-05T15:00:00Z","observed_at":"2026-11-05T10:00:00Z"},{"limit_id":"codex","window":"secondary","used_percent":10,"remaining_percent":90,"window_minutes":null,"resets_at":null,"observed_at":"2026-11-05T10:00:00Z"}],"credits":{"has_credits":true,"unlimited":false,"balance":"12.5","observed_at":"2026-11-05T10:00:00Z"},"remaining_tokens":null,"remaining_requests":null,"remaining_percent":75,"resets_at":"2036-11-05T15:00:00Z","observed_at":"2026-11-05T10:00:00Z"}`, uuid.NewString(), user}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	migration, err := os.ReadFile("../../internal/database/migrations/0130_code_allowance_observation.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), string(migration)); err != nil {
		t.Fatal(err)
	}
	var document []byte
	if err = pool.QueryRow(t.Context(), `SELECT allowance FROM olp.code_accounts WHERE id=$1`, account).Scan(&document); err != nil {
		t.Fatal(err)
	}
	var allowance codemode.Allowance
	if err = json.Unmarshal(document, &allowance); err != nil {
		t.Fatal(err)
	}
	if len(allowance.Windows) != 2 {
		t.Fatalf("windows = %s", document)
	}
	primary := allowance.Windows[0]
	if primary.Unit != codemode.AllowanceUnitPercent || primary.Evidence != codemode.AllowanceEvidencePassive || primary.UsedValue != nil || primary.RemainingValue != nil {
		t.Fatalf("primary window not backfilled: %s", document)
	}
	if primary.UsedPercent != 25 || primary.RemainingPercent != 75 || primary.ResetsAt == nil || !primary.ResetsAt.Equal(time.Date(2036, 11, 5, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("primary window values changed: %s", document)
	}
	if allowance.Windows[1].Unit != codemode.AllowanceUnitPercent || allowance.Windows[1].Evidence != codemode.AllowanceEvidencePassive || allowance.Credits == nil || allowance.Credits.Balance == nil || *allowance.Credits.Balance != "12.5" {
		t.Fatalf("secondary window or credits changed: %s", document)
	}
	var available bool
	if err = pool.QueryRow(t.Context(), `SELECT olp.code_account_available(a) FROM olp.code_accounts a WHERE id=$1`, account).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatalf("account with remaining allowance became unavailable: %s", document)
	}
}

func TestMigrationsApplyConcurrentlyAndRefuseChangedHistory(t *testing.T) {
	pool, _ := newDatabase(t)
	var group sync.WaitGroup
	failures := make(chan error, 2)
	for range 2 {
		group.Go(func() { failures <- database.Migrate(t.Context(), pool) })
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	first, err := database.Installation(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	second, err := database.Installation(t.Context(), pool)
	if err != nil || first != second {
		t.Fatalf("installation identity changed from %s to %s: %v", first, second, err)
	}
	if _, err = pool.Exec(t.Context(), "UPDATE olp.migrations SET checksum='changed'"); err != nil {
		t.Fatal(err)
	}
	if database.Migrate(t.Context(), pool) == nil {
		t.Fatal("migrate accepted a changed checksum")
	}
}

// TestServiceRunsUnderRuntimeRole follows the documented deployment: the owner
// migrates and grants, and the process connects as the restricted runtime role.
func TestServiceRunsUnderRuntimeRole(t *testing.T) {
	pool, databaseURL := newDatabase(t)
	role := "runtime_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{role}.Sanitize()
	if _, err := pool.Exec(t.Context(), "CREATE ROLE "+identifier+" LOGIN PASSWORD 'runtime-test'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "DROP OWNED BY "+identifier); err != nil {
			t.Errorf("drop owned: %v", err)
		}
		if _, err := pool.Exec(ctx, "DROP ROLE "+identifier); err != nil {
			t.Errorf("drop role: %v", err)
		}
	})
	env := newEnvironment(t, databaseURL)
	runBinary(t, env, "migrate", "--runtime-role", role)

	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword(role, "runtime-test")
	env.values["OLP_DATABASE_URL"] = parsed.String()
	s := startProcess(t, pool, env)
	owner := s.owner()
	s.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "runtime-role"}, idempotent(), http.StatusCreated)
	s.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "runtime-role", "scopes": []string{"inference"}}, idempotent(), http.StatusCreated)

	config, err := pgx.ParseConfig(parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	restricted, err := pgx.ConnectConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer restricted.Close(context.Background())
	if _, err = restricted.Exec(t.Context(), "DELETE FROM olp.migrations"); err == nil {
		t.Fatal("the runtime role can rewrite migration history")
	}
}

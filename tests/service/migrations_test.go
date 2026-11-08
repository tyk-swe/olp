//go:build service

package service_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	migration, err := os.ReadFile("../../internal/database/migrations/0137_code_allowance_observation.sql")
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

func TestCodeDecisionsMigrationKeepsRecordsContentFree(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	user, project, provider, credential, account, other, key, route, revision, binding, attempt := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id, email, display_name, role, etag) VALUES($1, 'decision@example.invalid', 'Decision', 'owner', $2)`, []any{user, uuid.NewString()}},
		{`INSERT INTO olp.projects(id, name, etag, created_by) VALUES($1, 'decision-migration', $2, $3)`, []any{project, uuid.NewString(), user}},
		{`INSERT INTO olp.providers(id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, 'decision-migration', 'plugin', 'active', '{}', $2, $3, $4)`, []any{provider, uuid.NewString(), uuid.NewString(), user}},
		{`INSERT INTO olp.provider_credentials(id, provider_id, version) VALUES($1, $2, 1)`, []any{credential, provider}},
		{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, etag, created_by) VALUES($1, $2, $3, $4, 'principal', 'account', '["model"]', $5, $6)`, []any{account, project, provider, credential, uuid.NewString(), user}},
		{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, etag, created_by) VALUES($1, $2, $3, $4, 'other-principal', 'other', '["model"]', $5, $6)`, []any{other, project, provider, credential, uuid.NewString(), user}},
		{`INSERT INTO olp.api_keys(id, lookup_id, digest, name, created_by, policy, etag, project_id) VALUES($1, 'decision-lookup', '\x00', 'decision', $2, '{}', $3, $4)`, []any{key, user, uuid.NewString(), project}},
		{`INSERT INTO olp.code_routes(id, project_id, slug, draft, etag, created_by) VALUES($1, $2, 'decision-route', '{}', $3, $4)`, []any{route, project, uuid.NewString(), user}},
		{`INSERT INTO olp.code_route_revisions(id, route_id, revision, document, connections, created_by) VALUES($1, $2, 1, '{}', '{}', $3)`, []any{revision, route, user}},
		{`INSERT INTO olp.code_bindings(id, project_id, route_id, api_key_id, conversation, root_id, account_id, principal) VALUES($1, $2, $3, $4, 'conversation', $1, $5, 'principal')`, []any{binding, project, route, key, account}},
		{`INSERT INTO olp.code_attempts(id, project_id, route_id, route_revision_id, api_key_id, binding_id, account_id, operation, model, state, reserved_tokens) VALUES($1, $2, $3, $4, $5, $6, $7, 'responses', 'model', 'prepared', 0)`, []any{attempt, project, route, revision, key, binding, account}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	insert := `INSERT INTO olp.code_decisions(id,attempt_id,project_id,route_id,route_revision_id,api_key_id,binding_id,root_id,chosen_account_id,reason,band,evidence,evidence_observed_at,skipped,recovery_envelope,recovery_ordinal)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`
	skipped := `[{"account_id":"` + other + `","class":"cooldown"}]`
	if _, err := pool.Exec(t.Context(), insert, uuid.NewString(), attempt, project, route, revision, key, binding, binding, account, "chosen", nil, "passive", time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC), skipped, "codex_usage_limit_reached", 1); err != nil {
		t.Fatal(err)
	}
	var document []byte
	if err := pool.QueryRow(t.Context(), `SELECT to_jsonb(x) FROM olp.code_decisions x WHERE x.attempt_id=$1 AND x.chosen_account_id=$2`, attempt, account).Scan(&document); err != nil {
		t.Fatal(err)
	}
	var decision codemode.Decision
	if err := json.Unmarshal(document, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Reason != codemode.DecisionChosen || decision.Band != nil || decision.Evidence == nil || *decision.Evidence != codemode.AllowanceEvidencePassive ||
		len(decision.Skipped) != 1 || decision.Skipped[0].AccountID != other || decision.Skipped[0].Class != codemode.SkipCooldown ||
		decision.RecoveryEnvelope == nil || *decision.RecoveryEnvelope != "codex_usage_limit_reached" || decision.RecoveryOrdinal == nil || *decision.RecoveryOrdinal != 1 {
		t.Fatalf("decision projection = %s", document)
	}
	if _, err := pool.Exec(t.Context(), insert, uuid.NewString(), attempt, project, route, revision, key, binding, binding, account, "pin", nil, nil, nil, "[]", nil, nil); err == nil {
		t.Fatal("second decision for one attempt accepted")
	}
	for range 2 {
		if _, err := pool.Exec(t.Context(), insert, uuid.NewString(), nil, project, route, revision, key, binding, binding, account, "connection", nil, nil, nil, "[]", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	rejected := []struct {
		name string
		tail []any
	}{
		{"unknown reason", []any{"guessed", nil, nil, nil, "[]", nil, nil}},
		{"evidence without observation time", []any{"chosen", nil, "passive", nil, "[]", nil, nil}},
		{"free-form recovery envelope", []any{"chosen", nil, nil, nil, "[]", "raw error body", nil}},
		{"skipped not an array", []any{"chosen", nil, nil, nil, "{}", nil, nil}},
	}
	for _, rejectedInsert := range rejected {
		fresh := uuid.NewString()
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.code_attempts(id, project_id, route_id, route_revision_id, api_key_id, binding_id, account_id, operation, model, state, reserved_tokens) VALUES($1, $2, $3, $4, $5, $6, $7, 'responses', 'model', 'prepared', 0)`, fresh, project, route, revision, key, binding, account); err != nil {
			t.Fatal(err)
		}
		args := append([]any{uuid.NewString(), fresh, project, route, revision, key, binding, binding, account}, rejectedInsert.tail...)
		if _, err := pool.Exec(t.Context(), insert, args...); err == nil {
			t.Fatalf("%s accepted", rejectedInsert.name)
		}
	}
}

func migrateBefore(t *testing.T, pool *pgxpool.Pool, version string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `CREATE SCHEMA IF NOT EXISTS olp;
		CREATE TABLE IF NOT EXISTS olp.migrations (
			version text PRIMARY KEY, checksum bytea NOT NULL, applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatal(err)
	}
	directory := "../../internal/database/migrations"
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name >= version {
			return
		}
		var applied bool
		if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp.migrations WHERE version=$1)`, name).Scan(&applied); err != nil {
			t.Fatal(err)
		}
		if applied {
			continue
		}
		sql, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), string(sql)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		checksum := sha256.Sum256(sql)
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.migrations(version,checksum) VALUES($1,$2)`, name, checksum[:]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpgradeKeepsLegacyBillingGapsUnimported(t *testing.T) {
	pool, _ := newDatabase(t)
	migrateBefore(t, pool, "0127")
	user, provider := uuid.NewString(), uuid.NewString()
	legacy := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id, email, display_name, role, etag) VALUES($1, 'billing-gap@example.invalid', 'Billing gap', 'owner', $2)`, []any{user, uuid.NewString()}},
		{`INSERT INTO olp.providers(id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, 'billing-gap', 'plugin', 'active', '{}', $2, $3, $4)`, []any{provider, uuid.NewString(), uuid.NewString(), user}},
		{`INSERT INTO olp.provider_billing_imports(provider_id, source, scope, etag, created_by, last_success_at, imported_from, imported_through)
			VALUES($1, 'openai_costs', '{}', $2, $3, now() - interval '1 day', '2026-10-01', '2026-10-26')`, []any{provider, uuid.NewString(), user}},
		{`INSERT INTO olp.provider_invoiced_costs(provider_id, day, line_item, currency, amount, source)
			VALUES($1, '2026-10-05', 'gpt', 'USD', 5, 'openai_costs'), ($1, '2026-10-24', 'gpt', 'USD', 7, 'openai_costs')`, []any{provider}},
	}
	for _, statement := range legacy {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	migrateBefore(t, pool, "0141")
	if _, err := pool.Exec(t.Context(), `UPDATE olp.provider_billing_days SET last_success_at=now() WHERE provider_id=$1 AND day='2026-10-17'`, provider); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	var covered []string
	if err := pool.QueryRow(t.Context(), `SELECT COALESCE(array_agg(day::text ORDER BY day), '{}') FROM olp.provider_billing_days WHERE provider_id=$1`, provider).Scan(&covered); err != nil {
		t.Fatal(err)
	}
	if strings.Join(covered, ",") != "2026-10-05,2026-10-17,2026-10-24" {
		t.Fatalf("covered days = %v, want fetched invoice days and the day refetched after the upgrade", covered)
	}
	var invoicedKept bool
	var from, through string
	if err := pool.QueryRow(t.Context(), `SELECT (SELECT sum(amount)=12 FROM olp.provider_invoiced_costs WHERE provider_id=$1),
		COALESCE(imported_from::text, ''), COALESCE(imported_through::text, '') FROM olp.provider_billing_imports WHERE provider_id=$1`, provider).Scan(&invoicedKept, &from, &through); err != nil {
		t.Fatal(err)
	}
	if !invoicedKept || from != "2026-10-24" || through != "2026-10-24" {
		t.Fatalf("invoiced kept=%t imported_from=%q imported_through=%q", invoicedKept, from, through)
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
	if _, err = restricted.Exec(t.Context(), "SELECT olp.dirty_commitment(date_trunc('day', now()), date_trunc('day', now()) + interval '1 day')"); err != nil {
		t.Fatalf("the runtime role cannot mark commitment usage for export: %v", err)
	}
	var dirtyDays int
	if err = restricted.QueryRow(t.Context(), "SELECT count(*) FROM olp.focus_dirty_days").Scan(&dirtyDays); err != nil || dirtyDays != 1 {
		t.Fatalf("commitment export days = %d, want 1: %v", dirtyDays, err)
	}
	if _, err = restricted.Exec(t.Context(), "DELETE FROM olp.migrations"); err == nil {
		t.Fatal("the runtime role can rewrite migration history")
	}
}

func TestNotificationEventConstraintMatchesPublishedEvents(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	var definition string
	if err := pool.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='notification_rules_event_check'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	allowed := slices.Compact(slices.Sorted(func(yield func(string) bool) {
		for _, match := range regexp.MustCompile(`'([a-z_.]+)'`).FindAllStringSubmatch(definition, -1) {
			if !yield(match[1]) {
				return
			}
		}
	}))
	raw, err := os.ReadFile(filepath.Join("..", "..", "management", "management.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Components struct {
			Schemas struct {
				NotificationEvent struct {
					Enum []string `json:"enum"`
				} `json:"NotificationEvent"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err = json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	published := slices.Sorted(slices.Values(spec.Components.Schemas.NotificationEvent.Enum))
	if len(published) == 0 || !slices.Equal(allowed, published) {
		t.Fatalf("database allows %v, API publishes %v", allowed, published)
	}
}

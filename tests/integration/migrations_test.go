//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/tests/harness"
)

func TestCodeDecisionsKeepRecordsContentFree(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
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
	insert := `INSERT INTO olp.code_decisions(id,attempt_id,project_id,route_id,route_revision_id,api_key_id,binding_id,root_id,chosen_account_id,reason,skipped,recovery_envelope,recovery_ordinal)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
	skipped := `[{"account_id":"` + other + `","class":"cooldown"}]`
	if _, err := pool.Exec(t.Context(), insert, uuid.NewString(), attempt, project, route, revision, key, binding, binding, account, "chosen", skipped, "codex_usage_limit_reached", 1); err != nil {
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
	if decision.Reason != codemode.DecisionChosen ||
		len(decision.Skipped) != 1 || decision.Skipped[0].AccountID != other || decision.Skipped[0].Class != codemode.SkipCooldown ||
		decision.RecoveryEnvelope == nil || *decision.RecoveryEnvelope != "codex_usage_limit_reached" || decision.RecoveryOrdinal == nil || *decision.RecoveryOrdinal != 1 {
		t.Fatalf("decision projection = %s", document)
	}
	if _, err := pool.Exec(t.Context(), insert, uuid.NewString(), attempt, project, route, revision, key, binding, binding, account, "pin", "[]", nil, nil); err == nil {
		t.Fatal("second decision for one attempt accepted")
	}
	for range 2 {
		if _, err := pool.Exec(t.Context(), insert, uuid.NewString(), nil, project, route, revision, key, binding, binding, account, "connection", "[]", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	rejected := []struct {
		name string
		tail []any
	}{
		{"unknown reason", []any{"guessed", "[]", nil, nil}},
		{"free-form recovery envelope", []any{"chosen", "[]", "raw error body", nil}},
		{"skipped not an array", []any{"chosen", "{}", nil, nil}},
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

func TestMigrationsApplyConcurrentlyAndRefuseChangedHistory(t *testing.T) {
	t.Parallel()
	pool, _ := harness.EmptyDatabase(t)
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
	if _, err := database.Installation(t.Context(), pool); err == nil {
		t.Fatal("installation accepted a changed checksum")
	}
	if _, err = pool.Exec(t.Context(), "DELETE FROM olp.migrations; INSERT INTO olp.migrations(version,checksum) VALUES('0168_retire_allowance_ordering.sql','old')"); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(t.Context(), pool); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Fatalf("migrate upgraded a pre-baseline history: %v", err)
	}
	if _, err := database.Installation(t.Context(), pool); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Fatalf("installation accepted a pre-baseline history: %v", err)
	}
}

func TestMigrationsIgnoreSessionStatementAndLockTimeouts(t *testing.T) {
	t.Parallel()
	const statementTimeout = time.Second
	const lockTimeout = 100 * time.Millisecond
	holder, address := harness.EmptyDatabase(t)
	config, err := database.Configuration(address, 2, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["statement_timeout"] = statementTimeout.String()
	config.ConnConfig.RuntimeParams["lock_timeout"] = lockTimeout.String()
	pool, err := database.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tx, err := holder.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), "SELECT pg_advisory_xact_lock(726419823071)"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- database.Migrate(ctx, pool) }()
	holderPID := int32(tx.Conn().PgConn().PID())
	harness.Eventually(t, "migration waiting for the advisory lock", 5*time.Second, func() (bool, string) {
		select {
		case err := <-result:
			t.Fatalf("migration returned before its lock wait was observed: %v", err)
		default:
		}
		var blocked bool
		if err := holder.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		return blocked, "PostgreSQL has not reported the migration blocked by the lock holder"
	})
	select {
	case err := <-result:
		t.Fatalf("migration returned while the advisory lock was held: %v", err)
	case <-time.After(statementTimeout + lockTimeout):
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestNotificationEventConstraintMatchesPublishedEvents(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
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

func TestNotificationBudgetEvidenceFreezesSubjectAtDelivery(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	user, key, nextKey, destination, rule := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, s := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1,'snapshot@example.invalid','Snapshot','owner',$1)`, []any{user}},
		{`INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag) VALUES($1::uuid,$1::text,decode('00','hex'),'first',$3,'{}',$1::uuid),($2::uuid,$2::text,decode('01','hex'),'second',$3,'{}',$2::uuid)`, []any{key, nextKey, user}},
		{`INSERT INTO olp.notification_destinations(id,name,url,etag,created_by) VALUES($1,'snapshot','https://example.invalid/notify',$1,$2)`, []any{destination, user}},
		{`INSERT INTO olp.notification_rules(id,name,event,subject_kind,subject_id,window_kind,threshold_percent,destination_id,etag,created_by) VALUES($1,'snapshot','budget.threshold','api_key',$2,'day',75,$3,$1,$4)`, []any{rule, key, destination, user}},
		{`INSERT INTO olp.notification_deliveries(id,rule_id,window_id,threshold_percent,accrued,limit_amount,status) VALUES($1,$2,1,75,80,100,'failed')`, []any{uuid.NewString(), rule}},
	} {
		if _, err := pool.Exec(t.Context(), s.sql, s.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_deliveries(id,rule_id,window_id,threshold_percent,accrued,limit_amount,status) VALUES($1,$2,2,75,80,100,'pending')`, uuid.NewString(), rule); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.notification_rules SET subject_id=$2,window_kind='month' WHERE id=$1`, rule, nextKey); err != nil {
		t.Fatal(err)
	}
	var count int
	var preserved bool
	if err := pool.QueryRow(t.Context(), `SELECT count(*),bool_and(budget_subject_kind='api_key' AND budget_subject_id=$2 AND budget_window_kind='day' AND accrued=80 AND limit_amount=100) FROM olp.notification_deliveries WHERE rule_id=$1`, rule, key).Scan(&count, &preserved); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.notification_deliveries(id,rule_id,window_id,threshold_percent,accrued,limit_amount,status,budget_subject_kind) VALUES($1,$2,3,75,80,100,'pending','api_key')`, uuid.NewString(), rule); err == nil {
		t.Fatal("partial budget identity snapshot accepted")
	}
	if count != 2 || !preserved {
		t.Fatalf("delivery evidence changed after retarget: rows=%d preserved=%t", count, preserved)
	}
}

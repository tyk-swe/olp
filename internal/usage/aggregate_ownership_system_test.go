//go:build integration

package usage

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/limits"
)

func aggregateProject(t *testing.T, pool *pgxpool.Pool, owner string, organization *string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.projects(id,name,etag,created_by,organization_id,budget_policy,attribution_budgets)
        VALUES($1,$2,$3,$4,$5,'{"daily_cost_limit":"100","weekly_cost_limit":"100","monthly_cost_limit":"100"}',
        '{"team":{"blue":{"daily_cost_limit":"100"}}}')`, id, "project-"+id, uuid.NewString(), owner, organization); err != nil {
		t.Fatal(err)
	}
	return id
}

func aggregateAccount(t *testing.T, pool *pgxpool.Pool, level, subject string) string {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := limits.EnsureAggregateBudget(t.Context(), tx, level, subject); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return limits.AggregateBudgetID(level, subject)
}

func aggregateBalance(t *testing.T, pool *pgxpool.Pool, account string, want string) {
	t.Helper()
	var got string
	if err := pool.QueryRow(t.Context(), `SELECT accrued::text FROM olp.aggregate_cost_windows WHERE account_id=$1 AND window_kind='month'`, account).Scan(&got); err != nil || got != want {
		t.Fatalf("aggregate balance %s: got %q want %q: %v", account, got, want, err)
	}
}

func aggregateReport(t *testing.T, pool *pgxpool.Pool, table, subject, query, want string) {
	t.Helper()
	var report struct {
		Monthly struct {
			Accrued string `json:"accrued"`
		} `json:"monthly"`
	}
	var raw []byte
	if err := pool.QueryRow(t.Context(), "SELECT "+query+" FROM olp."+table+" p WHERE p.id=$1", subject).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &report); err != nil || parityCost(t, report.Monthly.Accrued).Cmp(parityCost(t, want)) != 0 {
		t.Fatalf("reported balance %q want %q: %v", report.Monthly.Accrued, want, err)
	}
}

func aggregateObservedAt() time.Time {
	now := time.Now().UTC()
	observed := now.Add(-time.Minute)
	if observed.Year() != now.Year() || observed.Month() != now.Month() {
		return now
	}
	return observed
}

func aggregatePriceCase(model, rate string) parityCase {
	zero, one := int64(0), int64(1)
	zeroPrice := "0"
	return parityCase{model: model, price: Price{Model: model, InputPerMillion: &rate, OutputPerMillion: &zeroPrice},
		usage: AttemptUsage{Observed: true, Complete: true, InputTokens: &one, OutputTokens: &zero}}
}

// The receipt gate must not apply the same per-owner facts twice.

// Shared installation provider.

// Run the actual transactional rollup, then prove both retained reporting
// and a fresh reconstruction agree with the incremental charge subjects.

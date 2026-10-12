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

func TestIntegrationAggregateDeltasKeepEachAttemptProviderOwner(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	firstProvider, finalProvider := seedParityProvider(t, pool, owner), seedParityProvider(t, pool, owner)
	firstProject, finalProject := aggregateProject(t, pool, owner, nil), aggregateProject(t, pool, owner, nil)
	for _, pair := range [][2]string{{firstProvider, firstProject}, {finalProvider, finalProject}} {
		if _, err := pool.Exec(t.Context(), `UPDATE olp.providers SET project_id=$2 WHERE id=$1`, pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	firstAccount, finalAccount := aggregateAccount(t, pool, "project", firstProject), aggregateAccount(t, pool, "project", finalProject)
	observed := aggregateObservedAt()
	cases := []parityCase{aggregatePriceCase("first", "1000000"), aggregatePriceCase("final", "2000000")}
	seedParityPrices(t, pool, owner, observed.Add(-time.Hour), cases)
	for range 2 {
		event := parityEvent(t, key, firstProvider, observed, cases)
		event.Attempts[1].ProviderID = finalProvider
		event.ProviderID = &event.Attempts[1].ProviderID
		persistParity(t, pool, event)
		// The receipt gate must not apply the same per-owner facts twice.
		duplicate, _ := persistParity(t, pool, event)
		if duplicate.Outcome != PersistOutcomeDuplicate {
			t.Fatal("redelivery applied aggregate spend twice", duplicate.Outcome)
		}
	}
	aggregateBalance(t, pool, firstAccount, "2.000000000000")
	aggregateBalance(t, pool, finalAccount, "4.000000000000")
	aggregateReport(t, pool, "projects", firstProject, limits.AggregateBudgetSQL(true), "2")
	aggregateReport(t, pool, "projects", finalProject, limits.AggregateBudgetSQL(true), "4")
}

func TestIntegrationAggregateRouteOwnershipSurvivesRetentionAndRebuild(t *testing.T) {
	pool := notificationPool(t)
	owner, _ := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner) // Shared installation provider.
	organization := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.organizations(id,name,etag,created_by,budget_policy)
        VALUES($1,'aggregate-owner',$2,$3,'{"daily_cost_limit":"100"}')`, organization, uuid.NewString(), owner); err != nil {
		t.Fatal(err)
	}
	project := aggregateProject(t, pool, owner, &organization)
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.routes(id,slug,created_by,latest_revision,latest_revision_id,etag,project_id)
        VALUES($1,'parity',$2,1,$3,$4,$5)`, uuid.NewString(), owner, uuid.NewString(), uuid.NewString(), project); err != nil {
		t.Fatal(err)
	}
	projectAccount, organizationAccount := aggregateAccount(t, pool, "project", project), aggregateAccount(t, pool, "organization", organization)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := limits.EnsureAttributionBudget(t.Context(), tx, project, "team", "blue"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	attributionAccount := limits.AttributionBudgetID(project, "team", "blue")
	observed := aggregateObservedAt()
	cases := []parityCase{aggregatePriceCase("shared", "1000000")}
	seedParityPrices(t, pool, owner, observed.Add(-time.Hour), cases)
	for range 2 {
		event := parityEvent(t, "", provider, observed, cases)
		event.Origin, event.Attribution = OriginProbe, map[string]string{"team": "blue"}
		persistParity(t, pool, event)
	}
	accounts := []string{projectAccount, organizationAccount, attributionAccount}
	for _, id := range accounts {
		aggregateBalance(t, pool, id, "2.000000000000")
	}
	aggregateReport(t, pool, "projects", project, limits.AggregateBudgetSQL(true), "2")
	aggregateReport(t, pool, "organizations", organization, limits.OrganizationBudgetSQL, "2")
	aggregateReport(t, pool, "projects", project, "("+limits.AttributionBudgetsSQL+")#>'{team,blue}'", "2")
	// Run the actual transactional rollup, then prove both retained reporting
	// and a fresh reconstruction agree with the incremental charge subjects.
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, facts, err := rollUpAttemptUsage(t.Context(), conn.Conn(), observed.Add(2*time.Hour))
	conn.Release()
	if err != nil || facts != 2 {
		t.Fatal("rollup did not retain the ownership fixture", facts, err)
	}
	aggregateReport(t, pool, "projects", project, limits.AggregateBudgetSQL(true), "2")
	aggregateReport(t, pool, "organizations", organization, limits.OrganizationBudgetSQL, "2")
	aggregateReport(t, pool, "projects", project, "("+limits.AttributionBudgetsSQL+")#>'{team,blue}'", "2")
	if _, err := pool.Exec(t.Context(), `DELETE FROM olp.aggregate_cost_windows WHERE account_id=ANY($1::uuid[])`, accounts); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.aggregate_budget_accounts SET initialized=false WHERE id=ANY($1::uuid[])`, accounts); err != nil {
		t.Fatal(err)
	}
	conn, err = pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = limits.ReconciliationSnapshots(t.Context(), conn.Conn(), time.Now())
	conn.Release()
	if err != nil {
		t.Fatal("reconstruct retained balances", err)
	}
	for _, id := range accounts {
		aggregateBalance(t, pool, id, "2.000000000000")
	}
}

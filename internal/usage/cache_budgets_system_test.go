//go:build integration

package usage

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/limits"
)

func TestIntegrationCacheBudgetDebitsSurviveReconstructionAndRetention(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	organization := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.organizations(id,name,etag,created_by)
        VALUES($1,'Cache organization',$1,$2)`, organization, owner); err != nil {
		t.Fatal(err)
	}
	project := aggregateProject(t, pool, owner, &organization)
	group := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.budget_groups(id,name,project_id,daily_cost_limit,weekly_cost_limit,monthly_cost_limit,etag,created_by)
        VALUES($1,'Cache group',$2,100,100,100,$1,$3)`, group, project, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.api_keys SET project_id=$2,budget_group_id=$3 WHERE id=$1`, key, project, group); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.routes(id,slug,created_by,latest_revision,latest_revision_id,etag,project_id)
        VALUES($1,'parity',$2,1,$3,$1,$4)`, uuid.NewString(), owner, uuid.NewString(), project); err != nil {
		t.Fatal(err)
	}
	var installation string
	if err := pool.QueryRow(t.Context(), `SELECT id::text FROM olp.installation WHERE singleton`).Scan(&installation); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC()
	digest := strings.Repeat("ab", 32)
	cases := []parityCase{aggregatePriceCase("cached", "1000000")}
	hit := func(debit, exempt bool) *Event {
		event := parityEvent(t, key, provider, observed, cases)
		event.BudgetGroupID = &group
		event.Attribution = map[string]string{"team": "blue"}
		event.EndUserDigest = digest
		event.CacheResult = "hit"
		evidence := &CacheEvidence{Result: "hit", AvoidedCost: "2", Currency: "USD"}
		if debit {
			evidence.BudgetCost, evidence.BudgetCharge = "2", "3"
		}
		event.Attempts[0].Routing = &Routing{Cache: evidence, BudgetExempt: exempt}
		if exempt {
			event.Attempts[0].Routing.CredentialSource = "caller"
		}
		return event
	}
	// Aggregate accounts created after a hit must initialize from its debit.
	persistParity(t, pool, hit(true, false))
	accounts := []string{
		aggregateAccount(t, pool, "installation", installation),
		aggregateAccount(t, pool, "organization", organization),
		aggregateAccount(t, pool, "project", project),
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := limits.EnsureAttributionBudget(t.Context(), tx, project, "team", "blue"); err != nil {
		t.Fatal(err)
	}
	if err := limits.EnsureKeyRouteBudget(t.Context(), tx, key, "parity"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	accounts = append(accounts, limits.AttributionBudgetID(project, "team", "blue"), limits.KeyRouteBudgetID(key, "parity"))

	check := func(cost, charge string) {
		t.Helper()
		for _, scope := range []struct {
			table, column string
			ids           []string
		}{
			{"api_key_cost_windows", "api_key_id", []string{key}},
			{"budget_group_cost_windows", "budget_group_id", []string{group}},
			{"end_user_cost_windows", "account_id", []string{limits.EndUserKeyID(key, digest), limits.EndUserProjectID(project, digest)}},
			{"aggregate_cost_windows", "account_id", accounts},
		} {
			for _, id := range scope.ids {
				for _, window := range []string{"day", "week", "month"} {
					var gotCost, gotCharge string
					err := pool.QueryRow(t.Context(), "SELECT accrued::text,charged::text FROM olp."+scope.table+" WHERE "+scope.column+"=$1 AND window_kind=$2", id, window).Scan(&gotCost, &gotCharge)
					if err != nil || gotCost != cost || gotCharge != charge {
						t.Fatalf("%s %s %s: cost=%s charge=%s, want %s/%s: %v", scope.table, id, window, gotCost, gotCharge, cost, charge, err)
					}
				}
			}
		}
		aggregateReport(t, pool, "projects", project, limits.AggregateBudgetSQL(true), cost)
		aggregateReport(t, pool, "organizations", organization, limits.OrganizationBudgetSQL, cost)
		aggregateReport(t, pool, "projects", project, "("+limits.AttributionBudgetsSQL+")#>'{team,blue}'", cost)
		// Cost-basis and charge-basis reports must use the same debit history.
		if _, err := pool.Exec(t.Context(), `UPDATE olp.projects SET budget_policy=jsonb_set(budget_policy,'{cost_basis}','"charge"') WHERE id=$1`, project); err != nil {
			t.Fatal(err)
		}
		aggregateReport(t, pool, "projects", project, limits.AggregateBudgetSQL(true), charge)
		if _, err := pool.Exec(t.Context(), `UPDATE olp.projects SET budget_policy=jsonb_set(budget_policy,'{cost_basis}','"cost"') WHERE id=$1`, project); err != nil {
			t.Fatal(err)
		}
	}
	rebuild := func() {
		t.Helper()
		for _, table := range []string{"api_key_cost_windows", "budget_group_cost_windows", "end_user_cost_windows", "aggregate_cost_windows"} {
			if _, err := pool.Exec(t.Context(), "DELETE FROM olp."+table); err != nil {
				t.Fatal(err)
			}
		}
		conn, err := pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err := limits.ReconciliationSnapshots(t.Context(), conn.Conn(), observed); err != nil {
			t.Fatal(err)
		}
	}
	rollup := func() {
		t.Helper()
		conn, err := pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, _, err := rollUpAttemptUsage(t.Context(), conn.Conn(), observed.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	second := hit(true, false)
	persistParity(t, pool, second)
	check("4.000000000000", "6.000000000000")
	duplicate, _ := persistParity(t, pool, second)
	if duplicate.Outcome != PersistOutcomeDuplicate {
		t.Fatal("cache-hit redelivery was not deduplicated")
	}
	persistParity(t, pool, hit(false, false))
	persistParity(t, pool, hit(true, true))
	check("4.000000000000", "6.000000000000")
	rebuild()
	check("4.000000000000", "6.000000000000")
	rollup()
	rebuild()
	check("4.000000000000", "6.000000000000")
	// A later delivery in an already retained hour must add its debit once.
	persistParity(t, pool, hit(true, false))
	check("6.000000000000", "9.000000000000")
	rollup()
	rebuild()
	check("6.000000000000", "9.000000000000")
	var paidCost, paidCharge string
	if err := pool.QueryRow(t.Context(), `SELECT COALESCE(sum(estimated_cost),0)::text,COALESCE(sum(charge),0)::text FROM olp.attempt_usage_hourly`).Scan(&paidCost, &paidCharge); err != nil || parityCost(t, paidCost).Cmp(parityCost(t, "0")) != 0 || parityCost(t, paidCharge).Cmp(parityCost(t, "0")) != 0 {
		t.Fatalf("cache debits changed provider billing: cost=%s charge=%s error=%v", paidCost, paidCharge, err)
	}
}

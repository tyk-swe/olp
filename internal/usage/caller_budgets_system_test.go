//go:build integration

package usage

import (
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
)

func TestIntegrationCallerPaidUsageSurvivesRetentionWithoutBudgetAccrual(t *testing.T) {
	pool := notificationPool(t)
	owner, _ := seedParityOwner(t, pool)
	key := seedParityKey(t, pool, owner)
	provider := seedParityProvider(t, pool, owner)
	var revision string
	if err := pool.QueryRow(t.Context(), "SELECT active_revision_id::text FROM olp.providers WHERE id=$1", provider).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	input, output := "1000000", "0"
	count := int64(2)
	cases := []parityCase{{model: "paid", price: Price{Model: "paid", InputPerMillion: &input, OutputPerMillion: &output}, usage: AttemptUsage{Observed: true, Complete: true, InputTokens: &count}}}
	seedParityPrices(t, pool, owner, at.Add(-time.Minute), cases)
	var installation string
	if err := pool.QueryRow(t.Context(), "SELECT id::text FROM olp.installation").Scan(&installation); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = limits.EnsureAggregateBudget(t.Context(), tx, "installation", installation); err != nil {
		t.Fatal(err)
	}
	if err = limits.EnsureKeyRouteBudget(t.Context(), tx, key, "parity"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, exempt := range []bool{true, false} {
		ev := parityEvent(t, key, provider, at, cases)
		ev.EndUserDigest = strings.Repeat("a", 64)
		ev.Attempts[0].Routing = &Routing{ProviderRevisionID: revision, BudgetExempt: exempt, CredentialSource: "caller"}
		persistParity(t, pool, ev)
	}
	check := func() {
		t.Helper()
		conn, e := pool.Acquire(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		defer conn.Release()
		snapshots, e := limits.ReconciliationSnapshots(t.Context(), conn.Conn(), at)
		if e != nil {
			t.Fatal(e)
		}
		for _, id := range []string{key, limits.AggregateBudgetID("installation", installation), limits.KeyRouteBudgetID(key, "parity"), limits.EndUserKeyID(key, strings.Repeat("a", 64))} {
			found := false
			for _, s := range snapshots {
				if s.CostOwnerID == id {
					found = true
					if s.DailyAccrued != "2.000000000000" {
						t.Fatalf("owner %s accrued %s", id, s.DailyAccrued)
					}
				}
			}
			if !found {
				t.Fatalf("missing owner %s", id)
			}
		}
	}

	check()
	var rolled, expired int64
	if err = pool.QueryRow(t.Context(), rollupSQL, at.Add(time.Hour), 100).Scan(&rolled, &expired); err != nil {
		t.Fatal(err)
	}
	if rolled != 2 || expired != 2 {
		t.Fatalf("exempt dimensions merged: %d/%d", rolled, expired)
	}
	for _, table := range []string{"api_key_cost_windows", "end_user_cost_windows", "aggregate_cost_windows"} {
		if _, err = pool.Exec(t.Context(), "DELETE FROM olp."+table); err != nil {
			t.Fatal(err)
		}
	}
	check()
	var total string
	if err = pool.QueryRow(t.Context(), "SELECT sum(estimated_cost)::text FROM olp.attempt_usage_hourly WHERE api_key_id=$1::uuid", key).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != "4.000000000000" {
		t.Fatalf("caller usage lost: %s", total)
	}
}

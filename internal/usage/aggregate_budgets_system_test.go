//go:build integration

package usage

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/limits"
)

func TestIntegrationAggregateBudgetsIncludeSystemWorkAndHistory(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	project := uuid.NewString()
	organization := uuid.NewString()
	if _, err := pool.Exec(t.Context(), "INSERT INTO olp.organizations(id,name,etag,created_by) VALUES($1,'Aggregate organization',$1,$2)", organization, owner); err != nil {
		t.Fatal(err)
	}
	var installation string
	if err := pool.QueryRow(t.Context(), "SELECT id::text FROM olp.installation WHERE singleton").Scan(&installation); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "INSERT INTO olp.projects(id,name,etag,created_by,organization_id) VALUES ($1,'Aggregate project',$2,$3,$4)", project, uuid.NewString(), owner, organization); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"api_keys", "providers"} {
		id := key
		if table == "providers" {
			id = provider
		}
		if _, err := pool.Exec(t.Context(), "UPDATE olp."+table+" SET project_id=$2 WHERE id=$1", id, project); err != nil {
			t.Fatal(err)
		}
	}
	rate := "1000000"
	input := int64(2)
	at := time.Now().UTC().Add(-time.Second)
	cases := []parityCase{{model: "priced", price: Price{Model: "priced", InputPerMillion: &rate}, usage: AttemptUsage{Observed: true, Complete: true, InputTokens: &input}}}
	seedParityPrices(t, pool, owner, at.Add(-time.Hour), cases)
	historical := parityEvent(t, key, provider, at, cases)
	historical.Attribution = map[string]string{"team": "core"}
	persistParity(t, pool, historical)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	for level, subject := range map[string]string{"installation": installation, "organization": organization, "project": project} {
		if err = limits.EnsureAggregateBudget(t.Context(), tx, level, subject); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"core", "edge"} {
		if err = limits.EnsureAttributionBudget(t.Context(), tx, project, "team", value); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{OriginCaller, OriginShadow, OriginProbe, OriginPlayground} {
		event := parityEvent(t, key, provider, at, cases)
		event.Origin = origin
		if origin != OriginProbe && origin != OriginPlayground {
			event.Attribution = map[string]string{"team": "core"}
		}
		if Keyless(origin) {
			event.APIKeyID = ""
			event.ParentRequestID = nil
			if origin == OriginShadow {
				parent := uuid.NewString()
				event.ParentRequestID = &parent
			}
		}
		persistParity(t, pool, event)
	}
	otherProject := uuid.NewString()
	otherKey := seedParityKey(t, pool, owner)
	otherProvider := seedParityProvider(t, pool, owner)
	if _, err = pool.Exec(t.Context(), "INSERT INTO olp.projects(id,name,etag,created_by,organization_id) VALUES($1,'Other team',$1,$2,$3)", otherProject, owner, organization); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), "UPDATE olp.api_keys SET project_id=$2 WHERE id=$1", otherKey, otherProject); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), "UPDATE olp.providers SET project_id=$2 WHERE id=$1", otherProvider, otherProject); err != nil {
		t.Fatal(err)
	}
	persistParity(t, pool, parityEvent(t, otherKey, otherProvider, at, cases))
	for level, subject := range map[string]string{"installation": installation, "organization": organization, "project": project} {
		var accrued string
		if err = pool.QueryRow(t.Context(), "SELECT accrued::text FROM olp.aggregate_cost_windows WHERE account_id=$1 AND window_kind='day'", limits.AggregateBudgetID(level, subject)).Scan(&accrued); err != nil {
			t.Fatal(err)
		}
		want := "12.000000000000"
		if level == "project" {
			want = "10.000000000000"
		}
		if accrued != want {
			t.Fatalf("%s spend %s", level, accrued)
		}
	}
	checkLabels := func() {
		t.Helper()
		conn, err := pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err = limits.ReconciliationSnapshots(t.Context(), conn.Conn(), at); err != nil {
			t.Fatal(err)
		}

		for level, subject := range map[string]string{"organization": organization, "installation": installation, "project": project} {
			var amount string
			ownerID := limits.AggregateBudgetID(level, subject)
			if e := pool.QueryRow(t.Context(), "SELECT accrued::text FROM olp.aggregate_cost_windows WHERE account_id=$1 AND window_kind='day'", ownerID).Scan(&amount); e != nil {
				t.Fatal(e)
			}
			want := "12.000000000000"
			if level == "project" {
				want = "10.000000000000"
			}
			if amount != want {
				t.Fatalf("%s historical spend %s, want %s", level, amount, want)
			}
		}

		for value, want := range map[string]string{"core": "6.000000000000", "edge": "0.000000000000"} {
			var accrued string
			if err = pool.QueryRow(t.Context(), "SELECT accrued::text FROM olp.aggregate_cost_windows WHERE account_id=$1 AND window_kind='day'", limits.AttributionBudgetID(project, "team", value)).Scan(&accrued); err != nil {
				t.Fatal(err)
			}
			if accrued != want {
				t.Fatalf("attributed system spend %s: %s want %s", value, accrued, want)
			}
		}
	}
	checkLabels()
	var rolled, expired int
	if err = pool.QueryRow(t.Context(), rollupSQL, at.Add(time.Hour), 100).Scan(&rolled, &expired); err != nil || expired != 6 {
		t.Fatalf("rollup %d/%d %v", rolled, expired, err)
	}
	if _, err = pool.Exec(t.Context(), "DELETE FROM olp.aggregate_cost_windows"); err != nil {
		t.Fatal(err)
	}
	checkLabels()
}

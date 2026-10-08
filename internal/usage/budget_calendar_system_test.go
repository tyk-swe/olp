//go:build integration

package usage

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/budgetcalendar"
	"github.com/tyk-swe/olp/internal/limits"
)

func TestIntegrationBudgetCalendarBoundariesAndPendingChanges(t *testing.T) {
	pool := notificationPool(t)
	for _, tc := range []struct{ zone, at string }{
		{"America/New_York", "2026-03-08T12:00:00Z"}, {"America/New_York", "2026-11-01T12:00:00Z"},
		{"Australia/Lord_Howe", "2026-10-04T12:00:00Z"}, {"Asia/Kathmandu", "2026-01-01T12:00:00Z"},
		{"America/Havana", "2026-11-01T12:00:00Z"}, {"Pacific/Apia", "2011-12-30T12:00:00Z"},
	} {
		t.Run(tc.zone+tc.at, func(t *testing.T) {
			at, _ := time.Parse(time.RFC3339, tc.at)
			if _, err := pool.Exec(t.Context(), "INSERT INTO olp.budget_calendar_changes SELECT k,'2000-01-01'::timestamptz,$1 FROM unnest(ARRAY['day','week','month']) k ON CONFLICT(window_kind,effective_at) DO UPDATE SET time_zone=excluded.time_zone", tc.zone); err != nil {
				t.Fatal(err)
			}
			w, err := limits.CurrentBudgetWindows(t.Context(), pool, at)
			if err != nil {
				t.Fatal(err)
			}
			calendar, _ := budgetcalendar.New(tc.zone)
			want := calendar.Windows(at)
			if !w.DailyStart.Equal(want.Day.Start) || !w.DailyEnd.Equal(want.Day.End) || !w.WeeklyStart.Equal(want.Week.Start) || !w.WeeklyEnd.Equal(want.Week.End) || !w.MonthlyStart.Equal(want.Month.Start) || !w.MonthlyEnd.Equal(want.Month.End) {
				t.Fatalf("SQL/calendar disagreement: %+v / %+v", w, want)
			}
		})
	}
	if _, err := pool.Exec(t.Context(), "DELETE FROM olp.budget_calendar_changes WHERE effective_at>'1970-01-01'"); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	before, err := limits.CurrentBudgetWindows(t.Context(), pool, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), "SELECT olp.schedule_budget_zone('Asia/Kathmandu',$1)", at); err != nil {
		t.Fatal(err)
	}
	after, err := limits.CurrentBudgetWindows(t.Context(), pool, at)
	if err != nil || before != after {
		t.Fatalf("active windows changed: %+v / %+v: %v", before, after, err)
	}
	next, err := limits.CurrentBudgetWindows(t.Context(), pool, before.DailyEnd)
	if err != nil {
		t.Fatal(err)
	}
	if !next.DailyStart.Equal(before.DailyEnd) || next.DailyID != next.DailyStart.Unix() || !next.MonthlyEnd.Equal(before.MonthlyEnd) {
		t.Fatalf("boundary transition: %+v", next)
	}
	// Changing the requested zone replaces pending changes without rewriting history.
	if _, err = pool.Exec(t.Context(), "SELECT olp.schedule_budget_zone('UTC',$1)", at); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err = pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.budget_calendar_changes WHERE effective_at>$1", at).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending=%d: %v", pending, err)
	}
}

func TestIntegrationBudgetSubhourRetentionRemainsExact(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	if _, err := pool.Exec(t.Context(), "INSERT INTO olp.budget_calendar_changes SELECT k,'2025-01-01'::timestamptz,'Asia/Kathmandu' FROM unnest(ARRAY['day','week','month']) k"); err != nil {
		t.Fatal(err)
	}
	price := "1000000"
	at := time.Now().UTC().Add(-time.Second)
	cases := []parityCase{{model: "priced", price: Price{Model: "priced", InputPerMillion: &price}}}
	seedParityPrices(t, pool, owner, at.Add(-time.Hour), cases)
	for _, sample := range []struct {
		date   string
		tokens int64
	}{{"2025-12-31T18:14:00Z", 8}, {"2025-12-31T18:16:00Z", 4}} {
		cases[0].usage = AttemptUsage{Observed: true, Complete: true, InputTokens: &sample.tokens}
		event := parityEvent(t, key, provider, at, cases)
		persistParity(t, pool, event)
		if _, err := pool.Exec(t.Context(), "UPDATE olp.attempt_usage_facts SET observed_at=$2::timestamptz WHERE request_id=$1", event.RequestID, sample.date); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		conn, err := pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()

		var report map[string]any
		reportSQL := "SELECT " + strings.ReplaceAll(limits.BudgetSQL, "now()", "$2::timestamptz") + " FROM olp.api_keys k WHERE k.id=$1"
		if err := conn.QueryRow(t.Context(), reportSQL, key, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).Scan(&report); err != nil {
			t.Fatal(err)
		}
		for period, want := range map[string]string{"daily": "4.000000000000", "weekly": "12.000000000000", "monthly": "4.000000000000"} {
			if report[period].(map[string]any)["accrued"] != want {
				t.Fatalf("report mixed %s costs: %+v", period, report)
			}
		}
		snapshots, err := limits.ReconciliationSnapshots(t.Context(), conn.Conn(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range snapshots {
			if s.CostOwnerID == key {
				if s.DailyAccrued != "4.000000000000" || s.MonthlyAccrued != "4.000000000000" || s.WeeklyAccrued != "12.000000000000" {
					t.Fatalf("mixed subhour costs: %+v", s)
				}
				return
			}
		}
		t.Fatal("missing key snapshot")
	}
	check()
	var rolled, expired int64
	if err := pool.QueryRow(t.Context(), rollupSQL, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), 100).Scan(&rolled, &expired); err != nil {
		t.Fatal(err)
	}
	if rolled != 2 || expired != 2 {
		t.Fatalf("rollup merged calendar sides: %d/%d", rolled, expired)
	}
	if _, err := pool.Exec(t.Context(), "DELETE FROM olp.api_key_cost_windows"); err != nil {
		t.Fatal(err)
	}
	check()
}

// A change requested on Saturday activates the day on Sunday, while a Monday
// first-of-month leaves week and month on UTC until Monday. The new-zone day
// then begins before both other active periods; their union alone loses spend.
func TestIntegrationBudgetMigrationDayPrecedesWeekAndMonth(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	if _, err := pool.Exec(t.Context(), `SELECT olp.schedule_budget_zone('Asia/Kathmandu','2026-05-30T12:00:00Z'::timestamptz)`); err != nil {
		t.Fatal(err)
	}
	project := uuid.NewString()
	group := uuid.NewString()
	digest := strings.Repeat("c", 64)
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.projects(id,etag,name,created_by) VALUES($1,$1,'Migrating allocations',$2)`, project, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.api_keys SET project_id=$2 WHERE id=$1`, key, project); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.providers SET project_id=$2 WHERE id=$1`, provider, project); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.budget_groups(id,etag,name,created_by,daily_cost_limit,project_id) VALUES($1,$1,'Migrating group',$2,100,$3)`, group, owner, project); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = limits.EnsureAttributionBudget(t.Context(), tx, project, "team", "core"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	price := "1000000"
	at := time.Now().UTC()
	cases := []parityCase{{model: "priced", price: Price{Model: "priced", InputPerMillion: &price}}}
	seedParityPrices(t, pool, owner, at.Add(-time.Hour), cases)
	for _, sample := range []struct {
		date   string
		tokens int64
	}{{"2026-05-31T20:00:00Z", 2}, {"2026-06-01T00:30:00Z", 4}} {
		cases[0].usage = AttemptUsage{Observed: true, Complete: true, InputTokens: &sample.tokens}
		event := parityEvent(t, key, provider, at, cases)
		event.BudgetGroupID = &group
		event.EndUserDigest = digest
		event.Attribution = map[string]string{"team": "core"}
		persistParity(t, pool, event)
		if _, err = pool.Exec(t.Context(), `UPDATE olp.attempt_usage_facts SET observed_at=$2::timestamptz WHERE request_id=$1`, event.RequestID, sample.date); err != nil {
			t.Fatal(err)
		}
	}
	observed := time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC)
	check := func() {
		t.Helper()
		conn, err := pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		var report map[string]any
		if err = conn.QueryRow(t.Context(), "SELECT "+strings.ReplaceAll(limits.BudgetSQL, "now()", "$2::timestamptz")+" FROM olp.api_keys k WHERE id=$1", key, observed).Scan(&report); err != nil {
			t.Fatal(err)
		}
		if report["daily"].(map[string]any)["accrued"] != "6.000000000000" {
			t.Fatalf("daily report lost earlier spend: %+v", report)
		}
		snapshots, err := limits.ReconciliationSnapshots(t.Context(), conn.Conn(), observed)
		if err != nil {
			t.Fatal(err)
		}
		wanted := map[string]bool{key: false, group: false, limits.EndUserKeyID(key, digest): false, limits.AttributionBudgetID(project, "team", "core"): false}
		for _, snapshot := range snapshots {
			if _, ok := wanted[snapshot.CostOwnerID]; !ok {
				continue
			}
			wanted[snapshot.CostOwnerID] = true
			if snapshot.DailyAccrued != "6.000000000000" || snapshot.WeeklyAccrued != "4.000000000000" || snapshot.MonthlyAccrued != "4.000000000000" {
				t.Fatalf("calendar union lost spend: %+v", snapshot)
			}
		}
		for id, found := range wanted {
			if !found {
				t.Fatalf("missing snapshot %s", id)
			}
		}
	}
	check()
	var rolled, expired int
	if err = pool.QueryRow(t.Context(), rollupSQL, observed.Add(time.Hour), 100).Scan(&rolled, &expired); err != nil || expired != 2 {
		t.Fatalf("rollup %d/%d %v", rolled, expired, err)
	}
	for _, table := range []string{"api_key_cost_windows", "budget_group_cost_windows", "end_user_cost_windows", "aggregate_cost_windows"} {
		if _, err = pool.Exec(t.Context(), "DELETE FROM olp."+table); err != nil {
			t.Fatal(err)
		}
	}
	check()
}

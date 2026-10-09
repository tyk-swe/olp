//go:build integration

package usage

import (
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/limits"
	"strings"
	"testing"
	"time"
)

func TestIntegrationWeeklyReconstructionCrossesMonthAndYear(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	group := uuid.NewString()
	if _, err := pool.Exec(t.Context(), "INSERT INTO olp.budget_groups(id,name,daily_cost_limit,created_by,etag) VALUES($1,'Weekly',100,$2,$3)", group, owner, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	var installation string
	if err := pool.QueryRow(t.Context(), "SELECT id::text FROM olp.installation WHERE singleton").Scan(&installation); err != nil {
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
	price := "1000000"
	at := time.Now().UTC().Add(-time.Second)
	cases := []parityCase{{model: "priced", price: Price{Model: "priced", InputPerMillion: &price}}}
	seedParityPrices(t, pool, owner, at.Add(-time.Hour), cases)
	digest := strings.Repeat("a", 64)
	for _, sample := range []struct {
		date     string
		tokens   int64
		unpriced bool
	}{{"2025-12-28T12:00:00Z", 8, false}, {"2025-12-29T12:00:00Z", 1, false}, {"2025-12-31T12:00:00Z", 2, false}, {"2026-01-01T12:00:00Z", 4, false}, {"2025-12-30T12:00:00Z", 1, true}, {"2026-01-01T12:00:00Z", 1, true}} {
		cases[0].model = "priced"
		if sample.unpriced {
			cases[0].model = "unpriced"
		}
		cases[0].usage = AttemptUsage{Observed: true, Complete: true, InputTokens: &sample.tokens}
		event := parityEvent(t, key, provider, at, cases)
		event.BudgetGroupID = &group
		event.EndUserDigest = digest
		persistParity(t, pool, event)
		// Seed historical boundaries independently of the receipt replay clock.
		if _, err = pool.Exec(t.Context(), "UPDATE olp.attempt_usage_facts SET observed_at=$2::timestamptz WHERE request_id=$1", event.RequestID, sample.date); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		conn, err := pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		snapshots, err := limits.ReconciliationSnapshots(t.Context(), conn.Conn(), time.Date(2026, 1, 1, 18, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		expected := map[string]bool{key: false, group: false, limits.EndUserKeyID(key, digest): false, limits.AggregateBudgetID("installation", installation): false, limits.KeyRouteBudgetID(key, "parity"): false}
		for _, s := range snapshots {
			if _, ok := expected[s.CostOwnerID]; !ok {
				continue
			}
			expected[s.CostOwnerID] = true
			if s.WeeklyAccrued != "7.000000000000" || s.MonthlyAccrued != "4.000000000000" || s.DailyAccrued != "4.000000000000" || s.UnpricedAttempts != 1 {
				t.Fatalf("wrong cross-year windows: %+v", s)
			}
		}
		for id, found := range expected {
			if !found {
				t.Fatalf("missing budget owner %s", id)
			}
		}
	}
	check()
	var rolled, expired int64
	if err = pool.QueryRow(t.Context(), rollupSQL, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), 100).Scan(&rolled, &expired); err != nil {
		t.Fatal(err)
	}
	if expired != 6 {
		t.Fatalf("retained %d of six facts", expired)
	}
	for _, table := range []string{"api_key_cost_windows", "budget_group_cost_windows", "end_user_cost_windows", "aggregate_cost_windows"} {
		if _, err = pool.Exec(t.Context(), "DELETE FROM olp."+table); err != nil {
			t.Fatal(err)
		}
	}
	check()
}

func TestIntegrationWeeklyNotificationsRequireCompleteEvidence(t *testing.T) {
	for _, zone := range []string{"UTC", "Asia/Kathmandu"} {
		t.Run(zone, func(t *testing.T) { testWeeklyNotificationCalendar(t, zone) })
	}
}
func testWeeklyNotificationCalendar(t *testing.T, zone string) {
	pool := notificationPool(t)
	f := newNotificationDeliveryFixture(t, pool)
	f.worker.now = time.Now
	f.worker.newID = uuid7
	owner, _ := seedParityOwner(t, pool)
	key := seedParityKey(t, pool, owner)
	f.exec(t, `DELETE FROM olp.notification_deliveries`)
	f.exec(t, `UPDATE olp.api_keys SET policy=policy||'{"weekly_cost_limit":"10"}'::jsonb WHERE id=$1`, key)
	f.exec(t, `UPDATE olp.notification_rules SET subject_id=$2,window_kind='week' WHERE id=$1`, f.ruleID, key)
	if zone != "UTC" {
		f.exec(t, `INSERT INTO olp.budget_calendar_changes SELECT k,'2020-01-01'::timestamptz,$1 FROM unnest(ARRAY['day','week','month']) k`, zone)
	}
	windows, err := limits.CurrentBudgetWindows(t.Context(), pool, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO olp.api_key_cost_windows(api_key_id,window_kind,window_id,accrued,unpriced_attempts) VALUES($1,'week',$2,6,0)`, key, windows.WeeklyID)
	if n, err := f.worker.claimDue(t.Context()); err != nil || n != 0 {
		t.Fatalf("incomplete evidence: %d %v", n, err)
	}
	f.exec(t, `UPDATE olp.api_key_cost_windows SET weekly_complete=true WHERE api_key_id=$1`, key)
	if n, err := f.worker.claimDue(t.Context()); err != nil || n != 1 {
		t.Fatalf("complete evidence: %d %v", n, err)
	}
	if n, err := f.worker.claimDue(t.Context()); err != nil || n != 0 {
		t.Fatalf("duplicate threshold: %d %v", n, err)
	}
	pending, err := f.worker.pending(t.Context())
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%d err=%v", len(pending), err)
	}
	var windowID int64
	if err := pool.QueryRow(t.Context(), `SELECT window_id FROM olp.notification_deliveries WHERE id=$1`, pending[0].id).Scan(&windowID); err != nil || windowID != windows.WeeklyID {
		t.Fatalf("wrong week %d: %v", windowID, err)
	}
	if !f.worker.deliver(t.Context(), pending[0]) || f.requests.Load() != 1 {
		t.Fatal("weekly threshold was not delivered")
	}
}

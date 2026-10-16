//go:build integration

package usage

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
)

// Seed historical boundaries independently of the receipt replay clock.

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

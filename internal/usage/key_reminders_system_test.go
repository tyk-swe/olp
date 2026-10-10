//go:build integration

package usage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestIntegrationKeyRemindersDeduplicateAndCancelSupersededSchedules(t *testing.T) {
	pool := notificationPool(t)
	f := newNotificationDeliveryFixture(t, pool)
	f.worker.now = time.Now
	f.worker.newID = uuid7
	ctx := t.Context()
	var owner string
	if err := pool.QueryRow(ctx, "SELECT created_by::text FROM olp.notification_rules WHERE id=$1", f.ruleID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	key := seedParityKey(t, pool, owner)
	f.exec(t, `UPDATE olp.api_keys SET created_at=now()-interval '2 days',expires_at=now()+interval '6 hours',policy='{"rotation_interval_days":2}' WHERE id=$1`, key)
	f.exec(t, "DELETE FROM olp.notification_deliveries WHERE rule_id=$1", f.ruleID)
	f.exec(t, `UPDATE olp.notification_rules SET event='key.expiring',subject_id=$2,window_kind=NULL,threshold_percent=NULL WHERE id=$1`, f.ruleID, key)
	claim := func(want int) {
		t.Helper()
		n, err := f.worker.claimDue(ctx)
		if err != nil || n != want {
			t.Fatalf("claims: %d, %v; want %d", n, err, want)
		}
	}
	claim(2)
	claim(0)
	pending, err := f.worker.pending(ctx)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending: %d %v", len(pending), err)
	}
	var expiry, rotation delivery
	for _, d := range pending {
		data, err := webhookBody(d)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err = json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload) != 9 || payload["event"] != "key.expiring" || payload["api_key_id"] != key || strings.Contains(string(data), "digest") || strings.Contains(string(data), "lookup_id") {
			t.Fatalf("unexpected reminder fields: %v", payload)
		}
		if payload["reason"] == "expiry" {
			expiry = d
		} else {
			rotation = d
		}
	}
	// Revalidate immediately before delivery, even when the worker has already
	// fetched the old pending rows and has not run another evaluation pass.
	f.exec(t, "UPDATE olp.api_keys SET rotated_at=now() WHERE id=$1", key)
	if f.worker.deliver(ctx, rotation) {
		t.Fatal("superseded reminder attempted delivery")
	}
	var status string
	var attempts int
	if err = pool.QueryRow(ctx, "SELECT status,attempts FROM olp.notification_deliveries WHERE id=$1", rotation.id).Scan(&status, &attempts); err != nil || status != "cancelled" || attempts != 0 {
		t.Fatalf("cancelled state: %s %d %v", status, attempts, err)
	}
	if !f.worker.deliver(ctx, expiry) || f.requests.Load() != 1 {
		t.Fatal("current expiry reminder was not sent")
	}
	claim(0)
	// A changed deadline is a new schedule. Revocation then retires it while
	// preserving both the delivered event and the cancellation history.
	f.exec(t, "UPDATE olp.api_keys SET expires_at=expires_at+interval '1 hour' WHERE id=$1", key)
	claim(1)
	f.exec(t, "UPDATE olp.api_keys SET revoked_at=now() WHERE id=$1", key)
	claim(0)
	pending, err = f.worker.pending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("revoked reminders remained pending: %d %v", len(pending), err)
	}
	if f.requests.Load() != 1 {
		t.Fatal("reminders caused an additional send")
	}
	// A rule reassignment between evaluation and the pending read must not
	// reinterpret a queued reminder as a different event.
	f.exec(t, "UPDATE olp.api_keys SET revoked_at=NULL,expires_at=now()+interval '8 hours' WHERE id=$1", key)
	claim(1)
	f.exec(t, "UPDATE olp.notification_rules SET event='provider.grant.lapsed',subject_kind=NULL,subject_id=NULL WHERE id=$1", f.ruleID)
	pending, err = f.worker.pending(ctx)
	if err != nil || len(pending) != 1 || pending[0].event != "key.expiring" {
		t.Fatalf("reminder evidence changed after rule edit: %v", err)
	}
	if f.worker.deliver(ctx, pending[0]) || f.requests.Load() != 1 {
		t.Fatal("reassigned rule delivered obsolete reminder")
	}

}

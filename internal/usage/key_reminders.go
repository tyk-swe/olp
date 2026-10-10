package usage

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// The event timestamp identifies a schedule, not an attempt to rotate. Reminders
// have no secret material and never create a replacement credential.
const keyDueSQL = `SELECT k.id,k.name,k.project_id,'expiry'::text AS reason,k.expires_at AS due_at
 FROM olp.api_keys k WHERE k.revoked_at IS NULL AND k.expires_at IS NOT NULL
 UNION ALL
 SELECT k.id,k.name,k.project_id,'rotation',COALESCE(k.rotated_at,k.created_at)+make_interval(secs=>86400.0*(k.policy->>'rotation_interval_days')::int)
 FROM olp.api_keys k WHERE k.revoked_at IS NULL AND k.policy->>'rotation_interval_days' IS NOT NULL
 AND (k.expires_at IS NULL OR k.expires_at>now())`

func (w *notificationWorker) claimKeyReminders(ctx context.Context, tx pgx.Tx) (int, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM olp.api_key_overlaps WHERE expires_at<=now()
 OR api_key_id IN (SELECT id FROM olp.api_keys WHERE revoked_at IS NOT NULL)`); err != nil {
		return 0, err
	}
	// Keep the delivery history, but retire pending evidence superseded by an
	// expiry edit, completed rotation, rule reassignment or key revocation.
	if _, err := tx.Exec(ctx, `WITH due AS (`+keyDueSQL+`)
 UPDATE olp.notification_deliveries v SET status='cancelled',last_error_code='superseded'
 WHERE v.api_key_id IS NOT NULL AND v.status IN ('pending','failed') AND NOT EXISTS(
 SELECT 1 FROM due JOIN olp.notification_rules r ON r.subject_id=due.id
 WHERE r.id=v.rule_id AND r.event='key.expiring' AND due.id=v.api_key_id AND due.reason=v.reason AND due.due_at=v.due_at)`); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `WITH due AS (`+keyDueSQL+`)
 INSERT INTO olp.notification_deliveries(id,rule_id,api_key_id,due_at,reason,payload,status)
 SELECT uuidv7(),r.id,due.id,due.due_at,due.reason,
 jsonb_build_object('api_key_id',due.id,'api_key_name',due.name,'project_id',due.project_id,'due_at',due.due_at,'reason',due.reason),'pending'
 FROM due JOIN olp.notification_rules r ON r.event='key.expiring' AND r.subject_kind='api_key' AND r.subject_id=due.id AND r.project_id IS NOT DISTINCT FROM due.project_id
 JOIN olp.notification_destinations d ON d.id=r.destination_id
 WHERE r.enabled AND d.enabled AND due.due_at<=now()+make_interval(secs=>COALESCE((r.configuration->>'lead_time_seconds')::integer,86400))
 ON CONFLICT DO NOTHING`)
	return int(tag.RowsAffected()), err
}

func keyReminderCurrent(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	// Hold the key until the delivery attempt is committed, just like the
	// destination/rule locks. Later changes cannot cancel an in-flight send.
	var current bool
	err := tx.QueryRow(ctx, `SELECT COALESCE(k.revoked_at IS NULL AND r.event='key.expiring' AND r.subject_id=k.id AND r.project_id IS NOT DISTINCT FROM k.project_id AND CASE v.reason
 WHEN 'expiry' THEN k.expires_at=v.due_at
 ELSE k.policy->>'rotation_interval_days' IS NOT NULL AND (k.expires_at IS NULL OR k.expires_at>now())
 AND COALESCE(k.rotated_at,k.created_at)+make_interval(secs=>86400.0*(k.policy->>'rotation_interval_days')::int)=v.due_at END,false)
 FROM olp.notification_deliveries v JOIN olp.api_keys k ON k.id=v.api_key_id JOIN olp.notification_rules r ON r.id=v.rule_id
 WHERE v.id=$1 FOR SHARE OF k,r`, id).Scan(&current)
	return current, err
}

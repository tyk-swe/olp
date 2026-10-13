package limits

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This namespace identifies accounting rows, not authentication credentials.
// Identity itself is already a project-scoped, purpose-separated HMAC digest.
var endUserNamespace = uuid.MustParse("15dc4671-a780-5723-8275-fdb6c9f90eb4")

func EndUserKeyID(keyID, digest string) string {
	return uuid.NewSHA1(endUserNamespace, []byte("key\x00"+keyID+"\x00"+digest)).String()
}

func EndUserProjectID(projectID, digest string) string {
	return uuid.NewSHA1(endUserNamespace, []byte("project\x00"+projectID+"\x00"+digest)).String()
}

func EndUserLookup(accountID string) string { return "eu_" + strings.ReplaceAll(accountID, "-", "") }

// The same decimal delta and window rules apply to all cost owners.
var addEndUserCostDeltaSQL = costDeltaSQL(costOwnerEndUser)

// AddEndUserCostDelta registers the digest at its key and project boundaries.
// A new account reconstructs history, including this event, instead of
// assuming zero. All work belongs to the event's idempotent transaction.
func AddEndUserCostDelta(ctx context.Context, tx pgx.Tx, keyID, digest string, observedAt time.Time, cost string, unpriced int64, charge string, uncharged int64) ([]CostSnapshot, error) {
	var projectID *string
	if err := tx.QueryRow(ctx, "SELECT project_id::text FROM olp.api_keys WHERE id=$1", keyID).Scan(&projectID); err != nil {
		return nil, err
	}
	accounts := []struct {
		id           string
		key, project *string
	}{{id: EndUserKeyID(keyID, digest), key: &keyID}}
	if projectID != nil {
		accounts = append(accounts, struct {
			id           string
			key, project *string
		}{id: EndUserProjectID(*projectID, digest), project: projectID})
	}
	snapshots := make([]CostSnapshot, 0, len(accounts))
	for _, account := range accounts {
		tag, err := tx.Exec(ctx, `INSERT INTO olp.end_user_accounts (id,api_key_id,project_id,end_user_digest)
			VALUES ($1,$2,$3,$4) ON CONFLICT (id) DO NOTHING`, account.id, account.key, account.project, digest)
		if err != nil {
			return nil, err
		}
		var snapshot []CostSnapshot
		if tag.RowsAffected() == 0 {
			snapshot, err = addOwnerDelta(ctx, tx, addEndUserCostDeltaSQL, account.id, observedAt, cost, unpriced, charge, uncharged)
		} else {
			snapshot, err = scanSnapshots(tx.QueryRow(ctx, endUserSnapshotsSQL, observedAt, account.id))
		}
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot...)
	}
	return snapshots, nil
}

var endUserSnapshotsSQL = reconcileCostWindowsSQL(costOwnerEndUser, true, `accounts AS (
 SELECT a.* FROM olp.end_user_accounts a
 WHERE $1::timestamptz IS NOT NULL AND ($2::uuid IS NULL OR a.id=$2::uuid)
), usage AS (
 SELECT a.id,f.observed_at,COALESCE(f.estimated_cost,0)::numeric AS cost,
   COALESCE(f.charge,0)::numeric AS charge,
   CASE WHEN f.charge_status<>'not_billable' AND f.unpriced THEN 1 ELSE 0 END::bigint AS unpriced_attempts,
   CASE WHEN f.charge_status<>'not_billable' AND f.charge IS NULL THEN 1 ELSE 0 END::bigint AS uncharged_attempts
 FROM accounts a JOIN (SELECT * FROM olp.attempt_usage_facts WHERE NOT budget_exempt) f ON f.end_user_digest=a.end_user_digest
 JOIN olp.api_keys k ON k.id=f.api_key_id
 WHERE (a.api_key_id=k.id OR a.project_id=k.project_id)
   AND f.observed_at >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND f.observed_at < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
 UNION ALL
 SELECT a.id,h.budget_bucket,COALESCE(h.estimated_cost,0)::numeric,COALESCE(h.charge,0)::numeric,h.unpriced_attempt_count,h.uncharged_attempt_count
 FROM accounts a JOIN (SELECT * FROM olp.attempt_usage_hourly WHERE NOT budget_exempt) h ON h.end_user_digest=a.end_user_digest
 JOIN olp.api_keys k ON k.id=h.api_key_id
 WHERE (a.api_key_id=k.id OR a.project_id=k.project_id)
   AND h.budget_bucket >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND h.budget_bucket < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
)`)

func endUserSnapshots(ctx context.Context, conn *pgx.Conn, now time.Time) ([]CostSnapshot, error) {
	rows, err := conn.Query(ctx, endUserSnapshotsSQL, now, nil)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snapshots []CostSnapshot
	for rows.Next() {
		snapshot, err := scanSnapshots(rows)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot...)
	}
	return snapshots, rows.Err()
}

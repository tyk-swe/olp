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
var addEndUserCostDeltaSQL = strings.NewReplacer(
	"api_key_cost_windows", "end_user_cost_windows", "api_key_id", "account_id",
).Replace(addCostDeltaSQL)

// AddEndUserCostDelta registers the digest at its key and project boundaries.
// A new account reconstructs history, including this event, instead of
// assuming zero. All work belongs to the event's idempotent transaction.
func AddEndUserCostDelta(ctx context.Context, tx pgx.Tx, keyID, digest string, observedAt time.Time, cost string, unpriced int64) ([]CostSnapshot, error) {
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
		var snapshot CostSnapshot
		if tag.RowsAffected() == 0 {
			snapshot, err = addOwnerDelta(ctx, tx, addEndUserCostDeltaSQL, account.id, observedAt, cost, unpriced)
		} else {
			snapshot, err = scanSnapshot(tx.QueryRow(ctx, endUserSnapshotsSQL, observedAt, account.id))
		}
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

const endUserSnapshotsSQL = `WITH budget_calendar AS MATERIALIZED (SELECT * FROM olp.budget_windows($1::timestamptz)),
accounts AS (
 SELECT a.* FROM olp.end_user_accounts a
 WHERE $1::timestamptz IS NOT NULL AND ($2::uuid IS NULL OR a.id=$2::uuid)
), usage AS (
 SELECT a.id,f.observed_at,COALESCE(f.estimated_cost,0)::numeric AS cost,
   CASE WHEN f.charge_status<>'not_billable' AND f.unpriced THEN 1 ELSE 0 END::bigint AS unpriced_attempts
 FROM accounts a JOIN (SELECT * FROM olp.attempt_usage_facts WHERE NOT budget_exempt) f ON f.end_user_digest=a.end_user_digest
 JOIN olp.api_keys k ON k.id=f.api_key_id
 WHERE (a.api_key_id=k.id OR a.project_id=k.project_id)
   AND f.observed_at >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND f.observed_at < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
 UNION ALL
 SELECT a.id,h.budget_bucket,COALESCE(h.estimated_cost,0)::numeric,h.unpriced_attempt_count
 FROM accounts a JOIN (SELECT * FROM olp.attempt_usage_hourly WHERE NOT budget_exempt) h ON h.end_user_digest=a.end_user_digest
 JOIN olp.api_keys k ON k.id=h.api_key_id
 WHERE (a.api_key_id=k.id OR a.project_id=k.project_id)
   AND h.budget_bucket >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND h.budget_bucket < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
), totals AS (
 SELECT a.id,
  COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at >= (SELECT daily_start FROM budget_calendar) AND u.observed_at < (SELECT daily_end FROM budget_calendar)),0) AS daily_accrued,
  COALESCE(SUM(u.cost) FILTER(WHERE u.observed_at >= (SELECT monthly_start FROM budget_calendar) AND u.observed_at < (SELECT monthly_end FROM budget_calendar)),0) AS monthly_accrued,
 COALESCE(SUM(u.cost) FILTER(WHERE u.observed_at >= (SELECT weekly_start FROM budget_calendar) AND u.observed_at < (SELECT weekly_end FROM budget_calendar)),0) AS weekly_accrued,
  COALESCE(SUM(u.unpriced_attempts) FILTER(WHERE u.observed_at >= (SELECT monthly_start FROM budget_calendar) AND u.observed_at < (SELECT monthly_end FROM budget_calendar)),0)::bigint AS unpriced_attempts
 FROM accounts a LEFT JOIN usage u ON u.id=a.id GROUP BY a.id
), pruned AS (
 DELETE FROM olp.end_user_cost_windows
 WHERE ($2::uuid IS NULL OR account_id=$2::uuid)
   AND ((window_kind='day' AND window_id<(SELECT daily_id FROM budget_calendar)) OR (window_kind='month' AND window_id<(SELECT monthly_id FROM budget_calendar)) OR (window_kind='week' AND window_id<(SELECT weekly_id FROM budget_calendar))) RETURNING 1
), desired AS (
 SELECT id,'day'::text AS window_kind,(SELECT daily_id FROM budget_calendar) AS window_id,daily_accrued AS accrued,0::bigint AS unpriced_attempts FROM totals
 UNION ALL
 SELECT id,'month',(SELECT monthly_id FROM budget_calendar),monthly_accrued,unpriced_attempts FROM totals
 UNION ALL SELECT id,'week',(SELECT weekly_id FROM budget_calendar),weekly_accrued,0 FROM totals
), reconciled AS (
 INSERT INTO olp.end_user_cost_windows (account_id,window_kind,window_id,accrued,unpriced_attempts,weekly_complete)
 SELECT id,window_kind,window_id,accrued,unpriced_attempts,true FROM desired
 ON CONFLICT (account_id,window_kind,window_id) DO UPDATE SET weekly_complete=true,
  accrued=GREATEST(olp.end_user_cost_windows.accrued,EXCLUDED.accrued),
  unpriced_attempts=GREATEST(olp.end_user_cost_windows.unpriced_attempts,EXCLUDED.unpriced_attempts)
 RETURNING account_id,window_kind,window_id,accrued,unpriced_attempts
) SELECT account_id::text,(SELECT daily_id FROM budget_calendar),
 MAX(accrued) FILTER (WHERE window_kind='day')::text,(SELECT monthly_id FROM budget_calendar),
 MAX(accrued) FILTER (WHERE window_kind='month')::text,
 MAX(unpriced_attempts) FILTER (WHERE window_kind='month')::bigint,
 (SELECT weekly_id FROM budget_calendar),MAX(accrued) FILTER(WHERE window_kind='week')::text
 , (SELECT daily_start FROM budget_calendar), (SELECT daily_end FROM budget_calendar), (SELECT monthly_start FROM budget_calendar), (SELECT monthly_end FROM budget_calendar), (SELECT weekly_start FROM budget_calendar), (SELECT weekly_end FROM budget_calendar)
FROM reconciled GROUP BY account_id ORDER BY account_id`

func endUserSnapshots(ctx context.Context, conn *pgx.Conn, now time.Time) ([]CostSnapshot, error) {
	rows, err := conn.Query(ctx, endUserSnapshotsSQL, now, nil)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snapshots []CostSnapshot
	for rows.Next() {
		snapshot, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, rows.Err()
}

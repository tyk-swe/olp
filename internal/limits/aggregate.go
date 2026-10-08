package limits

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/attribution"
)

var aggregateNamespace = uuid.MustParse("e7d0305c-722f-5484-99a2-69a019fd74d5")

func AggregateBudgetID(level, subject string) string {
	return uuid.NewSHA1(aggregateNamespace, []byte(level+"\x00"+subject)).String()
}
func AggregateBudgetLookup(owner string) string { return "ab_" + simpleUUID(owner) }

// EnsureAggregateBudget keeps a stable accounting identity even when a cap is disabled.
func EnsureAggregateBudget(ctx context.Context, tx pgx.Tx, level, subject string) error {
	var installation, project, organization *string
	switch level {
	case "installation":
		installation = &subject
	case "project":
		project = &subject
	case "organization":
		organization = &subject
	default:
		return fmt.Errorf("invalid aggregate budget level")
	}
	_, err := tx.Exec(ctx, `INSERT INTO olp.aggregate_budget_accounts (id,installation_id,project_id,organization_id) VALUES ($1,$2,$3,$4) ON CONFLICT (id) DO NOTHING`, AggregateBudgetID(level, subject), installation, project, organization)
	return err
}

var addAggregateCostDeltaSQL = strings.NewReplacer("olp.api_key_cost_windows", "olp.aggregate_cost_windows", "api_key_id", "account_id").Replace(addCostDeltaSQL)

// AddAggregateCostDelta accounts caller and system work. The first observation
// reconstructs all current-window history instead of treating a newly enabled cap as zero.
func AddAggregateCostDelta(ctx context.Context, tx pgx.Tx, keyID string, providerID *string, route string, labels map[string]string, at time.Time, cost string, unpriced int64) ([]CostSnapshot, error) {
	rows, err := tx.Query(ctx, `SELECT id::text FROM olp.aggregate_budget_accounts
 WHERE installation_id IS NOT NULL OR organization_id=(SELECT organization_id FROM olp.projects WHERE id=COALESCE(
 (SELECT project_id FROM olp.api_keys WHERE id=NULLIF($1,'')::uuid),
 (SELECT project_id FROM olp.providers WHERE id=NULLIF($2,'')::uuid),
 (SELECT project_id FROM olp.routes WHERE slug=$3))) OR (project_id=COALESCE(
 (SELECT project_id FROM olp.api_keys WHERE id=NULLIF($1,'')::uuid),
 (SELECT project_id FROM olp.providers WHERE id=NULLIF($2,'')::uuid),
 (SELECT project_id FROM olp.routes WHERE slug=$3)) AND (attribution_key IS NULL OR $4::jsonb->>attribution_key=attribution_value)) OR (api_key_id=NULLIF($1,'')::uuid AND route_slug=$3) ORDER BY id`, keyID, providerID, route, attribution.JSON(labels))
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var snapshots []CostSnapshot
	for _, id := range ids {
		tag, err := tx.Exec(ctx, `UPDATE olp.aggregate_budget_accounts SET initialized=true WHERE id=$1 AND NOT initialized`, id)
		if err != nil {
			return nil, err
		}
		var snapshot CostSnapshot
		if tag.RowsAffected() > 0 {
			snapshot, err = scanSnapshot(tx.QueryRow(ctx, aggregateSnapshotsSQL, at, id))
		} else {
			snapshot, err = addOwnerDelta(ctx, tx, addAggregateCostDeltaSQL, id, at, cost, unpriced)
		}
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

const aggregateSnapshotsSQL = `WITH budget_calendar AS MATERIALIZED (SELECT * FROM olp.budget_windows($1::timestamptz)),
accounts AS (
 SELECT a.* FROM olp.aggregate_budget_accounts a
 WHERE $1::timestamptz IS NOT NULL AND ($2::uuid IS NULL OR a.id=$2::uuid)
), usage AS (
 SELECT a.id,f.observed_at,COALESCE(f.estimated_cost,0)::numeric AS cost,
   CASE WHEN f.charge_status<>'not_billable' AND f.unpriced THEN 1 ELSE 0 END::bigint AS unpriced_attempts
 FROM accounts a JOIN (SELECT * FROM olp.attempt_usage_facts WHERE NOT budget_exempt) f ON true
 LEFT JOIN olp.api_keys k ON k.id=f.api_key_id
 LEFT JOIN olp.providers p ON p.id=f.provider_id
 WHERE (a.installation_id IS NOT NULL OR a.organization_id=(SELECT organization_id FROM olp.projects WHERE id=COALESCE(k.project_id,p.project_id)) OR (a.project_id=COALESCE(k.project_id,p.project_id) AND (a.attribution_key IS NULL OR f.attribution->>a.attribution_key=a.attribution_value)) OR (a.api_key_id=k.id AND a.route_slug=f.route_slug))
   AND f.observed_at >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND f.observed_at < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
 UNION ALL
 SELECT a.id,h.budget_bucket,COALESCE(h.estimated_cost,0)::numeric,h.unpriced_attempt_count
 FROM accounts a JOIN (SELECT * FROM olp.attempt_usage_hourly WHERE NOT budget_exempt) h ON true
 LEFT JOIN olp.api_keys k ON k.id=h.api_key_id
 LEFT JOIN olp.providers p ON p.id=h.provider_id
 WHERE (a.installation_id IS NOT NULL OR a.organization_id=(SELECT organization_id FROM olp.projects WHERE id=COALESCE(k.project_id,p.project_id)) OR (a.project_id=COALESCE(k.project_id,p.project_id) AND (a.attribution_key IS NULL OR h.attribution->>a.attribution_key=a.attribution_value)) OR (a.api_key_id=k.id AND a.route_slug=h.route_slug))
   AND h.budget_bucket >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND h.budget_bucket < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
), totals AS (
 SELECT a.id,
  COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at >= (SELECT daily_start FROM budget_calendar) AND u.observed_at < (SELECT daily_end FROM budget_calendar)),0) AS daily_accrued,
  COALESCE(SUM(u.cost) FILTER(WHERE u.observed_at >= (SELECT monthly_start FROM budget_calendar) AND u.observed_at < (SELECT monthly_end FROM budget_calendar)),0) AS monthly_accrued,
 COALESCE(SUM(u.cost) FILTER(WHERE u.observed_at >= (SELECT weekly_start FROM budget_calendar) AND u.observed_at < (SELECT weekly_end FROM budget_calendar)),0) AS weekly_accrued,
  COALESCE(SUM(u.unpriced_attempts) FILTER(WHERE u.observed_at >= (SELECT monthly_start FROM budget_calendar) AND u.observed_at < (SELECT monthly_end FROM budget_calendar)),0)::bigint AS unpriced_attempts
 FROM accounts a LEFT JOIN usage u ON u.id=a.id GROUP BY a.id
), pruned AS (
 DELETE FROM olp.aggregate_cost_windows
 WHERE ($2::uuid IS NULL OR account_id=$2::uuid)
   AND ((window_kind='day' AND window_id<(SELECT daily_id FROM budget_calendar)) OR (window_kind='month' AND window_id<(SELECT monthly_id FROM budget_calendar)) OR (window_kind='week' AND window_id<(SELECT weekly_id FROM budget_calendar))) RETURNING 1
), desired AS (
 SELECT id,'day'::text AS window_kind,(SELECT daily_id FROM budget_calendar) AS window_id,daily_accrued AS accrued,0::bigint AS unpriced_attempts FROM totals
 UNION ALL
 SELECT id,'month',(SELECT monthly_id FROM budget_calendar),monthly_accrued,unpriced_attempts FROM totals
 UNION ALL SELECT id,'week',(SELECT weekly_id FROM budget_calendar),weekly_accrued,0 FROM totals
), reconciled AS (
 INSERT INTO olp.aggregate_cost_windows (account_id,window_kind,window_id,accrued,unpriced_attempts,weekly_complete)
 SELECT id,window_kind,window_id,accrued,unpriced_attempts,true FROM desired
 ON CONFLICT (account_id,window_kind,window_id) DO UPDATE SET weekly_complete=true,
  accrued=GREATEST(olp.aggregate_cost_windows.accrued,EXCLUDED.accrued),
  unpriced_attempts=GREATEST(olp.aggregate_cost_windows.unpriced_attempts,EXCLUDED.unpriced_attempts)
 RETURNING account_id,window_kind,window_id,accrued,unpriced_attempts
) SELECT account_id::text,(SELECT daily_id FROM budget_calendar),
 MAX(accrued) FILTER (WHERE window_kind='day')::text,(SELECT monthly_id FROM budget_calendar),
 MAX(accrued) FILTER (WHERE window_kind='month')::text,
 MAX(unpriced_attempts) FILTER (WHERE window_kind='month')::bigint,
 (SELECT weekly_id FROM budget_calendar),MAX(accrued) FILTER(WHERE window_kind='week')::text
 , (SELECT daily_start FROM budget_calendar), (SELECT daily_end FROM budget_calendar), (SELECT monthly_start FROM budget_calendar), (SELECT monthly_end FROM budget_calendar), (SELECT weekly_start FROM budget_calendar), (SELECT weekly_end FROM budget_calendar)
FROM reconciled GROUP BY account_id ORDER BY account_id`

// ReconcileAggregateBudget recomputes one aggregate account's current spend
// from the usage history tx sees, including a project tx has just placed under
// it, so the balance can be installed before the change publishes. It reports
// false when the subject keeps no account.
func ReconcileAggregateBudget(ctx context.Context, tx pgx.Tx, level, subject string, at time.Time) (CostSnapshot, bool, error) {
	snapshot, err := scanSnapshot(tx.QueryRow(ctx, aggregateSnapshotsSQL, at, AggregateBudgetID(level, subject)))
	if errors.Is(err, pgx.ErrNoRows) {
		return CostSnapshot{}, false, nil
	}
	return snapshot, err == nil, err
}

func aggregateSnapshots(ctx context.Context, conn *pgx.Conn, now time.Time) ([]CostSnapshot, error) {
	rows, err := conn.Query(ctx, aggregateSnapshotsSQL, now, nil)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snapshots []CostSnapshot
	for rows.Next() {
		s, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, s)
	}
	return snapshots, rows.Err()
}

// AggregateBudgetSQL reports authoritative live and retained spend. The project
// expression expects the surrounding project row to be named p.
func AggregateBudgetSQL(project bool) string {
	raw, hourly := "", ""
	if project {
		raw = "COALESCE((SELECT project_id FROM olp.api_keys WHERE id=f.api_key_id),(SELECT project_id FROM olp.providers WHERE id=f.provider_id))=p.id AND "
		hourly = "COALESCE((SELECT project_id FROM olp.api_keys WHERE id=h.api_key_id),(SELECT project_id FROM olp.providers WHERE id=h.provider_id))=p.id AND "
	}
	return strings.NewReplacer("f.api_key_id=k.id AND ", raw, "h.api_key_id=k.id AND ", hourly).Replace(BudgetSQL)
}

func KeyRouteBudgetID(key, route string) string {
	return AggregateBudgetID("key_route", key+"\x00"+route)
}
func EnsureKeyRouteBudget(ctx context.Context, tx pgx.Tx, key, route string) error {
	_, err := tx.Exec(ctx, `INSERT INTO olp.aggregate_budget_accounts (id,api_key_id,route_slug) VALUES ($1,$2,$3) ON CONFLICT(id) DO NOTHING`, KeyRouteBudgetID(key, route), key, route)
	return err
}

func AttributionBudgetID(project, key, value string) string {
	return AggregateBudgetID("attribution", project+"\x00"+key+"\x00"+value)
}
func EnsureAttributionBudget(ctx context.Context, tx pgx.Tx, project, key, value string) error {
	_, err := tx.Exec(ctx, `INSERT INTO olp.aggregate_budget_accounts(id,project_id,attribution_key,attribution_value) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO NOTHING`, AttributionBudgetID(project, key, value), project, key, value)
	return err
}

// AttributionBudgetsSQL returns live/retained usage for each configured pair in
// one control-plane query. The surrounding project row must be named p.
var AttributionBudgetsSQL = `(SELECT COALESCE(jsonb_object_agg(label,pair_usage),'{}'::jsonb) FROM (
 SELECT b.key AS label,jsonb_object_agg(v.key,` + strings.NewReplacer("f.api_key_id=k.id AND ", "COALESCE((SELECT project_id FROM olp.api_keys WHERE id=f.api_key_id),(SELECT project_id FROM olp.providers WHERE id=f.provider_id))=p.id AND f.attribution->>b.key=v.key AND ", "h.api_key_id=k.id AND ", "COALESCE((SELECT project_id FROM olp.api_keys WHERE id=h.api_key_id),(SELECT project_id FROM olp.providers WHERE id=h.provider_id))=p.id AND h.attribution->>b.key=v.key AND ").Replace(BudgetSQL) + `) AS pair_usage
 FROM jsonb_each(p.attribution_budgets) b CROSS JOIN LATERAL jsonb_each(b.value) v GROUP BY b.key
) grouped)`

// Organization ownership is immutable once assigned, so live and retained spend
// retain the same boundary when aggregate policies change.
var OrganizationBudgetSQL = strings.NewReplacer("f.api_key_id=k.id AND ", "(SELECT organization_id FROM olp.projects WHERE id=COALESCE((SELECT project_id FROM olp.api_keys WHERE id=f.api_key_id),(SELECT project_id FROM olp.providers WHERE id=f.provider_id)))=p.id AND ", "h.api_key_id=k.id AND ", "(SELECT organization_id FROM olp.projects WHERE id=COALESCE((SELECT project_id FROM olp.api_keys WHERE id=h.api_key_id),(SELECT project_id FROM olp.providers WHERE id=h.provider_id)))=p.id AND ").Replace(BudgetSQL)

package limits

import (
	"context"
	"errors"
	"fmt"
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

var addAggregateCostDeltaSQL = costDeltaSQL(costOwnerAggregate)

// Aggregate ownership follows the same project precedence everywhere: the
// calling key, then the attempt provider, then its route. Keyless work on an
// installation-wide provider therefore still belongs to the route's project.
func budgetProjectSQL(key, provider, route string) string {
	return fmt.Sprintf("COALESCE((SELECT project_id FROM olp.api_keys WHERE id=%s),(SELECT project_id FROM olp.providers WHERE id=%s),(SELECT project_id FROM olp.routes WHERE slug=%s))", key, provider, route)
}
func usageProjectSQL(alias string) string {
	return budgetProjectSQL(alias+".api_key_id", alias+".provider_id", alias+".route_slug")
}
func aggregateOwnershipSQL(account, project, key, route, labels string) string {
	return fmt.Sprintf(`(%[1]s.installation_id IS NOT NULL OR
 %[1]s.organization_id=(SELECT organization_id FROM olp.projects WHERE id=%[2]s) OR
 (%[1]s.project_id=%[2]s AND (%[1]s.attribution_key IS NULL OR %[5]s->>%[1]s.attribution_key=%[1]s.attribution_value)) OR
 (%[1]s.api_key_id=%[3]s AND %[1]s.route_slug=%[4]s))`, account, project, key, route, labels)
}

var aggregateEventDeltasSQL = `WITH event_facts AS MATERIALIZED (
 SELECT * FROM olp.attempt_usage_facts WHERE event_id=$5::uuid AND ($6::uuid IS NULL OR attempt_id=$6::uuid) AND NOT budget_exempt
), subjects AS MATERIALIZED (
 SELECT ` + usageProjectSQL("f") + ` AS project_id,f.api_key_id,f.route_slug,f.attribution FROM event_facts f
 UNION SELECT ` + budgetProjectSQL("NULLIF($1,'')::uuid", "$2::uuid", "$3") + `,NULLIF($1,'')::uuid,$3,$4::jsonb
), owners AS (
 SELECT id FROM olp.aggregate_budget_accounts WHERE installation_id IS NOT NULL
 UNION SELECT a.id FROM subjects s JOIN olp.projects p ON p.id=s.project_id
 JOIN olp.aggregate_budget_accounts a ON a.organization_id=p.organization_id
 UNION SELECT a.id FROM subjects s JOIN olp.aggregate_budget_accounts a ON a.project_id=s.project_id
 WHERE a.attribution_key IS NULL OR s.attribution->>a.attribution_key=a.attribution_value
 UNION SELECT a.id FROM subjects s JOIN olp.aggregate_budget_accounts a ON a.api_key_id=s.api_key_id AND a.route_slug=s.route_slug
) SELECT a.id::text,COALESCE(SUM(COALESCE(f.estimated_cost,0)+f.cache_budget_cost),0)::text,
 count(f.attempt_id) FILTER(WHERE f.charge_status<>'not_billable' AND f.unpriced),
 COALESCE(SUM(COALESCE(f.charge,0)+f.cache_budget_charge),0)::text,
 count(f.attempt_id) FILTER(WHERE f.charge_status<>'not_billable' AND f.charge IS NULL)
 FROM owners o JOIN olp.aggregate_budget_accounts a ON a.id=o.id LEFT JOIN event_facts f ON ` +
	aggregateOwnershipSQL("a", usageProjectSQL("f"), "f.api_key_id", "f.route_slug", "f.attribution") + `
 GROUP BY a.id ORDER BY a.id`

// AddAggregateCostDelta accounts each attempt to its own aggregate subjects.
// The enclosing idempotent event transaction owns the facts and their deltas.
// A first observation reconstructs all history, including this entire event;
// an existing account adds only this event's eligible facts. A refusal still
// initializes the subjects whose current balance admission needs to observe.
func AddAggregateCostDelta(ctx context.Context, tx pgx.Tx, eventID, keyID string, providerID *string, route string, labels map[string]string, at time.Time) ([]CostSnapshot, error) {
	return addAggregateCostDelta(ctx, tx, eventID, keyID, providerID, route, labels, at, nil)
}

func AddAggregateAttemptCostDelta(ctx context.Context, tx pgx.Tx, eventID, keyID string, providerID *string, route string, labels map[string]string, at time.Time, attemptID string) ([]CostSnapshot, error) {
	return addAggregateCostDelta(ctx, tx, eventID, keyID, providerID, route, labels, at, &attemptID)
}

func addAggregateCostDelta(ctx context.Context, tx pgx.Tx, eventID, keyID string, providerID *string, route string, labels map[string]string, at time.Time, attemptID *string) ([]CostSnapshot, error) {
	rows, err := tx.Query(ctx, aggregateEventDeltasSQL, keyID, providerID, route, attribution.JSON(labels), eventID, attemptID)
	if err != nil {
		return nil, err
	}
	type delta struct {
		id, cost, charge    string
		unpriced, uncharged int64
	}
	var deltas []delta
	for rows.Next() {
		var row delta
		if err = rows.Scan(&row.id, &row.cost, &row.unpriced, &row.charge, &row.uncharged); err != nil {
			rows.Close()
			return nil, err
		}
		deltas = append(deltas, row)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var snapshots []CostSnapshot
	for _, row := range deltas {
		tag, err := tx.Exec(ctx, `UPDATE olp.aggregate_budget_accounts SET initialized=true WHERE id=$1 AND NOT initialized`, row.id)
		if err != nil {
			return nil, err
		}
		var snapshot []CostSnapshot
		if tag.RowsAffected() > 0 {
			snapshot, err = scanSnapshots(tx.QueryRow(ctx, aggregateSnapshotsSQL, at, row.id))
		} else {
			snapshot, err = addOwnerDelta(ctx, tx, addAggregateCostDeltaSQL, row.id, at, row.cost, row.unpriced, row.charge, row.uncharged)
		}
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot...)
	}
	return snapshots, nil
}

var aggregateSnapshotsSQL = reconcileCostWindowsSQL(costOwnerAggregate, true, `accounts AS (
 SELECT a.* FROM olp.aggregate_budget_accounts a
 WHERE $1::timestamptz IS NOT NULL AND ($2::uuid IS NULL OR a.id=$2::uuid)
), usage AS (
 SELECT a.id,f.observed_at,(COALESCE(f.estimated_cost,0)+f.cache_budget_cost)::numeric AS cost,
   (COALESCE(f.charge,0)+f.cache_budget_charge)::numeric AS charge,
   CASE WHEN f.charge_status<>'not_billable' AND f.unpriced THEN 1 ELSE 0 END::bigint AS unpriced_attempts,
   CASE WHEN f.charge_status<>'not_billable' AND f.charge IS NULL THEN 1 ELSE 0 END::bigint AS uncharged_attempts
 FROM accounts a JOIN (SELECT * FROM olp.attempt_usage_facts WHERE NOT budget_exempt) f ON true
 LEFT JOIN olp.api_keys k ON k.id=f.api_key_id
 LEFT JOIN olp.providers p ON p.id=f.provider_id
 LEFT JOIN olp.routes r ON r.slug=f.route_slug
 WHERE `+aggregateOwnershipSQL("a", "COALESCE(k.project_id,p.project_id,r.project_id)", "f.api_key_id", "f.route_slug", "f.attribution")+`
   AND f.observed_at >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND f.observed_at < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
 UNION ALL
 SELECT a.id,h.budget_bucket,(COALESCE(h.estimated_cost,0)+h.cache_budget_cost)::numeric,(COALESCE(h.charge,0)+h.cache_budget_charge)::numeric,h.unpriced_attempt_count,h.uncharged_attempt_count
 FROM accounts a JOIN (SELECT * FROM olp.attempt_usage_hourly WHERE NOT budget_exempt) h ON true
 LEFT JOIN olp.api_keys k ON k.id=h.api_key_id
 LEFT JOIN olp.providers p ON p.id=h.provider_id
 LEFT JOIN olp.routes r ON r.slug=h.route_slug
 WHERE `+aggregateOwnershipSQL("a", "COALESCE(k.project_id,p.project_id,r.project_id)", "h.api_key_id", "h.route_slug", "h.attribution")+`
   AND h.budget_bucket >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND h.budget_bucket < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
)`)

// ReconcileAggregateBudget recomputes one aggregate account's current spend
// from the usage history tx sees, including a project tx has just placed under
// it, so the balance can be installed before the change publishes. It reports
// false when the subject keeps no account.
func ReconcileAggregateBudget(ctx context.Context, tx pgx.Tx, level, subject string, at time.Time) ([]CostSnapshot, bool, error) {
	snapshot, err := scanSnapshots(tx.QueryRow(ctx, aggregateSnapshotsSQL, at, AggregateBudgetID(level, subject)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return snapshot, err == nil, err
}

func aggregateSnapshots(ctx context.Context, conn *pgx.Conn, now time.Time) ([]CostSnapshot, error) {
	return querySnapshots(ctx, conn, aggregateSnapshotsSQL, now, nil)
}

// AggregateBudgetSQL reports authoritative live and retained spend. The project
// expression expects the surrounding project row to be named p.
func AggregateBudgetSQL(project bool) string {
	owner := budgetOwnership{"true", "true"}
	if project {
		owner = budgetOwnership{usageProjectSQL("f") + "=p.id", usageProjectSQL("h") + "=p.id"}
	}
	return budgetReportSQL(owner, "COALESCE(p.budget_policy->>'cost_basis','cost')")
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
 SELECT b.key AS label,jsonb_object_agg(v.key,` + budgetReportSQL(budgetOwnership{
	usageProjectSQL("f") + "=p.id AND f.attribution->>b.key=v.key",
	usageProjectSQL("h") + "=p.id AND h.attribution->>b.key=v.key",
}, "COALESCE(v.value->>'cost_basis','cost')") + `) AS pair_usage
 FROM jsonb_each(p.attribution_budgets) b CROSS JOIN LATERAL jsonb_each(b.value) v GROUP BY b.key
) grouped)`

// Organization ownership is immutable once assigned, so live and retained spend
// retain the same boundary when aggregate policies change.
var OrganizationBudgetSQL = budgetReportSQL(budgetOwnership{
	"(SELECT organization_id FROM olp.projects WHERE id=" + usageProjectSQL("f") + ")=p.id",
	"(SELECT organization_id FROM olp.projects WHERE id=" + usageProjectSQL("h") + ")=p.id",
}, "COALESCE(p.budget_policy->>'cost_basis','cost')")

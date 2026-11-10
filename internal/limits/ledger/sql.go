package ledger

import "fmt"

type storage struct{ table, column string }

var (
	keyWindows       = storage{"olp.api_key_cost_windows", "api_key_id"}
	groupWindows     = storage{"olp.budget_group_cost_windows", "budget_group_id"}
	endUserWindows   = storage{"olp.end_user_cost_windows", "account_id"}
	aggregateWindows = storage{"olp.aggregate_cost_windows", "account_id"}
)

func costDeltaSQL(s storage) string {
	return fmt.Sprintf(`WITH budget_calendar AS MATERIALIZED (SELECT * FROM olp.budget_windows($6::timestamptz)),
 deltas (window_kind,window_id,accrued,unpriced_attempts,charged,uncharged_attempts) AS (
 VALUES ('day'::text,(SELECT daily_id FROM budget_calendar),$2::text::numeric,0::bigint,$4::text::numeric,0::bigint),
 ('month'::text,(SELECT monthly_id FROM budget_calendar),$2::text::numeric,$3::bigint,$4::text::numeric,$5::bigint),
 ('week'::text,(SELECT weekly_id FROM budget_calendar),$2::text::numeric,0::bigint,$4::text::numeric,0::bigint)
 ), applied AS (
 INSERT INTO %[1]s (%[2]s,window_kind,window_id,accrued,unpriced_attempts,charged,uncharged_attempts)
 SELECT $1::uuid,window_kind,window_id,accrued,unpriced_attempts,charged,uncharged_attempts FROM deltas
 ON CONFLICT (%[2]s,window_kind,window_id) DO UPDATE SET
 accrued=%[1]s.accrued+EXCLUDED.accrued,
 unpriced_attempts=%[1]s.unpriced_attempts+EXCLUDED.unpriced_attempts,
 charged=%[1]s.charged+EXCLUDED.charged,
 uncharged_attempts=%[1]s.uncharged_attempts+EXCLUDED.uncharged_attempts
 RETURNING %[2]s,window_kind,window_id,accrued,unpriced_attempts,charged,uncharged_attempts,weekly_complete
 ) SELECT %[2]s::text,
 MAX(window_id) FILTER(WHERE window_kind='day')::bigint,
 MAX(accrued) FILTER(WHERE window_kind='day')::text,
 MAX(charged) FILTER(WHERE window_kind='day')::text,
 MAX(window_id) FILTER(WHERE window_kind='month')::bigint,
 MAX(accrued) FILTER(WHERE window_kind='month')::text,
 MAX(charged) FILTER(WHERE window_kind='month')::text,
 MAX(unpriced_attempts) FILTER(WHERE window_kind='month')::bigint,
 MAX(uncharged_attempts) FILTER(WHERE window_kind='month')::bigint,
 MAX(window_id) FILTER(WHERE window_kind='week')::bigint,
 CASE WHEN bool_or(weekly_complete) FILTER(WHERE window_kind='week') THEN MAX(accrued) FILTER(WHERE window_kind='week')::text ELSE '' END,
 CASE WHEN bool_or(weekly_complete) FILTER(WHERE window_kind='week') THEN MAX(charged) FILTER(WHERE window_kind='week')::text ELSE '' END,
 (SELECT daily_start FROM budget_calendar),(SELECT daily_end FROM budget_calendar),
 (SELECT monthly_start FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),
 (SELECT weekly_start FROM budget_calendar),(SELECT weekly_end FROM budget_calendar)
 FROM applied GROUP BY %[2]s`, s.table, s.column)
}

func (s storage) pruneScope(scoped bool) string {
	if !scoped {
		return ""
	}
	return "($2::uuid IS NULL OR " + s.column + "=$2::uuid) AND "
}

func reconcileCostWindowsSQL(s storage, scoped bool, head string) string {
	return fmt.Sprintf(`WITH budget_calendar AS MATERIALIZED (SELECT * FROM olp.budget_windows($1::timestamptz)),
%[4]s, totals AS (
 SELECT a.id,
  COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at >= (SELECT daily_start FROM budget_calendar) AND u.observed_at < (SELECT daily_end FROM budget_calendar)),0) AS daily_accrued,
  COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at >= (SELECT monthly_start FROM budget_calendar) AND u.observed_at < (SELECT monthly_end FROM budget_calendar)),0) AS monthly_accrued,
  COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at >= (SELECT weekly_start FROM budget_calendar) AND u.observed_at < (SELECT weekly_end FROM budget_calendar)),0) AS weekly_accrued,
  COALESCE(SUM(u.unpriced_attempts) FILTER (WHERE u.observed_at >= (SELECT monthly_start FROM budget_calendar) AND u.observed_at < (SELECT monthly_end FROM budget_calendar)),0)::bigint AS unpriced_attempts,
  COALESCE(SUM(u.charge) FILTER (WHERE u.observed_at >= (SELECT daily_start FROM budget_calendar) AND u.observed_at < (SELECT daily_end FROM budget_calendar)),0) AS daily_charged,
  COALESCE(SUM(u.charge) FILTER (WHERE u.observed_at >= (SELECT monthly_start FROM budget_calendar) AND u.observed_at < (SELECT monthly_end FROM budget_calendar)),0) AS monthly_charged,
  COALESCE(SUM(u.charge) FILTER (WHERE u.observed_at >= (SELECT weekly_start FROM budget_calendar) AND u.observed_at < (SELECT weekly_end FROM budget_calendar)),0) AS weekly_charged,
  COALESCE(SUM(u.uncharged_attempts) FILTER (WHERE u.observed_at >= (SELECT monthly_start FROM budget_calendar) AND u.observed_at < (SELECT monthly_end FROM budget_calendar)),0)::bigint AS uncharged_attempts
 FROM accounts a LEFT JOIN usage u ON u.id=a.id GROUP BY a.id
), pruned AS (
 DELETE FROM %[1]s
 WHERE %[3]s((window_kind='day' AND window_id<(SELECT daily_id FROM budget_calendar)) OR (window_kind='month' AND window_id<(SELECT monthly_id FROM budget_calendar)) OR (window_kind='week' AND window_id<(SELECT weekly_id FROM budget_calendar))) RETURNING 1
), desired AS (
 SELECT id,'day'::text AS window_kind,(SELECT daily_id FROM budget_calendar) AS window_id,daily_accrued AS accrued,0::bigint AS unpriced_attempts,daily_charged AS charged,0::bigint AS uncharged_attempts FROM totals
 UNION ALL
 SELECT id,'month',(SELECT monthly_id FROM budget_calendar),monthly_accrued,unpriced_attempts,monthly_charged,uncharged_attempts FROM totals
 UNION ALL SELECT id,'week',(SELECT weekly_id FROM budget_calendar),weekly_accrued,0,weekly_charged,0 FROM totals
), reconciled AS (
 INSERT INTO %[1]s (%[2]s,window_kind,window_id,accrued,unpriced_attempts,charged,uncharged_attempts,weekly_complete)
 SELECT id,window_kind,window_id,accrued,unpriced_attempts,charged,uncharged_attempts,true FROM desired
 ON CONFLICT (%[2]s,window_kind,window_id) DO UPDATE SET weekly_complete=true,
  accrued=GREATEST(%[1]s.accrued,EXCLUDED.accrued),
  unpriced_attempts=GREATEST(%[1]s.unpriced_attempts,EXCLUDED.unpriced_attempts),
  charged=GREATEST(%[1]s.charged,EXCLUDED.charged),
  uncharged_attempts=GREATEST(%[1]s.uncharged_attempts,EXCLUDED.uncharged_attempts)
 RETURNING %[2]s,window_kind,window_id,accrued,unpriced_attempts,charged,uncharged_attempts
) SELECT %[2]s::text,(SELECT daily_id FROM budget_calendar),
 MAX(accrued) FILTER (WHERE window_kind='day')::text,MAX(charged) FILTER (WHERE window_kind='day')::text,(SELECT monthly_id FROM budget_calendar),
 MAX(accrued) FILTER (WHERE window_kind='month')::text,MAX(charged) FILTER (WHERE window_kind='month')::text,
 MAX(unpriced_attempts) FILTER (WHERE window_kind='month')::bigint,
 MAX(uncharged_attempts) FILTER (WHERE window_kind='month')::bigint,
 (SELECT weekly_id FROM budget_calendar),MAX(accrued) FILTER (WHERE window_kind='week')::text,MAX(charged) FILTER (WHERE window_kind='week')::text,
 (SELECT daily_start FROM budget_calendar),(SELECT daily_end FROM budget_calendar),
 (SELECT monthly_start FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),
 (SELECT weekly_start FROM budget_calendar),(SELECT weekly_end FROM budget_calendar)
FROM reconciled GROUP BY %[2]s ORDER BY %[2]s`, s.table, s.column, s.pruneScope(scoped), head)
}

var reconciliationSnapshotsSQL = reconcileCostWindowsSQL(keyWindows, false, `accounts AS (
  SELECT id FROM olp.api_keys
  WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > $1::timestamptz)
), usage AS (
  SELECT a.id, fact.observed_at,
         (COALESCE(fact.estimated_cost, 0)+fact.cache_budget_cost)::numeric AS cost,
         (COALESCE(fact.charge, 0)+fact.cache_budget_charge)::numeric AS charge,
         CASE WHEN fact.charge_status <> 'not_billable' AND fact.unpriced
              THEN 1 ELSE 0 END::bigint AS unpriced_attempts,
         CASE WHEN fact.charge_status <> 'not_billable' AND fact.charge IS NULL
              THEN 1 ELSE 0 END::bigint AS uncharged_attempts
  FROM (SELECT * FROM olp.attempt_usage_facts WHERE NOT budget_exempt) fact
  JOIN accounts a ON a.id = fact.api_key_id OR a.id = fact.agent_principal_id
  WHERE fact.observed_at >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND fact.observed_at < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
  UNION ALL
  SELECT a.id, hourly.budget_bucket,
         (COALESCE(hourly.estimated_cost, 0)+hourly.cache_budget_cost)::numeric,
         (COALESCE(hourly.charge, 0)+hourly.cache_budget_charge)::numeric,
         hourly.unpriced_attempt_count, hourly.uncharged_attempt_count
  FROM (SELECT * FROM olp.attempt_usage_hourly WHERE NOT budget_exempt) hourly
  JOIN accounts a ON a.id = hourly.api_key_id OR a.id = hourly.agent_principal_id
  WHERE hourly.budget_bucket >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND hourly.budget_bucket < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
  UNION ALL
  SELECT a.id,d.closed_at,d.observed_cost,d.observed_charge,0::bigint,0::bigint
  FROM olp.execution_debits d JOIN accounts a ON EXISTS(SELECT 1 FROM olp.execution_debit_owners o WHERE o.dispatch_id=d.id AND o.owner_kind='key' AND o.owner_id=a.id)
  WHERE d.kind IN ('tool','agent') AND d.state='settled'
    AND d.closed_at >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND d.closed_at < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
)`)

var reconciliationGroupSnapshotsSQL = reconcileCostWindowsSQL(groupWindows, false, `accounts AS (
 SELECT id FROM olp.budget_groups_with_limits
 WHERE $1::timestamptz IS NOT NULL
   AND (effective_limits->>'daily_cost_limit' IS NOT NULL OR effective_limits->>'monthly_cost_limit' IS NOT NULL OR effective_limits->>'weekly_cost_limit' IS NOT NULL)
), usage AS (
 SELECT fact.budget_group_id AS id,fact.observed_at,(COALESCE(fact.estimated_cost,0)+fact.cache_budget_cost)::numeric AS cost,
   (COALESCE(fact.charge,0)+fact.cache_budget_charge)::numeric AS charge,
   CASE WHEN fact.charge_status<>'not_billable' AND fact.unpriced THEN 1 ELSE 0 END::bigint AS unpriced_attempts,
   CASE WHEN fact.charge_status<>'not_billable' AND fact.charge IS NULL THEN 1 ELSE 0 END::bigint AS uncharged_attempts
 FROM (SELECT * FROM olp.attempt_usage_facts WHERE NOT budget_exempt) fact JOIN accounts a ON a.id = fact.budget_group_id
 WHERE fact.observed_at >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND fact.observed_at < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
 UNION ALL
 SELECT hourly.budget_group_id AS id,hourly.budget_bucket,(COALESCE(hourly.estimated_cost,0)+hourly.cache_budget_cost)::numeric,(COALESCE(hourly.charge,0)+hourly.cache_budget_charge)::numeric,hourly.unpriced_attempt_count,hourly.uncharged_attempt_count
 FROM (SELECT * FROM olp.attempt_usage_hourly WHERE NOT budget_exempt) hourly JOIN accounts a ON a.id = hourly.budget_group_id
 WHERE hourly.budget_bucket >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND hourly.budget_bucket < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
 UNION ALL
 SELECT a.id,d.closed_at,d.observed_cost,d.observed_charge,0::bigint,0::bigint
 FROM olp.execution_debits d JOIN accounts a ON EXISTS(SELECT 1 FROM olp.execution_debit_owners o WHERE o.dispatch_id=d.id AND o.owner_kind='group' AND o.owner_id=a.id)
 WHERE d.kind IN ('tool','agent') AND d.state='settled'
   AND d.closed_at >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND d.closed_at < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
)`)

func aggregateOwnershipSQL(account, project, key, route, labels string) string {
	return fmt.Sprintf(`(%[1]s.installation_id IS NOT NULL OR
 %[1]s.organization_id=(SELECT organization_id FROM olp.projects WHERE id=%[2]s) OR
 (%[1]s.project_id=%[2]s AND (%[1]s.attribution_key IS NULL OR %[5]s->>%[1]s.attribution_key=%[1]s.attribution_value)) OR
 (%[1]s.api_key_id=%[3]s AND %[1]s.route_slug=%[4]s))`, account, project, key, route, labels)
}

var aggregateEventDeltasSQL = `WITH event_facts AS MATERIALIZED (
 SELECT * FROM olp.attempt_usage_facts WHERE event_id=$5::uuid AND ($6::uuid IS NULL OR attempt_id=$6::uuid) AND NOT budget_exempt
), subjects AS MATERIALIZED (
 SELECT ` + usageProjectSQL("f") + ` AS project_id,f.api_key_id,f.route_slug,` + aggregateFactLabelsSQL("f") + ` AS attribution FROM event_facts f
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
	aggregateOwnershipSQL("a", usageProjectSQL("f"), "f.api_key_id", "f.route_slug", aggregateFactLabelsSQL("f")) + `
 GROUP BY a.id ORDER BY a.id`

var aggregateSnapshotsSQL = reconcileCostWindowsSQL(aggregateWindows, true, `accounts AS (
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
 WHERE `+aggregateOwnershipSQL("a", "COALESCE(k.project_id,p.project_id,r.project_id)", "f.api_key_id", "f.route_slug", aggregateFactLabelsSQL("f"))+`
   AND f.observed_at >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND f.observed_at < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
 UNION ALL
 SELECT a.id,h.budget_bucket,(COALESCE(h.estimated_cost,0)+h.cache_budget_cost)::numeric,(COALESCE(h.charge,0)+h.cache_budget_charge)::numeric,h.unpriced_attempt_count,h.uncharged_attempt_count
 FROM accounts a JOIN (SELECT * FROM olp.attempt_usage_hourly WHERE NOT budget_exempt) h ON true
 LEFT JOIN olp.api_keys k ON k.id=h.api_key_id
 LEFT JOIN olp.providers p ON p.id=h.provider_id
 LEFT JOIN olp.routes r ON r.slug=h.route_slug
 WHERE `+aggregateOwnershipSQL("a", "COALESCE(k.project_id,p.project_id,r.project_id)", "h.api_key_id", "h.route_slug", aggregateFactLabelsSQL("h"))+`
   AND h.budget_bucket >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND h.budget_bucket < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
 UNION ALL
 SELECT a.id,d.closed_at,d.observed_cost,d.observed_charge,0::bigint,0::bigint
 FROM olp.execution_debits d JOIN accounts a ON EXISTS(SELECT 1 FROM olp.execution_debit_owners o WHERE o.dispatch_id=d.id AND o.owner_kind='aggregate' AND o.owner_id=a.id)
 WHERE d.kind IN ('tool','agent') AND d.state='settled'
   AND d.closed_at >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND d.closed_at < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
)`)

var endUserSnapshotsSQL = reconcileCostWindowsSQL(endUserWindows, true, `accounts AS (
 SELECT a.* FROM olp.end_user_accounts a
 WHERE $1::timestamptz IS NOT NULL AND ($2::uuid IS NULL OR a.id=$2::uuid)
), usage AS (
 SELECT a.id,f.observed_at,(COALESCE(f.estimated_cost,0)+f.cache_budget_cost)::numeric AS cost,
   (COALESCE(f.charge,0)+f.cache_budget_charge)::numeric AS charge,
   CASE WHEN f.charge_status<>'not_billable' AND f.unpriced THEN 1 ELSE 0 END::bigint AS unpriced_attempts,
   CASE WHEN f.charge_status<>'not_billable' AND f.charge IS NULL THEN 1 ELSE 0 END::bigint AS uncharged_attempts
 FROM accounts a JOIN (SELECT * FROM olp.attempt_usage_facts WHERE NOT budget_exempt) f ON f.end_user_digest=a.end_user_digest
 JOIN olp.api_keys k ON k.id=f.api_key_id
 WHERE (a.api_key_id=k.id OR a.project_id=k.project_id)
   AND f.observed_at >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND f.observed_at < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
 UNION ALL
 SELECT a.id,h.budget_bucket,(COALESCE(h.estimated_cost,0)+h.cache_budget_cost)::numeric,(COALESCE(h.charge,0)+h.cache_budget_charge)::numeric,h.unpriced_attempt_count,h.uncharged_attempt_count
 FROM accounts a JOIN (SELECT * FROM olp.attempt_usage_hourly WHERE NOT budget_exempt) h ON h.end_user_digest=a.end_user_digest
 JOIN olp.api_keys k ON k.id=h.api_key_id
 WHERE (a.api_key_id=k.id OR a.project_id=k.project_id)
   AND h.budget_bucket >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND h.budget_bucket < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
)`)

const addSupplyCostDeltaSQL = `WITH budget_calendar AS MATERIALIZED (SELECT * FROM olp.budget_windows($4::timestamptz)),
deltas (window_kind,window_id,accrued,unpriced_attempts) AS (
 VALUES ('day'::text,(SELECT daily_id FROM budget_calendar),$2::text::numeric,0::bigint),
        ('month'::text,(SELECT monthly_id FROM budget_calendar),$2::text::numeric,$3::bigint),
 ('week'::text,(SELECT weekly_id FROM budget_calendar),$2::text::numeric,0::bigint)
), applied AS (
 INSERT INTO olp.supply_cost_windows (owner_id,window_kind,window_id,accrued,unpriced_attempts)
 SELECT $1::uuid,window_kind,window_id,accrued,unpriced_attempts FROM deltas
 ON CONFLICT (owner_id,window_kind,window_id) DO UPDATE SET
   accrued=olp.supply_cost_windows.accrued+EXCLUDED.accrued,
   unpriced_attempts=olp.supply_cost_windows.unpriced_attempts+EXCLUDED.unpriced_attempts
 RETURNING owner_id,window_kind,window_id,accrued,unpriced_attempts,weekly_complete
) SELECT owner_id::text,
 MAX(window_id) FILTER (WHERE window_kind='day')::bigint,
 MAX(accrued) FILTER (WHERE window_kind='day')::text,
 MAX(window_id) FILTER (WHERE window_kind='month')::bigint,
 MAX(accrued) FILTER (WHERE window_kind='month')::text,
 MAX(unpriced_attempts) FILTER (WHERE window_kind='month')::bigint,
 MAX(window_id) FILTER(WHERE window_kind='week')::bigint,
 CASE WHEN bool_or(weekly_complete) FILTER(WHERE window_kind='week') THEN MAX(accrued) FILTER(WHERE window_kind='week')::text ELSE '' END
 , (SELECT daily_start FROM budget_calendar), (SELECT daily_end FROM budget_calendar), (SELECT monthly_start FROM budget_calendar), (SELECT monthly_end FROM budget_calendar), (SELECT weekly_start FROM budget_calendar), (SELECT weekly_end FROM budget_calendar)
FROM applied GROUP BY owner_id`

const supplySnapshotsSQL = `WITH budget_calendar AS MATERIALIZED (SELECT * FROM olp.budget_windows($1::timestamptz)),
pruned AS (
 DELETE FROM olp.supply_cost_windows
 WHERE (window_kind='day' AND window_id<(SELECT daily_id FROM budget_calendar)) OR (window_kind='month' AND window_id<(SELECT monthly_id FROM budget_calendar)) OR (window_kind='week' AND window_id<(SELECT weekly_id FROM budget_calendar)) RETURNING 1
), current AS (
 INSERT INTO olp.supply_cost_windows (owner_id,window_kind,window_id,accrued,unpriced_attempts)
 SELECT b.owner_id,w.kind,w.id,0,0 FROM olp.supply_budgets b
 CROSS JOIN (VALUES ('day'::text,(SELECT daily_id FROM budget_calendar)),('month'::text,(SELECT monthly_id FROM budget_calendar))) AS w(kind,id)
 ON CONFLICT (owner_id,window_kind,window_id) DO UPDATE SET accrued=olp.supply_cost_windows.accrued
 RETURNING owner_id,window_kind,window_id,accrued,unpriced_attempts
) SELECT owner_id::text,(SELECT daily_id FROM budget_calendar),
 MAX(accrued) FILTER (WHERE window_kind='day')::text,(SELECT monthly_id FROM budget_calendar),
 MAX(accrued) FILTER (WHERE window_kind='month')::text,
 MAX(unpriced_attempts) FILTER (WHERE window_kind='month')::bigint,(SELECT weekly_id FROM budget_calendar),''::text
 , (SELECT daily_start FROM budget_calendar), (SELECT daily_end FROM budget_calendar), (SELECT monthly_start FROM budget_calendar), (SELECT monthly_end FROM budget_calendar), (SELECT weekly_start FROM budget_calendar), (SELECT weekly_end FROM budget_calendar)
FROM current GROUP BY owner_id ORDER BY owner_id`

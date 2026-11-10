package ledger

import (
	"fmt"

	"github.com/tyk-swe/olp/internal/attribution"
)

type budgetOwnership struct{ raw, hourly, tool string }

func budgetTotalsSQL(owner budgetOwnership, basis string) string {
	return fmt.Sprintf(` FROM olp.budget_windows(now()) w,
 LATERAL (SELECT
 COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at>=w.daily_start AND u.observed_at<w.daily_end),0) AS daily_accrued,
 COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at>=w.weekly_start AND u.observed_at<w.weekly_end),0) AS weekly_accrued,
 COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at>=w.monthly_start AND u.observed_at<w.monthly_end),0) AS monthly_accrued,
 COALESCE(SUM(u.charge) FILTER (WHERE u.observed_at>=w.daily_start AND u.observed_at<w.daily_end),0) AS daily_charged,
 COALESCE(SUM(u.charge) FILTER (WHERE u.observed_at>=w.weekly_start AND u.observed_at<w.weekly_end),0) AS weekly_charged,
 COALESCE(SUM(u.charge) FILTER (WHERE u.observed_at>=w.monthly_start AND u.observed_at<w.monthly_end),0) AS monthly_charged,
 COALESCE(SUM(u.unpriced_attempts) FILTER (WHERE u.observed_at>=w.monthly_start AND u.observed_at<w.monthly_end),0)::bigint AS unpriced_attempts,
 COALESCE(SUM(u.uncharged_attempts) FILTER (WHERE u.observed_at>=w.monthly_start AND u.observed_at<w.monthly_end),0)::bigint AS uncharged_attempts
 FROM (SELECT f.observed_at,(COALESCE(f.estimated_cost,0)+f.cache_budget_cost)::numeric AS cost,(COALESCE(f.charge,0)+f.cache_budget_charge)::numeric AS charge,
 CASE WHEN f.charge_status<>'not_billable' AND f.unpriced THEN 1 ELSE 0 END::bigint AS unpriced_attempts,
 CASE WHEN f.charge_status<>'not_billable' AND f.charge IS NULL THEN 1 ELSE 0 END::bigint AS uncharged_attempts
 FROM olp.attempt_usage_facts f WHERE NOT f.budget_exempt AND (%s)
 AND f.observed_at>=LEAST(w.daily_start,w.weekly_start,w.monthly_start) AND f.observed_at<GREATEST(w.daily_end,w.weekly_end,w.monthly_end)
 UNION ALL SELECT h.budget_bucket,(COALESCE(h.estimated_cost,0)+h.cache_budget_cost)::numeric,(COALESCE(h.charge,0)+h.cache_budget_charge)::numeric,h.unpriced_attempt_count,h.uncharged_attempt_count
 FROM olp.attempt_usage_hourly h WHERE NOT h.budget_exempt AND (%s)
 AND h.budget_bucket>=LEAST(w.daily_start,w.weekly_start,w.monthly_start) AND h.budget_bucket<GREATEST(w.daily_end,w.weekly_end,w.monthly_end)
 UNION ALL SELECT d.closed_at,d.observed_cost,d.observed_charge,0::bigint,0::bigint
 FROM olp.execution_debits d WHERE d.kind IN ('tool','agent') AND d.state='settled' AND (%s)
 AND d.closed_at>=LEAST(w.daily_start,w.weekly_start,w.monthly_start) AND d.closed_at<GREATEST(w.daily_end,w.weekly_end,w.monthly_end)) u) t
 , LATERAL (SELECT %s='charge' AS charged) basis`, owner.raw, owner.hourly, owner.tool, basis)
}

func budgetReportSQL(owner budgetOwnership, basis string) string {
	return `(SELECT jsonb_build_object(
 'daily',jsonb_build_object('accrued',CASE WHEN basis.charged THEN t.daily_charged ELSE t.daily_accrued END::text,'window_ends_at',w.daily_end),
 'weekly',jsonb_build_object('accrued',CASE WHEN basis.charged THEN t.weekly_charged ELSE t.weekly_accrued END::text,'window_ends_at',w.weekly_end),
 'monthly',jsonb_build_object('accrued',CASE WHEN basis.charged THEN t.monthly_charged ELSE t.monthly_accrued END::text,'window_ends_at',w.monthly_end),
 'cost_basis',CASE WHEN basis.charged THEN 'charge' ELSE 'cost' END,
 'unpriced_attempts',t.unpriced_attempts,'uncharged_attempts',t.uncharged_attempts)` + budgetTotalsSQL(owner, basis) + `)`
}

func KeyBudgetSQL(key string) string {
	return budgetReportSQL(budgetOwnership{"(f.api_key_id=" + key + ".id OR f.agent_principal_id=" + key + ".id)", "(h.api_key_id=" + key + ".id OR h.agent_principal_id=" + key + ".id)", "EXISTS(SELECT 1 FROM olp.execution_debit_owners o WHERE o.dispatch_id=d.id AND o.owner_kind='key' AND o.owner_id=" + key + ".id)"}, `COALESCE(olp.effective_limits(`+key+`.policy,(SELECT p.limit_templates->(`+key+`.policy->>'limit_template') FROM olp.projects p WHERE p.id=`+key+`.project_id))->>'cost_basis','cost')`)
}

func GroupBudgetSQL(group string) string {
	return `(SELECT jsonb_build_object(
 'daily',jsonb_build_object('accrued',CASE WHEN basis.charged THEN t.daily_charged ELSE t.daily_accrued END::text,'limit',(` + group + `.effective_limits->>'daily_cost_limit')::numeric::text,
 'remaining',CASE WHEN ` + group + `.effective_limits->>'daily_cost_limit' IS NULL THEN NULL ELSE GREATEST((` + group + `.effective_limits->>'daily_cost_limit')::numeric-(CASE WHEN basis.charged THEN t.daily_charged ELSE t.daily_accrued END),0)::text END,'reset_at',w.daily_end),
 'weekly',jsonb_build_object('accrued',CASE WHEN basis.charged THEN t.weekly_charged ELSE t.weekly_accrued END::text,'limit',(` + group + `.effective_limits->>'weekly_cost_limit')::numeric::text,
 'remaining',CASE WHEN ` + group + `.effective_limits->>'weekly_cost_limit' IS NULL THEN NULL ELSE GREATEST((` + group + `.effective_limits->>'weekly_cost_limit')::numeric-(CASE WHEN basis.charged THEN t.weekly_charged ELSE t.weekly_accrued END),0)::text END,'reset_at',w.weekly_end),
 'monthly',jsonb_build_object('accrued',CASE WHEN basis.charged THEN t.monthly_charged ELSE t.monthly_accrued END::text,'limit',(` + group + `.effective_limits->>'monthly_cost_limit')::numeric::text,
 'remaining',CASE WHEN ` + group + `.effective_limits->>'monthly_cost_limit' IS NULL THEN NULL ELSE GREATEST((` + group + `.effective_limits->>'monthly_cost_limit')::numeric-(CASE WHEN basis.charged THEN t.monthly_charged ELSE t.monthly_accrued END),0)::text END,'reset_at',w.monthly_end),
 'cost_basis',COALESCE(` + group + `.effective_limits->>'cost_basis','cost'),
 'unpriced_attempts',t.unpriced_attempts,'uncharged_attempts',t.uncharged_attempts)` + budgetTotalsSQL(budgetOwnership{"f.budget_group_id=" + group + ".id", "h.budget_group_id=" + group + ".id", "EXISTS(SELECT 1 FROM olp.execution_debit_owners o WHERE o.dispatch_id=d.id AND o.owner_kind='group' AND o.owner_id=" + group + ".id)"}, "COALESCE("+group+".effective_limits->>'cost_basis','cost')") + `)`
}

func budgetProjectSQL(key, provider, route string) string {
	return fmt.Sprintf("COALESCE((SELECT project_id FROM olp.api_keys WHERE id=%s),(SELECT project_id FROM olp.providers WHERE id=%s),(SELECT project_id FROM olp.routes WHERE slug=%s))", key, provider, route)
}
func usageProjectSQL(alias string) string {
	return budgetProjectSQL(alias+".api_key_id", alias+".provider_id", alias+".route_slug")
}

func aggregateFactLabelsSQL(alias string) string {
	return fmt.Sprintf(`((COALESCE(%[1]s.attribution,'{}'::jsonb) - '%[2]s') || CASE WHEN NULLIF(%[1]s.delegation_pair_digest,'') IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('%[2]s',%[1]s.delegation_pair_digest) END)`, alias, attribution.DelegationPairKey)
}

func InstallationBudgetSQL(installation string) string {
	return budgetReportSQL(budgetOwnership{"true", "true", "true"}, "COALESCE("+installation+".budget_policy->>'cost_basis','cost')")
}

func ProjectBudgetSQL(project string) string {
	return budgetReportSQL(budgetOwnership{
		usageProjectSQL("f") + "=" + project + ".id", usageProjectSQL("h") + "=" + project + ".id", "d.project_id=" + project + ".id",
	}, "COALESCE("+project+".budget_policy->>'cost_basis','cost')")
}

func AttributionBudgetsSQL(project string) string {
	return `(SELECT COALESCE(jsonb_object_agg(label,pair_usage),'{}'::jsonb) FROM (
 SELECT b.key AS label,jsonb_object_agg(v.key,` + budgetReportSQL(budgetOwnership{
		usageProjectSQL("f") + "=" + project + ".id AND " + aggregateFactLabelsSQL("f") + "->>b.key=v.key",
		usageProjectSQL("h") + "=" + project + ".id AND " + aggregateFactLabelsSQL("h") + "->>b.key=v.key",
		"EXISTS(SELECT 1 FROM olp.execution_debit_owners o JOIN olp.aggregate_budget_accounts a ON a.id=o.owner_id WHERE o.dispatch_id=d.id AND o.owner_kind='aggregate' AND a.project_id=" + project + ".id AND a.attribution_key=b.key AND a.attribution_value=v.key)",
	}, "COALESCE(v.value->>'cost_basis','cost')") + `) AS pair_usage
 FROM jsonb_each(` + project + `.attribution_budgets) b CROSS JOIN LATERAL jsonb_each(b.value) v GROUP BY b.key
) grouped)`
}

func DelegationPairBudgetSQL(project, pair string) string {
	return budgetReportSQL(budgetOwnership{
		usageProjectSQL("f") + "=" + project + ".id AND f.delegation_pair_digest=" + pair + ".digest",
		usageProjectSQL("h") + "=" + project + ".id AND h.delegation_pair_digest=" + pair + ".digest",
		"EXISTS(SELECT 1 FROM olp.execution_debit_owners o JOIN olp.aggregate_budget_accounts a ON a.id=o.owner_id WHERE o.dispatch_id=d.id AND o.owner_kind='aggregate' AND a.project_id=" + project + ".id AND a.attribution_key='delegation_pair' AND a.attribution_value=" + pair + ".digest)",
	}, "COALESCE("+project+".attribution_budgets->'delegation_pair'->"+pair+".digest->>'cost_basis','cost')")
}

func OrganizationBudgetSQL(organization string) string {
	return budgetReportSQL(budgetOwnership{
		"(SELECT organization_id FROM olp.projects WHERE id=" + usageProjectSQL("f") + ")=" + organization + ".id",
		"(SELECT organization_id FROM olp.projects WHERE id=" + usageProjectSQL("h") + ")=" + organization + ".id",
		"(SELECT organization_id FROM olp.projects WHERE id=d.project_id)=" + organization + ".id",
	}, "COALESCE("+organization+".budget_policy->>'cost_basis','cost')")
}

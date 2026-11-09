package focus

import (
	"github.com/tyk-swe/olp/internal/export/destination"
	"github.com/tyk-swe/olp/internal/usage"
)

type basis struct {
	name         destination.CostBasis
	rowsSQL      string
	countsSQL    string
	accountedSQL string
}

func (b basis) billsCallers() bool {
	return b.name == destination.Charge
}

type pricingSQL struct {
	included, callerPaid, total, lines string
	listFallback, contractedFallback   string
	source, shares                     string
}

var bases = map[destination.CostBasis]basis{
	destination.Cost: newBasis(destination.Cost, pricingSQL{
		included:           `f.estimated_cost IS NOT NULL AND NOT f.caller_credential AND NOT f.budget_exempt`,
		callerPaid:         `f.usage_complete AND f.estimated_cost IS NOT NULL AND (f.caller_credential OR f.budget_exempt)`,
		total:              `f.estimated_cost`,
		lines:              `cost_lines`,
		listFallback:       `COALESCE(f.list_cost, f.estimated_cost + COALESCE(f.commitment_weight,0))`,
		contractedFallback: `COALESCE(f.contracted_cost, f.estimated_cost + COALESCE(f.commitment_weight,0))`,
		source: factsSourceSQL + ` AND NOT EXISTS(SELECT 1 FROM olp.commitment_usage u WHERE u.attempt_id=f.attempt_id)
 UNION ALL SELECT u.payload,COALESCE(a.allocated_cost,0),p.schedule_id FROM olp.commitment_usage u
 JOIN olp.commitment_periods p ON p.id=u.period_id LEFT JOIN olp.commitment_allocations a ON a.attempt_id=u.attempt_id
 WHERE u.observed_at >= $1::date AND u.observed_at < $1::date+1`,
		shares: `, focus_buckets AS (
 SELECT DISTINCT provider_id, commitment_period, commitment_bucket FROM facts WHERE commitment_period IS NOT NULL),
 commitment_shares AS (
 SELECT p.provider_id, p.commitment_period, p.commitment_bucket, w.period_weight, w.latest_amount
 FROM focus_buckets p CROSS JOIN LATERAL (` + usage.CommitmentSharesLateral() + `) w)`,
	}),
	destination.Charge: newBasis(destination.Charge, pricingSQL{
		included:           `f.charge IS NOT NULL`,
		callerPaid:         `false`,
		total:              `f.charge`,
		lines:              `charge_lines`,
		listFallback:       `f.charge`,
		contractedFallback: `f.charge`,
		source:             factsSourceSQL,
		shares: `, commitment_shares AS (SELECT NULL::uuid AS provider_id, NULL::text AS commitment_period,
 NULL::timestamp AS commitment_bucket, NULL::numeric AS period_weight, NULL::numeric AS latest_amount WHERE false)`,
	}),
}

const (
	factsSourceSQL = `SELECT to_jsonb(f) AS doc, NULL::numeric AS allocated, NULL::uuid AS schedule_id
 FROM olp.attempt_usage_facts f WHERE f.observed_at >= $1::date AND f.observed_at < $1::date+1`

	factsFromSQL = ` FROM source src CROSS JOIN LATERAL jsonb_populate_record(NULL::olp.attempt_usage_facts,src.doc) f
 LEFT JOIN olp.api_keys k ON k.id=f.api_key_id
 LEFT JOIN olp.providers pr ON pr.id=f.provider_id
 LEFT JOIN olp.routes rt ON rt.slug=f.route_slug`

	ownerSQL = `COALESCE(k.project_id, pr.project_id, rt.project_id)`

	scopeSQL = ` WHERE f.observed_at >= $1::date AND f.observed_at < ($1::date + 1)::timestamptz
 AND f.charge_status='billable'
 AND ($2::text IS NULL OR (CASE WHEN src.schedule_id IS NOT NULL THEN src.doc->>'consumer' ELSE ` + ownerSQL + `::text END) = $2)
 AND ($3::text IS NULL OR f.route_slug = $3)`

	lineListCostSQL = `(line->>'quantity')::numeric*(line->>'list_unit_price')::numeric`

	purchasesSQL = `SELECT s.provider_id::text, s.period, p.bucket_start AT TIME ZONE 'UTC', p.amount::text,
 s.provider_name, COALESCE(s.vendor_id,''), COALESCE(s.provider_kind,''), s.id::text,
 (p.amount-COALESCE((SELECT SUM(allocated_cost) FROM olp.commitment_allocations a WHERE a.period_id=p.id),0))::text
 FROM olp.commitment_periods p JOIN olp.commitment_schedules s ON s.id=p.schedule_id
 WHERE p.bucket_start >= $1::date AND p.bucket_start < ($1::date+1)::timestamptz
 ORDER BY p.bucket_start,s.id`
)

func scheduled(key, otherwise string) string {
	return `CASE WHEN src.schedule_id IS NOT NULL THEN src.doc->>'` + key + `' ELSE ` + otherwise + ` END`
}

func newBasis(name destination.CostBasis, p pricingSQL) basis {
	withSource := `WITH source AS (` + p.source + `)`
	included := ` AND f.usage_complete AND (` + p.included + `)`
	return basis{
		name: name,
		rowsSQL: withSource + `, facts AS (
 SELECT f.attempt_id::text, f.provider_id, f.route_slug, f.api_key_id::text, src.allocated, src.schedule_id,
  f.upstream_model, f.operation, f.attribution::text, f.commitment_period,
  CASE WHEN f.commitment_period IS NULL THEN NULL
   ELSE COALESCE(f.commitment_bucket AT TIME ZONE 'UTC',date_trunc(f.commitment_period, f.observed_at AT TIME ZONE 'UTC')) END AS commitment_bucket,
  ` + scheduled("consumer", ownerSQL+`::text`) + ` AS consumer,
  ` + scheduled("consumer_name", `pj.name`) + ` AS consumer_name,
  COALESCE(src.doc->>'connection_name',pr.name) AS connection_name, COALESCE(src.doc->>'provider_kind',pr.kind) AS kind,
  ` + scheduled("vendor_id", `pr.configuration#>>'{options,vendor_id}'`) + ` AS vendor_id,
  ` + scheduled("provider_project", `pr.project_id::text`) + ` AS provider_project,
  COALESCE(f.` + p.lines + `, jsonb_build_array(jsonb_build_object(
   'component','attempt','tier',null,'unit','Requests','quantity','1',
   'list_unit_price', trim_scale(` + p.listFallback + `)::text,
   'contracted_unit_price', trim_scale(` + p.contractedFallback + `)::text))) AS lines
 ` + factsFromSQL + `
 LEFT JOIN olp.projects pj ON pj.id=` + ownerSQL + scopeSQL + included + `)` + p.shares + `, expanded AS (
 SELECT f.*,line,
  CASE WHEN allocated IS NOT NULL THEN CASE WHEN SUM(` + lineListCostSQL + `) OVER (PARTITION BY attempt_id)>0 THEN
   round(allocated*SUM(` + lineListCostSQL + `) OVER (PARTITION BY attempt_id ORDER BY ord)/SUM(` + lineListCostSQL + `) OVER (PARTITION BY attempt_id),12)
   -round(allocated*(SUM(` + lineListCostSQL + `) OVER (PARTITION BY attempt_id ORDER BY ord)-` + lineListCostSQL + `)/SUM(` + lineListCostSQL + `) OVER (PARTITION BY attempt_id),12)
   ELSE 0 END END AS line_allocation
 FROM facts f CROSS JOIN LATERAL jsonb_array_elements(f.lines) WITH ORDINALITY x(line,ord))
SELECT f.consumer, COALESCE(f.consumer_name,''), f.provider_id::text, COALESCE(f.connection_name,''),
 COALESCE(f.vendor_id,''), COALESCE(f.kind,''),
 f.upstream_model, f.operation, COALESCE(f.route_slug,''), COALESCE(f.api_key_id,''),
 COALESCE(f.attribution,''),
 (f.commitment_period IS NOT NULL) AS committed,
 COALESCE(f.commitment_period,''), COALESCE(f.commitment_bucket::text,''),
 line->>'component', line->>'tier', line->>'unit',
 (line->>'list_unit_price')::numeric, (line->>'contracted_unit_price')::numeric,
 trim_scale(SUM((line->>'quantity')::numeric))::text,
 trim_scale(SUM(` + lineListCostSQL + `))::text,
 trim_scale(SUM((line->>'quantity')::numeric * (line->>'contracted_unit_price')::numeric))::text,
 s.period_weight::text, s.latest_amount::text,
 count(DISTINCT f.attempt_id), COALESCE(f.schedule_id::text,''), SUM(f.line_allocation)::text
FROM expanded f
LEFT JOIN commitment_shares s ON s.provider_id=f.provider_id AND s.commitment_period=f.commitment_period
 AND s.commitment_bucket=f.commitment_bucket
GROUP BY f.consumer, f.consumer_name, f.provider_id, f.connection_name, f.vendor_id, f.kind,
 f.provider_project, f.upstream_model, f.operation, f.route_slug, f.api_key_id, f.attribution,
 f.commitment_period IS NOT NULL, f.commitment_period, f.commitment_bucket,
 line->>'component', line->>'tier', line->>'unit',
 line->>'list_unit_price', line->>'contracted_unit_price', s.period_weight, s.latest_amount, f.schedule_id
ORDER BY 1,3,7,8,9,10,11,12,13,14,15,16,17,18,19,20`,
		countsSQL: withSource + ` SELECT
 count(*) FILTER (WHERE f.usage_complete AND (` + p.included + `)),
 count(*) FILTER (WHERE f.usage_complete AND (` + p.included + `) AND f.` + p.lines + ` IS NULL),
 count(*) FILTER (WHERE f.unpriced),
 count(*) FILTER (WHERE NOT f.usage_complete),
 count(*) FILTER (WHERE f.charge IS NULL),
 count(*) FILTER (WHERE ` + p.callerPaid + `),
 EXISTS(SELECT 1 FROM olp.request_metadata_ingestion_gaps g
  WHERE g.first_observed_at < ($1::date + 1)::timestamptz AND g.last_observed_at >= $1::date)` + factsFromSQL + scopeSQL,
		accountedSQL: withSource + ` SELECT trim_scale(SUM(` + p.total + `))::text` + factsFromSQL + scopeSQL + included,
	}
}

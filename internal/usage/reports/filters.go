package reports

import (
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/usage"
)

// Filters select the slice of usage a report covers. Start is inclusive and
// End exclusive, so adjacent ranges tile without double counting.
type Filters struct {
	Start, End       time.Time
	Route            *string
	ProviderID       *string
	Model            *string
	APIKey           *string
	Operation        *string
	AttributionKey   *string
	AttributionValue *string
	Projects         ProjectScope
	providerIDs      []string
}

// filterQuery assembles one parameterised statement. Filters are optional and
// vary per request, so the text is built alongside its arguments and every
// value is bound; nothing a caller supplies is ever concatenated into SQL.
type filterQuery struct {
	text strings.Builder
	args []any
}

func (q *filterQuery) push(sql string) { q.text.WriteString(sql) }

// bind records one argument and returns its placeholder.
func (q *filterQuery) bind(value any) string {
	q.args = append(q.args, value)
	return "$" + strconv.Itoa(len(q.args))
}

// pushBind appends a fragment followed by the placeholder for one value.
func (q *filterQuery) pushBind(sql string, value any) { q.push(sql + q.bind(value)) }

func (q *filterQuery) sql() string { return q.text.String() }

// countScope names which of the fact table's request-count flags a report must
// use. A request that failed over to a second provider counts once per request,
// once per provider, once per model, and once per provider/model target, so the
// filters in force decide which of those counts answers the question asked.
type countScope struct{ count, unpriced, incomplete, hourlyCount, hourlyUnpriced, hourlyIncomplete string }

func scopeFor(f Filters) countScope {
	switch {
	case f.ProviderID != nil && f.Model != nil:
		return scopeTarget
	case f.ProviderID != nil:
		return scopeProvider
	case f.Model != nil:
		return scopeModel
	default:
		return scopeRequest
	}
}

// validate rejects a range that cannot be answered exactly. The report window
// must be positive and bounded, and an operation filter must name a real
// operation rather than silently matching nothing.
func (f Filters) validate() error {
	if !f.End.After(f.Start) || f.End.Sub(f.Start) > MaxUsageRangeDays*24*time.Hour {
		return fail(FailureInvalidRange,
			"Use a positive usage range no longer than 366 days.")
	}
	if f.Operation != nil && !usage.ValidOperation(*f.Operation) {
		return fail(FailureInvalidOperation, "The operation filter is invalid.")
	}
	return validateAttributionFilter(f.AttributionKey, f.AttributionValue)
}

// validateAttributionFilter checks an attribution key and value filter pair.
func validateAttributionFilter(key, value *string) error {
	if value != nil && key == nil {
		return fail(FailureInvalidFilter,
			"The attribution_value filter requires attribution_key.")
	}
	if key != nil && !attribution.ValidKey(*key) {
		return fail(FailureInvalidFilter, "The attribution_key filter is invalid.")
	}
	if value != nil && !attribution.ValidValue(*value) {
		return fail(FailureInvalidFilter, "The attribution_value filter is invalid.")
	}
	return nil
}

// dimensions appends the dimension filters, which are spelled identically in
// the fact table, the hourly rollup and the boundary probe.
func (f Filters) dimensions(q *filterQuery) {
	if !f.Projects.all {
		q.push(" AND api_key_id IN (SELECT id FROM olp.api_keys WHERE project_id = ANY(" + q.bind(f.Projects.ids) + "::uuid[]))")
	}
	if f.Route != nil {
		q.pushBind(" AND route_slug = ", *f.Route)
	}
	if f.ProviderID != nil {
		q.pushBind(" AND provider_id = ", *f.ProviderID)
	}
	if len(f.providerIDs) > 0 {
		q.push(" AND provider_id = ANY(" + q.bind(f.providerIDs) + "::uuid[])")
	}
	if f.Model != nil {
		q.pushBind(" AND upstream_model = ", *f.Model)
	}
	if f.APIKey != nil {
		q.pushBind(" AND api_key_id = ", *f.APIKey)
	}
	if f.Operation != nil {
		q.pushBind(" AND operation = ", *f.Operation)
	}
	if f.AttributionKey != nil {
		q.pushBind(" AND attribution ? ", *f.AttributionKey)
	}
	if f.AttributionValue != nil {
		q.push(" AND attribution->>" + q.bind(*f.AttributionKey) + " = " + q.bind(*f.AttributionValue))
	}
}

// estimatePaired selects the fact rows whose estimation error is knowable: an
// attempt that had an estimate and whose provider reported its input. An
// attempt that failed before reporting, or was never estimated, says nothing
// about how close the estimate was and must not dilute it.
const EstimatePaired = "usage_observed AND input_tokens IS NOT NULL AND estimated_input_tokens IS NOT NULL"

// usageRows opens the statement with the CTE both sources feed. Live facts are
// filtered on the exact range; retained hourly rows are included only where a
// whole bucket lies inside it, so no aggregate is ever cut in half.
func (f Filters) usageRows(q *filterQuery, scope countScope) {
	q.push("WITH live_facts AS (SELECT attempt_id, observed_at, route_slug, provider_id, upstream_model," +
		" api_key_id, operation, surface, attribution, end_user_digest, model_family, estimate_provenance," +
		" CASE WHEN " + scope.count + " THEN 1 ELSE 0 END::bigint AS request_count," +
		" COALESCE(input_tokens, 0)::numeric AS input_tokens," +
		" COALESCE(output_tokens, 0)::numeric AS output_tokens," +
		" COALESCE(cached_input_tokens, 0)::numeric AS cached_input_tokens," +
		" COALESCE(cache_write_input_tokens, 0)::numeric AS cache_write_input_tokens," +
		" COALESCE(cache_write_5m_input_tokens, 0)::numeric AS cache_write_5m_input_tokens," +
		" COALESCE(cache_write_1h_input_tokens, 0)::numeric AS cache_write_1h_input_tokens," +
		" COALESCE(audio_input_tokens, 0)::numeric AS audio_input_tokens," +
		" COALESCE(cached_audio_input_tokens, 0)::numeric AS cached_audio_input_tokens," +
		" COALESCE(audio_output_tokens, 0)::numeric AS audio_output_tokens," +
		" COALESCE(image_input_tokens, 0)::numeric AS image_input_tokens," +
		" COALESCE(cached_image_input_tokens, 0)::numeric AS cached_image_input_tokens," +
		" COALESCE(image_output_tokens, 0)::numeric AS image_output_tokens," +
		" COALESCE(training_tokens,0)::numeric AS training_tokens,COALESCE(storage_byte_seconds,0)::numeric AS storage_byte_seconds,COALESCE(cache_token_seconds,0)::numeric AS cache_token_seconds,container_sessions," +
		" COALESCE(media_units, 0)::numeric AS media_units, estimated_cost, charge," +
		" CASE WHEN charge_status <> 'not_billable' AND charge IS NULL THEN 1 ELSE 0 END AS uncharged_attempt_count," +
		" CASE WHEN " + scope.unpriced + " THEN 1 ELSE 0 END::bigint AS unpriced_count," +
		" CASE WHEN " + scope.incomplete + " THEN 1 ELSE 0 END::bigint AS incomplete_count," +
		" currency::text AS currency," +
		" CASE WHEN " + EstimatePaired + " THEN estimated_input_tokens ELSE 0 END::numeric AS estimated_input_tokens," +
		" CASE WHEN " + EstimatePaired + " THEN input_tokens ELSE 0 END::numeric AS estimate_reported_input_tokens," +
		" CASE WHEN " + EstimatePaired + " THEN 1 ELSE 0 END::bigint AS estimate_attempt_count," +
		" commitment_weight::numeric AS commitment_weight," +
		" commitment_amount::numeric AS commitment_amount," +
		" commitment_period," +
		" CASE WHEN commitment_period IS NOT NULL THEN date_trunc(commitment_period, observed_at AT TIME ZONE 'UTC')" +
		" END AS commitment_bucket," +
		// Reconciliation attributes spend only to operator-cost rows: caller
		// credentials bill the caller and budget-exempt attempts sit outside
		// what the provider invoices for.
		" budget_exempt, caller_credential," +
		" CASE WHEN NOT response_cache AND charge_status <> 'not_billable' AND usage_complete AND estimated_cost IS NOT NULL" +
		" THEN 1 ELSE 0 END::bigint AS reconcil_priced_attempts," +
		" CASE WHEN NOT response_cache AND charge_status <> 'not_billable' AND unpriced" +
		" THEN 1 ELSE 0 END::bigint AS reconcil_unpriced_attempts," +
		" CASE WHEN NOT response_cache AND charge_status <> 'not_billable' AND NOT usage_complete" +
		" THEN 1 ELSE 0 END::bigint AS reconcil_incomplete_attempts," +
		" CASE WHEN NOT response_cache AND charge_status <> 'not_billable' AND (caller_credential OR budget_exempt)" +
		" THEN 1 ELSE 0 END::bigint AS reconcil_caller_attempts" +
		" FROM olp.attempt_usage_facts WHERE true")
	q.pushBind(" AND observed_at >= ", f.Start)
	q.pushBind(" AND observed_at < ", f.End)
	f.dimensions(q)
	q.push("), live_commitments AS (" +
		" SELECT u.attempt_id, u.period_id FROM live_facts f" +
		" JOIN olp.commitment_usage u USING (attempt_id)" +
		" JOIN olp.commitment_periods p ON p.id = u.period_id AND NOT p.legacy" +
		"), durable_periods AS (SELECT DISTINCT period_id FROM live_commitments)," +
		" durable_weights AS (SELECT u.attempt_id, u.weight, p.amount," +
		" SUM(u.weight) OVER (PARTITION BY u.period_id) AS total_weight," +
		" SUM(u.weight) OVER (PARTITION BY u.period_id ORDER BY u.observed_at, u.attempt_id ROWS UNBOUNDED PRECEDING) AS running_weight" +
		" FROM durable_periods d JOIN olp.commitment_usage u ON u.period_id = d.period_id" +
		" JOIN olp.commitment_periods p ON p.id = u.period_id WHERE u.eligible" +
		"), durable_allocations AS (SELECT c.attempt_id," +
		" CASE WHEN w.total_weight > 0 THEN" +
		" round(w.amount * w.running_weight / w.total_weight, 12) -" +
		" round(w.amount * (w.running_weight - w.weight) / w.total_weight, 12)" +
		" ELSE 0 END AS allocated_cost" +
		" FROM live_commitments c LEFT JOIN durable_weights w USING (attempt_id)" +
		"), usage_rows AS (SELECT f.*, da.allocated_cost AS durable_allocation" +
		" FROM live_facts f LEFT JOIN durable_allocations da USING (attempt_id)")
	q.push(" UNION ALL SELECT NULL::uuid, bucket AS observed_at, route_slug, provider_id, upstream_model," +
		" api_key_id, operation, surface, attribution, end_user_digest, model_family, estimate_provenance, " +
		scope.hourlyCount + ", input_tokens, output_tokens," +
		" cached_input_tokens, cache_write_input_tokens, cache_write_5m_input_tokens," +
		" cache_write_1h_input_tokens," +
		" audio_input_tokens, cached_audio_input_tokens, audio_output_tokens," +
		" image_input_tokens, cached_image_input_tokens, image_output_tokens," +
		" training_tokens,storage_byte_seconds,cache_token_seconds,container_sessions,media_units, estimated_cost, charge, uncharged_attempt_count, " + scope.hourlyUnpriced + ", " +
		scope.hourlyIncomplete + ", currency::text AS currency, estimated_input_tokens," +
		" estimate_reported_input_tokens, estimate_attempt_count, commitment_weight," +
		" commitment_amount, commitment_period," +
		" CASE WHEN commitment_period IS NOT NULL THEN date_trunc(commitment_period, bucket AT TIME ZONE 'UTC')" +
		" END, budget_exempt, false," +
		// Hourly rows keep only request-grain counts beyond unpriced attempts:
		// the priced and incomplete attempt counts reconciliation reports for
		// rolled-up hours approximate them with provider-scope request counts.
		" GREATEST(0::bigint, provider_request_count - provider_cache_hit_count - provider_unpriced_count - provider_incomplete_count)," +
		" unpriced_attempt_count, provider_incomplete_count," +
		// An hourly row cannot tell caller-paid from budget-exempt, so the
		// excluded count for rolled-up hours is the exempt request count.
		" CASE WHEN budget_exempt THEN provider_request_count - provider_cache_hit_count ELSE 0 END::bigint," +
		" olp.commitment_hourly_allocation(attempt_usage_hourly)" +
		" FROM olp.attempt_usage_hourly WHERE true")
	q.pushBind(" AND bucket >= ", ceilHour(f.Start))
	q.pushBind(" AND bucket + interval '1 hour' <= ", f.End)
	f.dimensions(q)
	q.push(")")
	// A committed row shares its commitment period's amount by list-cost weight.
	// The divisor is the whole period's weight — every fact and hourly row the
	// (provider, period) bucket holds, whether or not this report's filters
	// select it — and the amount is the one the bucket's latest row recorded.
	q.push(", commitment_periods AS (" +
		" SELECT DISTINCT provider_id, commitment_period, commitment_bucket FROM usage_rows" +
		" WHERE commitment_period IS NOT NULL AND durable_allocation IS NULL)")
	q.push(", commitment_shares AS (" +
		" SELECT p.provider_id, p.commitment_period, p.commitment_bucket, w.period_weight, w.latest_amount" +
		" FROM commitment_periods p" +
		" CROSS JOIN LATERAL (" + CommitmentSharesLateral(reportCommitmentDriver) + ") w)")
	q.push(", report_rows AS (" +
		" SELECT usage_rows.*," +
		" COALESCE(usage_rows.durable_allocation,round(shares.latest_amount * usage_rows.commitment_weight / NULLIF(shares.period_weight, 0), 12))" +
		" AS allocated_commitment" +
		" FROM usage_rows" +
		" LEFT JOIN commitment_shares shares" +
		"   ON usage_rows.durable_allocation IS NULL AND shares.provider_id = usage_rows.provider_id" +
		"  AND shares.commitment_period = usage_rows.commitment_period" +
		"  AND shares.commitment_bucket = usage_rows.commitment_bucket)")
}

var reportCommitmentDriver = CommitmentDriver{Provider: "p.provider_id", Period: "p.commitment_period", Bucket: "p.commitment_bucket"}

// CommitmentDriver names the SQL columns of the row a commitment share is
// computed for.
type CommitmentDriver struct {
	Provider, Period, Bucket string
}

// CommitmentSharesLateral is the bucket-scoped commitment aggregation used by
// both the usage reports' shares CTE and the FOCUS generator: per driver row it
// sums the whole bucket's commitment weight across facts and retained hourly
// rows and takes the latest row's amount.
func CommitmentSharesLateral(driver CommitmentDriver) string {
	return "   SELECT SUM(rows.weight) AS period_weight," +
		"          (ARRAY_AGG(rows.amount ORDER BY rows.at DESC, rows.amount DESC))[1] AS latest_amount" +
		"   FROM (" +
		"     SELECT f.commitment_weight AS weight, f.commitment_amount AS amount, f.observed_at AS at" +
		"     FROM olp.attempt_usage_facts f" +
		"     WHERE f.provider_id = " + driver.Provider + " AND f.commitment_period = " + driver.Period + "" +
		"       AND f.observed_at >= " + driver.Bucket + " AT TIME ZONE 'UTC'" +
		"       AND f.observed_at < (" + driver.Bucket + " + ('1 ' || " + driver.Period + ")::interval) AT TIME ZONE 'UTC'" +
		"     UNION ALL" +
		"     SELECT h.commitment_weight, h.commitment_amount, h.bucket" +
		"     FROM olp.attempt_usage_hourly h" +
		"     WHERE h.provider_id = " + driver.Provider + " AND h.commitment_period = " + driver.Period + "" +
		"       AND h.bucket >= " + driver.Bucket + " AT TIME ZONE 'UTC'" +
		"       AND h.bucket < (" + driver.Bucket + " + ('1 ' || " + driver.Period + ")::interval) AT TIME ZONE 'UTC'" +
		"   ) rows"
}

var (
	scopeRequest = countScope{"request_counted", "request_unpriced_counted", "request_incomplete_counted",
		"request_count", "request_unpriced_count", "request_incomplete_count"}
	scopeProvider = countScope{"provider_request_counted", "provider_unpriced_counted", "provider_incomplete_counted",
		"provider_request_count", "provider_unpriced_count", "provider_incomplete_count"}
	scopeModel = countScope{"model_request_counted", "model_unpriced_counted", "model_incomplete_counted",
		"model_request_count", "model_unpriced_count", "model_incomplete_count"}
	scopeTarget = countScope{"target_request_counted", "target_unpriced_counted", "target_incomplete_counted",
		"target_request_count", "target_unpriced_count", "target_incomplete_count"}
)

package usage

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

// errInvalidUsageCount reports an aggregate the database cannot legitimately
// hold. Reports fail closed on it: an understated total reads as good news.
var errInvalidUsageCount = errors.New("stored usage aggregate is invalid")

// MaxUsageRangeDays bounds a report window. A longer range is refused rather
// than silently truncated: an operator comparing two periods must know which
// one the numbers describe.
const MaxUsageRangeDays = 366

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
	AllProjects      bool
	AllowedProjects  []string
}

// Coverage says how much of the requested range the totals actually describe.
// Retained hourly aggregates are indivisible: a partial hour that survives only
// as a rollup is excluded and reported, never prorated.
type Coverage struct {
	RangeComplete bool `json:"range_complete"`
	Approximate   bool `json:"approximate"`
	// ExcludedBoundaries counts the boundary hours dropped for that reason.
	ExcludedBoundaries int `json:"excluded_partial_aggregate_boundaries"`
}

// Totals are the aggregate columns every usage report shares. Token counts and
// money are exact decimal strings: they are summed in PostgreSQL as `numeric`
// and never pass through a float.
type Totals struct {
	RequestCount            int64   `json:"request_count"`
	InputTokens             string  `json:"input_tokens"`
	OutputTokens            string  `json:"output_tokens"`
	CachedInputTokens       string  `json:"cached_input_tokens"`
	CacheWriteInputTokens   string  `json:"cache_write_input_tokens"`
	CacheWrite5MInputTokens string  `json:"cache_write_5m_input_tokens"`
	CacheWrite1HInputTokens string  `json:"cache_write_1h_input_tokens"`
	MediaUnits              string  `json:"media_units"`
	EstimatedCost           *string `json:"estimated_cost"`
	Currency                *string `json:"currency"`
	UnpricedCount           int64   `json:"unpriced_count"`
	IncompleteCount         int64   `json:"incomplete_count"`
}

// Summary is the totals for a range plus everything known about how much of the
// range is missing: excluded boundaries, recorded ingestion gaps, and the state
// of the consumer that would have delivered anything still outstanding.
type Summary struct {
	Totals
	GapEvents         int64          `json:"request_metadata_gap_events"`
	UncertainGapCount int64          `json:"uncertain_request_metadata_gap_count"`
	Coverage          Coverage       `json:"coverage"`
	Consumer          ConsumerStatus `json:"request_metadata_consumer"`
	Complete          bool           `json:"complete"`
}

// Completeness is the summary with the priced share made explicit, for the
// console's completeness panel.
type Completeness struct {
	Totals
	PricedCount       int64          `json:"priced_count"`
	GapEvents         int64          `json:"request_metadata_gap_events"`
	UncertainGapCount int64          `json:"uncertain_request_metadata_gap_count"`
	Coverage          Coverage       `json:"coverage"`
	Consumer          ConsumerStatus `json:"request_metadata_consumer"`
	Complete          bool           `json:"complete"`
}

// BreakdownItem is one grouped row of a breakdown report.
type BreakdownItem struct {
	Dimension string `json:"dimension"`
	Totals
}

// Breakdown groups a range by one dimension, largest first.
type Breakdown struct {
	Items    []BreakdownItem `json:"items"`
	Coverage Coverage        `json:"coverage"`
}

// Point is one bucket of a time series.
type Point struct {
	Bucket time.Time `json:"bucket"`
	Totals
}

// Series is a range bucketed by hour or day, oldest first.
type Series struct {
	Items    []Point  `json:"items"`
	Coverage Coverage `json:"coverage"`
}

// Breakdown dimensions accepted by the reports API.
const (
	DimensionRoute       = "route"
	DimensionProvider    = "provider"
	DimensionModel       = "model"
	DimensionAPIKey      = "api_key"
	DimensionOperation   = "operation"
	DimensionAttribution = "attribution"
)

// Time series bucket sizes accepted by the reports API.
const (
	GranularityHour = "hour"
	GranularityDay  = "day"
)

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

// Validate rejects a range that cannot be answered exactly. The report window
// must be positive and bounded, and an operation filter must name a real
// operation rather than silently matching nothing.
func (f Filters) Validate() error {
	if !f.End.After(f.Start) || f.End.Sub(f.Start) > MaxUsageRangeDays*24*time.Hour {
		return access.Fail(400, "invalid_range",
			"Use a positive usage range no longer than 366 days.")
	}
	if f.Operation != nil && !validOperation(*f.Operation) {
		return access.Fail(400, "invalid_operation", "The operation filter is invalid.")
	}
	if f.AttributionValue != nil && f.AttributionKey == nil {
		return access.Fail(400, "invalid_filter",
			"The attribution_value filter requires attribution_key.")
	}
	if f.AttributionKey != nil && !AttributionKeyPattern.MatchString(*f.AttributionKey) {
		return access.Fail(400, "invalid_filter", "The attribution_key filter is invalid.")
	}
	if f.AttributionValue != nil && !AttributionValuePattern.MatchString(*f.AttributionValue) {
		return access.Fail(400, "invalid_filter", "The attribution_value filter is invalid.")
	}
	return nil
}

// dimensions appends the dimension filters, which are spelled identically in
// the fact table, the hourly rollup and the boundary probe.
func (f Filters) dimensions(q *filterQuery) {
	if !f.AllProjects {
		q.push(" AND api_key_id IN (SELECT id FROM olp_go.api_keys WHERE project_id = ANY(" + q.bind(f.AllowedProjects) + "::uuid[]))")
	}
	if f.Route != nil {
		q.pushBind(" AND route_slug = ", *f.Route)
	}
	if f.ProviderID != nil {
		q.pushBind(" AND provider_id = ", *f.ProviderID)
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

// usageRows opens the statement with the CTE both sources feed. Live facts are
// filtered on the exact range; retained hourly rows are included only where a
// whole bucket lies inside it, so no aggregate is ever cut in half.
func (f Filters) usageRows(q *filterQuery, scope countScope) {
	q.push("WITH usage_rows AS (SELECT observed_at, route_slug, provider_id, upstream_model," +
		" api_key_id, operation, surface, attribution, CASE WHEN " + scope.count + " THEN 1 ELSE 0 END::bigint AS request_count," +
		" COALESCE(input_tokens, 0)::numeric AS input_tokens," +
		" COALESCE(output_tokens, 0)::numeric AS output_tokens," +
		" COALESCE(cached_input_tokens, 0)::numeric AS cached_input_tokens," +
		" COALESCE(cache_write_input_tokens, 0)::numeric AS cache_write_input_tokens," +
		" COALESCE(cache_write_5m_input_tokens, 0)::numeric AS cache_write_5m_input_tokens," +
		" COALESCE(cache_write_1h_input_tokens, 0)::numeric AS cache_write_1h_input_tokens," +
		" COALESCE(media_units, 0)::numeric AS media_units, estimated_cost," +
		" CASE WHEN " + scope.unpriced + " THEN 1 ELSE 0 END::bigint AS unpriced_count," +
		" CASE WHEN " + scope.incomplete + " THEN 1 ELSE 0 END::bigint AS incomplete_count," +
		" currency::text AS currency FROM olp_go.attempt_usage_facts WHERE true")
	q.pushBind(" AND observed_at >= ", f.Start)
	q.pushBind(" AND observed_at < ", f.End)
	f.dimensions(q)
	q.push(" UNION ALL SELECT bucket AS observed_at, route_slug, provider_id, upstream_model," +
		" api_key_id, operation, surface, attribution, " + scope.hourlyCount + ", input_tokens, output_tokens," +
		" cached_input_tokens, cache_write_input_tokens, cache_write_5m_input_tokens," +
		" cache_write_1h_input_tokens, media_units, estimated_cost, " + scope.hourlyUnpriced + ", " +
		scope.hourlyIncomplete + ", currency::text AS currency FROM olp_go.attempt_usage_hourly WHERE true")
	q.pushBind(" AND bucket >= ", ceilHour(f.Start))
	q.pushBind(" AND bucket + interval '1 hour' <= ", f.End)
	f.dimensions(q)
	q.push(")")
}

// totalsColumns is the aggregate projection shared by every report. The
// currency falls back to the installation's configured currency so a range
// whose rows are all unpriced still labels its zero.
const totalsColumns = "COALESCE(SUM(request_count), 0)::bigint," +
	" COALESCE(SUM(input_tokens), 0)::text," +
	" COALESCE(SUM(output_tokens), 0)::text," +
	" COALESCE(SUM(cached_input_tokens), 0)::text," +
	" COALESCE(SUM(cache_write_input_tokens), 0)::text," +
	" COALESCE(SUM(cache_write_5m_input_tokens), 0)::text," +
	" COALESCE(SUM(cache_write_1h_input_tokens), 0)::text," +
	" COALESCE(SUM(media_units), 0)::text," +
	" SUM(estimated_cost)::text," +
	" COALESCE(SUM(unpriced_count), 0)::bigint," +
	" COALESCE(SUM(incomplete_count), 0)::bigint," +
	" COALESCE(MAX(btrim(currency))," +
	" (SELECT btrim(currency) FROM olp_go.pricing_currency WHERE singleton))"

// scanTargets lists the destinations for totalsColumns in its column order.
func (t *Totals) scanTargets() []any {
	return []any{&t.RequestCount, &t.InputTokens, &t.OutputTokens, &t.CachedInputTokens,
		&t.CacheWriteInputTokens, &t.CacheWrite5MInputTokens, &t.CacheWrite1HInputTokens,
		&t.MediaUnits, &t.EstimatedCost, &t.UnpricedCount, &t.IncompleteCount, &t.Currency}
}

// valid rejects stored counts that cannot be true, so a corrupt aggregate
// surfaces as an error instead of an understated total.
func (t Totals) valid() bool {
	return t.RequestCount >= 0 && t.UnpricedCount >= 0 && t.IncompleteCount >= 0
}

// ReadSummary totals one range and reports how complete that total is.
func ReadSummary(ctx context.Context, q access.Queryer, f Filters, now time.Time) (Summary, error) {
	if err := f.Validate(); err != nil {
		return Summary{}, err
	}
	var query filterQuery
	f.usageRows(&query, scopeFor(f))
	query.push(" SELECT " + totalsColumns + " FROM usage_rows")
	var summary Summary
	if err := q.QueryRow(ctx, query.sql(), query.args...).Scan(summary.Totals.scanTargets()...); err != nil {
		return Summary{}, fmt.Errorf("read usage summary: %w", err)
	}
	if !summary.Totals.valid() {
		return Summary{}, errInvalidUsageCount
	}
	gapEvents, uncertain, err := readGapEvidence(ctx, q, f)
	if err != nil {
		return Summary{}, err
	}
	coverage, err := readCoverage(ctx, q, f)
	if err != nil {
		return Summary{}, err
	}
	consumer, err := ReadConsumerStatus(ctx, q, now)
	if err != nil {
		return Summary{}, err
	}
	summary.GapEvents, summary.UncertainGapCount = gapEvents, uncertain
	summary.Coverage, summary.Consumer = coverage, consumer
	summary.Complete = summary.UnpricedCount == 0 && summary.IncompleteCount == 0 &&
		gapEvents == 0 && uncertain == 0 && coverage.RangeComplete && consumer.Complete()
	return summary, nil
}

// ReadCompleteness is the summary with the priced request count derived, so the
// console can show what share of a range is accounted for.
func ReadCompleteness(ctx context.Context, q access.Queryer, f Filters, now time.Time) (Completeness, error) {
	summary, err := ReadSummary(ctx, q, f, now)
	if err != nil {
		return Completeness{}, err
	}
	if summary.UnpricedCount > summary.RequestCount {
		return Completeness{}, errInvalidUsageCount
	}
	return Completeness{
		Totals:            summary.Totals,
		PricedCount:       summary.RequestCount - summary.UnpricedCount,
		GapEvents:         summary.GapEvents,
		UncertainGapCount: summary.UncertainGapCount,
		Coverage:          summary.Coverage,
		Consumer:          summary.Consumer,
		Complete:          summary.Complete,
	}, nil
}

// ReadBreakdown groups a range by one dimension. The count scope follows the
// dimension as well as the filters: breaking down by provider counts provider
// attempts, unless a model filter already narrowed the question to one target.
func ReadBreakdown(ctx context.Context, q access.Queryer, f Filters, dimension string, limit int) (Breakdown, error) {
	if err := f.Validate(); err != nil {
		return Breakdown{}, err
	}
	var expression string
	scope := scopeFor(f)
	switch dimension {
	case DimensionRoute:
		expression = "route_slug"
	case DimensionProvider:
		expression = "provider_id::text"
		if f.Model == nil {
			scope = scopeProvider
		} else {
			scope = scopeTarget
		}
	case DimensionModel:
		expression = "upstream_model"
		if f.ProviderID == nil {
			scope = scopeModel
		} else {
			scope = scopeTarget
		}
	case DimensionAPIKey:
		expression = "COALESCE(api_key_id::text, 'unknown')"
	case DimensionOperation:
		expression = "operation"
	case DimensionAttribution:
		if f.AttributionKey == nil {
			return Breakdown{}, access.Fail(400, "invalid_filter",
				"The attribution breakdown requires the attribution_key filter.")
		}
		expression = "attribution->>$attribution_key$"
	default:
		return Breakdown{}, access.Fail(400, "invalid_dimension",
			"Dimension must be route, provider, model, api_key, operation, or attribution.")
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	var query filterQuery
	f.usageRows(&query, scope)
	if dimension == DimensionAttribution {
		expression = "attribution->>" + query.bind(*f.AttributionKey)
	}
	query.push(" SELECT " + expression + " AS dimension, " + totalsColumns + " FROM usage_rows" +
		" GROUP BY dimension ORDER BY 2 DESC, dimension LIMIT ")
	query.push(query.bind(int64(limit)))
	rows, err := q.Query(ctx, query.sql(), query.args...)
	if err != nil {
		return Breakdown{}, fmt.Errorf("read usage breakdown: %w", err)
	}
	defer rows.Close()
	report := Breakdown{Items: []BreakdownItem{}}
	for rows.Next() {
		var item BreakdownItem
		if err = rows.Scan(append([]any{&item.Dimension}, item.Totals.scanTargets()...)...); err != nil {
			return Breakdown{}, fmt.Errorf("read usage breakdown: %w", err)
		}
		if !item.Totals.valid() {
			return Breakdown{}, errInvalidUsageCount
		}
		report.Items = append(report.Items, item)
	}
	if err = rows.Err(); err != nil {
		return Breakdown{}, fmt.Errorf("read usage breakdown: %w", err)
	}
	if report.Coverage, err = readCoverage(ctx, q, f); err != nil {
		return Breakdown{}, err
	}
	return report, nil
}

// ReadSeries buckets a range by hour or day. The bucket boundary is stated in
// UTC in SQL so a series bucket can never disagree with a rollup bucket.
func ReadSeries(ctx context.Context, q access.Queryer, f Filters, granularity string) (Series, error) {
	if err := f.Validate(); err != nil {
		return Series{}, err
	}
	var unit string
	switch granularity {
	case GranularityHour:
		unit = "hour"
	case GranularityDay:
		unit = "day"
	default:
		return Series{}, access.Fail(400, "invalid_granularity", "Granularity must be hour or day.")
	}
	var query filterQuery
	f.usageRows(&query, scopeFor(f))
	query.push(" SELECT date_trunc('" + unit + "', observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'" +
		" AS bucket, " + totalsColumns + " FROM usage_rows GROUP BY bucket ORDER BY bucket")
	rows, err := q.Query(ctx, query.sql(), query.args...)
	if err != nil {
		return Series{}, fmt.Errorf("read usage series: %w", err)
	}
	defer rows.Close()
	report := Series{Items: []Point{}}
	for rows.Next() {
		var point Point
		if err = rows.Scan(append([]any{&point.Bucket}, point.Totals.scanTargets()...)...); err != nil {
			return Series{}, fmt.Errorf("read usage series: %w", err)
		}
		if !point.Totals.valid() {
			return Series{}, errInvalidUsageCount
		}
		point.Bucket = point.Bucket.UTC()
		report.Items = append(report.Items, point)
	}
	if err = rows.Err(); err != nil {
		return Series{}, fmt.Errorf("read usage series: %w", err)
	}
	if report.Coverage, err = readCoverage(ctx, q, f); err != nil {
		return Series{}, err
	}
	return report, nil
}

const gapEvidenceSQL = `SELECT COALESCE(SUM(event_count), 0)::bigint,
        COALESCE(SUM(uncertain_gap_count), 0)::bigint
    FROM (
      SELECT event_count,
             CASE WHEN certainty = 'lower_bound' THEN 1::bigint ELSE 0::bigint END AS uncertain_gap_count
        FROM olp_go.request_metadata_ingestion_gaps
       WHERE last_observed_at >= $1 AND first_observed_at < $2
      UNION ALL
      SELECT event_count, uncertain_gap_count FROM olp_go.request_metadata_gap_hourly
       WHERE last_observed_at >= $1 AND first_observed_at < $2
    ) retained_gaps`

// readGapEvidence sums the metadata known to be missing from the range, from
// both live gap rows and their retained hourly rollups.
func readGapEvidence(ctx context.Context, q access.Queryer, f Filters) (int64, int64, error) {
	var events, uncertain int64
	if err := q.QueryRow(ctx, gapEvidenceSQL, f.Start, f.End).Scan(&events, &uncertain); err != nil {
		return 0, 0, fmt.Errorf("read request metadata gap evidence: %w", err)
	}
	if events < 0 || uncertain < 0 {
		return 0, 0, errInvalidUsageCount
	}
	return events, uncertain, nil
}

// readCoverage counts the partial boundary hours the range cannot include
// because they survive only as retained aggregates.
func readCoverage(ctx context.Context, q access.Queryer, f Filters) (Coverage, error) {
	buckets := make([]time.Time, 0, 2)
	if lower := floorHour(f.Start); !lower.Equal(f.Start) {
		buckets = append(buckets, lower)
	}
	if upper := floorHour(f.End); !upper.Equal(f.End) && !containsTime(buckets, upper) {
		buckets = append(buckets, upper)
	}
	if len(buckets) == 0 {
		return Coverage{RangeComplete: true}, nil
	}
	var query filterQuery
	query.push("SELECT COUNT(DISTINCT bucket)::bigint FROM olp_go.attempt_usage_hourly WHERE bucket = ANY(")
	query.push(query.bind(buckets) + "::timestamptz[])")
	f.dimensions(&query)
	var excluded int64
	if err := q.QueryRow(ctx, query.sql(), query.args...).Scan(&excluded); err != nil {
		return Coverage{}, fmt.Errorf("read usage range coverage: %w", err)
	}
	if excluded < 0 || excluded > int64(len(buckets)) {
		return Coverage{}, errInvalidUsageCount
	}
	return Coverage{
		RangeComplete:      excluded == 0,
		Approximate:        excluded > 0,
		ExcludedBoundaries: int(excluded),
	}, nil
}

func containsTime(values []time.Time, value time.Time) bool {
	for _, existing := range values {
		if existing.Equal(value) {
			return true
		}
	}
	return false
}

// floorHour truncates to the start of the containing UTC hour. Truncate on a
// time.Time floors toward the zero time, which is the epoch-relative floor for
// hours, so it is exact on both sides of 1970.
func floorHour(value time.Time) time.Time { return value.UTC().Truncate(time.Hour) }

func ceilHour(value time.Time) time.Time {
	floor := floorHour(value)
	if floor.Equal(value) {
		return floor
	}
	return floor.Add(time.Hour)
}

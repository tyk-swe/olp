package reports

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage/completeness"
)

// errInvalidUsageCount reports an aggregate the database cannot legitimately
// hold. Reports fail closed on it: an understated total reads as good news.
var errInvalidUsageCount = errors.New("stored usage aggregate is invalid")

// MaxUsageRangeDays bounds a report window. A longer range is refused rather
// than silently truncated: an operator comparing two periods must know which
// one the numbers describe.
const MaxUsageRangeDays = 366

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
	TrainingTokens          string            `json:"training_tokens"`
	StorageByteSeconds      string            `json:"storage_byte_seconds"`
	CacheTokenSeconds       string            `json:"cache_token_seconds"`
	ContainerSessions       map[string]string `json:"container_sessions"`
	RequestCount            int64             `json:"request_count"`
	InputTokens             string            `json:"input_tokens"`
	OutputTokens            string            `json:"output_tokens"`
	CachedInputTokens       string            `json:"cached_input_tokens"`
	CacheWriteInputTokens   string            `json:"cache_write_input_tokens"`
	CacheWrite5MInputTokens string            `json:"cache_write_5m_input_tokens"`
	CacheWrite1HInputTokens string            `json:"cache_write_1h_input_tokens"`
	AudioInputTokens        string            `json:"audio_input_tokens"`
	CachedAudioInputTokens  string            `json:"cached_audio_input_tokens"`
	AudioOutputTokens       string            `json:"audio_output_tokens"`
	ImageInputTokens        string            `json:"image_input_tokens"`
	CachedImageInputTokens  string            `json:"cached_image_input_tokens"`
	ImageOutputTokens       string            `json:"image_output_tokens"`
	MediaUnits              string            `json:"media_units"`
	EstimatedCost           *string           `json:"estimated_cost"`
	Currency                *string           `json:"currency"`
	UnpricedCount           int64             `json:"unpriced_count"`
	IncompleteCount         int64             `json:"incomplete_count"`
	// EstimatedInputTokens and ReportedInputTokens cover only the attempts that
	// had both an admission estimate and observed usage, and EstimatedAttemptCount
	// is how many those were. They are summed over the same attempts so the two
	// compare: the estimation error is (estimated - reported) / reported, and
	// the all-attempt InputTokens would not do for the denominator.
	EstimatedInputTokens  string `json:"estimated_input_tokens"`
	ReportedInputTokens   string `json:"reported_input_tokens"`
	EstimatedAttemptCount int64  `json:"estimated_attempt_count"`
	// AllocatedCommitment is this row's share of the commitment amounts its
	// committed attempts divide by list-cost weight. A range or filter holding
	// no committed usage allocates nothing.
	AllocatedCommitment string `json:"allocated_commitment"`
	// Charge is what the attempts' consumers were billed under their rate
	// cards, null like EstimatedCost when nothing was charged.
	Charge *string `json:"charge"`
	// UnchargedAttemptCount counts billable attempts no charge was resolved
	// for, such as a rate card that could not price the usage.
	UnchargedAttemptCount int64 `json:"uncharged_attempt_count"`
}

// Summary is the totals for a range plus everything known about how much of the
// range is missing: excluded boundaries, recorded ingestion gaps, and the state
// of the consumer that would have delivered anything still outstanding.
type Summary struct {
	Totals
	GapEvents         int64                       `json:"request_metadata_gap_events"`
	UncertainGapCount int64                       `json:"uncertain_request_metadata_gap_count"`
	Coverage          Coverage                    `json:"coverage"`
	Consumer          completeness.ConsumerStatus `json:"request_metadata_consumer"`
	Complete          bool                        `json:"complete"`
}

type Completeness struct {
	RequestCount      int64                       `json:"request_count"`
	UnpricedCount     int64                       `json:"unpriced_count"`
	IncompleteCount   int64                       `json:"incomplete_count"`
	EstimatedCost     *string                     `json:"estimated_cost"`
	Currency          *string                     `json:"currency"`
	PricedCount       int64                       `json:"priced_count"`
	GapEvents         int64                       `json:"request_metadata_gap_events"`
	UncertainGapCount int64                       `json:"uncertain_request_metadata_gap_count"`
	Coverage          Coverage                    `json:"coverage"`
	Consumer          completeness.ConsumerStatus `json:"request_metadata_consumer"`
	Complete          bool                        `json:"complete"`
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
	" COALESCE(SUM(audio_input_tokens), 0)::text," +
	" COALESCE(SUM(cached_audio_input_tokens), 0)::text," +
	" COALESCE(SUM(audio_output_tokens), 0)::text," +
	" COALESCE(SUM(image_input_tokens), 0)::text," +
	" COALESCE(SUM(cached_image_input_tokens), 0)::text," +
	" COALESCE(SUM(image_output_tokens), 0)::text," +
	" COALESCE(SUM(media_units), 0)::text," +
	" SUM(estimated_cost)::text," +
	" COALESCE(SUM(unpriced_count), 0)::bigint," +
	" COALESCE(SUM(incomplete_count), 0)::bigint," +
	" COALESCE(MAX(btrim(currency))," +
	" (SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton))," +
	" COALESCE(SUM(estimated_input_tokens), 0)::text," +
	" COALESCE(SUM(estimate_reported_input_tokens), 0)::text," +
	" COALESCE(SUM(estimate_attempt_count), 0)::bigint," +
	" COALESCE(SUM(allocated_commitment), 0)::text," +
	" SUM(charge)::text," +
	" COALESCE(SUM(uncharged_attempt_count), 0)::bigint," +
	" COALESCE(SUM(training_tokens),0)::text,COALESCE(SUM(storage_byte_seconds),0)::text,COALESCE(SUM(cache_token_seconds),0)::text," +
	" olp.container_session_string_totals(jsonb_agg(container_sessions) FILTER (WHERE container_sessions IS NOT NULL AND container_sessions <> '{}'::jsonb))"

// scanTargets lists the destinations for totalsColumns in its column order.
func (t *Totals) scanTargets() []any {
	return []any{&t.RequestCount, &t.InputTokens, &t.OutputTokens, &t.CachedInputTokens,
		&t.CacheWriteInputTokens, &t.CacheWrite5MInputTokens, &t.CacheWrite1HInputTokens,
		&t.AudioInputTokens, &t.CachedAudioInputTokens, &t.AudioOutputTokens,
		&t.ImageInputTokens, &t.CachedImageInputTokens, &t.ImageOutputTokens,
		&t.MediaUnits, &t.EstimatedCost, &t.UnpricedCount, &t.IncompleteCount, &t.Currency,
		&t.EstimatedInputTokens, &t.ReportedInputTokens, &t.EstimatedAttemptCount,
		&t.AllocatedCommitment, &t.Charge, &t.UnchargedAttemptCount, &t.TrainingTokens, &t.StorageByteSeconds, &t.CacheTokenSeconds, &t.ContainerSessions}
}

// valid rejects stored counts that cannot be true, so a corrupt aggregate
// surfaces as an error instead of an understated total.
func (t Totals) valid() bool {
	return t.RequestCount >= 0 && t.UnpricedCount >= 0 && t.IncompleteCount >= 0 &&
		t.EstimatedAttemptCount >= 0
}

// ReadSummary totals one range and reports how complete that total is.
func ReadSummary(ctx context.Context, q access.Queryer, f Filters, now time.Time) (Summary, error) {
	var query filterQuery
	f.usageRows(&query, scopeFor(f))
	query.push(" SELECT " + totalsColumns + " FROM report_rows")
	var summary Summary
	if err := q.QueryRow(ctx, query.sql(), query.args...).Scan(summary.Totals.scanTargets()...); err != nil {
		return Summary{}, fmt.Errorf("read usage summary: %w", err)
	}
	if !summary.Totals.valid() || summary.UnpricedCount > summary.RequestCount {
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
	consumer, err := completeness.ReadConsumerStatus(ctx, q, now)
	if err != nil {
		return Summary{}, err
	}
	summary.GapEvents, summary.UncertainGapCount = gapEvents, uncertain
	summary.Coverage, summary.Consumer = coverage, consumer
	summary.Complete = summary.UnpricedCount == 0 && summary.IncompleteCount == 0 &&
		gapEvents == 0 && uncertain == 0 && coverage.RangeComplete && consumer.Complete()
	return summary, nil
}

func ReadCompleteness(ctx context.Context, q access.Queryer, f Filters, now time.Time) (Completeness, error) {
	scope := scopeFor(f)
	var query filterQuery
	query.push("WITH completeness_rows AS (SELECT" +
		" CASE WHEN " + scope.count + " THEN 1 ELSE 0 END::bigint AS request_count," +
		" CASE WHEN " + scope.unpriced + " THEN 1 ELSE 0 END::bigint AS unpriced_count," +
		" CASE WHEN " + scope.incomplete + " THEN 1 ELSE 0 END::bigint AS incomplete_count," +
		" estimated_cost, currency::text AS currency FROM olp.attempt_usage_facts WHERE true")
	query.pushBind(" AND observed_at >= ", f.Start)
	query.pushBind(" AND observed_at < ", f.End)
	f.dimensions(&query)
	query.push(" UNION ALL SELECT " + scope.hourlyCount + ", " + scope.hourlyUnpriced + ", " +
		scope.hourlyIncomplete + ", estimated_cost, currency::text FROM olp.attempt_usage_hourly WHERE true")
	query.pushBind(" AND bucket >= ", ceilHour(f.Start))
	query.pushBind(" AND bucket + interval '1 hour' <= ", f.End)
	f.dimensions(&query)
	query.push(") SELECT COALESCE(SUM(request_count), 0)::bigint," +
		" COALESCE(SUM(unpriced_count), 0)::bigint, COALESCE(SUM(incomplete_count), 0)::bigint," +
		" SUM(estimated_cost)::text, COALESCE(MAX(btrim(currency))," +
		" (SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton)) FROM completeness_rows")
	var report Completeness
	if err := q.QueryRow(ctx, query.sql(), query.args...).Scan(&report.RequestCount, &report.UnpricedCount,
		&report.IncompleteCount, &report.EstimatedCost, &report.Currency); err != nil {
		return Completeness{}, fmt.Errorf("read usage completeness: %w", err)
	}
	if report.RequestCount < 0 || report.UnpricedCount < 0 || report.IncompleteCount < 0 || report.UnpricedCount > report.RequestCount {
		return Completeness{}, errInvalidUsageCount
	}
	gapEvents, uncertain, err := readGapEvidence(ctx, q, f)
	if err != nil {
		return Completeness{}, err
	}
	coverage, err := readCoverage(ctx, q, f)
	if err != nil {
		return Completeness{}, err
	}
	consumer, err := completeness.ReadConsumerStatus(ctx, q, now)
	if err != nil {
		return Completeness{}, err
	}
	return Completeness{
		RequestCount: report.RequestCount, UnpricedCount: report.UnpricedCount, IncompleteCount: report.IncompleteCount,
		EstimatedCost: report.EstimatedCost, Currency: report.Currency, PricedCount: report.RequestCount - report.UnpricedCount,
		GapEvents: gapEvents, UncertainGapCount: uncertain, Coverage: coverage, Consumer: consumer,
		Complete: report.UnpricedCount == 0 && report.IncompleteCount == 0 &&
			gapEvents == 0 && uncertain == 0 && coverage.RangeComplete && consumer.Complete(),
	}, nil
}

// ReadBreakdown groups a range by one dimension. The count scope follows the
// dimension as well as the filters: breaking down by provider counts provider
// attempts, unless a model filter already narrowed the question to one target.
func ReadBreakdown(ctx context.Context, q access.Queryer, f Filters, dimension Dimension, limit int) (Breakdown, error) {
	return readBreakdown(ctx, q, f, dimension, limit, 200)
}

func ReadBreakdownExport(ctx context.Context, q access.Queryer, f Filters, dimension Dimension) (Breakdown, error) {
	report, err := readBreakdown(ctx, q, f, dimension, 10001, 10001)
	if err == nil && len(report.Items) > 10000 {
		return Breakdown{}, fail(FailureExportTooLarge, "Narrow the usage filters to at most 10000 groups.")
	}
	return report, err
}

func readBreakdown(ctx context.Context, q access.Queryer, f Filters, dimension Dimension, limit, maximum int) (Breakdown, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > maximum {
		limit = maximum
	}
	var query filterQuery
	scope := dimensionScope(dimension, f)
	f.usageRows(&query, scope)
	expression := dimensionExpressions[dimension]
	if dimension == DimensionAttribution {
		expression = "attribution->>" + query.bind(*f.AttributionKey)
	}
	query.push(" SELECT " + expression + " AS dimension, " + totalsColumns + " FROM report_rows" +
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
func ReadSeries(ctx context.Context, q access.Queryer, f Filters, granularity Granularity) (Series, error) {
	var query filterQuery
	f.usageRows(&query, scopeFor(f))
	query.push(" SELECT date_trunc('" + string(granularity) + "', observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'" +
		" AS bucket, " + totalsColumns + " FROM report_rows GROUP BY bucket ORDER BY bucket")
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

package reports

import (
	"context"
	"fmt"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

const gapEvidenceSQL = `SELECT COALESCE(SUM(event_count), 0)::bigint,
        COALESCE(SUM(uncertain_gap_count), 0)::bigint
    FROM (
      SELECT event_count,
             CASE WHEN certainty = 'lower_bound' THEN 1::bigint ELSE 0::bigint END AS uncertain_gap_count
        FROM olp.request_metadata_ingestion_gaps
       WHERE last_observed_at >= $1 AND first_observed_at < $2
      UNION ALL
      SELECT event_count, uncertain_gap_count FROM olp.request_metadata_gap_hourly
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
	query.push("SELECT COUNT(DISTINCT bucket)::bigint FROM olp.attempt_usage_hourly WHERE bucket = ANY(")
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

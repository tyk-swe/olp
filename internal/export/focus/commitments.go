package focus

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	commitmentPeriodsSQL = `WITH periods AS (
 SELECT p.* FROM olp.commitment_periods p
 WHERE ((p.bucket_start < $1::date+1 AND p.bucket_end>$1::date)
 OR EXISTS(SELECT 1 FROM olp.commitment_usage u WHERE u.period_id=p.id AND u.observed_at >= $1::date AND u.observed_at<$1::date+1))
 AND (($4::text IS NULL AND $5::text IS NULL) OR EXISTS(
 SELECT 1 FROM olp.commitment_usage u WHERE u.period_id=p.id
 AND ($4::text IS NULL OR u.payload->>'consumer'=$4)
 AND ($5::text IS NULL OR u.payload->>'route_slug'=$5)))
 ), amounts AS (
 SELECT p.*,COALESCE((SELECT SUM(a.allocated_cost) FROM olp.commitment_allocations a WHERE a.period_id=p.id),0) allocated,
 NOT EXISTS(SELECT 1 FROM olp.commitment_usage u WHERE u.period_id=p.id AND NOT u.complete)
 AND NOT EXISTS(SELECT 1 FROM olp.request_metadata_ingestion_gaps g WHERE g.first_observed_at<p.bucket_end AND g.last_observed_at>=p.bucket_start) evidence_complete
 FROM periods p)
 SELECT trim_scale(COALESCE(SUM(amount),0))::text,trim_scale(COALESCE(SUM(allocated),0))::text,
 trim_scale(COALESCE(SUM(amount-allocated),0))::text,
 COALESCE(bool_or(bucket_end > $2::timestamptz-interval '2 hours'),false),
 COALESCE(bool_and(complete AND evidence_complete AND bucket_start >= $3::timestamptz),true)
 FROM amounts`

	preLedgerFactsSQL = `SELECT EXISTS(SELECT 1 FROM olp.attempt_usage_facts f
 LEFT JOIN olp.api_keys k ON k.id=f.api_key_id LEFT JOIN olp.providers pr ON pr.id=f.provider_id
 LEFT JOIN olp.routes rt ON rt.slug=f.route_slug
 WHERE f.observed_at >= $1::date AND f.observed_at<$1::date+1
 AND f.commitment_period IS NOT NULL AND f.commitment_schedule_id IS NULL
 AND ($2::text IS NULL OR ` + ownerSQL + `::text=$2)
 AND ($3::text IS NULL OR f.route_slug=$3))`

	missingPeriodsSQL = `SELECT EXISTS(SELECT 1 FROM olp.commitment_schedules s
 CROSS JOIN LATERAL generate_series(olp.commitment_bucket(s.period,GREATEST(s.effective_at,$1::date)),
  LEAST($1::date+1-interval '1 microsecond',COALESCE(s.ends_at,'infinity')-interval '1 microsecond'),('1 '||s.period)::interval) b
 WHERE s.effective_at<$1::date+1 AND (s.ends_at IS NULL OR s.ends_at>$1::date)
 AND NOT EXISTS(SELECT 1 FROM olp.commitment_periods p WHERE p.provider_id=s.provider_id AND p.period=s.period AND p.bucket_start=b))`

	scopedAllocationSQL = `SELECT trim_scale(COALESCE(SUM(a.allocated_cost),0))::text
 FROM olp.commitment_allocations a WHERE a.observed_at >= $1::date AND a.observed_at<$1::date+1
 AND ($2::text IS NULL OR a.payload->>'consumer'=$2)
 AND ($3::text IS NULL OR a.payload->>'route_slug'=$3)`
)

func readCommitmentTotals(ctx context.Context, tx pgx.Tx, s sink, day, now time.Time) (*CommitmentTotals, error) {
	date := day.Format(time.DateOnly)
	var totals CommitmentTotals
	if err := tx.QueryRow(ctx, commitmentPeriodsSQL, date, now, s.first, s.scope.project, s.scope.route).
		Scan(&totals.PurchasedCost, &totals.AllocatedCost, &totals.UnusedCost, &totals.Provisional, &totals.Complete); err != nil {
		return nil, err
	}
	var preLedger bool
	if err := tx.QueryRow(ctx, preLedgerFactsSQL, date, s.scope.project, s.scope.route).Scan(&preLedger); err != nil {
		return nil, err
	}
	if s.scope.scoped() {
		var allocated string
		if err := tx.QueryRow(ctx, scopedAllocationSQL, date, s.scope.project, s.scope.route).Scan(&allocated); err != nil {
			return nil, err
		}
		return &CommitmentTotals{
			PurchasedCost: "0", AllocatedCost: allocated, UnusedCost: "0",
			Provisional: totals.Provisional, Complete: totals.Complete && !preLedger,
		}, nil
	}
	var missingPeriods bool
	if err := tx.QueryRow(ctx, missingPeriodsSQL, date).Scan(&missingPeriods); err != nil {
		return nil, err
	}
	return &CommitmentTotals{
		PurchasedCost: totals.PurchasedCost, AllocatedCost: totals.AllocatedCost, UnusedCost: totals.UnusedCost,
		Provisional: totals.Provisional, Complete: totals.Complete && !preLedger && !missingPeriods,
	}, nil
}

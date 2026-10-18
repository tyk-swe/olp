package export

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// CommitmentTotals describes the complete periods touching a dataset. Amounts
// are decimal strings; daily row totals remain in the separate totals object.
type CommitmentTotals struct {
	PurchasedCost string `json:"purchased_cost"`
	AllocatedCost string `json:"allocated_cost"`
	UnusedCost    string `json:"unused_cost"`
	Provisional   bool   `json:"provisional"`
	Complete      bool   `json:"complete"`
}

func readCommitmentTotals(ctx context.Context, tx pgx.Tx, sink focusSink, day, now time.Time) (*CommitmentTotals, error) {
	if sink.costBasis != "cost" {
		return nil, nil
	}
	first := sink.CreatedAt.UTC().Truncate(24 * time.Hour)
	if sink.startDate != nil && sink.startDate.After(first) {
		first = *sink.startDate
	}
	var days int
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT value::integer FROM olp.settings WHERE key='retention.usage_days'),90)`).Scan(&days); err != nil {
		return nil, err
	}
	cutoff := now.UTC().AddDate(0, 0, -days).Truncate(24*time.Hour).AddDate(0, 0, 1)
	if cutoff.After(first) {
		first = cutoff
	}
	totals := &CommitmentTotals{}
	err := tx.QueryRow(ctx, `WITH periods AS (
 SELECT p.* FROM olp.commitment_periods p
 WHERE ((p.bucket_start < $1::date+1 AND p.bucket_end>$1::date)
 OR EXISTS(SELECT 1 FROM olp.commitment_usage u WHERE u.period_id=p.id AND u.observed_at >= $1::date AND u.observed_at<$1::date+1))
 AND (($4::text IS NULL AND $5::text IS NULL AND $6::text IS NULL) OR EXISTS(
 SELECT 1 FROM olp.commitment_usage u WHERE u.period_id=p.id
 AND ($4::text IS NULL OR u.payload->>'consumer'=$4) AND ($5::text IS NULL OR u.payload->>'consumer'=$5)
 AND ($6::text IS NULL OR u.payload->>'route_slug'=$6)))
 ), amounts AS (
 SELECT p.*,COALESCE((SELECT SUM(a.allocated_cost) FROM olp.commitment_allocations a WHERE a.period_id=p.id),0) allocated,
 NOT EXISTS(SELECT 1 FROM olp.commitment_usage u WHERE u.period_id=p.id AND NOT u.complete)
 AND NOT EXISTS(SELECT 1 FROM olp.request_metadata_ingestion_gaps g WHERE g.first_observed_at<p.bucket_end AND g.last_observed_at>=p.bucket_start) evidence_complete
 FROM periods p)
 SELECT trim_scale(COALESCE(SUM(amount),0))::text,trim_scale(COALESCE(SUM(allocated),0))::text,
 trim_scale(COALESCE(SUM(amount-allocated),0))::text,
 COALESCE(bool_or(bucket_end > $2::timestamptz-interval '2 hours'),false),
 COALESCE(bool_and(complete AND evidence_complete AND bucket_start >= $3::timestamptz),true)
 FROM amounts`, day.Format("2006-01-02"), now, first, textOrNil(sink.ProjectID), textOrNil(sink.filterProject), textOrNil(sink.filterRoute)).Scan(&totals.PurchasedCost, &totals.AllocatedCost, &totals.UnusedCost, &totals.Provisional, &totals.Complete)
	if err != nil {
		return nil, err
	}
	// Retained pre-ledger usage without a published obligation is evidence of
	// incomplete history, never proof of an idle or commitment-free period.
	var missing bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.attempt_usage_facts f
 LEFT JOIN olp.api_keys k ON k.id=f.api_key_id LEFT JOIN olp.providers pr ON pr.id=f.provider_id
 LEFT JOIN olp.routes rt ON rt.slug=f.route_slug
 WHERE f.observed_at >= $1::date AND f.observed_at<$1::date+1
 AND f.commitment_period IS NOT NULL AND f.commitment_schedule_id IS NULL
 AND ($2::text IS NULL OR COALESCE(k.project_id,pr.project_id,rt.project_id)::text=$2)
 AND ($3::text IS NULL OR COALESCE(k.project_id,pr.project_id,rt.project_id)::text=$3)
 AND ($4::text IS NULL OR f.route_slug=$4))`, day.Format("2006-01-02"), textOrNil(sink.ProjectID), textOrNil(sink.filterProject), textOrNil(sink.filterRoute)).Scan(&missing)
	if err != nil {
		return nil, err
	}
	totals.Complete = totals.Complete && !missing
	if sink.ProjectID == nil && sink.filterProject == nil && sink.filterRoute == nil {
		var missingPeriods bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.commitment_schedules s
   CROSS JOIN LATERAL generate_series(olp.commitment_bucket(s.period,GREATEST(s.effective_at,$1::date)),
    LEAST($1::date+1-interval '1 microsecond',COALESCE(s.ends_at,'infinity')-interval '1 microsecond'),('1 '||s.period)::interval) b
   WHERE s.effective_at<$1::date+1 AND (s.ends_at IS NULL OR s.ends_at>$1::date)
   AND NOT EXISTS(SELECT 1 FROM olp.commitment_periods p WHERE p.provider_id=s.provider_id AND p.period=s.period AND p.bucket_start=b))`, day.Format("2006-01-02")).Scan(&missingPeriods)
		if err != nil {
			return nil, err
		}
		totals.Complete = totals.Complete && !missingPeriods
	}
	if sink.ProjectID != nil || sink.filterProject != nil || sink.filterRoute != nil {
		// Scoped exports disclose their allocation only. They never expose another
		// consumer's spend or the installation's purchased/unused obligations.
		totals.PurchasedCost, totals.UnusedCost = "0", "0"
		err = tx.QueryRow(ctx, `SELECT trim_scale(COALESCE(SUM(a.allocated_cost),0))::text
   FROM olp.commitment_allocations a WHERE a.observed_at >= $1::date AND a.observed_at<$1::date+1
   AND ($2::text IS NULL OR a.payload->>'consumer'=$2) AND ($3::text IS NULL OR a.payload->>'consumer'=$3)
   AND ($4::text IS NULL OR a.payload->>'route_slug'=$4)`, day.Format("2006-01-02"), textOrNil(sink.ProjectID), textOrNil(sink.filterProject), textOrNil(sink.filterRoute)).Scan(&totals.AllocatedCost)
	}
	return totals, err
}

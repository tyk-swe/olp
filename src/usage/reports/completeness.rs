use crate::database::cursor::Error;
use crate::usage::ingestion::delivery_health::ConsumerStatus;
use crate::usage::reports::Coverage;
use crate::usage::reports::Filters;

#[derive(Clone, Debug)]
pub struct Report {
    pub request_count: u64,
    pub priced_count: u64,
    pub unpriced_count: u64,
    pub incomplete_count: u64,
    /// Exact known loss plus the last durable in-flight lower bounds for
    /// unclean gateway epochs.
    pub request_metadata_gap_events: u64,
    pub uncertain_request_metadata_gap_count: u64,
    pub estimated_cost: Option<String>,
    pub currency: Option<String>,
    pub coverage: Coverage,
    pub request_metadata_consumer: ConsumerStatus,
    pub complete: bool,
}

#[derive(Clone, Copy, Debug)]
pub(crate) struct RequestMetadataGapEvidence {
    pub(crate) event_count: i64,
    pub(crate) uncertain_gap_count: i64,
}

pub async fn usage_completeness(pool: &sqlx::PgPool, filters: &Filters) -> Result<Report, Error> {
    let summary = crate::usage::reports::summary::usage_summary(pool, filters).await?;
    let priced_count = summary
        .request_count
        .checked_sub(summary.unpriced_count)
        .ok_or_else(|| Error::Invalid("stored priced count is invalid".to_owned()))?;
    Ok(Report {
        request_count: summary.request_count,
        priced_count,
        unpriced_count: summary.unpriced_count,
        incomplete_count: summary.incomplete_count,
        request_metadata_gap_events: summary.request_metadata_gap_events,
        uncertain_request_metadata_gap_count: summary.uncertain_request_metadata_gap_count,
        estimated_cost: summary.estimated_cost,
        currency: summary.currency,
        coverage: summary.coverage,
        request_metadata_consumer: summary.request_metadata_consumer,
        complete: summary.complete,
    })
}

pub(crate) async fn request_metadata_gap_evidence(
    pool: &sqlx::PgPool,
    filters: &Filters,
) -> Result<RequestMetadataGapEvidence, Error> {
    let row = sqlx::query_as::<_, RequestMetadataGapEvidenceRow>(
        "SELECT COALESCE(SUM(event_count), 0)::bigint AS \"event_count\", \
                    COALESCE(SUM(uncertain_gap_count), 0)::bigint AS \"uncertain_gap_count\" \
             FROM ( \
               SELECT event_count, \
                      CASE WHEN certainty = 'lower_bound'::request_metadata_gap_certainty \
                           THEN 1::bigint ELSE 0::bigint END AS uncertain_gap_count \
               FROM request_metadata_ingestion_gaps \
                WHERE last_observed_at >= $1 AND first_observed_at < $2 \
               UNION ALL \
               SELECT event_count, uncertain_gap_count FROM request_metadata_gap_hourly \
                WHERE last_observed_at >= $1 AND first_observed_at < $2 \
             ) retained_gaps",
    )
    .bind(filters.observed_after)
    .bind(filters.observed_before)
    .fetch_one(pool)
    .await?;
    Ok(RequestMetadataGapEvidence {
        event_count: row.event_count,
        uncertain_gap_count: row.uncertain_gap_count,
    })
}

#[derive(sqlx::FromRow)]
struct RequestMetadataGapEvidenceRow {
    event_count: i64,
    uncertain_gap_count: i64,
}

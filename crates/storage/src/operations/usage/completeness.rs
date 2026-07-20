use sqlx::{Postgres, QueryBuilder, Row};

use super::{
    UsageFilters, UsageRangeCoverage,
    query::{push_usage_rows_cte, validate_usage_range},
};
use crate::{OperationsError, PgStore, UsageConsumerStatus, operations::cursor::checked_u64};

#[derive(Clone, Debug)]
pub struct UsageCompleteness {
    pub request_count: u64,
    pub priced_count: u64,
    pub unpriced_count: u64,
    pub incomplete_count: u64,
    /// Exact known loss plus the last durable in-flight lower bounds for
    /// unclean gateway epochs.
    pub ingestion_gap_events: u64,
    pub uncertain_gap_count: u64,
    pub estimated_cost: Option<String>,
    pub currency: Option<String>,
    pub coverage: UsageRangeCoverage,
    pub consumer: UsageConsumerStatus,
    pub complete: bool,
}

#[derive(Clone, Copy, Debug)]
pub(super) struct UsageGapEvidence {
    pub(super) event_count: i64,
    pub(super) uncertain_gap_count: i64,
}

impl PgStore {
    pub async fn usage_completeness(
        &self,
        filters: &UsageFilters,
    ) -> Result<UsageCompleteness, OperationsError> {
        validate_usage_range(filters)?;
        let mut query = QueryBuilder::<Postgres>::new("");
        push_usage_rows_cte(&mut query, filters);
        query.push(
            " SELECT COALESCE(SUM(request_count), 0)::bigint AS request_count, \
                    COALESCE(SUM(request_count - unpriced_count), 0)::bigint AS priced_count, \
                    COALESCE(SUM(unpriced_count), 0)::bigint AS unpriced_count, \
                    COALESCE(SUM(incomplete_count), 0)::bigint AS incomplete_count, \
                    SUM(estimated_cost)::text AS estimated_cost, \
                    COALESCE(MAX(btrim(currency)), \
                      (SELECT btrim(currency) FROM pricing_currency WHERE singleton)) AS currency \
             FROM usage_rows",
        );
        let row = query.build().fetch_one(self.pool()).await?;
        let gap = self.usage_gap_evidence(filters).await?;
        let unpriced_count = checked_u64(row.get("unpriced_count"), "unpriced count")?;
        let incomplete_count = checked_u64(row.get("incomplete_count"), "incomplete count")?;
        let ingestion_gap_events = checked_u64(gap.event_count, "gap event count")?;
        let uncertain_gap_count = checked_u64(gap.uncertain_gap_count, "uncertain gap count")?;
        let coverage = self.usage_range_coverage(filters).await?;
        let consumer = self.usage_consumer_status(chrono::Utc::now()).await?;
        Ok(UsageCompleteness {
            request_count: checked_u64(row.get("request_count"), "request count")?,
            priced_count: checked_u64(row.get("priced_count"), "priced count")?,
            unpriced_count,
            incomplete_count,
            ingestion_gap_events,
            uncertain_gap_count,
            estimated_cost: row.get("estimated_cost"),
            currency: super::super::cursor::trimmed_optional(row.get("currency")),
            coverage,
            consumer,
            complete: unpriced_count == 0
                && incomplete_count == 0
                && ingestion_gap_events == 0
                && uncertain_gap_count == 0
                && coverage.range_complete
                && consumer.complete(),
        })
    }

    pub(super) async fn usage_gap_evidence(
        &self,
        filters: &UsageFilters,
    ) -> Result<UsageGapEvidence, OperationsError> {
        let row = sqlx::query(
            "SELECT COALESCE(SUM(event_count), 0)::bigint AS event_count, \
                    COALESCE(SUM(uncertain_gap_count), 0)::bigint AS uncertain_gap_count \
             FROM ( \
               SELECT event_count, \
                      CASE WHEN certainty = 'lower_bound'::usage_gap_certainty \
                           THEN 1::bigint ELSE 0::bigint END AS uncertain_gap_count \
               FROM usage_ingestion_gaps \
                WHERE last_observed_at >= $1 AND first_observed_at < $2 \
               UNION ALL \
               SELECT event_count, uncertain_gap_count FROM usage_gap_hourly \
                WHERE last_observed_at >= $1 AND first_observed_at < $2 \
             ) retained_gaps",
        )
        .bind(filters.observed_after)
        .bind(filters.observed_before)
        .fetch_one(self.pool())
        .await?;
        Ok(UsageGapEvidence {
            event_count: row.get("event_count"),
            uncertain_gap_count: row.get("uncertain_gap_count"),
        })
    }
}

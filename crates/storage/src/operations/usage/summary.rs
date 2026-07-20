use sqlx::{Postgres, QueryBuilder, Row};

use super::{
    UsageFilters, UsageRangeCoverage,
    query::{push_usage_rows_cte, validate_usage_range},
};
use crate::{OperationsError, PgStore, UsageConsumerStatus, operations::cursor::checked_u64};

#[derive(Clone, Debug)]
pub struct UsageSummary {
    pub request_count: u64,
    pub input_tokens: String,
    pub output_tokens: String,
    pub cached_input_tokens: String,
    pub media_units: String,
    pub estimated_cost: Option<String>,
    pub currency: Option<String>,
    pub unpriced_count: u64,
    pub incomplete_count: u64,
    pub ingestion_gap_events: u64,
    pub uncertain_gap_count: u64,
    pub coverage: UsageRangeCoverage,
    pub consumer: UsageConsumerStatus,
    pub complete: bool,
}

impl PgStore {
    pub async fn usage_summary(
        &self,
        filters: &UsageFilters,
    ) -> Result<UsageSummary, OperationsError> {
        validate_usage_range(filters)?;
        let mut query = QueryBuilder::<Postgres>::new("");
        push_usage_rows_cte(&mut query, filters);
        query.push(
            " SELECT COALESCE(SUM(request_count), 0)::bigint AS request_count,
                    COALESCE(SUM(input_tokens), 0)::text AS input_tokens,
                    COALESCE(SUM(output_tokens), 0)::text AS output_tokens,
                    COALESCE(SUM(cached_input_tokens), 0)::text AS cached_input_tokens,
                    COALESCE(SUM(media_units), 0)::text AS media_units,
                    SUM(estimated_cost)::text AS estimated_cost,
                    COALESCE(SUM(unpriced_count), 0)::bigint AS unpriced_count,
                    COALESCE(SUM(incomplete_count), 0)::bigint AS incomplete_count,
                    COALESCE(MAX(btrim(currency)),
                      (SELECT btrim(currency) FROM pricing_currency WHERE singleton)) AS currency
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
        Ok(UsageSummary {
            request_count: checked_u64(row.get("request_count"), "request count")?,
            input_tokens: row.get("input_tokens"),
            output_tokens: row.get("output_tokens"),
            cached_input_tokens: row.get("cached_input_tokens"),
            media_units: row.get("media_units"),
            estimated_cost: row.get("estimated_cost"),
            currency: super::super::cursor::trimmed_optional(row.get("currency")),
            unpriced_count,
            incomplete_count,
            ingestion_gap_events,
            uncertain_gap_count,
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
}

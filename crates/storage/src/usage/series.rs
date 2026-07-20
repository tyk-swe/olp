use chrono::{DateTime, Utc};
use sqlx::{Postgres, QueryBuilder, Row};

use super::{
    UsageFilters, UsageGranularity, UsageRangeCoverage,
    query::{push_usage_rows_cte, validate_usage_range},
};
use crate::{OperationsError, PgStore, operations::cursor::checked_u64};

#[derive(Clone, Debug)]
pub struct UsagePoint {
    pub bucket: DateTime<Utc>,
    pub request_count: u64,
    pub input_tokens: String,
    pub output_tokens: String,
    pub cached_input_tokens: String,
    pub media_units: String,
    pub estimated_cost: Option<String>,
    pub currency: Option<String>,
    pub unpriced_count: u64,
    pub incomplete_count: u64,
}

#[derive(Clone, Debug)]
pub struct UsageSeries {
    pub points: Vec<UsagePoint>,
    pub coverage: UsageRangeCoverage,
}

impl PgStore {
    pub async fn usage_series(
        &self,
        filters: &UsageFilters,
        granularity: UsageGranularity,
    ) -> Result<UsageSeries, OperationsError> {
        validate_usage_range(filters)?;
        let bucket = match granularity {
            UsageGranularity::Hour => "date_trunc('hour', observed_at)",
            UsageGranularity::Day => "date_trunc('day', observed_at)",
        };
        let mut query = QueryBuilder::<Postgres>::new("");
        push_usage_rows_cte(&mut query, filters);
        query.push(" SELECT ");
        query.push(bucket);
        query.push(
            " AS bucket, COALESCE(SUM(request_count), 0)::bigint AS request_count, \
             COALESCE(SUM(input_tokens), 0)::text AS input_tokens, \
             COALESCE(SUM(output_tokens), 0)::text AS output_tokens, \
             COALESCE(SUM(cached_input_tokens), 0)::text AS cached_input_tokens, \
             COALESCE(SUM(media_units), 0)::text AS media_units, \
             SUM(estimated_cost)::text AS estimated_cost, \
             COALESCE(SUM(unpriced_count), 0)::bigint AS unpriced_count, \
             COALESCE(SUM(incomplete_count), 0)::bigint AS incomplete_count, \
             COALESCE(MAX(btrim(currency)), \
               (SELECT btrim(currency) FROM pricing_currency WHERE singleton)) AS currency \
             FROM usage_rows",
        );
        query.push(" GROUP BY bucket ORDER BY bucket");
        let rows = query.build().fetch_all(self.pool()).await?;
        let points = rows
            .into_iter()
            .map(usage_point_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        Ok(UsageSeries {
            points,
            coverage: self.usage_range_coverage(filters).await?,
        })
    }
}

fn usage_point_from_row(row: sqlx::postgres::PgRow) -> Result<UsagePoint, OperationsError> {
    Ok(UsagePoint {
        bucket: row.get("bucket"),
        request_count: checked_u64(row.get("request_count"), "request count")?,
        input_tokens: row.get("input_tokens"),
        output_tokens: row.get("output_tokens"),
        cached_input_tokens: row.get("cached_input_tokens"),
        media_units: row.get("media_units"),
        estimated_cost: row.get("estimated_cost"),
        currency: crate::operations::cursor::trimmed_optional(row.get("currency")),
        unpriced_count: checked_u64(row.get("unpriced_count"), "unpriced count")?,
        incomplete_count: checked_u64(row.get("incomplete_count"), "incomplete count")?,
    })
}

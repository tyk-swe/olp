use sqlx::{Postgres, QueryBuilder, Row};

use super::{
    UsageDimension, UsageFilters, UsageRangeCoverage,
    query::{push_usage_rows_cte, validate_usage_range},
};
use crate::{OperationsError, PgStore, operations::MAX_PAGE_SIZE};

#[derive(Clone, Debug)]
pub struct UsageBreakdown {
    pub dimension: String,
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
pub struct UsageBreakdownReport {
    pub items: Vec<UsageBreakdown>,
    pub coverage: UsageRangeCoverage,
}

impl PgStore {
    pub async fn usage_breakdown(
        &self,
        filters: &UsageFilters,
        dimension: UsageDimension,
        limit: u16,
    ) -> Result<UsageBreakdownReport, OperationsError> {
        validate_usage_range(filters)?;
        let expression = match dimension {
            UsageDimension::Route => "route_slug",
            UsageDimension::Provider => "provider_id::text",
            UsageDimension::Model => "upstream_model",
            UsageDimension::ApiKey => "COALESCE(api_key_id::text, 'unknown')",
            UsageDimension::Operation => "operation",
        };
        let mut query = QueryBuilder::<Postgres>::new("");
        push_usage_rows_cte(&mut query, filters);
        query.push(" SELECT ");
        query.push(expression);
        query.push(
            " AS dimension, COALESCE(SUM(request_count), 0)::bigint AS request_count, \
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
        query.push(" GROUP BY dimension ORDER BY request_count DESC, dimension LIMIT ");
        query.push_bind(i64::from(limit.clamp(1, MAX_PAGE_SIZE)));
        let rows = query.build().fetch_all(self.pool()).await?;
        let items = rows
            .into_iter()
            .map(|row| {
                Ok(UsageBreakdown {
                    dimension: row.get("dimension"),
                    request_count: crate::operations::cursor::checked_u64(
                        row.get("request_count"),
                        "request count",
                    )?,
                    input_tokens: row.get("input_tokens"),
                    output_tokens: row.get("output_tokens"),
                    cached_input_tokens: row.get("cached_input_tokens"),
                    media_units: row.get("media_units"),
                    estimated_cost: row.get("estimated_cost"),
                    currency: crate::operations::cursor::trimmed_optional(row.get("currency")),
                    unpriced_count: crate::operations::cursor::checked_u64(
                        row.get("unpriced_count"),
                        "unpriced count",
                    )?,
                    incomplete_count: crate::operations::cursor::checked_u64(
                        row.get("incomplete_count"),
                        "incomplete count",
                    )?,
                })
            })
            .collect::<Result<Vec<_>, OperationsError>>()?;
        Ok(UsageBreakdownReport {
            items,
            coverage: self.usage_range_coverage(filters).await?,
        })
    }
}

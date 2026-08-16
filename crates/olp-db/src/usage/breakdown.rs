use sqlx::{FromRow, Postgres, QueryBuilder};

use super::{
    Coverage, Dimension, Filters,
    query::{UsageCountScope, push_usage_rows_cte, validate_usage_range},
};
use crate::{
    operations::{MAX_PAGE_SIZE, cursor::Error},
    store::Store,
};

#[derive(Clone, Debug)]
pub struct Item {
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
pub struct Report {
    pub items: Vec<Item>,
    pub coverage: Coverage,
}

#[derive(Debug, FromRow)]
struct UsageBreakdownRow {
    dimension: String,
    request_count: i64,
    input_tokens: String,
    output_tokens: String,
    cached_input_tokens: String,
    media_units: String,
    estimated_cost: Option<String>,
    unpriced_count: i64,
    incomplete_count: i64,
    currency: Option<String>,
}

impl Store {
    pub async fn usage_breakdown(
        &self,
        filters: &Filters,
        dimension: Dimension,
        limit: u16,
    ) -> Result<Report, Error> {
        validate_usage_range(filters)?;
        let expression = match dimension {
            Dimension::Route => "route_slug",
            Dimension::Provider => "provider_id::text",
            Dimension::Model => "upstream_model",
            Dimension::ApiKey => "COALESCE(api_key_id::text, 'unknown')",
            Dimension::Operation => "operation",
        };
        let count_scope = match dimension {
            Dimension::Provider if filters.upstream_model.is_none() => UsageCountScope::Provider,
            Dimension::Model if filters.provider_id.is_none() => UsageCountScope::Model,
            Dimension::Provider | Dimension::Model => UsageCountScope::Target,
            Dimension::Route | Dimension::ApiKey | Dimension::Operation => {
                UsageCountScope::for_filters(filters)
            }
        };
        let mut query = QueryBuilder::<Postgres>::new("");
        push_usage_rows_cte(&mut query, filters, count_scope);
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
        let rows = query
            .build_query_as::<UsageBreakdownRow>()
            .fetch_all(self.pool())
            .await?;
        let items = rows
            .into_iter()
            .map(|row| {
                Ok(Item {
                    dimension: row.dimension,
                    request_count: crate::operations::cursor::checked_u64(
                        row.request_count,
                        "request count",
                    )?,
                    input_tokens: row.input_tokens,
                    output_tokens: row.output_tokens,
                    cached_input_tokens: row.cached_input_tokens,
                    media_units: row.media_units,
                    estimated_cost: row.estimated_cost,
                    currency: crate::operations::cursor::trimmed_optional(row.currency),
                    unpriced_count: crate::operations::cursor::checked_u64(
                        row.unpriced_count,
                        "unpriced count",
                    )?,
                    incomplete_count: crate::operations::cursor::checked_u64(
                        row.incomplete_count,
                        "incomplete count",
                    )?,
                })
            })
            .collect::<Result<Vec<_>, Error>>()?;
        Ok(Report {
            items,
            coverage: self.usage_range_coverage(filters).await?,
        })
    }
}

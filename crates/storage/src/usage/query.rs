use chrono::{DateTime, Utc};
use sqlx::{Postgres, QueryBuilder, Row};

use super::{UsageFilters, UsageRangeCoverage};
use crate::{OperationsError, PgStore};

impl PgStore {
    pub(super) async fn usage_range_coverage(
        &self,
        filters: &UsageFilters,
    ) -> Result<UsageRangeCoverage, OperationsError> {
        let mut boundary_buckets = Vec::with_capacity(2);
        let lower_bucket = floor_usage_hour(filters.observed_after);
        if lower_bucket != filters.observed_after {
            boundary_buckets.push(lower_bucket);
        }
        let upper_bucket = floor_usage_hour(filters.observed_before);
        if upper_bucket != filters.observed_before {
            boundary_buckets.push(upper_bucket);
        }
        boundary_buckets.sort_unstable();
        boundary_buckets.dedup();
        if boundary_buckets.is_empty() {
            return Ok(UsageRangeCoverage {
                range_complete: true,
                approximate: false,
                excluded_partial_aggregate_boundaries: 0,
            });
        }

        let mut query = QueryBuilder::<Postgres>::new(
            "SELECT COUNT(DISTINCT bucket)::bigint AS excluded_boundaries \
             FROM usage_hourly WHERE bucket = ANY(",
        );
        query.push_bind(&boundary_buckets).push("::timestamptz[])");
        push_usage_dimension_filters(&mut query, filters);
        let row = query.build().fetch_one(self.pool()).await?;
        let excluded = crate::operations::cursor::checked_u64(
            row.get("excluded_boundaries"),
            "excluded partial aggregate boundary count",
        )?;
        let excluded = u8::try_from(excluded).map_err(|_| {
            OperationsError::Invalid(
                "excluded partial aggregate boundary count is invalid".to_owned(),
            )
        })?;
        Ok(UsageRangeCoverage {
            range_complete: excluded == 0,
            approximate: excluded > 0,
            excluded_partial_aggregate_boundaries: excluded,
        })
    }
}

pub(super) fn push_usage_rows_cte(query: &mut QueryBuilder<Postgres>, filters: &UsageFilters) {
    query.push(
        "WITH usage_rows AS (\
         SELECT observed_at, route_slug, provider_id, upstream_model, api_key_id, operation, surface, \
                1::bigint AS request_count, COALESCE(input_tokens, 0)::numeric AS input_tokens, \
                COALESCE(output_tokens, 0)::numeric AS output_tokens, \
                COALESCE(cached_input_tokens, 0)::numeric AS cached_input_tokens, \
                COALESCE(media_units, 0)::numeric AS media_units, estimated_cost, \
                CASE WHEN unpriced THEN 1 ELSE 0 END::bigint AS unpriced_count, \
                CASE WHEN usage_complete THEN 0 ELSE 1 END::bigint AS incomplete_count, \
                currency::text AS currency \
         FROM usage_facts WHERE true",
    );
    push_usage_source_filters(query, filters, "observed_at", false);
    query.push(
        " UNION ALL \
         SELECT bucket AS observed_at, route_slug, provider_id, upstream_model, api_key_id, \
                operation, surface, request_count, input_tokens, output_tokens, \
                cached_input_tokens, media_units, estimated_cost, unpriced_count, \
                incomplete_count, currency::text AS currency \
         FROM usage_hourly WHERE true",
    );
    push_usage_source_filters(query, filters, "bucket", true);
    query.push(")");
}

fn push_usage_source_filters(
    query: &mut QueryBuilder<Postgres>,
    filters: &UsageFilters,
    observed_column: &str,
    hourly: bool,
) {
    if hourly {
        // Retained aggregates are indivisible. Include only buckets fully
        // covered by [observed_after, observed_before); boundary buckets are
        // reported separately as unavailable instead of being rounded down or
        // silently prorated.
        query
            .push(" AND ")
            .push(observed_column)
            .push(" >= ")
            .push_bind(ceil_usage_hour(filters.observed_after))
            .push(" AND ")
            .push(observed_column)
            .push(" + interval '1 hour' <= ")
            .push_bind(filters.observed_before);
    } else {
        query
            .push(" AND ")
            .push(observed_column)
            .push(" >= ")
            .push_bind(filters.observed_after)
            .push(" AND ")
            .push(observed_column)
            .push(" < ")
            .push_bind(filters.observed_before);
    }
    push_usage_dimension_filters(query, filters);
}

fn push_usage_dimension_filters(query: &mut QueryBuilder<Postgres>, filters: &UsageFilters) {
    if let Some(value) = &filters.route_slug {
        query.push(" AND route_slug = ").push_bind(value);
    }
    if let Some(value) = filters.provider_id {
        query.push(" AND provider_id = ").push_bind(value);
    }
    if let Some(value) = &filters.upstream_model {
        query.push(" AND upstream_model = ").push_bind(value);
    }
    if let Some(value) = filters.api_key_id {
        query.push(" AND api_key_id = ").push_bind(value);
    }
    if let Some(value) = filters.operation {
        query.push(" AND operation = ").push_bind(value.as_str());
    }
}

pub(crate) fn floor_usage_hour(value: DateTime<Utc>) -> DateTime<Utc> {
    let seconds = value.timestamp().div_euclid(60 * 60) * 60 * 60;
    DateTime::from_timestamp(seconds, 0).expect("a truncated valid timestamp remains valid")
}

pub(crate) fn ceil_usage_hour(value: DateTime<Utc>) -> DateTime<Utc> {
    let floor = floor_usage_hour(value);
    if floor == value {
        floor
    } else {
        floor + chrono::Duration::hours(1)
    }
}

pub(super) fn validate_usage_range(filters: &UsageFilters) -> Result<(), OperationsError> {
    if filters.observed_before <= filters.observed_after
        || filters.observed_before - filters.observed_after > chrono::Duration::days(366)
    {
        return Err(OperationsError::Invalid(
            "usage range must be positive and no longer than 366 days".to_owned(),
        ));
    }
    Ok(())
}

use chrono::{DateTime, Utc};
use sqlx::{FromRow, Postgres, QueryBuilder};

use super::{Coverage, Filters};
use crate::{operations::cursor::Error, store::Store};

#[derive(Debug, FromRow)]
struct UsageBoundaryRow {
    excluded_boundaries: i64,
}

#[derive(Clone, Copy)]
pub(super) enum UsageCountScope {
    Request,
    Provider,
    Model,
    Target,
}

impl UsageCountScope {
    pub(super) fn for_filters(filters: &Filters) -> Self {
        match (
            filters.provider_id.is_some(),
            filters.upstream_model.is_some(),
        ) {
            (false, false) => Self::Request,
            (true, false) => Self::Provider,
            (false, true) => Self::Model,
            (true, true) => Self::Target,
        }
    }

    fn raw_columns(self) -> (&'static str, &'static str, &'static str) {
        match self {
            Self::Request => (
                "request_counted",
                "request_unpriced_counted",
                "request_incomplete_counted",
            ),
            Self::Provider => (
                "provider_request_counted",
                "provider_unpriced_counted",
                "provider_incomplete_counted",
            ),
            Self::Model => (
                "model_request_counted",
                "model_unpriced_counted",
                "model_incomplete_counted",
            ),
            Self::Target => (
                "target_request_counted",
                "target_unpriced_counted",
                "target_incomplete_counted",
            ),
        }
    }

    fn hourly_columns(self) -> (&'static str, &'static str, &'static str) {
        match self {
            Self::Request => (
                "request_count",
                "request_unpriced_count",
                "request_incomplete_count",
            ),
            Self::Provider => (
                "provider_request_count",
                "provider_unpriced_count",
                "provider_incomplete_count",
            ),
            Self::Model => (
                "model_request_count",
                "model_unpriced_count",
                "model_incomplete_count",
            ),
            Self::Target => (
                "target_request_count",
                "target_unpriced_count",
                "target_incomplete_count",
            ),
        }
    }
}

impl Store {
    pub(super) async fn usage_range_coverage(&self, filters: &Filters) -> Result<Coverage, Error> {
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
            return Ok(Coverage {
                range_complete: true,
                approximate: false,
                excluded_partial_aggregate_boundaries: 0,
            });
        }

        let mut query = QueryBuilder::<Postgres>::new(
            "SELECT COUNT(DISTINCT bucket)::bigint AS excluded_boundaries \
             FROM attempt_usage_hourly WHERE bucket = ANY(",
        );
        query.push_bind(&boundary_buckets).push("::timestamptz[])");
        push_usage_dimension_filters(&mut query, filters);
        let row = query
            .build_query_as::<UsageBoundaryRow>()
            .fetch_one(self.pool())
            .await?;
        let excluded = crate::operations::cursor::checked_u64(
            row.excluded_boundaries,
            "excluded partial aggregate boundary count",
        )?;
        let excluded = u8::try_from(excluded).map_err(|_| {
            Error::Invalid("excluded partial aggregate boundary count is invalid".to_owned())
        })?;
        Ok(Coverage {
            range_complete: excluded == 0,
            approximate: excluded > 0,
            excluded_partial_aggregate_boundaries: excluded,
        })
    }
}

pub(super) fn push_usage_rows_cte(
    query: &mut QueryBuilder<Postgres>,
    filters: &Filters,
    count_scope: UsageCountScope,
) {
    let (raw_count, raw_unpriced, raw_incomplete) = count_scope.raw_columns();
    let (hourly_count, hourly_unpriced, hourly_incomplete) = count_scope.hourly_columns();
    query.push(
        "WITH usage_rows AS (\
         SELECT observed_at, route_slug, provider_id, upstream_model, api_key_id, operation, surface, \
                CASE WHEN ",
    );
    query.push(raw_count);
    query.push(
        " THEN 1 ELSE 0 END::bigint AS request_count, \
                COALESCE(input_tokens, 0)::numeric AS input_tokens, \
                COALESCE(output_tokens, 0)::numeric AS output_tokens, \
                COALESCE(cached_input_tokens, 0)::numeric AS cached_input_tokens, \
                COALESCE(media_units, 0)::numeric AS media_units, estimated_cost, \
                CASE WHEN ",
    );
    query.push(raw_unpriced);
    query.push(
        " THEN 1 ELSE 0 END::bigint AS unpriced_count, \
                CASE WHEN ",
    );
    query.push(raw_incomplete);
    query.push(
        " THEN 1 ELSE 0 END::bigint AS incomplete_count, \
                currency::text AS currency \
         FROM attempt_usage_facts WHERE true",
    );
    push_usage_source_filters(query, filters, "observed_at", false);
    query.push(
        " UNION ALL \
         SELECT bucket AS observed_at, route_slug, provider_id, upstream_model, api_key_id, \
                operation, surface, ",
    );
    query.push(hourly_count);
    query.push(", input_tokens, output_tokens, cached_input_tokens, media_units, estimated_cost, ");
    query.push(hourly_unpriced);
    query.push(", ");
    query.push(hourly_incomplete);
    query.push(
        ", currency::text AS currency \
         FROM attempt_usage_hourly WHERE true",
    );
    push_usage_source_filters(query, filters, "bucket", true);
    query.push(")");
}

fn push_usage_source_filters(
    query: &mut QueryBuilder<Postgres>,
    filters: &Filters,
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

fn push_usage_dimension_filters(query: &mut QueryBuilder<Postgres>, filters: &Filters) {
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

pub(super) fn validate_usage_range(filters: &Filters) -> Result<(), Error> {
    if filters.observed_before <= filters.observed_after
        || filters.observed_before - filters.observed_after > chrono::Duration::days(366)
    {
        return Err(Error::Invalid(
            "usage range must be positive and no longer than 366 days".to_owned(),
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use olp_engine::domain::canonical::identity::OperationKind;
    use uuid::Uuid;

    use super::*;

    fn filters(after: &str, before: &str) -> Filters {
        Filters {
            observed_after: after.parse().unwrap(),
            observed_before: before.parse().unwrap(),
            route_slug: None,
            provider_id: None,
            upstream_model: None,
            api_key_id: None,
            operation: None,
        }
    }

    #[test]
    fn count_scope_selects_the_exact_request_dimension() {
        let mut filters = filters("2026-01-01T00:00:00Z", "2026-01-01T01:00:00Z");
        for (provider, model, raw, hourly) in [
            (false, false, "request_counted", "request_count"),
            (
                true,
                false,
                "provider_request_counted",
                "provider_request_count",
            ),
            (false, true, "model_request_counted", "model_request_count"),
            (true, true, "target_request_counted", "target_request_count"),
        ] {
            filters.provider_id = provider.then(Uuid::now_v7);
            filters.upstream_model = model.then(|| "model-a".to_owned());
            let scope = UsageCountScope::for_filters(&filters);
            assert_eq!(scope.raw_columns().0, raw);
            assert_eq!(scope.hourly_columns().0, hourly);
        }
    }

    #[test]
    fn usage_cte_contains_raw_and_retained_ranges_with_every_dimension_filter() {
        let mut filters = filters("2026-01-01T00:15:00Z", "2026-01-01T03:45:00Z");
        filters.route_slug = Some("primary".to_owned());
        filters.provider_id = Some(Uuid::now_v7());
        filters.upstream_model = Some("model-a".to_owned());
        filters.api_key_id = Some(Uuid::now_v7());
        filters.operation = Some(OperationKind::Generation);
        let mut query = QueryBuilder::<Postgres>::new("");

        push_usage_rows_cte(&mut query, &filters, UsageCountScope::Target);
        let sql = query.sql();
        let sql = sql.as_str();

        for fragment in [
            "FROM attempt_usage_facts WHERE true",
            "target_request_counted",
            "target_unpriced_counted",
            "target_incomplete_counted",
            "FROM attempt_usage_hourly WHERE true",
            "target_request_count",
            "target_unpriced_count",
            "target_incomplete_count",
            "observed_at >= ",
            "observed_at < ",
            "bucket >= ",
            "bucket + interval '1 hour' <= ",
            "route_slug =",
            "provider_id =",
            "upstream_model =",
            "api_key_id =",
            "operation =",
        ] {
            assert!(sql.contains(fragment), "missing {fragment:?} in {sql}");
        }
    }

    #[test]
    fn hour_rounding_is_exact_on_both_sides_of_the_unix_epoch() {
        for (value, floor, ceil) in [
            (
                "2026-07-12T10:00:00Z",
                "2026-07-12T10:00:00Z",
                "2026-07-12T10:00:00Z",
            ),
            (
                "2026-07-12T10:59:59.999Z",
                "2026-07-12T10:00:00Z",
                "2026-07-12T11:00:00Z",
            ),
            (
                "1969-12-31T23:59:59Z",
                "1969-12-31T23:00:00Z",
                "1970-01-01T00:00:00Z",
            ),
        ] {
            let value = value.parse::<DateTime<Utc>>().unwrap();
            assert_eq!(
                floor_usage_hour(value),
                floor.parse::<DateTime<Utc>>().unwrap()
            );
            assert_eq!(
                ceil_usage_hour(value),
                ceil.parse::<DateTime<Utc>>().unwrap()
            );
        }
    }

    #[test]
    fn usage_range_is_positive_and_bounded_to_366_days() {
        let start = "2024-01-01T00:00:00Z".parse::<DateTime<Utc>>().unwrap();
        let mut filters = Filters {
            observed_after: start,
            observed_before: start + chrono::Duration::nanoseconds(1),
            route_slug: None,
            provider_id: None,
            upstream_model: None,
            api_key_id: None,
            operation: None,
        };
        assert!(validate_usage_range(&filters).is_ok());
        filters.observed_before = start + chrono::Duration::days(366);
        assert!(validate_usage_range(&filters).is_ok());

        for invalid_end in [
            start,
            start - chrono::Duration::nanoseconds(1),
            start + chrono::Duration::days(366) + chrono::Duration::nanoseconds(1),
        ] {
            filters.observed_before = invalid_end;
            assert!(
                matches!(validate_usage_range(&filters), Err(Error::Invalid(_))),
                "accepted range ending at {invalid_end}"
            );
        }
    }
}

use chrono::{DateTime, Utc};
use sqlx::{FromRow, Postgres, QueryBuilder};

use super::{
    Coverage, Filters, Granularity,
    query::{UsageCountScope, push_usage_rows_cte, validate_usage_range},
};
use crate::{
    operations::cursor::{Error, checked_u64},
    store::Store,
};

#[derive(Clone, Debug)]
pub struct Point {
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
pub struct Report {
    pub points: Vec<Point>,
    pub coverage: Coverage,
}

#[derive(Debug, FromRow)]
struct UsagePointRow {
    bucket: DateTime<Utc>,
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
    pub async fn usage_series(
        &self,
        filters: &Filters,
        granularity: Granularity,
    ) -> Result<Report, Error> {
        validate_usage_range(filters)?;
        // date_trunc on a timestamptz truncates in the session TimeZone.
        // Pool connections pin UTC, but state the boundary in SQL as well so a
        // series bucket can never disagree with a rollup bucket.
        let bucket = match granularity {
            Granularity::Hour => {
                "date_trunc('hour', observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'"
            }
            Granularity::Day => {
                "date_trunc('day', observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'"
            }
        };
        let mut query = QueryBuilder::<Postgres>::new("");
        push_usage_rows_cte(&mut query, filters, UsageCountScope::for_filters(filters));
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
        let rows = query
            .build_query_as::<UsagePointRow>()
            .fetch_all(self.pool())
            .await?;
        let points = rows
            .into_iter()
            .map(usage_point_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        Ok(Report {
            points,
            coverage: self.usage_range_coverage(filters).await?,
        })
    }
}

fn usage_point_from_row(row: UsagePointRow) -> Result<Point, Error> {
    Ok(Point {
        bucket: row.bucket,
        request_count: checked_u64(row.request_count, "request count")?,
        input_tokens: row.input_tokens,
        output_tokens: row.output_tokens,
        cached_input_tokens: row.cached_input_tokens,
        media_units: row.media_units,
        estimated_cost: row.estimated_cost,
        currency: crate::operations::cursor::trimmed_optional(row.currency),
        unpriced_count: checked_u64(row.unpriced_count, "unpriced count")?,
        incomplete_count: checked_u64(row.incomplete_count, "incomplete count")?,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn row() -> UsagePointRow {
        UsagePointRow {
            bucket: "2026-08-01T04:00:00Z".parse().unwrap(),
            request_count: 3,
            input_tokens: "10".to_owned(),
            output_tokens: "20".to_owned(),
            cached_input_tokens: "2".to_owned(),
            media_units: "0.5".to_owned(),
            estimated_cost: Some("0.025".to_owned()),
            unpriced_count: 1,
            incomplete_count: 0,
            currency: Some(" USD ".to_owned()),
        }
    }

    #[test]
    fn usage_rows_convert_without_losing_exact_numeric_strings() {
        let point = usage_point_from_row(row()).unwrap();
        assert_eq!(
            point.bucket,
            "2026-08-01T04:00:00Z".parse::<DateTime<Utc>>().unwrap()
        );
        assert_eq!(point.request_count, 3);
        assert_eq!(point.input_tokens, "10");
        assert_eq!(point.output_tokens, "20");
        assert_eq!(point.cached_input_tokens, "2");
        assert_eq!(point.media_units, "0.5");
        assert_eq!(point.estimated_cost.as_deref(), Some("0.025"));
        assert_eq!(point.currency.as_deref(), Some("USD"));
        assert_eq!(point.unpriced_count, 1);
        assert_eq!(point.incomplete_count, 0);
    }

    #[test]
    fn usage_rows_fail_closed_on_negative_database_counts() {
        let mutators: [fn(&mut UsagePointRow); 3] = [
            |row: &mut UsagePointRow| row.request_count = -1,
            |row: &mut UsagePointRow| row.unpriced_count = -1,
            |row: &mut UsagePointRow| row.incomplete_count = -1,
        ];
        for mutate in mutators {
            let mut candidate = row();
            mutate(&mut candidate);
            assert!(matches!(
                usage_point_from_row(candidate),
                Err(Error::Invalid(_))
            ));
        }
    }
}

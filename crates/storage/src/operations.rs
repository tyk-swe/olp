use std::{collections::HashSet, fmt};

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::{DateTime, Utc};
use olp_domain::{OperationKind, ProviderKind, ProviderState, Surface};
use serde::{Deserialize, Serialize};
use sqlx::{Postgres, QueryBuilder, Row};
use thiserror::Error;
use uuid::Uuid;

use crate::{
    IdempotencyOutcome, IdempotencyResponse, PersistenceError, PgStore, ReplayableIdempotency,
    UsageConsumerStatus, split_page,
    store::{
        ReplayableIdempotencyClaim, claim_replayable_idempotency, complete_replayable_idempotency,
    },
};

const MAX_PAGE_SIZE: u16 = 200;
const PRICING_LOCK_ID: i64 = 0x4f4c_505f_5052; // "OLP_PR"

#[derive(Debug, Error)]
pub enum OperationsError {
    #[error(transparent)]
    Persistence(#[from] PersistenceError),
    #[error("database operation failed")]
    Database(#[from] sqlx::Error),
    #[error("cursor is invalid")]
    InvalidCursor,
    #[error("resource was not found")]
    NotFound,
    #[error("the resource changed; refresh and retry")]
    PreconditionFailed,
    #[error("idempotency key has already been used for this operation")]
    IdempotencyConflict,
    #[error("an operation with this idempotency key is still in progress")]
    IdempotencyInProgress,
    #[error("operation input is invalid: {0}")]
    Invalid(String),
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct TimestampCursor {
    pub at: DateTime<Utc>,
    pub id: Uuid,
}

impl TimestampCursor {
    pub fn parse(value: &str) -> Result<Self, OperationsError> {
        let bytes = URL_SAFE_NO_PAD
            .decode(value)
            .map_err(|_| OperationsError::InvalidCursor)?;
        let cursor: Self =
            serde_json::from_slice(&bytes).map_err(|_| OperationsError::InvalidCursor)?;
        if cursor.id.get_version_num() != 7 {
            return Err(OperationsError::InvalidCursor);
        }
        Ok(cursor)
    }

    #[must_use]
    pub fn encode(&self) -> String {
        URL_SAFE_NO_PAD
            .encode(serde_json::to_vec(self).expect("timestamp cursor serialization cannot fail"))
    }
}

#[derive(Clone, Debug)]
pub struct Page<T> {
    pub items: Vec<T>,
    pub next_cursor: Option<String>,
}

#[derive(Clone, Debug, Default)]
pub struct RequestFilters {
    pub route_slug: Option<String>,
    pub provider_id: Option<Uuid>,
    pub upstream_model: Option<String>,
    pub api_key_id: Option<Uuid>,
    pub operation: Option<OperationKind>,
    pub status_code: Option<u16>,
    pub error_class: Option<String>,
    pub started_after: Option<DateTime<Utc>>,
    pub started_before: Option<DateTime<Utc>>,
}

#[derive(Clone, Debug)]
pub struct RequestRecord {
    pub id: Uuid,
    pub runtime_generation_id: Uuid,
    pub api_key_id: Uuid,
    pub route_slug: String,
    pub operation: OperationKind,
    pub surface: Surface,
    pub started_at: DateTime<Utc>,
    pub completed_at: Option<DateTime<Utc>>,
    pub status_code: Option<u16>,
    pub error_class: Option<String>,
    pub total_latency_ms: Option<u64>,
    pub first_byte_ms: Option<u64>,
    pub attempt_count: u16,
    pub input_tokens: Option<u64>,
    pub output_tokens: Option<u64>,
    pub cached_input_tokens: Option<u64>,
    pub estimated_cost: Option<String>,
    pub currency: Option<String>,
    pub unpriced: Option<bool>,
    pub usage_complete: Option<bool>,
}

#[derive(Clone, Debug)]
pub struct AttemptRecord {
    pub id: Uuid,
    pub ordinal: u16,
    pub provider_id: Uuid,
    pub provider_name: String,
    pub upstream_model: String,
    pub started_at: DateTime<Utc>,
    pub completed_at: Option<DateTime<Utc>>,
    pub status_code: Option<u16>,
    pub error_class: Option<String>,
    pub committed: bool,
    pub latency_ms: Option<u64>,
    pub first_byte_ms: Option<u64>,
}

#[derive(Clone, Debug)]
pub struct RequestDetail {
    pub request: RequestRecord,
    pub attempts: Vec<AttemptRecord>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum UsageGranularity {
    Hour,
    Day,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum UsageDimension {
    Route,
    Provider,
    Model,
    ApiKey,
    Operation,
}

#[derive(Clone, Debug)]
pub struct UsageFilters {
    pub observed_after: DateTime<Utc>,
    pub observed_before: DateTime<Utc>,
    pub route_slug: Option<String>,
    pub provider_id: Option<Uuid>,
    pub upstream_model: Option<String>,
    pub api_key_id: Option<Uuid>,
    pub operation: Option<OperationKind>,
}

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

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct UsageRangeCoverage {
    /// False only when a requested partial hour exists solely as a retained
    /// hourly aggregate and therefore cannot be sliced without guessing.
    pub range_complete: bool,
    /// Signals that returned totals cover only the exact, representable subset
    /// of the requested range. OLP never prorates hourly aggregates.
    pub approximate: bool,
    pub excluded_partial_aggregate_boundaries: u8,
}

#[derive(Clone, Debug)]
pub struct UsageSeries {
    pub points: Vec<UsagePoint>,
    pub coverage: UsageRangeCoverage,
}

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

#[derive(Clone, Copy, Debug)]
struct UsageGapEvidence {
    event_count: i64,
    uncertain_gap_count: i64,
}

#[derive(Clone, Debug)]
pub struct AuditRecord {
    pub id: Uuid,
    pub actor_user_id: Option<Uuid>,
    pub actor_email: Option<String>,
    pub action: String,
    pub resource_type: String,
    pub resource_id: Option<String>,
    pub outcome: String,
    pub source_ip: Option<String>,
    pub user_agent_family: Option<String>,
    pub occurred_at: DateTime<Utc>,
}

#[derive(Clone, Debug)]
pub struct RuntimeGenerationRecord {
    pub id: Uuid,
    pub sequence: u64,
    pub sha256_hex: String,
    pub created_by: Uuid,
    pub created_by_email: String,
    pub created_at: DateTime<Utc>,
}

#[derive(Clone, Debug)]
pub struct ProviderHealthRecord {
    pub provider_id: Uuid,
    pub provider_name: String,
    pub provider_kind: ProviderKind,
    pub provider_state: ProviderState,
    pub status: String,
    pub last_probe_at: Option<DateTime<Utc>>,
    pub last_probe_status: Option<String>,
    pub last_probe_detail: Option<String>,
    pub last_attempt_at: Option<DateTime<Utc>>,
    pub attempt_count: u64,
    pub success_count: u64,
    pub rate_limit_count: u64,
    pub server_error_count: u64,
    pub transport_error_count: u64,
    pub average_latency_ms: Option<f64>,
}

/// Bounded, metadata-only rollup used by the Prometheus endpoint. It reads the
/// same normalized request/attempt records as the operator API and never
/// exposes prompt or response content.
#[derive(Clone, Debug, Default)]
pub struct PrometheusOperationsSummary {
    pub request_count: u64,
    pub success_count: u64,
    pub cancelled_attempt_count: u64,
    pub p95_latency_ms: Option<f64>,
    pub p99_latency_ms: Option<f64>,
}

#[derive(Clone, Debug)]
pub struct SettingRecord {
    pub key: String,
    pub value: String,
    pub etag: Uuid,
    pub updated_by: Uuid,
    pub updated_at: DateTime<Utc>,
}

#[derive(Clone, Debug)]
pub struct PriceInput {
    pub provider_kind: ProviderKind,
    pub provider_id: Option<Uuid>,
    pub model: String,
    pub operation: OperationKind,
    pub input_per_million: Option<String>,
    pub output_per_million: Option<String>,
    pub unit_price: Option<String>,
    pub currency: String,
}

#[derive(Clone, Debug)]
pub struct PricingRevisionRecord {
    pub id: Uuid,
    pub revision: u32,
    pub effective_at: DateTime<Utc>,
    pub created_by: Uuid,
    pub created_at: DateTime<Utc>,
    pub prices: Vec<PriceInput>,
}

impl PgStore {
    pub async fn prometheus_operations_summary(
        &self,
        window_minutes: u16,
    ) -> Result<PrometheusOperationsSummary, OperationsError> {
        let window_minutes = window_minutes.clamp(1, 60);
        let row = sqlx::query(
            "WITH recent_requests AS MATERIALIZED (\
                 SELECT id, started_at, status_code, error_class, total_latency_ms \
                   FROM requests \
                  WHERE started_at >= now() - make_interval(mins => $1)\
             ) \
             SELECT count(*) AS request_count, \
                    count(*) FILTER (WHERE error_class IS NULL \
                        AND status_code BETWEEN 200 AND 399) AS success_count, \
                    percentile_cont(0.95) WITHIN GROUP \
                        (ORDER BY total_latency_ms) \
                        FILTER (WHERE total_latency_ms IS NOT NULL) AS p95_latency_ms, \
                    percentile_cont(0.99) WITHIN GROUP \
                        (ORDER BY total_latency_ms) \
                        FILTER (WHERE total_latency_ms IS NOT NULL) AS p99_latency_ms, \
                    (SELECT count(*) FROM attempts a \
                      JOIN recent_requests r \
                        ON r.id = a.request_id \
                       AND r.started_at = a.request_started_at \
                     WHERE a.error_class = 'cancelled') AS cancelled_attempt_count \
               FROM recent_requests",
        )
        .bind(i32::from(window_minutes))
        .fetch_one(self.pool())
        .await?;
        Ok(PrometheusOperationsSummary {
            request_count: checked_u64(row.get("request_count"), "request count")?,
            success_count: checked_u64(row.get("success_count"), "success count")?,
            cancelled_attempt_count: checked_u64(
                row.get("cancelled_attempt_count"),
                "cancelled attempt count",
            )?,
            p95_latency_ms: row.get("p95_latency_ms"),
            p99_latency_ms: row.get("p99_latency_ms"),
        })
    }

    pub async fn requests(
        &self,
        filters: &RequestFilters,
        cursor: Option<&TimestampCursor>,
        limit: u16,
    ) -> Result<Page<RequestRecord>, OperationsError> {
        let page_size = limit.clamp(1, MAX_PAGE_SIZE);
        let mut query = QueryBuilder::<Postgres>::new(
            "SELECT r.id, r.runtime_generation_id, r.api_key_id, r.route_slug, r.operation, \
                    r.surface, r.started_at, r.completed_at, r.status_code, r.error_class, \
                    r.total_latency_ms, r.first_byte_ms, r.attempt_count, u.input_tokens, \
                    u.output_tokens, u.cached_input_tokens, u.estimated_cost::text AS estimated_cost, \
                    u.currency::text AS currency, u.unpriced, u.usage_complete \
             FROM requests r LEFT JOIN usage_facts u \
               ON u.request_id = r.id AND u.request_started_at = r.started_at WHERE true",
        );
        push_request_filters(&mut query, filters);
        if let Some(cursor) = cursor {
            query.push(" AND (r.started_at, r.id) < (");
            query.push_bind(cursor.at);
            query.push(", ");
            query.push_bind(cursor.id);
            query.push(")");
        }
        query.push(" ORDER BY r.started_at DESC, r.id DESC LIMIT ");
        query.push_bind(i64::from(page_size) + 1);
        let rows = query.build().fetch_all(self.pool()).await?;
        let items = rows
            .into_iter()
            .map(request_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        let (items, next_cursor) = split_page(items, usize::from(page_size), |item| {
            TimestampCursor {
                at: item.started_at,
                id: item.id,
            }
            .encode()
        });
        Ok(Page { items, next_cursor })
    }

    pub async fn request_detail(&self, id: Uuid) -> Result<RequestDetail, OperationsError> {
        let row = sqlx::query(
            "SELECT r.id, r.runtime_generation_id, r.api_key_id, r.route_slug, r.operation, \
                    r.surface, r.started_at, r.completed_at, r.status_code, r.error_class, \
                    r.total_latency_ms, r.first_byte_ms, r.attempt_count, u.input_tokens, \
                    u.output_tokens, u.cached_input_tokens, u.estimated_cost::text AS estimated_cost, \
                    u.currency::text AS currency, u.unpriced, u.usage_complete \
             FROM requests r LEFT JOIN usage_facts u \
               ON u.request_id = r.id AND u.request_started_at = r.started_at \
             WHERE r.id = $1 ORDER BY r.started_at DESC LIMIT 1",
        )
        .bind(id)
        .fetch_optional(self.pool())
        .await?
        .ok_or(OperationsError::NotFound)?;
        let request = request_from_row(row)?;
        let rows = sqlx::query(
            "SELECT a.id, a.ordinal, a.provider_id, p.name AS provider_name, a.upstream_model, \
                    a.started_at, a.completed_at, a.status_code, a.error_class, a.committed, \
                    a.latency_ms, a.first_byte_ms \
             FROM attempts a JOIN providers p ON p.id = a.provider_id \
             WHERE a.request_id = $1 AND a.request_started_at = $2 ORDER BY a.ordinal",
        )
        .bind(request.id)
        .bind(request.started_at)
        .fetch_all(self.pool())
        .await?;
        let attempts = rows
            .into_iter()
            .map(|row| {
                Ok(AttemptRecord {
                    id: row.get("id"),
                    ordinal: checked_u16(row.get::<i16, _>("ordinal"), "attempt ordinal")?,
                    provider_id: row.get("provider_id"),
                    provider_name: row.get("provider_name"),
                    upstream_model: row.get("upstream_model"),
                    started_at: row.get("started_at"),
                    completed_at: row.get("completed_at"),
                    status_code: optional_u16(row.get("status_code"), "attempt status")?,
                    error_class: row.get("error_class"),
                    committed: row.get("committed"),
                    latency_ms: optional_i32_u64(row.get("latency_ms"), "attempt latency")?,
                    first_byte_ms: optional_i32_u64(
                        row.get("first_byte_ms"),
                        "attempt first byte",
                    )?,
                })
            })
            .collect::<Result<Vec<_>, OperationsError>>()?;
        Ok(RequestDetail { request, attempts })
    }

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
                    request_count: checked_u64(row.get("request_count"), "request count")?,
                    input_tokens: row.get("input_tokens"),
                    output_tokens: row.get("output_tokens"),
                    cached_input_tokens: row.get("cached_input_tokens"),
                    media_units: row.get("media_units"),
                    estimated_cost: row.get("estimated_cost"),
                    currency: trimmed_optional(row.get("currency")),
                    unpriced_count: checked_u64(row.get("unpriced_count"), "unpriced count")?,
                    incomplete_count: checked_u64(row.get("incomplete_count"), "incomplete count")?,
                })
            })
            .collect::<Result<Vec<_>, OperationsError>>()?;
        Ok(UsageBreakdownReport {
            items,
            coverage: self.usage_range_coverage(filters).await?,
        })
    }

    async fn usage_range_coverage(
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
        let excluded = checked_u64(
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
        let consumer = self.usage_consumer_status(Utc::now()).await?;
        Ok(UsageCompleteness {
            request_count: checked_u64(row.get("request_count"), "request count")?,
            priced_count: checked_u64(row.get("priced_count"), "priced count")?,
            unpriced_count,
            incomplete_count,
            ingestion_gap_events,
            uncertain_gap_count,
            estimated_cost: row.get("estimated_cost"),
            currency: trimmed_optional(row.get("currency")),
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
        let consumer = self.usage_consumer_status(Utc::now()).await?;
        Ok(UsageSummary {
            request_count: checked_u64(row.get("request_count"), "request count")?,
            input_tokens: row.get("input_tokens"),
            output_tokens: row.get("output_tokens"),
            cached_input_tokens: row.get("cached_input_tokens"),
            media_units: row.get("media_units"),
            estimated_cost: row.get("estimated_cost"),
            currency: trimmed_optional(row.get("currency")),
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

    async fn usage_gap_evidence(
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

    pub async fn audit_events(
        &self,
        cursor: Option<&TimestampCursor>,
        limit: u16,
    ) -> Result<Page<AuditRecord>, OperationsError> {
        let page_size = limit.clamp(1, MAX_PAGE_SIZE);
        let mut query = QueryBuilder::<Postgres>::new(
            "SELECT a.id, a.actor_user_id, u.email AS actor_email, a.action, a.resource_type, \
                    a.resource_id, a.outcome, a.source_ip::text AS source_ip, \
                    a.user_agent_family, a.occurred_at \
             FROM audit_events a LEFT JOIN users u ON u.id = a.actor_user_id WHERE true",
        );
        if let Some(cursor) = cursor {
            query.push(" AND (a.occurred_at, a.id) < (");
            query.push_bind(cursor.at);
            query.push(", ");
            query.push_bind(cursor.id);
            query.push(")");
        }
        query.push(" ORDER BY a.occurred_at DESC, a.id DESC LIMIT ");
        query.push_bind(i64::from(page_size) + 1);
        let rows = query.build().fetch_all(self.pool()).await?;
        let items = rows
            .into_iter()
            .map(|row| AuditRecord {
                id: row.get("id"),
                actor_user_id: row.get("actor_user_id"),
                actor_email: row.get("actor_email"),
                action: row.get("action"),
                resource_type: row.get("resource_type"),
                resource_id: row.get("resource_id"),
                outcome: row.get("outcome"),
                source_ip: row.get("source_ip"),
                user_agent_family: row.get("user_agent_family"),
                occurred_at: row.get("occurred_at"),
            })
            .collect::<Vec<_>>();
        let (items, next_cursor) = split_page(items, usize::from(page_size), |item| {
            TimestampCursor {
                at: item.occurred_at,
                id: item.id,
            }
            .encode()
        });
        Ok(Page { items, next_cursor })
    }

    pub async fn runtime_generations(
        &self,
        before_sequence: Option<u64>,
        limit: u16,
    ) -> Result<Page<RuntimeGenerationRecord>, OperationsError> {
        let page_size = limit.clamp(1, MAX_PAGE_SIZE);
        let before = before_sequence
            .map(i64::try_from)
            .transpose()
            .map_err(|_| OperationsError::InvalidCursor)?;
        let rows = sqlx::query(
            "SELECT g.id, g.sequence, encode(g.release_sha256, 'hex') AS sha256_hex, \
                    g.created_by, u.email AS created_by_email, g.created_at \
             FROM runtime_generations g JOIN users u ON u.id = g.created_by \
             WHERE ($1::bigint IS NULL OR g.sequence < $1) \
             ORDER BY g.sequence DESC LIMIT $2",
        )
        .bind(before)
        .bind(i64::from(page_size) + 1)
        .fetch_all(self.pool())
        .await?;
        let items = rows
            .into_iter()
            .map(|row| {
                Ok(RuntimeGenerationRecord {
                    id: row.get("id"),
                    sequence: checked_u64(row.get("sequence"), "generation sequence")?,
                    sha256_hex: row.get("sha256_hex"),
                    created_by: row.get("created_by"),
                    created_by_email: row.get("created_by_email"),
                    created_at: row.get("created_at"),
                })
            })
            .collect::<Result<Vec<_>, OperationsError>>()?;
        let (items, next_cursor) = split_page(items, usize::from(page_size), |item| {
            item.sequence.to_string()
        });
        Ok(Page { items, next_cursor })
    }

    pub async fn provider_health(
        &self,
        window_minutes: u16,
        cursor: Option<Uuid>,
        limit: u16,
    ) -> Result<Page<ProviderHealthRecord>, OperationsError> {
        let window_minutes = window_minutes.clamp(1, 1_440);
        let page_size = limit.clamp(1, MAX_PAGE_SIZE);
        let rows = sqlx::query(
            "SELECT p.id AS provider_id, p.name AS provider_name, p.kind AS provider_kind,
                    p.state::text AS provider_state, p.last_probe_at, p.last_probe_status,
                    p.last_probe_detail, max(a.started_at) AS last_attempt_at,
                    count(a.id) AS attempt_count,
                    count(a.id) FILTER (WHERE a.error_class IS NULL
                        AND (a.status_code IS NULL OR a.status_code < 400)) AS success_count,
                    count(a.id) FILTER (WHERE a.status_code = 429
                        OR a.error_class = 'rate_limit') AS rate_limit_count,
                    count(a.id) FILTER (WHERE a.status_code >= 500
                        OR a.error_class = 'upstream_server') AS server_error_count,
                    count(a.id) FILTER (WHERE a.error_class IN
                        ('connect', 'timeout', 'transport', 'cancelled', 'ambiguous'))
                        AS transport_error_count,
                    avg(a.latency_ms)::float8 AS average_latency_ms
             FROM providers p
             LEFT JOIN attempts a ON a.provider_id = p.id
                AND a.started_at >= now() - make_interval(mins => $1)
             WHERE ($2::uuid IS NULL OR p.id > $2)
             GROUP BY p.id, p.name, p.kind, p.state, p.last_probe_at,
                      p.last_probe_status, p.last_probe_detail
             ORDER BY p.id LIMIT $3",
        )
        .bind(i32::from(window_minutes))
        .bind(cursor)
        .bind(i64::from(page_size) + 1)
        .fetch_all(self.pool())
        .await?;
        let items = rows
            .into_iter()
            .map(|row| {
                let provider_state: ProviderState = row
                    .get::<String, _>("provider_state")
                    .parse()
                    .map_err(|_| {
                        OperationsError::Invalid("stored provider state is invalid".to_owned())
                    })?;
                let last_probe_at: Option<DateTime<Utc>> = row.get("last_probe_at");
                let last_probe_status: Option<String> = row.get("last_probe_status");
                let last_attempt_at: Option<DateTime<Utc>> = row.get("last_attempt_at");
                let attempt_count = checked_u64(row.get("attempt_count"), "attempt count")?;
                let success_count = checked_u64(row.get("success_count"), "success count")?;
                let status = provider_health_status(
                    provider_state,
                    last_probe_at,
                    last_probe_status.as_deref(),
                    last_attempt_at,
                    attempt_count,
                    success_count,
                );
                Ok(ProviderHealthRecord {
                    provider_id: row.get("provider_id"),
                    provider_name: row.get("provider_name"),
                    provider_kind: row.get::<String, _>("provider_kind").parse().map_err(|_| {
                        OperationsError::Invalid("stored provider kind is invalid".to_owned())
                    })?,
                    provider_state,
                    status: status.to_owned(),
                    last_probe_at,
                    last_probe_status,
                    last_probe_detail: row.get("last_probe_detail"),
                    last_attempt_at,
                    attempt_count,
                    success_count,
                    rate_limit_count: checked_u64(row.get("rate_limit_count"), "rate-limit count")?,
                    server_error_count: checked_u64(
                        row.get("server_error_count"),
                        "server-error count",
                    )?,
                    transport_error_count: checked_u64(
                        row.get("transport_error_count"),
                        "transport-error count",
                    )?,
                    average_latency_ms: row.get("average_latency_ms"),
                })
            })
            .collect::<Result<Vec<_>, OperationsError>>()?;
        let (items, next_cursor) = split_page(items, usize::from(page_size), |item| {
            item.provider_id.to_string()
        });
        Ok(Page { items, next_cursor })
    }

    pub async fn settings(&self) -> Result<Vec<SettingRecord>, OperationsError> {
        let rows = sqlx::query(
            "SELECT key, value, etag, updated_by, updated_at FROM settings ORDER BY key",
        )
        .fetch_all(self.pool())
        .await?;
        Ok(rows
            .into_iter()
            .map(|row| SettingRecord {
                key: row.get("key"),
                value: row.get("value"),
                etag: row.get("etag"),
                updated_by: row.get("updated_by"),
                updated_at: row.get("updated_at"),
            })
            .collect())
    }

    pub async fn update_setting(
        &self,
        key: &str,
        value: &str,
        expected_etag: Uuid,
        actor: Uuid,
    ) -> Result<SettingRecord, OperationsError> {
        if key.trim().is_empty() || key.len() > 100 || value.len() > 4_096 {
            return Err(OperationsError::Invalid(
                "setting key or value exceeds its limit".to_owned(),
            ));
        }
        if matches!(
            key,
            "retention.requests_days" | "retention.usage_days" | "retention.audit_days"
        ) && value
            .parse::<i64>()
            .ok()
            .is_none_or(|days| !(1..=3_650).contains(&days))
        {
            return Err(OperationsError::Invalid(
                "retention days must be an integer between 1 and 3650".to_owned(),
            ));
        }
        let mut transaction = self.pool().begin().await?;
        let etag = Uuid::now_v7();
        let now = Utc::now();
        let row = sqlx::query(
            "UPDATE settings SET value = $1, etag = $2, updated_by = $3, updated_at = $4 \
             WHERE key = $5 AND etag = $6 \
             RETURNING key, value, etag, updated_by, updated_at",
        )
        .bind(value)
        .bind(etag)
        .bind(actor)
        .bind(now)
        .bind(key)
        .bind(expected_etag)
        .fetch_optional(&mut *transaction)
        .await?;
        let Some(row) = row else {
            let exists: bool =
                sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM settings WHERE key = $1)")
                    .bind(key)
                    .fetch_one(&mut *transaction)
                    .await?;
            return Err(if exists {
                OperationsError::PreconditionFailed
            } else {
                OperationsError::NotFound
            });
        };
        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'setting.update', 'setting', $3, 'success', $4)",
        )
        .bind(Uuid::now_v7())
        .bind(actor)
        .bind(key)
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        Ok(SettingRecord {
            key: row.get("key"),
            value: row.get("value"),
            etag: row.get("etag"),
            updated_by: row.get("updated_by"),
            updated_at: row.get("updated_at"),
        })
    }

    pub async fn create_pricing_revision<F>(
        &self,
        actor: Uuid,
        idempotency_key: &str,
        effective_at: DateTime<Utc>,
        prices: &[PriceInput],
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<PricingRevisionRecord>, OperationsError>
    where
        F: FnOnce(&PricingRevisionRecord) -> Result<IdempotencyResponse, PersistenceError>,
    {
        let mut transaction = self.pool().begin().await?;
        match claim_replayable_idempotency(
            &mut transaction,
            actor,
            "pricing_revision.create",
            idempotency_key,
            replay.request_fingerprint(),
            replay.master_key(),
        )
        .await?
        {
            ReplayableIdempotencyClaim::Execute => {}
            ReplayableIdempotencyClaim::Replay(response) => {
                transaction.rollback().await?;
                return Ok(IdempotencyOutcome::Replayed(response));
            }
            ReplayableIdempotencyClaim::Conflict => {
                transaction.rollback().await?;
                return Err(OperationsError::IdempotencyConflict);
            }
            ReplayableIdempotencyClaim::InProgress => {
                transaction.rollback().await?;
                return Err(OperationsError::IdempotencyInProgress);
            }
        }
        validate_prices(prices)?;
        sqlx::query("SELECT pg_advisory_xact_lock($1)")
            .bind(PRICING_LOCK_ID)
            .execute(&mut *transaction)
            .await?;
        let requested_currency = prices
            .first()
            .expect("validated pricing revision is nonempty")
            .currency
            .trim()
            .to_uppercase();
        let configured_currency: Option<String> =
            sqlx::query_scalar("SELECT currency::text FROM pricing_currency WHERE singleton")
                .fetch_optional(&mut *transaction)
                .await?;
        if configured_currency
            .as_deref()
            .is_some_and(|currency| currency.trim() != requested_currency)
        {
            return Err(OperationsError::Invalid(format!(
                "pricing currency must match the installation currency {}",
                configured_currency
                    .as_deref()
                    .map(str::trim)
                    .unwrap_or_default()
            )));
        }
        let revision: i32 =
            sqlx::query_scalar("SELECT COALESCE(MAX(revision), 0) + 1 FROM pricing_revisions")
                .fetch_one(&mut *transaction)
                .await?;
        let id = Uuid::now_v7();
        let now = Utc::now();
        sqlx::query(
            "INSERT INTO pricing_revisions (id, revision, effective_at, created_by, created_at) \
             VALUES ($1, $2, $3, $4, $5)",
        )
        .bind(id)
        .bind(revision)
        .bind(effective_at)
        .bind(actor)
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        for price in prices {
            if let Some(provider_id) = price.provider_id {
                let provider_kind: Option<String> =
                    sqlx::query_scalar("SELECT kind FROM providers WHERE id = $1")
                        .bind(provider_id)
                        .fetch_optional(&mut *transaction)
                        .await?;
                if provider_kind.as_deref() != Some(price.provider_kind.as_str()) {
                    return Err(OperationsError::Invalid(
                        "a pricing override must reference a provider of the declared kind"
                            .to_owned(),
                    ));
                }
            }
            sqlx::query(
                "INSERT INTO prices \
                 (pricing_revision_id, provider_kind, provider_id, model, operation, \
                  input_per_million, output_per_million, unit_price, currency) \
                 VALUES ($1, $2, $3, $4, $5, $6::numeric, $7::numeric, $8::numeric, $9)",
            )
            .bind(id)
            .bind(price.provider_kind.as_str())
            .bind(price.provider_id)
            .bind(price.model.trim())
            .bind(price.operation.as_str())
            .bind(price.input_per_million.as_deref())
            .bind(price.output_per_million.as_deref())
            .bind(price.unit_price.as_deref())
            .bind(price.currency.trim().to_uppercase())
            .execute(&mut *transaction)
            .await?;
        }
        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'pricing_revision.create', 'pricing_revision', $3, 'success', $4)",
        )
        .bind(Uuid::now_v7())
        .bind(actor)
        .bind(id.to_string())
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        let record = PricingRevisionRecord {
            id,
            revision: u32::try_from(revision)
                .map_err(|_| OperationsError::Invalid("revision overflow".to_owned()))?,
            effective_at,
            created_by: actor,
            created_at: now,
            prices: prices.to_vec(),
        };
        let response = build_response(&record)?;
        complete_replayable_idempotency(
            &mut transaction,
            actor,
            "pricing_revision.create",
            idempotency_key,
            replay.request_fingerprint(),
            replay.master_key(),
            &response,
        )
        .await?;
        transaction.commit().await?;
        Ok(IdempotencyOutcome::Executed {
            value: record,
            response,
        })
    }

    pub async fn pricing_revisions_page(
        &self,
        before_revision: Option<u32>,
        limit: u16,
    ) -> Result<Page<PricingRevisionRecord>, OperationsError> {
        let before_revision = before_revision
            .map(i32::try_from)
            .transpose()
            .map_err(|_| OperationsError::InvalidCursor)?;
        let page_size = limit.clamp(1, MAX_PAGE_SIZE);
        let rows = sqlx::query(
            "SELECT r.id, r.revision, r.effective_at, r.created_by, r.created_at, \
                    p.provider_kind, p.provider_id, p.model, p.operation, \
                    p.input_per_million::text AS input_per_million, \
                    p.output_per_million::text AS output_per_million, p.unit_price::text AS unit_price, \
                    p.currency::text AS currency \
             FROM pricing_revisions r LEFT JOIN prices p ON p.pricing_revision_id = r.id \
             WHERE r.id IN (SELECT id FROM pricing_revisions \
                            WHERE ($1::int IS NULL OR revision < $1) \
                            ORDER BY revision DESC LIMIT $2) \
             ORDER BY r.revision DESC, p.provider_kind, p.provider_id NULLS FIRST, \
                      p.model, p.operation",
        )
        .bind(before_revision)
        .bind(i64::from(page_size) + 1)
        .fetch_all(self.pool())
        .await?;
        let mut revisions = Vec::<PricingRevisionRecord>::new();
        for row in rows {
            let id: Uuid = row.get("id");
            if revisions.last().is_none_or(|revision| revision.id != id) {
                revisions.push(PricingRevisionRecord {
                    id,
                    revision: u32::try_from(row.get::<i32, _>("revision")).map_err(|_| {
                        OperationsError::Invalid("stored pricing revision is invalid".to_owned())
                    })?,
                    effective_at: row.get("effective_at"),
                    created_by: row.get("created_by"),
                    created_at: row.get("created_at"),
                    prices: Vec::new(),
                });
            }
            let provider_kind: Option<String> = row.get("provider_kind");
            if let Some(provider_kind) = provider_kind {
                revisions
                    .last_mut()
                    .expect("revision was inserted above")
                    .prices
                    .push(PriceInput {
                        provider_kind: provider_kind.parse().map_err(|_| {
                            OperationsError::Invalid(
                                "stored pricing provider kind is invalid".to_owned(),
                            )
                        })?,
                        provider_id: row.get("provider_id"),
                        model: row.get("model"),
                        operation: row.get::<String, _>("operation").parse().map_err(|_| {
                            OperationsError::Invalid(
                                "stored pricing operation is invalid".to_owned(),
                            )
                        })?,
                        input_per_million: row.get("input_per_million"),
                        output_per_million: row.get("output_per_million"),
                        unit_price: row.get("unit_price"),
                        currency: row.get::<String, _>("currency").trim().to_owned(),
                    });
            }
        }
        let (revisions, next_cursor) = split_page(revisions, usize::from(page_size), |revision| {
            revision.revision.to_string()
        });
        Ok(Page {
            items: revisions,
            next_cursor,
        })
    }
}

fn push_request_filters(query: &mut QueryBuilder<Postgres>, filters: &RequestFilters) {
    if let Some(value) = &filters.route_slug {
        query.push(" AND r.route_slug = ").push_bind(value);
    }
    if let Some(value) = filters.provider_id {
        query.push(" AND u.provider_id = ").push_bind(value);
    }
    if let Some(value) = &filters.upstream_model {
        query.push(" AND u.upstream_model = ").push_bind(value);
    }
    if let Some(value) = filters.api_key_id {
        query.push(" AND r.api_key_id = ").push_bind(value);
    }
    if let Some(value) = filters.operation {
        query.push(" AND r.operation = ").push_bind(value.as_str());
    }
    if let Some(value) = filters.status_code {
        query
            .push(" AND r.status_code = ")
            .push_bind(i32::from(value));
    }
    if let Some(value) = &filters.error_class {
        query.push(" AND r.error_class = ").push_bind(value);
    }
    if let Some(value) = filters.started_after {
        query.push(" AND r.started_at >= ").push_bind(value);
    }
    if let Some(value) = filters.started_before {
        query.push(" AND r.started_at < ").push_bind(value);
    }
}

fn push_usage_rows_cte(query: &mut QueryBuilder<Postgres>, filters: &UsageFilters) {
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

fn floor_usage_hour(value: DateTime<Utc>) -> DateTime<Utc> {
    let seconds = value.timestamp().div_euclid(60 * 60) * 60 * 60;
    DateTime::from_timestamp(seconds, 0).expect("a truncated valid timestamp remains valid")
}

fn ceil_usage_hour(value: DateTime<Utc>) -> DateTime<Utc> {
    let floor = floor_usage_hour(value);
    if floor == value {
        floor
    } else {
        floor + chrono::Duration::hours(1)
    }
}

fn request_from_row(row: sqlx::postgres::PgRow) -> Result<RequestRecord, OperationsError> {
    Ok(RequestRecord {
        id: row.get("id"),
        runtime_generation_id: row.get("runtime_generation_id"),
        api_key_id: row.get("api_key_id"),
        route_slug: row.get("route_slug"),
        operation: row
            .get::<String, _>("operation")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("request operation"))?,
        surface: row
            .get::<String, _>("surface")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("request surface"))?,
        started_at: row.get("started_at"),
        completed_at: row.get("completed_at"),
        status_code: optional_u16(row.get("status_code"), "request status")?,
        error_class: row.get("error_class"),
        total_latency_ms: optional_i32_u64(row.get("total_latency_ms"), "request latency")?,
        first_byte_ms: optional_i32_u64(row.get("first_byte_ms"), "request first byte")?,
        attempt_count: checked_u16(row.get::<i16, _>("attempt_count"), "attempt count")?,
        input_tokens: optional_u64(row.get("input_tokens"), "input tokens")?,
        output_tokens: optional_u64(row.get("output_tokens"), "output tokens")?,
        cached_input_tokens: optional_u64(row.get("cached_input_tokens"), "cached tokens")?,
        estimated_cost: row.get("estimated_cost"),
        currency: trimmed_optional(row.get("currency")),
        unpriced: row.get("unpriced"),
        usage_complete: row.get("usage_complete"),
    })
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
        currency: trimmed_optional(row.get("currency")),
        unpriced_count: checked_u64(row.get("unpriced_count"), "unpriced count")?,
        incomplete_count: checked_u64(row.get("incomplete_count"), "incomplete count")?,
    })
}

fn trimmed_optional(value: Option<String>) -> Option<String> {
    value.map(|value| value.trim().to_owned())
}

fn validate_usage_range(filters: &UsageFilters) -> Result<(), OperationsError> {
    if filters.observed_before <= filters.observed_after
        || filters.observed_before - filters.observed_after > chrono::Duration::days(366)
    {
        return Err(OperationsError::Invalid(
            "usage range must be positive and no longer than 366 days".to_owned(),
        ));
    }
    Ok(())
}

fn validate_prices(prices: &[PriceInput]) -> Result<(), OperationsError> {
    if prices.is_empty() || prices.len() > 10_000 {
        return Err(OperationsError::Invalid(
            "a pricing revision must contain 1-10000 entries".to_owned(),
        ));
    }
    let mut dimensions = HashSet::with_capacity(prices.len());
    let mut revision_currency: Option<String> = None;
    for price in prices {
        let currency = price.currency.trim();
        if price.model.trim().is_empty()
            || currency.len() != 3
            || !currency.bytes().all(|byte| byte.is_ascii_alphabetic())
            || (price.input_per_million.is_none()
                && price.output_per_million.is_none()
                && price.unit_price.is_none())
        {
            return Err(OperationsError::Invalid(
                "pricing entries require dimensions, ISO currency, and at least one price"
                    .to_owned(),
            ));
        }
        let normalized_currency = currency.to_ascii_uppercase();
        if revision_currency
            .as_ref()
            .is_some_and(|expected| expected != &normalized_currency)
        {
            return Err(OperationsError::Invalid(
                "a pricing revision cannot mix currencies".to_owned(),
            ));
        }
        revision_currency.get_or_insert(normalized_currency);
        if !dimensions.insert((
            price.provider_kind,
            price.provider_id,
            price.model.trim(),
            price.operation,
        )) {
            return Err(OperationsError::Invalid(
                "pricing revision contains duplicate scoped dimensions".to_owned(),
            ));
        }
        for amount in [
            &price.input_per_million,
            &price.output_per_million,
            &price.unit_price,
        ]
        .into_iter()
        .flatten()
        {
            validate_decimal(amount)?;
        }
    }
    Ok(())
}

fn validate_decimal(value: &str) -> Result<(), OperationsError> {
    let value = value.trim();
    let mut parts = value.split('.');
    let integer = parts.next().unwrap_or_default();
    let fraction = parts.next();
    if value.is_empty()
        || value.starts_with('-')
        || parts.next().is_some()
        || integer.is_empty()
        || !integer.bytes().all(|byte| byte.is_ascii_digit())
        || fraction.is_some_and(|part| {
            part.is_empty() || part.len() > 12 || !part.bytes().all(|byte| byte.is_ascii_digit())
        })
        || integer.len() > 12
    {
        return Err(OperationsError::Invalid(
            "prices must be non-negative decimals with at most 12 fractional digits".to_owned(),
        ));
    }
    Ok(())
}

fn checked_u16(value: i16, name: &str) -> Result<u16, OperationsError> {
    u16::try_from(value).map_err(|_| OperationsError::Invalid(format!("stored {name} is invalid")))
}

fn optional_u16(value: Option<i32>, name: &str) -> Result<Option<u16>, OperationsError> {
    value
        .map(u16::try_from)
        .transpose()
        .map_err(|_| OperationsError::Invalid(format!("stored {name} is invalid")))
}

fn checked_u64(value: i64, name: &str) -> Result<u64, OperationsError> {
    u64::try_from(value).map_err(|_| OperationsError::Invalid(format!("stored {name} is invalid")))
}

fn optional_u64(value: Option<i64>, name: &str) -> Result<Option<u64>, OperationsError> {
    value
        .map(u64::try_from)
        .transpose()
        .map_err(|_| OperationsError::Invalid(format!("stored {name} is invalid")))
}

fn optional_i32_u64(value: Option<i32>, name: &str) -> Result<Option<u64>, OperationsError> {
    value
        .map(u64::try_from)
        .transpose()
        .map_err(|_| OperationsError::Invalid(format!("stored {name} is invalid")))
}

fn provider_health_status(
    provider_state: ProviderState,
    last_probe_at: Option<DateTime<Utc>>,
    last_probe_status: Option<&str>,
    last_attempt_at: Option<DateTime<Utc>>,
    attempt_count: u64,
    success_count: u64,
) -> &'static str {
    if provider_state == ProviderState::Disabled {
        return "disabled";
    }
    let failed_probe_is_latest = last_probe_status == Some("failed")
        && match (last_probe_at, last_attempt_at) {
            (Some(probe), Some(attempt)) => probe >= attempt,
            (Some(_), None) => true,
            _ => false,
        };
    if failed_probe_is_latest {
        return "unavailable";
    }
    if attempt_count == 0 {
        return match last_probe_status {
            Some("succeeded") => "healthy",
            Some("failed") => "unavailable",
            _ => "unknown",
        };
    }
    let failures = attempt_count.saturating_sub(success_count);
    if u128::from(failures) * 2 >= u128::from(attempt_count) {
        "unavailable"
    } else if u128::from(failures) * 10 >= u128::from(attempt_count) {
        "degraded"
    } else {
        "healthy"
    }
}

impl fmt::Display for UsageDimension {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::Route => "route",
            Self::Provider => "provider",
            Self::Model => "model",
            Self::ApiKey => "api_key",
            Self::Operation => "operation",
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn timestamp_cursor_round_trips_and_rejects_non_v7_ids() {
        let cursor = TimestampCursor {
            at: Utc::now(),
            id: Uuid::now_v7(),
        };
        assert_eq!(TimestampCursor::parse(&cursor.encode()).unwrap(), cursor);
        let invalid = TimestampCursor {
            at: cursor.at,
            id: Uuid::nil(),
        };
        assert!(matches!(
            TimestampCursor::parse(&invalid.encode()),
            Err(OperationsError::InvalidCursor)
        ));
    }

    #[test]
    fn validates_exact_non_negative_decimal_prices() {
        for valid in ["0", "0.000001", "123456789012.123456789012"] {
            validate_decimal(valid).unwrap();
        }
        for invalid in ["", "-1", ".1", "1.", "1e3", "1.0000000000001"] {
            assert!(validate_decimal(invalid).is_err(), "accepted {invalid}");
        }
    }

    #[test]
    fn rejects_duplicate_pricing_dimensions_within_a_scope() {
        let price = PriceInput {
            provider_kind: ProviderKind::OpenAi,
            provider_id: None,
            model: "model".to_owned(),
            operation: OperationKind::Generation,
            input_per_million: Some("1".to_owned()),
            output_per_million: None,
            unit_price: None,
            currency: "USD".to_owned(),
        };
        assert!(matches!(
            validate_prices(&[price.clone(), price]),
            Err(OperationsError::Invalid(message))
                if message.contains("duplicate scoped dimensions")
        ));
    }

    #[test]
    fn accepts_unit_only_media_pricing() {
        validate_prices(&[PriceInput {
            provider_kind: ProviderKind::OpenAi,
            provider_id: None,
            model: "image-model".to_owned(),
            operation: OperationKind::ImageGeneration,
            input_per_million: None,
            output_per_million: None,
            unit_price: Some("0.04".to_owned()),
            currency: "USD".to_owned(),
        }])
        .unwrap();
    }

    #[test]
    fn rejects_noncanonical_pricing_dimensions() {
        assert!("openai".parse::<ProviderKind>().is_err());
        assert!("chat".parse::<OperationKind>().is_err());
    }

    #[test]
    fn retained_hour_boundaries_are_never_rounded_down() {
        let exact = "2026-07-12T10:00:00Z".parse::<DateTime<Utc>>().unwrap();
        let partial = "2026-07-12T10:15:30Z".parse::<DateTime<Utc>>().unwrap();
        assert_eq!(floor_usage_hour(partial), exact);
        assert_eq!(ceil_usage_hour(exact), exact);
        assert_eq!(
            ceil_usage_hour(partial),
            "2026-07-12T11:00:00Z".parse::<DateTime<Utc>>().unwrap()
        );
    }

    #[test]
    fn provider_health_prioritizes_latest_probe_and_error_ratio() {
        let now = Utc::now();
        assert_eq!(
            provider_health_status(ProviderState::Disabled, None, None, None, 0, 0),
            "disabled"
        );
        assert_eq!(
            provider_health_status(ProviderState::Active, Some(now), Some("failed"), None, 0, 0,),
            "unavailable"
        );
        assert_eq!(
            provider_health_status(ProviderState::Active, None, None, Some(now), 100, 95),
            "healthy"
        );
        assert_eq!(
            provider_health_status(ProviderState::Active, None, None, Some(now), 100, 89),
            "degraded"
        );
        assert_eq!(
            provider_health_status(ProviderState::Active, None, None, Some(now), 10, 5),
            "unavailable"
        );
    }
}

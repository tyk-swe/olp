use chrono::{DateTime, Utc};
use olp_domain::{ProviderKind, ProviderState};
use sqlx::Row;
use uuid::Uuid;

use super::{
    MAX_PAGE_SIZE,
    cursor::{OperationsError, OperationsPage, checked_u64},
};
use crate::{PgStore, split_page};

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

    pub async fn provider_health(
        &self,
        window_minutes: u16,
        cursor: Option<Uuid>,
        limit: u16,
    ) -> Result<OperationsPage<ProviderHealthRecord>, OperationsError> {
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
        Ok(OperationsPage { items, next_cursor })
    }
}

pub(super) fn provider_health_status(
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

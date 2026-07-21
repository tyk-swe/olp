use chrono::{DateTime, Utc};
use olp_domain::{OperationKind, Surface};
use sqlx::{Postgres, QueryBuilder, Row};
use uuid::Uuid;

use super::{
    MAX_PAGE_SIZE,
    cursor::{
        OperationsError, OperationsPage, TimestampCursor, checked_u16, optional_i32_u64,
        optional_u16, optional_u64, trimmed_optional,
    },
};
use crate::{PersistenceError, PgStore, split_page};

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

impl PgStore {
    pub async fn requests(
        &self,
        filters: &RequestFilters,
        cursor: Option<&TimestampCursor>,
        limit: u16,
    ) -> Result<OperationsPage<RequestRecord>, OperationsError> {
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
        Ok(OperationsPage { items, next_cursor })
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

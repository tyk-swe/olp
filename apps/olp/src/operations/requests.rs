use axum::{
    Json,
    extract::{Path, Query, State},
    http::HeaderMap,
};
use chrono::{DateTime, Utc};
use olp_storage::{AttemptRecord, RequestFilters, RequestRecord, TimestampCursor};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};
use uuid::Uuid;

use super::helpers::{map_operations, page_limit, validate_time_range};
use crate::{
    ManagementState, Problem,
    management_api::{Permission, require_permission, require_read_session},
};

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub(super) struct RequestQuery {
    cursor: Option<String>,
    limit: Option<u16>,
    route: Option<String>,
    #[param(value_type = Option<String>, format = Uuid)]
    provider_id: Option<Uuid>,
    model: Option<String>,
    #[param(value_type = Option<String>, format = Uuid)]
    api_key_id: Option<Uuid>,
    operation: Option<String>,
    status_code: Option<u16>,
    error_class: Option<String>,
    started_after: Option<DateTime<Utc>>,
    started_before: Option<DateTime<Utc>>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct RequestSummary {
    #[schema(value_type = String, format = Uuid)]
    id: Uuid,
    #[schema(value_type = String, format = Uuid)]
    runtime_generation_id: Uuid,
    #[schema(value_type = String, format = Uuid)]
    api_key_id: Uuid,
    route: String,
    operation: String,
    surface: String,
    started_at: DateTime<Utc>,
    completed_at: Option<DateTime<Utc>>,
    status_code: Option<u16>,
    error_class: Option<String>,
    total_latency_ms: Option<u64>,
    first_byte_ms: Option<u64>,
    attempt_count: u16,
    input_tokens: Option<u64>,
    output_tokens: Option<u64>,
    cached_input_tokens: Option<u64>,
    estimated_cost: Option<String>,
    currency: Option<String>,
    unpriced: Option<bool>,
    usage_complete: Option<bool>,
}

impl From<RequestRecord> for RequestSummary {
    fn from(record: RequestRecord) -> Self {
        Self {
            id: record.id,
            runtime_generation_id: record.runtime_generation_id,
            api_key_id: record.api_key_id,
            route: record.route_slug,
            operation: record.operation.to_string(),
            surface: record.surface.to_string(),
            started_at: record.started_at,
            completed_at: record.completed_at,
            status_code: record.status_code,
            error_class: record.error_class,
            total_latency_ms: record.total_latency_ms,
            first_byte_ms: record.first_byte_ms,
            attempt_count: record.attempt_count,
            input_tokens: record.input_tokens,
            output_tokens: record.output_tokens,
            cached_input_tokens: record.cached_input_tokens,
            estimated_cost: record.estimated_cost,
            currency: record.currency,
            unpriced: record.unpriced,
            usage_complete: record.usage_complete,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct RequestListResponse {
    data: Vec<RequestSummary>,
    next_cursor: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct AttemptResponse {
    #[schema(value_type = String, format = Uuid)]
    id: Uuid,
    ordinal: u16,
    #[schema(value_type = String, format = Uuid)]
    provider_id: Uuid,
    provider_name: String,
    upstream_model: String,
    started_at: DateTime<Utc>,
    completed_at: Option<DateTime<Utc>>,
    status_code: Option<u16>,
    error_class: Option<String>,
    committed: bool,
    latency_ms: Option<u64>,
    first_byte_ms: Option<u64>,
}

impl From<AttemptRecord> for AttemptResponse {
    fn from(record: AttemptRecord) -> Self {
        Self {
            id: record.id,
            ordinal: record.ordinal,
            provider_id: record.provider_id,
            provider_name: record.provider_name,
            upstream_model: record.upstream_model,
            started_at: record.started_at,
            completed_at: record.completed_at,
            status_code: record.status_code,
            error_class: record.error_class,
            committed: record.committed,
            latency_ms: record.latency_ms,
            first_byte_ms: record.first_byte_ms,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct RequestDetailResponse {
    #[serde(flatten)]
    request: RequestSummary,
    attempts: Vec<AttemptResponse>,
}

#[utoipa::path(
    get,
    path = "/api/v1/requests",
    tag = "requests",
    params(RequestQuery),
    responses(
        (status = 200, description = "Metadata-only request page", body = RequestListResponse),
        (status = 400, description = "Invalid cursor or filter", body = Problem),
        (status = 401, description = "Authentication required", body = Problem),
        (status = 403, description = "Insufficient role", body = Problem)
    )
)]
pub(super) async fn list_requests(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<RequestQuery>,
) -> Result<Json<RequestListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let cursor = query
        .cursor
        .as_deref()
        .map(TimestampCursor::parse)
        .transpose()
        .map_err(map_operations)?;
    if let (Some(after), Some(before)) = (query.started_after, query.started_before) {
        validate_time_range("started_after", after, "started_before", before)?;
    }
    let limit = page_limit(query.limit)?;
    let page = state
        .store()
        .requests(
            &RequestFilters {
                route_slug: query.route,
                provider_id: query.provider_id,
                upstream_model: query.model,
                api_key_id: query.api_key_id,
                operation: query
                    .operation
                    .as_deref()
                    .map(str::parse)
                    .transpose()
                    .map_err(|_| {
                        Problem::bad_request(
                            "invalid_operation",
                            "The operation filter is invalid.",
                        )
                    })?,
                status_code: query.status_code,
                error_class: query.error_class,
                started_after: query.started_after,
                started_before: query.started_before,
            },
            cursor.as_ref(),
            limit,
        )
        .await
        .map_err(map_operations)?;
    Ok(Json(RequestListResponse {
        data: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor,
    }))
}

#[utoipa::path(
    get,
    path = "/api/v1/requests/{request_id}",
    tag = "requests",
    params(("request_id" = Uuid, Path, description = "UUIDv7 request ID")),
    responses(
        (status = 200, description = "Metadata timeline", body = RequestDetailResponse),
        (status = 404, description = "Request not found", body = Problem)
    )
)]
pub(super) async fn get_request(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Path(request_id): Path<Uuid>,
) -> Result<Json<RequestDetailResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let detail = state
        .store()
        .request_detail(request_id)
        .await
        .map_err(map_operations)?;
    Ok(Json(RequestDetailResponse {
        request: detail.request.into(),
        attempts: detail.attempts.into_iter().map(Into::into).collect(),
    }))
}

use crate::access::policy::Permission;
use crate::access::principal::ReadPrincipal;
use crate::usage::history::AttemptRecord;
use crate::usage::history::RequestFilters;
use crate::usage::history::RequestRecord;
use axum::Json;
use axum::extract::Path;
use axum::extract::Query;
use axum::extract::State;
use chrono::DateTime;
use chrono::Utc;
use serde::Deserialize;
use serde::Serialize;
use utoipa::IntoParams;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::access::permissions::require_permission;
use crate::http::control::operations::helpers::map_operations;
use crate::http::control::operations::helpers::timestamp_cursor;
use crate::http::control::operations::helpers::validate_time_range;
use crate::http::control::pagination::page_limit;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub(crate) struct RequestQuery {
    /// Opaque cursor returned by the previous page.
    cursor: Option<String>,
    /// Page size, from 1 to 200. Defaults to 50.
    #[param(minimum = 1, maximum = 200)]
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

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct RequestSummary {
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
pub(crate) struct RequestListResponse {
    data: Vec<RequestSummary>,
    items: Vec<RequestSummary>,
    next_cursor: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct AttemptResponse {
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
    charge_status: Option<String>,
    usage_observed: Option<bool>,
    usage_complete: Option<bool>,
    input_tokens: Option<u64>,
    output_tokens: Option<u64>,
    cached_input_tokens: Option<u64>,
    media_units: Option<String>,
    estimated_cost: Option<String>,
    currency: Option<String>,
    unpriced: Option<bool>,
    #[schema(value_type = Option<String>, format = Uuid)]
    pricing_revision_id: Option<Uuid>,
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
            charge_status: record.charge_status,
            usage_observed: record.usage_observed,
            usage_complete: record.usage_complete,
            input_tokens: record.input_tokens,
            output_tokens: record.output_tokens,
            cached_input_tokens: record.cached_input_tokens,
            media_units: record.media_units,
            estimated_cost: record.estimated_cost,
            currency: record.currency,
            unpriced: record.unpriced,
            pricing_revision_id: record.pricing_revision_id,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct RequestDetailResponse {
    #[serde(flatten)]
    request: RequestSummary,
    attempts: Vec<AttemptResponse>,
}

#[utoipa::path(
    get,
    path = "/api/v3/requests",
    tag = "requests",
    params(RequestQuery),
    responses(
        (status = 200, description = "Metadata-only request page", body = RequestListResponse),
        (status = 400, description = "Invalid cursor or filter", body = Problem, content_type = "application/problem+json"),
        (status = 401, description = "Authentication required", body = Problem, content_type = "application/problem+json"),
        (status = 403, description = "Insufficient role", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn list_requests(
    State(state): State<ManagementState>,
    Query(query): Query<RequestQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<RequestListResponse>, Problem> {
    require_permission(&principal, Permission::ReadOperations)?;
    let cursor = timestamp_cursor(query.cursor.as_deref())?;
    if let (Some(after), Some(before)) = (query.started_after, query.started_before) {
        validate_time_range("started_after", after, "started_before", before)?;
    }
    let limit = page_limit(query.limit)?;
    let page = crate::usage::history::requests(
        &state.request_boundary.pool,
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
                    Problem::bad_request("invalid_operation", "The operation filter is invalid.")
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
    let items = page.items.into_iter().map(Into::into).collect::<Vec<_>>();
    Ok(Json(RequestListResponse {
        data: items.clone(),
        items,
        next_cursor: page.next_cursor,
    }))
}

#[utoipa::path(
    get,
    path = "/api/v3/requests/{request_id}",
    tag = "requests",
    params(("request_id" = Uuid, Path, description = "UUIDv7 request ID")),
    responses(
        (status = 200, description = "Metadata timeline", body = RequestDetailResponse),
        (status = 404, description = "Request not found", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn get_request(
    State(state): State<ManagementState>,
    Path(request_id): Path<Uuid>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<RequestDetailResponse>, Problem> {
    require_permission(&principal, Permission::ReadOperations)?;
    let detail = crate::usage::history::request_detail(&state.request_boundary.pool, request_id)
        .await
        .map_err(map_operations)?;
    Ok(Json(RequestDetailResponse {
        request: detail.request.into(),
        attempts: detail.attempts.into_iter().map(Into::into).collect(),
    }))
}

pub(crate) fn router()
-> utoipa_axum::router::OpenApiRouter<crate::http::control::state::ManagementState> {
    utoipa_axum::router::OpenApiRouter::new()
        .routes(utoipa_axum::routes!(
            crate::usage::history_http::get_request
        ))
        .routes(utoipa_axum::routes!(
            crate::usage::history_http::list_requests
        ))
}

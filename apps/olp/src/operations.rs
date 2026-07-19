use axum::{
    Json, Router,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
    routing::get,
};
use chrono::{DateTime, Utc};
use olp_domain::{OperationKind, ProviderKind, Surface};
use olp_storage::{
    AttemptRecord, AuditRecord, IdempotencyResponse, MediaJobError, MediaJobFilters,
    MediaJobLifecycle, MediaJobRecord, MediaJobState, OperationsError, PriceInput,
    PricingRevisionRecord, ProviderHealthRecord, ReplayableIdempotency, RequestFilters,
    RequestRecord, RuntimeGenerationRecord, SettingRecord, TimestampCursor, UsageBreakdown,
    UsageCompleteness, UsageConsumerStatus, UsageDimension, UsageEpochAcknowledgement,
    UsageFilters, UsageGatewayEpochRecord, UsageGatewayEpochState, UsageGranularity, UsagePoint,
    UsageRangeCoverage, UsageSummary, idempotency_fingerprint,
};
use serde::{Deserialize, Serialize};
use tracing::error;
use utoipa::{IntoParams, OpenApi, ToSchema};
use uuid::Uuid;

use crate::{
    ApiState, FieldErrors, HealthResponse, Problem,
    management::{
        Permission, idempotency_http_response, json_payload, map_persistence,
        require_idempotency_key, require_mutation_session, require_permission,
        require_read_session, require_store,
    },
};

pub(crate) fn router() -> Router<ApiState> {
    Router::new()
        .route("/api/v1/requests", get(list_requests))
        .route("/api/v1/requests/{request_id}", get(get_request))
        .route("/api/v1/media-jobs", get(list_media_jobs))
        .route("/api/v1/media-jobs/{job_id}", get(get_media_job))
        .route("/api/v1/usage/time-series", get(usage_time_series))
        .route("/api/v1/usage/summary", get(usage_summary))
        .route("/api/v1/usage/breakdown", get(usage_breakdown))
        .route("/api/v1/usage/completeness", get(usage_completeness))
        .route(
            "/api/v1/usage/gateway-epochs",
            get(list_usage_gateway_epochs),
        )
        .route(
            "/api/v1/usage/gateway-epochs/{process_epoch}/acknowledge",
            axum::routing::post(acknowledge_usage_gateway_epoch),
        )
        .route("/api/v1/audit", get(list_audit_events))
        .route("/api/v1/health/ready", get(management_readiness))
        .route("/api/v1/provider-health", get(provider_health))
        .route("/api/v1/runtime-generations", get(list_runtime_generations))
        .route("/api/v1/settings", get(list_settings))
        .route(
            "/api/v1/settings/{key}",
            get(get_setting).put(update_setting),
        )
        .route(
            "/api/v1/pricing/revisions",
            get(list_pricing_revisions).post(create_pricing_revision),
        )
}

#[derive(OpenApi)]
#[openapi(
    paths(
        list_requests,
        get_request,
        list_media_jobs,
        get_media_job,
        usage_time_series,
        usage_summary,
        usage_breakdown,
        usage_completeness,
        list_usage_gateway_epochs,
        acknowledge_usage_gateway_epoch,
        list_audit_events,
        management_readiness,
        provider_health,
        list_runtime_generations,
        list_settings,
        get_setting,
        update_setting,
        list_pricing_revisions,
        create_pricing_revision
    ),
    components(schemas(
        RequestSummary,
        RequestListResponse,
        AttemptResponse,
        RequestDetailResponse,
        MediaJobItem,
        MediaJobListResponse,
        UsagePointResponse,
        UsageRangeCoverageResponse,
        UsageConsumerStatusResponse,
        UsageTimeSeriesResponse,
        UsageSummaryResponse,
        UsageBreakdownItem,
        UsageBreakdownResponse,
        UsageCompletenessResponse,
        UsageGatewayEpochListResponse,
        UsageGatewayEpochResponse,
        UsageEpochAcknowledgementResponse,
        AuditEventResponse,
        AuditListResponse,
        HealthResponse,
        ProviderHealthItem,
        ProviderHealthResponse,
        RuntimeGenerationItem,
        RuntimeGenerationListResponse,
        SettingResponse,
        SettingsResponse,
        UpdateSettingRequest,
        PriceProviderKind,
        PriceOperation,
        PriceRequest,
        PricingRevisionRequest,
        PriceResponse,
        PricingRevisionResponse,
        PricingRevisionsResponse,
        Problem
    )),
    tags(
        (name = "requests"),
        (name = "media-jobs"),
        (name = "usage"),
        (name = "audit"),
        (name = "health"),
        (name = "runtime"),
        (name = "settings"),
        (name = "pricing")
    )
)]
pub(crate) struct OperationsApiDoc;

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
struct RequestQuery {
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
struct RequestSummary {
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
struct RequestListResponse {
    data: Vec<RequestSummary>,
    next_cursor: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
struct AttemptResponse {
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
struct RequestDetailResponse {
    #[serde(flatten)]
    request: RequestSummary,
    attempts: Vec<AttemptResponse>,
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
struct MediaJobQuery {
    cursor: Option<String>,
    limit: Option<u16>,
    #[param(value_type = Option<String>, format = Uuid)]
    api_key_id: Option<Uuid>,
    #[param(value_type = Option<String>, format = Uuid)]
    provider_id: Option<Uuid>,
    route: Option<String>,
    state: Option<String>,
    lifecycle: Option<String>,
    created_after: Option<DateTime<Utc>>,
    created_before: Option<DateTime<Utc>>,
}

#[derive(Debug, Serialize, ToSchema)]
struct MediaJobItem {
    #[schema(value_type = String, format = Uuid)]
    id: Uuid,
    upstream_job_id: Option<String>,
    #[schema(value_type = String, format = Uuid)]
    api_key_id: Uuid,
    #[schema(value_type = String, format = Uuid)]
    provider_id: Uuid,
    provider_name: String,
    provider_model: String,
    route: String,
    operation: String,
    surface: String,
    state: String,
    lifecycle: String,
    progress_percent: Option<f32>,
    content_available: bool,
    expires_at: Option<DateTime<Utc>>,
    error_class: Option<String>,
    completed_at: Option<DateTime<Utc>>,
    last_polled_at: Option<DateTime<Utc>>,
    reconciliation_error: Option<String>,
    deleted_at: Option<DateTime<Utc>>,
    etag: String,
    created_at: DateTime<Utc>,
    updated_at: DateTime<Utc>,
}

impl From<MediaJobRecord> for MediaJobItem {
    fn from(record: MediaJobRecord) -> Self {
        Self {
            id: record.id,
            upstream_job_id: record.upstream_job_id,
            api_key_id: record.api_key_id,
            provider_id: record.provider_id,
            provider_name: record.provider_name,
            provider_model: record.provider_model,
            route: record.route_slug,
            operation: record.operation.to_string(),
            surface: media_job_surface_wire_value(record.surface).to_owned(),
            state: record.state.as_str().to_owned(),
            lifecycle: record.lifecycle.as_str().to_owned(),
            progress_percent: record.progress_percent,
            content_available: record.content_available,
            expires_at: record.expires_at,
            error_class: record.error_class,
            completed_at: record.completed_at,
            last_polled_at: record.last_polled_at,
            reconciliation_error: record.reconciliation_error,
            deleted_at: record.deleted_at,
            etag: format!("\"{}\"", record.etag),
            created_at: record.created_at,
            updated_at: record.updated_at,
        }
    }
}

const fn media_job_surface_wire_value(surface: Surface) -> &'static str {
    match surface {
        Surface::OpenAi => "openai",
        Surface::Anthropic => "anthropic",
        Surface::Gemini => "gemini",
    }
}

#[derive(Debug, Serialize, ToSchema)]
struct MediaJobListResponse {
    data: Vec<MediaJobItem>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/media-jobs",
    tag = "media-jobs",
    params(MediaJobQuery),
    responses(
        (status = 200, description = "Metadata-only asynchronous media job page", body = MediaJobListResponse),
        (status = 400, description = "Invalid cursor or filter", body = Problem),
        (status = 401, description = "Authentication required", body = Problem),
        (status = 403, description = "Insufficient role", body = Problem)
    )
)]
async fn list_media_jobs(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<MediaJobQuery>,
) -> Result<Json<MediaJobListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let cursor = query
        .cursor
        .as_deref()
        .map(TimestampCursor::parse)
        .transpose()
        .map_err(map_operations)?;
    let state_filter = query
        .state
        .as_deref()
        .map(parse_media_job_state)
        .transpose()?;
    let lifecycle_filter = query
        .lifecycle
        .as_deref()
        .map(parse_media_job_lifecycle)
        .transpose()?;
    if let (Some(after), Some(before)) = (query.created_after, query.created_before) {
        validate_time_range("created_after", after, "created_before", before)?;
    }
    let limit = page_limit(query.limit)?;
    let page = require_store(&state)?
        .media_jobs(
            &MediaJobFilters {
                api_key_id: query.api_key_id,
                provider_id: query.provider_id,
                route_slug: query.route,
                route_slugs: Vec::new(),
                operation: None,
                surface: None,
                state: state_filter,
                lifecycle: lifecycle_filter,
                created_after: query.created_after,
                created_before: query.created_before,
            },
            cursor.as_ref(),
            limit,
        )
        .await
        .map_err(map_media_job)?;
    Ok(Json(MediaJobListResponse {
        data: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor,
    }))
}

fn parse_media_job_lifecycle(value: &str) -> Result<MediaJobLifecycle, Problem> {
    match value {
        "creating" => Ok(MediaJobLifecycle::Creating),
        "active" => Ok(MediaJobLifecycle::Active),
        "create_ambiguous" => Ok(MediaJobLifecycle::CreateAmbiguous),
        "create_cleanup_pending" => Ok(MediaJobLifecycle::CreateCleanupPending),
        "delete_pending" => Ok(MediaJobLifecycle::DeletePending),
        "deleted" => Ok(MediaJobLifecycle::Deleted),
        _ => {
            let mut fields = FieldErrors::new();
            fields.insert(
                "lifecycle".to_owned(),
                vec!["Unknown media-job reconciliation lifecycle.".to_owned()],
            );
            Err(Problem::validation(fields))
        }
    }
}

#[utoipa::path(
    get,
    path = "/api/v1/media-jobs/{job_id}",
    tag = "media-jobs",
    params(("job_id" = Uuid, Path, description = "UUIDv7 OLP media job ID")),
    responses(
        (status = 200, description = "Metadata-only asynchronous media job", body = MediaJobItem),
        (status = 404, description = "Media job not found", body = Problem)
    )
)]
async fn get_media_job(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Path(job_id): Path<Uuid>,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let record = require_store(&state)?
        .media_job(job_id)
        .await
        .map_err(map_media_job)?;
    let etag =
        HeaderValue::from_str(&format!("\"{}\"", record.etag)).map_err(|_| Problem::internal())?;
    let mut response = Json(MediaJobItem::from(record)).into_response();
    response.headers_mut().insert(header::ETAG, etag);
    Ok(response)
}

fn parse_media_job_state(value: &str) -> Result<MediaJobState, Problem> {
    match value {
        "queued" => Ok(MediaJobState::Queued),
        "running" => Ok(MediaJobState::Running),
        "succeeded" => Ok(MediaJobState::Succeeded),
        "failed" => Ok(MediaJobState::Failed),
        "cancelled" => Ok(MediaJobState::Cancelled),
        _ => {
            let mut fields = FieldErrors::new();
            fields.insert(
                "state".to_owned(),
                vec!["State must be queued, running, succeeded, failed, or cancelled.".to_owned()],
            );
            Err(Problem::validation(fields))
        }
    }
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
async fn list_requests(
    State(state): State<ApiState>,
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
    let page = require_store(&state)?
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
async fn get_request(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Path(request_id): Path<Uuid>,
) -> Result<Json<RequestDetailResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let detail = require_store(&state)?
        .request_detail(request_id)
        .await
        .map_err(map_operations)?;
    Ok(Json(RequestDetailResponse {
        request: detail.request.into(),
        attempts: detail.attempts.into_iter().map(Into::into).collect(),
    }))
}

#[derive(Debug, Deserialize, IntoParams, ToSchema)]
#[into_params(parameter_in = Query)]
struct UsageQuery {
    start: DateTime<Utc>,
    end: DateTime<Utc>,
    route: Option<String>,
    #[param(value_type = Option<String>, format = Uuid)]
    provider_id: Option<Uuid>,
    model: Option<String>,
    #[param(value_type = Option<String>, format = Uuid)]
    api_key_id: Option<Uuid>,
    operation: Option<String>,
}

impl UsageQuery {
    fn filters(&self) -> Result<UsageFilters, Problem> {
        Ok(UsageFilters {
            observed_after: self.start,
            observed_before: self.end,
            route_slug: self.route.clone(),
            provider_id: self.provider_id,
            upstream_model: self.model.clone(),
            api_key_id: self.api_key_id,
            operation: self
                .operation
                .as_deref()
                .map(str::parse)
                .transpose()
                .map_err(|_| {
                    Problem::bad_request("invalid_operation", "The operation filter is invalid.")
                })?,
        })
    }

    fn validate(&self) -> Result<(), Problem> {
        validate_time_range("start", self.start, "end", self.end)
    }
}

#[derive(Debug, Deserialize)]
struct UsageSeriesQuery {
    #[serde(flatten)]
    usage: UsageQuery,
    granularity: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
struct UsagePointResponse {
    bucket: DateTime<Utc>,
    request_count: u64,
    input_tokens: String,
    output_tokens: String,
    cached_input_tokens: String,
    media_units: String,
    estimated_cost: Option<String>,
    currency: Option<String>,
    unpriced_count: u64,
    incomplete_count: u64,
}

impl From<UsagePoint> for UsagePointResponse {
    fn from(point: UsagePoint) -> Self {
        Self {
            bucket: point.bucket,
            request_count: point.request_count,
            input_tokens: point.input_tokens,
            output_tokens: point.output_tokens,
            cached_input_tokens: point.cached_input_tokens,
            media_units: point.media_units,
            estimated_cost: point.estimated_cost,
            currency: point.currency,
            unpriced_count: point.unpriced_count,
            incomplete_count: point.incomplete_count,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
struct UsageRangeCoverageResponse {
    range_complete: bool,
    approximate: bool,
    excluded_partial_aggregate_boundaries: u8,
}

impl From<UsageRangeCoverage> for UsageRangeCoverageResponse {
    fn from(coverage: UsageRangeCoverage) -> Self {
        Self {
            range_complete: coverage.range_complete,
            approximate: coverage.approximate,
            excluded_partial_aggregate_boundaries: coverage.excluded_partial_aggregate_boundaries,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
struct UsageConsumerStatusResponse {
    state: String,
    pending_events: u64,
    lag_events: u64,
    oldest_pending_at: Option<DateTime<Utc>>,
    checked_at: Option<DateTime<Utc>>,
    heartbeat_age_seconds: Option<u64>,
}

impl From<UsageConsumerStatus> for UsageConsumerStatusResponse {
    fn from(consumer: UsageConsumerStatus) -> Self {
        Self {
            state: consumer.state.as_str().to_owned(),
            pending_events: consumer.pending_events,
            lag_events: consumer.lag_events,
            oldest_pending_at: consumer.oldest_pending_at,
            checked_at: consumer.checked_at,
            heartbeat_age_seconds: consumer.heartbeat_age_seconds,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
struct UsageTimeSeriesResponse {
    data: Vec<UsagePointResponse>,
    coverage: UsageRangeCoverageResponse,
}

#[utoipa::path(
    get,
    path = "/api/v1/usage/time-series",
    tag = "usage",
    params(
        UsageQuery,
        ("granularity" = Option<String>, Query, description = "Bucket size: hour or day")
    ),
    responses((status = 200, description = "Usage time series", body = UsageTimeSeriesResponse))
)]
async fn usage_time_series(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<UsageSeriesQuery>,
) -> Result<Json<UsageTimeSeriesResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    query.usage.validate()?;
    let granularity = match query.granularity.as_deref().unwrap_or("hour") {
        "hour" => UsageGranularity::Hour,
        "day" => UsageGranularity::Day,
        _ => {
            return Err(Problem::bad_request(
                "invalid_granularity",
                "Granularity must be hour or day.",
            ));
        }
    };
    let filters = query.usage.filters()?;
    let series = require_store(&state)?
        .usage_series(&filters, granularity)
        .await
        .map_err(map_operations)?;
    Ok(Json(UsageTimeSeriesResponse {
        data: series.points.into_iter().map(Into::into).collect(),
        coverage: series.coverage.into(),
    }))
}

#[derive(Debug, Serialize, ToSchema)]
struct UsageSummaryResponse {
    request_count: u64,
    input_tokens: String,
    output_tokens: String,
    cached_input_tokens: String,
    media_units: String,
    estimated_cost: Option<String>,
    currency: Option<String>,
    unpriced_count: u64,
    incomplete_count: u64,
    /// Exact loss plus known in-flight lower bounds from unclean epochs.
    ingestion_gap_events: u64,
    /// Unclean gateway epochs make completeness unknown even when their last
    /// durable in-flight lower bound was zero.
    uncertain_gap_count: u64,
    coverage: UsageRangeCoverageResponse,
    consumer: UsageConsumerStatusResponse,
    complete: bool,
}

impl From<UsageSummary> for UsageSummaryResponse {
    fn from(summary: UsageSummary) -> Self {
        Self {
            request_count: summary.request_count,
            input_tokens: summary.input_tokens,
            output_tokens: summary.output_tokens,
            cached_input_tokens: summary.cached_input_tokens,
            media_units: summary.media_units,
            estimated_cost: summary.estimated_cost,
            currency: summary.currency,
            unpriced_count: summary.unpriced_count,
            incomplete_count: summary.incomplete_count,
            ingestion_gap_events: summary.ingestion_gap_events,
            uncertain_gap_count: summary.uncertain_gap_count,
            coverage: summary.coverage.into(),
            consumer: summary.consumer.into(),
            complete: summary.complete,
        }
    }
}

#[utoipa::path(
    get,
    path = "/api/v1/usage/summary",
    tag = "usage",
    params(UsageQuery),
    responses((status = 200, description = "Usage and estimated-cost summary", body = UsageSummaryResponse))
)]
async fn usage_summary(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<UsageQuery>,
) -> Result<Json<UsageSummaryResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    query.validate()?;
    let filters = query.filters()?;
    let summary = require_store(&state)?
        .usage_summary(&filters)
        .await
        .map_err(map_operations)?;
    Ok(Json(summary.into()))
}

#[derive(Debug, Deserialize)]
struct UsageBreakdownQuery {
    #[serde(flatten)]
    usage: UsageQuery,
    dimension: String,
    limit: Option<u16>,
}

#[derive(Debug, Serialize, ToSchema)]
struct UsageBreakdownItem {
    dimension: String,
    request_count: u64,
    input_tokens: String,
    output_tokens: String,
    cached_input_tokens: String,
    media_units: String,
    estimated_cost: Option<String>,
    currency: Option<String>,
    unpriced_count: u64,
    incomplete_count: u64,
}

impl From<UsageBreakdown> for UsageBreakdownItem {
    fn from(item: UsageBreakdown) -> Self {
        Self {
            dimension: item.dimension,
            request_count: item.request_count,
            input_tokens: item.input_tokens,
            output_tokens: item.output_tokens,
            cached_input_tokens: item.cached_input_tokens,
            media_units: item.media_units,
            estimated_cost: item.estimated_cost,
            currency: item.currency,
            unpriced_count: item.unpriced_count,
            incomplete_count: item.incomplete_count,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
struct UsageBreakdownResponse {
    data: Vec<UsageBreakdownItem>,
    coverage: UsageRangeCoverageResponse,
}

#[utoipa::path(
    get,
    path = "/api/v1/usage/breakdown",
    tag = "usage",
    params(
        UsageQuery,
        ("dimension" = String, Query, description = "Break down by route, provider, model, api_key, or operation"),
        ("limit" = Option<u16>, Query, minimum = 1, maximum = 200, description = "Maximum number of breakdown rows")
    ),
    responses((status = 200, description = "Usage breakdown", body = UsageBreakdownResponse))
)]
async fn usage_breakdown(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<UsageBreakdownQuery>,
) -> Result<Json<UsageBreakdownResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    query.usage.validate()?;
    let dimension = match query.dimension.as_str() {
        "route" => UsageDimension::Route,
        "provider" => UsageDimension::Provider,
        "model" => UsageDimension::Model,
        "api_key" => UsageDimension::ApiKey,
        "operation" => UsageDimension::Operation,
        _ => {
            return Err(Problem::bad_request(
                "invalid_dimension",
                "Dimension must be route, provider, model, api_key, or operation.",
            ));
        }
    };
    let filters = query.usage.filters()?;
    let report = require_store(&state)?
        .usage_breakdown(&filters, dimension, page_limit(query.limit)?)
        .await
        .map_err(map_operations)?;
    Ok(Json(UsageBreakdownResponse {
        data: report.items.into_iter().map(Into::into).collect(),
        coverage: report.coverage.into(),
    }))
}

#[derive(Debug, Serialize, ToSchema)]
struct UsageCompletenessResponse {
    request_count: u64,
    priced_count: u64,
    unpriced_count: u64,
    incomplete_count: u64,
    ingestion_gap_events: u64,
    uncertain_gap_count: u64,
    estimated_cost: Option<String>,
    currency: Option<String>,
    coverage: UsageRangeCoverageResponse,
    consumer: UsageConsumerStatusResponse,
    complete: bool,
}

impl From<UsageCompleteness> for UsageCompletenessResponse {
    fn from(summary: UsageCompleteness) -> Self {
        Self {
            request_count: summary.request_count,
            priced_count: summary.priced_count,
            unpriced_count: summary.unpriced_count,
            incomplete_count: summary.incomplete_count,
            ingestion_gap_events: summary.ingestion_gap_events,
            uncertain_gap_count: summary.uncertain_gap_count,
            estimated_cost: summary.estimated_cost,
            currency: summary.currency,
            coverage: summary.coverage.into(),
            consumer: summary.consumer.into(),
            complete: summary.complete,
        }
    }
}

#[utoipa::path(
    get,
    path = "/api/v1/usage/completeness",
    tag = "usage",
    params(UsageQuery),
    responses((status = 200, description = "Usage and pricing completeness", body = UsageCompletenessResponse))
)]
async fn usage_completeness(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<UsageQuery>,
) -> Result<Json<UsageCompletenessResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    query.validate()?;
    let filters = query.filters()?;
    let summary = require_store(&state)?
        .usage_completeness(&filters)
        .await
        .map_err(map_operations)?;
    Ok(Json(summary.into()))
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
struct UsageGatewayEpochQuery {
    cursor: Option<String>,
    #[param(minimum = 1, maximum = 200)]
    limit: Option<u16>,
    /// Filter by open, gracefully_closed, unresolved, or acknowledged.
    state: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
struct UsageGatewayEpochResponse {
    gateway_instance: String,
    #[schema(value_type = String, format = Uuid)]
    process_epoch: Uuid,
    state: String,
    started_at: DateTime<Utc>,
    updated_at: DateTime<Utc>,
    accepted: u64,
    persisted: u64,
    dropped: u64,
    abandoned: u64,
    uncertain_event_lower_bound: u64,
    retrying: bool,
    writer_closed: bool,
    gracefully_closed_at: Option<DateTime<Utc>>,
    stale_detected_at: Option<DateTime<Utc>>,
    acknowledged_at: Option<DateTime<Utc>>,
    #[schema(value_type = Option<String>, format = Uuid)]
    acknowledged_by: Option<Uuid>,
}

impl From<UsageGatewayEpochRecord> for UsageGatewayEpochResponse {
    fn from(value: UsageGatewayEpochRecord) -> Self {
        Self {
            gateway_instance: value.gateway_instance,
            process_epoch: value.process_epoch,
            state: value.state.as_str().to_owned(),
            started_at: value.started_at,
            updated_at: value.updated_at,
            accepted: value.accepted,
            persisted: value.persisted,
            dropped: value.dropped,
            abandoned: value.abandoned,
            uncertain_event_lower_bound: value.uncertain_event_lower_bound,
            retrying: value.retrying,
            writer_closed: value.writer_closed,
            gracefully_closed_at: value.gracefully_closed_at,
            stale_detected_at: value.stale_detected_at,
            acknowledged_at: value.acknowledged_at,
            acknowledged_by: value.acknowledged_by,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
struct UsageGatewayEpochListResponse {
    data: Vec<UsageGatewayEpochResponse>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/usage/gateway-epochs",
    tag = "usage",
    params(UsageGatewayEpochQuery),
    responses(
        (status = 200, description = "Metadata-only gateway process epoch page", body = UsageGatewayEpochListResponse),
        (status = 400, description = "Invalid cursor or state filter", body = Problem)
    )
)]
async fn list_usage_gateway_epochs(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<UsageGatewayEpochQuery>,
) -> Result<Json<UsageGatewayEpochListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let cursor = query
        .cursor
        .as_deref()
        .map(TimestampCursor::parse)
        .transpose()
        .map_err(map_operations)?;
    let state_filter = query
        .state
        .as_deref()
        .map(parse_usage_gateway_epoch_state)
        .transpose()?;
    let page = require_store(&state)?
        .usage_gateway_epochs(state_filter, cursor.as_ref(), page_limit(query.limit)?)
        .await
        .map_err(map_operations)?;
    Ok(Json(UsageGatewayEpochListResponse {
        data: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor,
    }))
}

fn parse_usage_gateway_epoch_state(value: &str) -> Result<UsageGatewayEpochState, Problem> {
    match value {
        "open" => Ok(UsageGatewayEpochState::Open),
        "gracefully_closed" => Ok(UsageGatewayEpochState::GracefullyClosed),
        "unresolved" => Ok(UsageGatewayEpochState::Unresolved),
        "acknowledged" => Ok(UsageGatewayEpochState::Acknowledged),
        _ => {
            let mut errors = FieldErrors::new();
            errors.insert(
                "state".to_owned(),
                vec!["Use open, gracefully_closed, unresolved, or acknowledged.".to_owned()],
            );
            Err(Problem::validation(errors))
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
struct UsageEpochAcknowledgementResponse {
    #[schema(value_type = String, format = Uuid)]
    process_epoch: Uuid,
    gateway_instance: String,
    acknowledged_at: DateTime<Utc>,
    #[schema(value_type = Option<String>, format = Uuid)]
    acknowledged_by: Option<Uuid>,
}

impl From<UsageEpochAcknowledgement> for UsageEpochAcknowledgementResponse {
    fn from(value: UsageEpochAcknowledgement) -> Self {
        Self {
            process_epoch: value.process_epoch,
            gateway_instance: value.gateway_instance,
            acknowledged_at: value.acknowledged_at,
            acknowledged_by: value.acknowledged_by,
        }
    }
}

#[utoipa::path(
    post,
    path = "/api/v1/usage/gateway-epochs/{process_epoch}/acknowledge",
    tag = "usage",
    params(("process_epoch" = Uuid, Path)),
    responses(
        (status = 200, description = "Unclean gateway epoch acknowledged; retained completeness evidence is unchanged", body = UsageEpochAcknowledgementResponse),
        (status = 404, description = "Unclean gateway epoch not found", body = Problem)
    )
)]
async fn acknowledge_usage_gateway_epoch(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Path(process_epoch): Path<Uuid>,
) -> Result<Json<UsageEpochAcknowledgementResponse>, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageSettings)?;
    let acknowledgement = require_store(&state)?
        .acknowledge_usage_gateway_epoch(process_epoch, principal.user_id)
        .await
        .map_err(map_persistence)?
        .ok_or_else(not_found)?;
    Ok(Json(acknowledgement.into()))
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
struct PageQuery {
    cursor: Option<String>,
    limit: Option<u16>,
}

#[derive(Debug, Serialize, ToSchema)]
struct AuditEventResponse {
    #[schema(value_type = String, format = Uuid)]
    id: Uuid,
    #[schema(value_type = Option<String>, format = Uuid)]
    actor_user_id: Option<Uuid>,
    actor_email: Option<String>,
    action: String,
    resource_type: String,
    resource_id: Option<String>,
    outcome: String,
    occurred_at: DateTime<Utc>,
}

impl From<AuditRecord> for AuditEventResponse {
    fn from(record: AuditRecord) -> Self {
        Self {
            id: record.id,
            actor_user_id: record.actor_user_id,
            actor_email: record.actor_email,
            action: record.action,
            resource_type: record.resource_type,
            resource_id: record.resource_id,
            outcome: record.outcome,
            occurred_at: record.occurred_at,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
struct AuditListResponse {
    data: Vec<AuditEventResponse>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/audit",
    tag = "audit",
    params(PageQuery),
    responses((status = 200, description = "Audit page", body = AuditListResponse))
)]
async fn list_audit_events(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<AuditListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let cursor = query
        .cursor
        .as_deref()
        .map(TimestampCursor::parse)
        .transpose()
        .map_err(map_operations)?;
    let limit = page_limit(query.limit)?;
    let page = require_store(&state)?
        .audit_events(cursor.as_ref(), limit)
        .await
        .map_err(map_operations)?;
    Ok(Json(AuditListResponse {
        data: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor,
    }))
}

#[utoipa::path(
    get,
    path = "/api/v1/health/ready",
    tag = "health",
    responses(
        (status = 200, description = "Cached readiness snapshot for an authenticated management session", body = HealthResponse),
        (status = 401, description = "Authentication required", body = Problem),
        (status = 403, description = "Insufficient role", body = Problem),
        (status = 503, description = "Readiness snapshot is stale or unavailable", body = Problem)
    )
)]
async fn management_readiness(
    State(state): State<ApiState>,
    headers: HeaderMap,
) -> Result<Json<HealthResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    Ok(Json(state.cached_readiness()?))
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
struct ProviderHealthQuery {
    #[param(minimum = 1, maximum = 1440, default = 15)]
    window_minutes: Option<u16>,
    #[param(value_type = Option<String>, format = Uuid)]
    cursor: Option<String>,
    #[param(minimum = 1, maximum = 200, default = 50)]
    limit: Option<u16>,
}

#[derive(Debug, Serialize, ToSchema)]
struct ProviderHealthItem {
    #[schema(value_type = String, format = Uuid)]
    provider_id: Uuid,
    provider_name: String,
    provider_kind: String,
    provider_state: String,
    status: String,
    last_probe_at: Option<DateTime<Utc>>,
    last_probe_status: Option<String>,
    last_probe_detail: Option<String>,
    last_attempt_at: Option<DateTime<Utc>>,
    attempt_count: u64,
    success_count: u64,
    rate_limit_count: u64,
    server_error_count: u64,
    transport_error_count: u64,
    average_latency_ms: Option<f64>,
}

impl From<ProviderHealthRecord> for ProviderHealthItem {
    fn from(record: ProviderHealthRecord) -> Self {
        Self {
            provider_id: record.provider_id,
            provider_name: record.provider_name,
            provider_kind: record.provider_kind.to_string(),
            provider_state: record.provider_state.to_string(),
            status: record.status,
            last_probe_at: record.last_probe_at,
            last_probe_status: record.last_probe_status,
            last_probe_detail: record.last_probe_detail,
            last_attempt_at: record.last_attempt_at,
            attempt_count: record.attempt_count,
            success_count: record.success_count,
            rate_limit_count: record.rate_limit_count,
            server_error_count: record.server_error_count,
            transport_error_count: record.transport_error_count,
            average_latency_ms: record.average_latency_ms,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
struct ProviderHealthResponse {
    window_minutes: u16,
    data: Vec<ProviderHealthItem>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/provider-health",
    tag = "health",
    params(ProviderHealthQuery),
    responses(
        (status = 200, description = "Probe and rolling-attempt provider health", body = ProviderHealthResponse),
        (status = 401, description = "Authentication required", body = Problem),
        (status = 403, description = "Insufficient role", body = Problem)
    )
)]
async fn provider_health(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<ProviderHealthQuery>,
) -> Result<Json<ProviderHealthResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let window_minutes = query.window_minutes.unwrap_or(15);
    if !(1..=1_440).contains(&window_minutes) {
        let mut fields = FieldErrors::new();
        fields.insert(
            "window_minutes".to_owned(),
            vec!["Window must be between 1 and 1440 minutes.".to_owned()],
        );
        return Err(Problem::validation(fields));
    }
    let cursor = query
        .cursor
        .as_deref()
        .map(Uuid::parse_str)
        .transpose()
        .map_err(|_| Problem::bad_request("invalid_cursor", "The cursor is invalid."))?;
    let page = require_store(&state)?
        .provider_health(window_minutes, cursor, page_limit(query.limit)?)
        .await
        .map_err(map_operations)?;
    Ok(Json(ProviderHealthResponse {
        window_minutes,
        data: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor,
    }))
}

#[derive(Debug, Serialize, ToSchema)]
struct RuntimeGenerationItem {
    #[schema(value_type = String, format = Uuid)]
    id: Uuid,
    sequence: u64,
    sha256: String,
    #[schema(value_type = String, format = Uuid)]
    created_by: Uuid,
    created_by_email: String,
    created_at: DateTime<Utc>,
}

impl From<RuntimeGenerationRecord> for RuntimeGenerationItem {
    fn from(record: RuntimeGenerationRecord) -> Self {
        Self {
            id: record.id,
            sequence: record.sequence,
            sha256: record.sha256_hex,
            created_by: record.created_by,
            created_by_email: record.created_by_email,
            created_at: record.created_at,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
struct RuntimeGenerationListResponse {
    data: Vec<RuntimeGenerationItem>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/runtime-generations",
    tag = "runtime",
    params(PageQuery),
    responses((status = 200, description = "Runtime generations", body = RuntimeGenerationListResponse))
)]
async fn list_runtime_generations(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<RuntimeGenerationListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let before = query
        .cursor
        .as_deref()
        .map(str::parse::<u64>)
        .transpose()
        .map_err(|_| Problem::bad_request("invalid_cursor", "The cursor is invalid."))?;
    let limit = page_limit(query.limit)?;
    let page = require_store(&state)?
        .runtime_generations(before, limit)
        .await
        .map_err(map_operations)?;
    Ok(Json(RuntimeGenerationListResponse {
        data: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor,
    }))
}

#[derive(Debug, Serialize, ToSchema)]
struct SettingResponse {
    key: String,
    value: String,
    #[schema(value_type = String, format = Uuid)]
    etag: Uuid,
    #[schema(value_type = String, format = Uuid)]
    updated_by: Uuid,
    updated_at: DateTime<Utc>,
}

impl From<SettingRecord> for SettingResponse {
    fn from(record: SettingRecord) -> Self {
        Self {
            key: record.key,
            value: record.value,
            etag: record.etag,
            updated_by: record.updated_by,
            updated_at: record.updated_at,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
struct SettingsResponse {
    data: Vec<SettingResponse>,
}

#[utoipa::path(
    get,
    path = "/api/v1/settings",
    tag = "settings",
    responses((status = 200, description = "Installation settings", body = SettingsResponse))
)]
async fn list_settings(
    State(state): State<ApiState>,
    headers: HeaderMap,
) -> Result<Json<SettingsResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let settings = require_store(&state)?
        .settings()
        .await
        .map_err(map_operations)?;
    Ok(Json(SettingsResponse {
        data: settings.into_iter().map(Into::into).collect(),
    }))
}

#[utoipa::path(
    get,
    path = "/api/v1/settings/{key}",
    tag = "settings",
    params(("key" = String, Path, description = "Setting key")),
    responses((status = 200, description = "Setting with ETag", body = SettingResponse))
)]
async fn get_setting(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Path(key): Path<String>,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let setting = require_store(&state)?
        .settings()
        .await
        .map_err(map_operations)?
        .into_iter()
        .find(|setting| setting.key == key)
        .ok_or_else(not_found)?;
    setting_response(setting)
}

#[derive(Debug, Deserialize, ToSchema)]
struct UpdateSettingRequest {
    value: String,
}

#[utoipa::path(
    put,
    path = "/api/v1/settings/{key}",
    tag = "settings",
    params(
        ("key" = String, Path, description = "Setting key"),
        ("If-Match" = String, Header, description = "Quoted setting ETag")
    ),
    request_body = UpdateSettingRequest,
    responses(
        (status = 200, description = "Updated setting", body = SettingResponse),
        (status = 412, description = "ETag mismatch", body = Problem)
    )
)]
async fn update_setting(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Path(key): Path<String>,
    payload: Result<Json<UpdateSettingRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageSettings)?;
    let etag = if_match(&headers)?;
    let request = json_payload(payload)?;
    let setting = require_store(&state)?
        .update_setting(&key, &request.value, etag, principal.user_id)
        .await
        .map_err(map_operations)?;
    setting_response(setting)
}

fn setting_response(setting: SettingRecord) -> Result<Response, Problem> {
    let etag =
        HeaderValue::from_str(&format!("\"{}\"", setting.etag)).map_err(|_| Problem::internal())?;
    let mut response = Json(SettingResponse::from(setting)).into_response();
    response.headers_mut().insert(header::ETAG, etag);
    Ok(response)
}

fn if_match(headers: &HeaderMap) -> Result<Uuid, Problem> {
    let value = headers
        .get(header::IF_MATCH)
        .and_then(|value| value.to_str().ok())
        .ok_or_else(|| {
            Problem::new(
                StatusCode::PRECONDITION_REQUIRED,
                "if_match_required",
                "Precondition required",
                "An If-Match header containing the current ETag is required.",
            )
        })?;
    let value = value
        .strip_prefix('"')
        .and_then(|value| value.strip_suffix('"'))
        .ok_or_else(|| {
            Problem::bad_request("invalid_if_match", "If-Match must be a strong ETag.")
        })?;
    Uuid::parse_str(value)
        .map_err(|_| Problem::bad_request("invalid_if_match", "If-Match contains an invalid ETag."))
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, ToSchema)]
#[serde(rename_all = "snake_case")]
enum PriceProviderKind {
    OpenAi,
    Anthropic,
    Gemini,
    VertexAi,
    Bedrock,
    AzureOpenAi,
    OpenAiCompatible,
}

impl From<PriceProviderKind> for ProviderKind {
    fn from(value: PriceProviderKind) -> Self {
        match value {
            PriceProviderKind::OpenAi => Self::OpenAi,
            PriceProviderKind::Anthropic => Self::Anthropic,
            PriceProviderKind::Gemini => Self::Gemini,
            PriceProviderKind::VertexAi => Self::VertexAi,
            PriceProviderKind::Bedrock => Self::Bedrock,
            PriceProviderKind::AzureOpenAi => Self::AzureOpenAi,
            PriceProviderKind::OpenAiCompatible => Self::OpenAiCompatible,
        }
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, ToSchema)]
#[serde(rename_all = "snake_case")]
enum PriceOperation {
    Generation,
    Embeddings,
    TokenCount,
    ImageGeneration,
    ImageEdit,
    ImageVariation,
    Speech,
    Transcription,
    VideoCreate,
    VideoList,
    VideoGet,
    VideoContent,
    VideoDelete,
    Moderation,
    ModelList,
    ModelGet,
}

impl From<PriceOperation> for OperationKind {
    fn from(value: PriceOperation) -> Self {
        match value {
            PriceOperation::Generation => Self::Generation,
            PriceOperation::Embeddings => Self::Embeddings,
            PriceOperation::TokenCount => Self::TokenCount,
            PriceOperation::ImageGeneration => Self::ImageGeneration,
            PriceOperation::ImageEdit => Self::ImageEdit,
            PriceOperation::ImageVariation => Self::ImageVariation,
            PriceOperation::Speech => Self::Speech,
            PriceOperation::Transcription => Self::Transcription,
            PriceOperation::VideoCreate => Self::VideoCreate,
            PriceOperation::VideoList => Self::VideoList,
            PriceOperation::VideoGet => Self::VideoGet,
            PriceOperation::VideoContent => Self::VideoContent,
            PriceOperation::VideoDelete => Self::VideoDelete,
            PriceOperation::Moderation => Self::Moderation,
            PriceOperation::ModelList => Self::ModelList,
            PriceOperation::ModelGet => Self::ModelGet,
        }
    }
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
struct PriceRequest {
    provider_kind: PriceProviderKind,
    #[schema(value_type = Option<String>, format = Uuid)]
    provider_id: Option<Uuid>,
    model: String,
    operation: PriceOperation,
    input_per_million: Option<String>,
    output_per_million: Option<String>,
    unit_price: Option<String>,
    currency: String,
}

impl From<PriceRequest> for PriceInput {
    fn from(price: PriceRequest) -> Self {
        Self {
            provider_kind: ProviderKind::from(price.provider_kind),
            provider_id: price.provider_id,
            model: price.model,
            operation: OperationKind::from(price.operation),
            input_per_million: price.input_per_million,
            output_per_million: price.output_per_million,
            unit_price: price.unit_price,
            currency: price.currency,
        }
    }
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
struct PricingRevisionRequest {
    effective_at: DateTime<Utc>,
    prices: Vec<PriceRequest>,
}

#[derive(Debug, Serialize, ToSchema)]
struct PriceResponse {
    provider_kind: String,
    #[schema(value_type = Option<String>, format = Uuid)]
    provider_id: Option<Uuid>,
    model: String,
    operation: String,
    input_per_million: Option<String>,
    output_per_million: Option<String>,
    unit_price: Option<String>,
    currency: String,
}

impl From<PriceInput> for PriceResponse {
    fn from(price: PriceInput) -> Self {
        Self {
            provider_kind: price.provider_kind.to_string(),
            provider_id: price.provider_id,
            model: price.model,
            operation: price.operation.to_string(),
            input_per_million: price.input_per_million,
            output_per_million: price.output_per_million,
            unit_price: price.unit_price,
            currency: price.currency,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
struct PricingRevisionResponse {
    #[schema(value_type = String, format = Uuid)]
    id: Uuid,
    revision: u32,
    effective_at: DateTime<Utc>,
    #[schema(value_type = String, format = Uuid)]
    created_by: Uuid,
    created_at: DateTime<Utc>,
    prices: Vec<PriceResponse>,
}

impl From<PricingRevisionRecord> for PricingRevisionResponse {
    fn from(revision: PricingRevisionRecord) -> Self {
        Self {
            id: revision.id,
            revision: revision.revision,
            effective_at: revision.effective_at,
            created_by: revision.created_by,
            created_at: revision.created_at,
            prices: revision.prices.into_iter().map(Into::into).collect(),
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
struct PricingRevisionsResponse {
    data: Vec<PricingRevisionResponse>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/pricing/revisions",
    tag = "pricing",
    params(PageQuery),
    responses((status = 200, description = "Pricing revisions", body = PricingRevisionsResponse))
)]
async fn list_pricing_revisions(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<PricingRevisionsResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let before = query
        .cursor
        .as_deref()
        .map(str::parse::<u32>)
        .transpose()
        .map_err(|_| Problem::bad_request("invalid_cursor", "The cursor is invalid."))?;
    let page = require_store(&state)?
        .pricing_revisions_page(before, page_limit(query.limit)?)
        .await
        .map_err(map_operations)?;
    Ok(Json(PricingRevisionsResponse {
        data: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor,
    }))
}

#[utoipa::path(
    post,
    path = "/api/v1/pricing/revisions",
    tag = "pricing",
    params(("Idempotency-Key" = String, Header, description = "Unique creation key")),
    request_body = PricingRevisionRequest,
    responses(
        (status = 201, description = "Pricing revision created", body = PricingRevisionResponse),
        (status = 409, description = "Idempotency key reused or request in progress", body = Problem),
        (status = 422, description = "Invalid pricing revision", body = Problem),
        (status = 503, description = "Master key or database unavailable", body = Problem)
    )
)]
async fn create_pricing_revision(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<PricingRevisionRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManagePricing)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let request_fingerprint = idempotency_fingerprint(&request).map_err(map_persistence)?;
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let prices = request
        .prices
        .into_iter()
        .map(Into::into)
        .collect::<Vec<_>>();
    let revision = require_store(&state)?
        .create_pricing_revision(
            principal.user_id,
            &idempotency_key,
            request.effective_at,
            &prices,
            ReplayableIdempotency::new(request_fingerprint, master_key),
            |revision| {
                IdempotencyResponse::json(
                    StatusCode::CREATED.as_u16(),
                    &PricingRevisionResponse::from(revision.clone()),
                    None,
                )
            },
        )
        .await
        .map_err(map_operations)?;
    idempotency_http_response(revision)
}

fn page_limit(value: Option<u16>) -> Result<u16, Problem> {
    let value = value.unwrap_or(50);
    if (1..=200).contains(&value) {
        return Ok(value);
    }
    let mut errors = FieldErrors::new();
    errors.insert(
        "limit".to_owned(),
        vec!["Use a page size between 1 and 200.".to_owned()],
    );
    Err(Problem::validation(errors))
}

fn validate_time_range(
    start_name: &str,
    start: DateTime<Utc>,
    end_name: &str,
    end: DateTime<Utc>,
) -> Result<(), Problem> {
    if start < end {
        return Ok(());
    }
    let mut errors = FieldErrors::new();
    errors.insert(
        end_name.to_owned(),
        vec![format!("{end_name} must be later than {start_name}.")],
    );
    Err(Problem::validation(errors))
}

fn not_found() -> Problem {
    Problem::new(
        StatusCode::NOT_FOUND,
        "resource_not_found",
        "Resource not found",
        "The requested resource does not exist.",
    )
}

fn map_operations(error: OperationsError) -> Problem {
    match error {
        OperationsError::InvalidCursor => {
            Problem::bad_request("invalid_cursor", "The cursor is invalid or expired.")
        }
        OperationsError::NotFound => not_found(),
        OperationsError::PreconditionFailed => Problem::new(
            StatusCode::PRECONDITION_FAILED,
            "etag_mismatch",
            "Precondition failed",
            "The resource changed; refresh it and retry with the current ETag.",
        ),
        OperationsError::IdempotencyConflict => Problem::conflict(
            "idempotency_key_reused",
            "The Idempotency-Key has already been used for this operation.",
        ),
        OperationsError::IdempotencyInProgress => Problem::conflict(
            "idempotency_in_progress",
            "An operation with this Idempotency-Key is still in progress.",
        ),
        OperationsError::Invalid(message) => {
            let mut errors = FieldErrors::new();
            errors.insert("request".to_owned(), vec![message]);
            Problem::validation(errors)
        }
        OperationsError::Database(error) => {
            error!(%error, "operations persistence query failed");
            Problem::internal()
        }
        OperationsError::Persistence(error) => map_persistence(error),
    }
}

fn map_media_job(error: MediaJobError) -> Problem {
    match error {
        MediaJobError::NotFound => not_found(),
        MediaJobError::PreconditionFailed => Problem::new(
            StatusCode::PRECONDITION_FAILED,
            "etag_mismatch",
            "Precondition failed",
            "The media job changed; refresh it and retry with the current ETag.",
        ),
        MediaJobError::UpstreamIdentityConflict => Problem::conflict(
            "media_job_upstream_identity_conflict",
            "The upstream media job is already bound to different metadata.",
        ),
        MediaJobError::Invalid(message) => {
            let mut fields = FieldErrors::new();
            fields.insert("media_job".to_owned(), vec![message]);
            Problem::validation(fields)
        }
        MediaJobError::Database(error) => {
            error!(%error, "media job persistence query failed");
            Problem::internal()
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn strong_etag_parser_rejects_wildcards_and_unquoted_values() {
        let id = Uuid::now_v7();
        let mut headers = HeaderMap::new();
        headers.insert(
            header::IF_MATCH,
            HeaderValue::from_str(&format!("\"{id}\"")).unwrap(),
        );
        assert_eq!(if_match(&headers).unwrap(), id);
        headers.insert(
            header::IF_MATCH,
            HeaderValue::from_str(&id.to_string()).unwrap(),
        );
        assert_eq!(if_match(&headers).unwrap_err().status, 400);
        headers.insert(header::IF_MATCH, HeaderValue::from_static("*"));
        assert_eq!(if_match(&headers).unwrap_err().status, 400);
    }

    #[test]
    fn pagination_and_time_ranges_reject_silent_clamping_or_reversal() {
        assert_eq!(page_limit(None).unwrap(), 50);
        assert_eq!(page_limit(Some(200)).unwrap(), 200);
        assert_eq!(page_limit(Some(0)).unwrap_err().status, 422);
        let now = Utc::now();
        assert!(validate_time_range("start", now, "end", now).is_err());
        assert!(
            validate_time_range("start", now - chrono::Duration::seconds(1), "end", now).is_ok()
        );
    }

    #[test]
    fn media_job_surface_preserves_wire_contract() {
        assert_eq!(media_job_surface_wire_value(Surface::OpenAi), "openai");
        assert_eq!(
            media_job_surface_wire_value(Surface::Anthropic),
            "anthropic"
        );
        assert_eq!(media_job_surface_wire_value(Surface::Gemini), "gemini");
    }

    #[test]
    fn audit_contract_omits_unavailable_request_provenance() {
        let document = serde_json::to_value(OperationsApiDoc::openapi()).unwrap();
        let properties = document["components"]["schemas"]["AuditEventResponse"]["properties"]
            .as_object()
            .unwrap();
        assert!(!properties.contains_key("source_ip"));
        assert!(!properties.contains_key("user_agent_family"));
    }

    #[test]
    fn usage_series_and_breakdown_publish_flat_query_parameters() {
        let document = serde_json::to_value(OperationsApiDoc::openapi()).unwrap();
        for (path, endpoint_parameter) in [
            ("/api/v1/usage/time-series", "granularity"),
            ("/api/v1/usage/breakdown", "dimension"),
        ] {
            let parameters = document["paths"][path]["get"]["parameters"]
                .as_array()
                .unwrap();
            let names = parameters
                .iter()
                .filter_map(|parameter| parameter["name"].as_str())
                .collect::<std::collections::BTreeSet<_>>();
            assert!(names.contains("start"));
            assert!(names.contains("end"));
            assert!(names.contains(endpoint_parameter));
            assert!(!names.contains("usage"));
        }
    }

    #[test]
    fn usage_contract_exposes_unclean_epoch_uncertainty() {
        let document = serde_json::to_value(OperationsApiDoc::openapi()).unwrap();
        for schema in ["UsageSummaryResponse", "UsageCompletenessResponse"] {
            let properties = document["components"]["schemas"][schema]["properties"]
                .as_object()
                .unwrap();
            assert!(properties.contains_key("ingestion_gap_events"));
            assert!(properties.contains_key("uncertain_gap_count"));
        }
    }
}

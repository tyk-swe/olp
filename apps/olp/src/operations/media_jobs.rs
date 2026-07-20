use axum::{
    Json,
    extract::{Path, Query, State},
    http::{HeaderMap, HeaderValue, header},
    response::{IntoResponse, Response},
};
use chrono::{DateTime, Utc};
use olp_domain::Surface;
use olp_storage::{
    MediaJobError, MediaJobFilters, MediaJobLifecycle, MediaJobRecord, MediaJobState,
    TimestampCursor,
};
use serde::{Deserialize, Serialize};
use tracing::error;
use utoipa::{IntoParams, ToSchema};
use uuid::Uuid;

use super::helpers::{map_operations, not_found, page_limit, validate_time_range};
use crate::{
    ApiState, FieldErrors, Problem,
    management::{Permission, require_permission, require_read_session, require_store},
};

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub(super) struct MediaJobQuery {
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
pub(super) struct MediaJobItem {
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

pub(super) const fn media_job_surface_wire_value(surface: Surface) -> &'static str {
    match surface {
        Surface::OpenAi => "openai",
        Surface::Anthropic => "anthropic",
        Surface::Gemini => "gemini",
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct MediaJobListResponse {
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
pub(super) async fn list_media_jobs(
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
pub(super) async fn get_media_job(
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

fn map_media_job(error: MediaJobError) -> Problem {
    match error {
        MediaJobError::NotFound => not_found(),
        MediaJobError::PreconditionFailed => Problem::new(
            axum::http::StatusCode::PRECONDITION_FAILED,
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

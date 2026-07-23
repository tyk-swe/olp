use std::collections::BTreeMap;

use axum::{
    Json,
    body::Body,
    extract::{Extension, Multipart, Path, Query, State},
    http::{HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use chrono::Utc;
use futures::{StreamExt, stream};
use olp_domain::{CanonicalResult, Operation, OperationKind, Surface, TransportMode};
use olp_protocols::openai::{
    OpenAiVideoContentQuery, OpenAiVideoCreateRequest, OpenAiVideoListQuery,
    decode_video_content_with_query, decode_video_create, decode_video_delete, decode_video_get,
    encode_video_delete_response, encode_video_list_response, encode_video_object,
};
use olp_storage::{
    MediaJobError, MediaJobFilters, MediaJobLifecycle, MediaJobOrder, MediaJobRecord,
    MediaJobUpdate, NewMediaJobReservation,
};
use tracing::error;

use crate::{GatewayState, InferencePrincipal, MultipartRequestAdmission};

use super::{
    error::InferenceError,
    execution::{RequiredTarget, authorize_principal, execute_routed_result, incompatible_result},
    limits::CleanupMediaStream,
    media::open_response_media,
    media_jobs::{
        attach_media_job_with_retry, mark_missing_delete_as_success, media_job_deletion_finalized,
        media_job_error, media_job_result, media_job_state, owned_media_job,
        refresh_video_list_record, select_video_create_target, set_video_route,
        valid_upstream_media_job_id,
    },
    multipart::parse_multipart,
};

pub(super) async fn video_create(
    State(state): State<GatewayState>,
    Extension(principal): Extension<InferencePrincipal>,
    Extension(admission): Extension<MultipartRequestAdmission>,
    multipart: Multipart,
) -> Result<Response, InferenceError> {
    let mut form = parse_multipart(
        &state,
        multipart,
        olp_protocols::openai::DEFAULT_VIDEO_REFERENCE_LIMIT,
        1,
        admission,
    )
    .await?;
    let request = OpenAiVideoCreateRequest {
        model: form.required("model")?,
        prompt: form.required("prompt")?,
        input_reference: form.take_single_file("input_reference")?,
        seconds: form.optional("seconds")?,
        size: form.optional("size")?,
        extra: form.take_extensions()?,
    };
    let operation = decode_video_create(request)
        .map_err(|error| InferenceError::invalid_request(error.to_string()))?;
    let local_job_id = uuid::Uuid::now_v7();
    let (key, route_slug, required_target) =
        select_video_create_target(&state, &principal, &operation, local_job_id)?;
    let reserved = state
        .store()
        .reserve_media_job(NewMediaJobReservation {
            id: local_job_id,
            runtime_generation_id: principal.runtime().generation.id.as_uuid(),
            api_key_id: key.id.as_uuid(),
            provider_id: required_target.provider_id,
            upstream_model: required_target.upstream_model.clone(),
            route_slug: route_slug.to_string(),
            operation: OperationKind::VideoCreate,
            surface: Surface::OpenAi,
        })
        .await
        .map_err(media_job_error)?;
    // From this point execution owns cleanup of every bounded request-media
    // handle. Until the durable reservation succeeds, the multipart guard
    // remains armed so selection or PostgreSQL failures cannot leak uploads.
    form.disarm_cleanup();
    // The accepted upstream create must outlive client disconnects. Capture
    // the HTTP inference context before spawning so it keeps the original
    // runtime generation, limits reservation, and metadata ownership.
    let task = crate::spawn_http_inference_task(
        &state,
        complete_video_create(
            state.clone(),
            principal,
            operation,
            reserved,
            required_target,
        ),
    );
    match task.await {
        Ok(result) => result,
        Err(error) => {
            error!(%error, "video create completion task stopped unexpectedly");
            Err(InferenceError::unavailable(
                "video_create_completion_unavailable",
            ))
        }
    }
}

async fn complete_video_create(
    state: GatewayState,
    principal: InferencePrincipal,
    operation: Operation,
    reserved: MediaJobRecord,
    required_target: RequiredTarget,
) -> Result<Response, InferenceError> {
    let mut executed = match execute_routed_result(
        &state,
        &principal,
        operation,
        TransportMode::Async,
        Some(required_target.clone()),
    )
    .await
    {
        Ok(executed) => executed,
        Err(error) => {
            if error.code == "ambiguous_upstream_result" {
                if let Err(persistence_error) = state
                    .store()
                    .mark_media_job_create_ambiguous(
                        reserved.id,
                        "upstream_create_result_ambiguous",
                    )
                    .await
                {
                    error!(job_id = %reserved.id, %persistence_error, "failed to mark ambiguous video creation");
                }
            } else {
                match media_job_deletion_finalized(state.store(), reserved.id).await {
                    Ok(true) => {}
                    Ok(false) => {
                        state.record_media_reconciliation_gap();
                        error!(job_id = %reserved.id, "abandoned video reservation was not finalized");
                    }
                    Err(persistence_error) => {
                        state.record_media_reconciliation_gap();
                        error!(job_id = %reserved.id, %persistence_error, "failed to retire abandoned video reservation");
                    }
                }
            }
            return Err(error);
        }
    };
    let mut result = match executed.result.as_ref() {
        CanonicalResult::VideoJob(result) => result.clone(),
        _ => {
            let failure = incompatible_result("video creation");
            if let Err(error) = state
                .store()
                .mark_media_job_create_ambiguous(
                    reserved.id,
                    "upstream_create_response_missing_job_identity",
                )
                .await
            {
                state.record_media_reconciliation_gap();
                error!(job_id = %reserved.id, %error, "failed to retire malformed video reservation");
            }
            executed.mark_failure(&failure);
            return Err(failure);
        }
    };
    let upstream_job_id = result.id.clone();
    if !valid_upstream_media_job_id(&upstream_job_id) {
        let failure = InferenceError::bad_gateway(
            "provider_protocol_error",
            "The provider returned an invalid video job identity.",
        );
        if let Err(error) = state
            .store()
            .mark_media_job_create_ambiguous(
                reserved.id,
                "upstream_create_response_invalid_job_identity",
            )
            .await
        {
            state.record_media_reconciliation_gap();
            error!(job_id = %reserved.id, %error, "failed to retire invalid video reservation");
        }
        executed.mark_failure(&failure);
        return Err(failure);
    }
    debug_assert_eq!(executed.provider_id, required_target.provider_id);
    debug_assert_eq!(executed.upstream_model, required_target.upstream_model);
    let state_update = match media_job_state(&result.status) {
        Ok(state_update) => state_update,
        Err(failure) => {
            if let Err(error) = state
                .store()
                .mark_media_job_create_cleanup_pending(
                    reserved.id,
                    &upstream_job_id,
                    "upstream_create_response_invalid_status",
                )
                .await
            {
                state.record_media_reconciliation_gap();
                error!(job_id = %reserved.id, %error, "failed to schedule malformed video cleanup");
            }
            executed.mark_failure(&failure);
            return Err(failure);
        }
    };
    let update = MediaJobUpdate {
        state: state_update,
        progress_percent: result.progress_percent,
        content_available: matches!(result.status, olp_domain::VideoStatus::Completed),
        expires_at: result
            .expires_at
            .and_then(chrono::DateTime::from_timestamp_secs),
        error_class: result
            .error
            .as_ref()
            .map(|error| format!("{:?}", error.class).to_lowercase()),
        last_polled_at: Utc::now(),
    };
    let record = attach_media_job_with_retry(&state, reserved.id, &upstream_job_id, update).await;
    let record = match record {
        Ok(record) => record,
        Err(error) => {
            let identity_conflict = matches!(error, MediaJobError::UpstreamIdentityConflict);
            // A compensation DELETE is only safe after PostgreSQL records the
            // upstream identity and cleanup intent. An ambiguous attachment
            // outcome can already have committed the active row.
            let cleanup_intent_persisted = if identity_conflict {
                false
            } else {
                match state
                    .store()
                    .mark_media_job_create_cleanup_pending(
                        reserved.id,
                        &upstream_job_id,
                        "upstream_created_local_attach_failed",
                    )
                    .await
                {
                    Ok(record)
                        if record.lifecycle == MediaJobLifecycle::CreateCleanupPending
                            && record.upstream_job_id.as_deref()
                                == Some(upstream_job_id.as_str()) =>
                    {
                        true
                    }
                    Ok(record) => {
                        error!(
                            job_id = %reserved.id,
                            lifecycle = record.lifecycle.as_str(),
                            "video cleanup intent did not retain the upstream identity"
                        );
                        false
                    }
                    Err(persistence_error) => {
                        error!(job_id = %reserved.id, %persistence_error, "failed to persist video cleanup reconciliation metadata");
                        false
                    }
                }
            };
            let compensation_confirmed = if cleanup_intent_persisted {
                let mut cleanup = decode_video_delete(upstream_job_id.clone());
                set_video_route(&mut cleanup, executed.route_slug.as_str())?;
                mark_missing_delete_as_success(&mut cleanup)?;
                let mut compensation = execute_routed_result(
                    &state,
                    &principal,
                    cleanup,
                    TransportMode::Unary,
                    Some(required_target),
                )
                .await;
                match &mut compensation {
                    Ok(compensation)
                        if matches!(
                            compensation.result.as_ref(),
                            CanonicalResult::VideoDelete(deleted) if deleted.deleted
                        ) =>
                    {
                        compensation.mark_success();
                        true
                    }
                    Ok(compensation) => {
                        let failure = incompatible_result("video deletion");
                        compensation.mark_failure(&failure);
                        false
                    }
                    Err(_) => false,
                }
            } else {
                false
            };
            if compensation_confirmed {
                match media_job_deletion_finalized(state.store(), reserved.id).await {
                    Ok(true) => {}
                    Ok(false) => {
                        state.record_media_reconciliation_gap();
                        error!(job_id = %reserved.id, "upstream cleanup succeeded but reconciliation tombstone was not finalized");
                    }
                    Err(persistence_error) => {
                        state.record_media_reconciliation_gap();
                        error!(job_id = %reserved.id, %persistence_error, "upstream cleanup succeeded but reconciliation tombstone failed");
                    }
                }
            } else {
                state.record_media_reconciliation_gap();
                error!(
                    job_id = %reserved.id,
                    upstream_job_id = %upstream_job_id,
                    provider_id = %executed.provider_id,
                    route = %executed.route_slug,
                    "video create reconciliation gap requires operator attention"
                );
            }
            let failure = InferenceError::unavailable("media_job_create_reconciliation_pending");
            executed.mark_failure(&failure);
            return Err(failure);
        }
    };
    result.id = record.id.to_string();
    result.model = Some(executed.route_slug.to_string());
    let response = encode_video_object(&result, executed.route_slug.as_str()).map_err(|error| {
        InferenceError::bad_gateway("provider_protocol_error", error.to_string())
    })?;
    executed.mark_success();
    Ok((StatusCode::CREATED, Json(response)).into_response())
}

pub(super) async fn video_list(
    State(state): State<GatewayState>,
    Extension(principal): Extension<InferencePrincipal>,
    Query(query): Query<OpenAiVideoListQuery>,
) -> Result<Response, InferenceError> {
    let key = authorize_principal(&principal, OperationKind::VideoList, None)?;
    if !query.extra.is_empty() {
        return Err(InferenceError::invalid_request(
            "Video list contains unsupported query parameters.",
        ));
    }
    if query.limit == Some(0) || query.limit.is_some_and(|limit| limit > 100) {
        return Err(InferenceError::invalid_request(
            "Video list limit must be between 1 and 100.",
        ));
    }
    if query
        .order
        .as_deref()
        .is_some_and(|value| !matches!(value, "asc" | "desc"))
    {
        return Err(InferenceError::invalid_request(
            "Video list order must be asc or desc.",
        ));
    }
    let cursor = query
        .after
        .as_deref()
        .map(uuid::Uuid::parse_str)
        .transpose()
        .map_err(|_| InferenceError::invalid_request("The video cursor is invalid."))?;
    let order = if query.order.as_deref() == Some("asc") {
        MediaJobOrder::Ascending
    } else {
        MediaJobOrder::Descending
    };
    let allowed_routes = key
        .allowed_routes
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let page = state
        .store()
        .media_jobs_after_id(
            &MediaJobFilters {
                api_key_id: Some(key.id.as_uuid()),
                route_slugs: allowed_routes,
                operation: Some(OperationKind::VideoCreate),
                surface: Some(Surface::OpenAi),
                ..MediaJobFilters::default()
            },
            cursor,
            order,
            query.limit.unwrap_or(20),
        )
        .await
        .map_err(media_job_error)?;
    let refreshed = stream::iter(page.items)
        .map(|record| refresh_video_list_record(&state, &principal, record))
        .buffered(4)
        .collect::<Vec<_>>()
        .await;
    let jobs = refreshed.iter().map(media_job_result).collect::<Vec<_>>();
    let result = olp_domain::VideoListResult {
        first_id: jobs.first().map(|job| job.id.clone()),
        last_id: jobs.last().map(|job| job.id.clone()),
        jobs,
        has_more: page.next_cursor.is_some(),
        extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    };
    let response = encode_video_list_response(&result, "video").map_err(|error| {
        InferenceError::bad_gateway("provider_protocol_error", error.to_string())
    })?;
    Ok((StatusCode::OK, Json(response)).into_response())
}

pub(super) async fn video_get(
    State(state): State<GatewayState>,
    Extension(principal): Extension<InferencePrincipal>,
    Path(video_id): Path<String>,
) -> Result<Response, InferenceError> {
    let (key, record) =
        owned_media_job(&state, &principal, &video_id, OperationKind::VideoGet).await?;
    let upstream_id = record
        .upstream_job_id
        .clone()
        .ok_or_else(|| InferenceError::unavailable("media_job_upstream_id_unavailable"))?;
    let mut operation = decode_video_get(upstream_id);
    set_video_route(&mut operation, &record.route_slug)?;
    let mut executed = execute_routed_result(
        &state,
        &principal,
        operation,
        TransportMode::Unary,
        Some(RequiredTarget {
            provider_id: record.provider_id,
            upstream_model: record.upstream_model.clone(),
        }),
    )
    .await?;
    debug_assert_eq!(executed.api_key_id, key.id.as_uuid());
    let mut result = match executed.result.as_ref() {
        CanonicalResult::VideoJob(result) => result.clone(),
        _ => return Err(incompatible_result("video status")),
    };
    let update = MediaJobUpdate {
        state: media_job_state(&result.status)?,
        progress_percent: result.progress_percent,
        content_available: matches!(result.status, olp_domain::VideoStatus::Completed),
        expires_at: result
            .expires_at
            .and_then(chrono::DateTime::from_timestamp_secs),
        error_class: result
            .error
            .as_ref()
            .map(|error| format!("{:?}", error.class).to_lowercase()),
        last_polled_at: Utc::now(),
    };
    let updated = state
        .store()
        .refresh_media_job(record.id, update)
        .await
        .map_err(media_job_error)?;
    result.id = updated.id.to_string();
    result.model = Some(updated.route_slug.clone());
    let response = encode_video_object(&result, &updated.route_slug).map_err(|error| {
        InferenceError::bad_gateway("provider_protocol_error", error.to_string())
    })?;
    executed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

pub(super) async fn video_content(
    State(state): State<GatewayState>,
    Extension(principal): Extension<InferencePrincipal>,
    Path(video_id): Path<String>,
    Query(query): Query<OpenAiVideoContentQuery>,
) -> Result<Response, InferenceError> {
    let (_, record) =
        owned_media_job(&state, &principal, &video_id, OperationKind::VideoContent).await?;
    let upstream_id = record
        .upstream_job_id
        .clone()
        .ok_or_else(|| InferenceError::unavailable("media_job_upstream_id_unavailable"))?;
    let mut operation = decode_video_content_with_query(upstream_id, query)
        .map_err(|error| InferenceError::invalid_request(error.to_string()))?;
    set_video_route(&mut operation, &record.route_slug)?;
    let mut executed = execute_routed_result(
        &state,
        &principal,
        operation,
        TransportMode::Unary,
        Some(RequiredTarget {
            provider_id: record.provider_id,
            upstream_model: record.upstream_model.clone(),
        }),
    )
    .await?;
    let result = match executed.result.as_ref() {
        CanonicalResult::VideoContent(result) => result.clone(),
        _ => return Err(incompatible_result("video content")),
    };
    let opened = open_response_media(&state, &result.media.handle).await?;
    let cleanup = CleanupMediaStream::new(
        opened.bytes,
        state.media_spool.clone(),
        opened.artifact.handle.clone(),
    );
    let mut response = Response::new(Body::from_stream(cleanup));
    if let Some(content_type) = opened.artifact.content_type {
        response.headers_mut().insert(
            header::CONTENT_TYPE,
            HeaderValue::from_str(&content_type).map_err(|_| {
                InferenceError::bad_gateway(
                    "provider_protocol_error",
                    "The provider returned an invalid video content type.",
                )
            })?,
        );
    }
    if let Some(length) = opened.artifact.content_length {
        response.headers_mut().insert(
            header::CONTENT_LENGTH,
            HeaderValue::from_str(&length.to_string()).map_err(|_| {
                InferenceError::bad_gateway(
                    "provider_protocol_error",
                    "The provider returned an invalid video length.",
                )
            })?,
        );
    }
    executed.mark_success();
    Ok(response)
}

pub(super) async fn video_delete(
    State(state): State<GatewayState>,
    Extension(principal): Extension<InferencePrincipal>,
    Path(video_id): Path<String>,
) -> Result<Response, InferenceError> {
    let (_, loaded) =
        owned_media_job(&state, &principal, &video_id, OperationKind::VideoDelete).await?;
    let record = state
        .store()
        .begin_media_job_deletion(loaded.id)
        .await
        .map_err(media_job_error)?;
    if record.lifecycle == MediaJobLifecycle::Deleted {
        let response = encode_video_delete_response(&olp_domain::VideoDeleteResult {
            id: record.id.to_string(),
            deleted: true,
            extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        })
        .map_err(|error| {
            InferenceError::bad_gateway("provider_protocol_error", error.to_string())
        })?;
        return Ok((StatusCode::OK, Json(response)).into_response());
    }
    let upstream_id = record
        .upstream_job_id
        .clone()
        .ok_or_else(|| InferenceError::unavailable("media_job_upstream_id_unavailable"))?;
    let mut operation = decode_video_delete(upstream_id);
    set_video_route(&mut operation, &record.route_slug)?;
    mark_missing_delete_as_success(&mut operation)?;
    let mut executed = execute_routed_result(
        &state,
        &principal,
        operation,
        TransportMode::Unary,
        Some(RequiredTarget {
            provider_id: record.provider_id,
            upstream_model: record.upstream_model.clone(),
        }),
    )
    .await?;
    let mut result = match executed.result.as_ref() {
        CanonicalResult::VideoDelete(result) => result.clone(),
        _ => return Err(incompatible_result("video deletion")),
    };
    if !result.deleted {
        let failure = InferenceError::bad_gateway(
            "video_delete_not_confirmed",
            "The provider did not confirm video deletion.",
        );
        executed.mark_failure(&failure);
        return Err(failure);
    }
    let finalized = media_job_deletion_finalized(state.store(), record.id)
        .await
        .map_err(media_job_error)?;
    if !finalized {
        state.record_media_reconciliation_gap();
        let failure = InferenceError::unavailable("media_job_delete_reconciliation_pending");
        executed.mark_failure(&failure);
        return Err(failure);
    }
    result.id = record.id.to_string();
    let response = encode_video_delete_response(&result).map_err(|error| {
        InferenceError::bad_gateway("provider_protocol_error", error.to_string())
    })?;
    executed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

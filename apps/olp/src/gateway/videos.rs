use crate::public_http::request_admission::HttpRequestAdmission;
use std::collections::{BTreeMap, BTreeSet};

use axum::{
    Json,
    extract::{Extension, Multipart, Path, Query, State},
    http::StatusCode,
    response::{IntoResponse, Response},
};
use futures::{StreamExt, stream};
use olp_db::{
    media_jobs::MediaJobError, media_jobs::MediaJobFilters, media_jobs::MediaJobLifecycle,
    media_jobs::MediaJobOrder, media_jobs::MediaJobRecord, media_jobs::MediaJobUpdate,
    media_jobs::NewMediaJobReservation,
};
use olp_engine::domain::{
    auth::{ApiKey, GatewayCapability},
    canonical::{
        identity::{OperationKind, Surface, TransportMode},
        requests::Operation,
        results::{CanonicalResult, VideoJobResult},
    },
    ids::RouteSlug,
    routing::provider::Provider,
};
use olp_engine::inference::execution::{RequiredTarget, RoutedUnaryResult};
use olp_engine::inference::selection::select_representable_attempts_filtered;
use olp_engine::protocols::openai::video::{
    OpenAiVideoContentQuery, OpenAiVideoCreateRequest, OpenAiVideoListQuery,
    decode_video_content_with_query, decode_video_create, decode_video_delete, decode_video_get,
    encode_video_delete_response, encode_video_list_response, encode_video_object,
};
use tracing::error;

use crate::{
    bootstrap::mode_dependencies::GatewayState,
    public_http::request_admission::multipart::MultipartRequestAdmission,
};

use super::{
    error::InferenceError,
    execution::{
        authorize_principal, defer_unary_outcome_to_body, execute_internal_routed_result,
        execute_routed_result, incompatible_result, mark_unary_outcome,
        mark_unary_outcome_with_status,
    },
    media::{open_response_media, response_from_opened_media},
    media_jobs::{
        attach_media_job_with_retry, mark_missing_delete_as_success, media_job_deletion_finalized,
        media_job_error, media_job_result, media_job_state, media_job_update, owned_media_job,
        refresh_video_list_record, set_video_route, valid_upstream_media_job_id,
    },
    multipart::parse_multipart,
};

fn select_video_create_target(
    state: &GatewayState,
    principal: &HttpRequestAdmission,
    operation: &Operation,
    local_job_id: uuid::Uuid,
) -> Result<(ApiKey, RouteSlug, RequiredTarget), InferenceError> {
    let route_slug = operation
        .route()
        .cloned()
        .ok_or_else(|| InferenceError::invalid_request("A route model is required."))?;
    let key = authorize_principal(
        state,
        principal,
        GatewayCapability::Inference,
        Some(&route_slug),
    )?;
    let snapshot = principal.runtime();
    let route = snapshot
        .routes
        .get(&route_slug)
        .ok_or_else(|| InferenceError::resource_not_found("route_not_found"))?;
    let attempt = select_representable_attempts_filtered(
        snapshot,
        &route_slug,
        operation,
        Surface::OpenAi,
        TransportMode::Async,
        local_job_id.as_bytes(),
        |provider, target| {
            state.circuits().is_selectable(target.id)
                && video_lifecycle_supported(&route.operations, provider, &target.upstream_model)
        },
    )?
    .into_iter()
    .next()
    .ok_or_else(|| InferenceError::unavailable("no_eligible_provider"))?;
    Ok((
        key.clone(),
        route_slug,
        RequiredTarget {
            provider_id: attempt.provider_id.as_uuid(),
            upstream_model: attempt.upstream_model,
        },
    ))
}

pub(super) fn video_lifecycle_supported(
    route_operations: &BTreeSet<OperationKind>,
    provider: &Provider,
    model: &str,
) -> bool {
    [
        OperationKind::VideoGet,
        OperationKind::VideoContent,
        OperationKind::VideoDelete,
    ]
    .into_iter()
    .all(|operation| {
        route_operations.contains(&operation)
            && provider.supports(model, operation, Surface::OpenAi, TransportMode::Unary)
    })
}

pub(super) async fn video_create(
    State(state): State<GatewayState>,
    Extension(principal): Extension<HttpRequestAdmission>,
    Extension(admission): Extension<MultipartRequestAdmission>,
    multipart: Multipart,
) -> Result<Response, InferenceError> {
    let mut form = parse_multipart(
        &state,
        multipart,
        olp_engine::protocols::openai::video::DEFAULT_VIDEO_REFERENCE_LIMIT,
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
    // The accepted upstream create must outlive client disconnects. The
    // admission moves into the task, so it keeps the original runtime
    // generation, limits reservation, and metadata ownership.
    let task = tokio::spawn(complete_video_create(
        state.clone(),
        principal,
        operation,
        reserved,
        required_target,
    ));
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

async fn retire_failed_video_create(
    state: &GatewayState,
    reserved_id: uuid::Uuid,
    failure: &InferenceError,
) {
    if failure.code() == "ambiguous_upstream_result" {
        if let Err(persistence_error) = state
            .store()
            .mark_media_job_create_ambiguous(reserved_id, "upstream_create_result_ambiguous")
            .await
        {
            error!(job_id = %reserved_id, %persistence_error, "failed to mark ambiguous video creation");
        }
        return;
    }

    match media_job_deletion_finalized(state.store(), reserved_id).await {
        Ok(true) => {}
        Ok(false) => {
            state.record_media_reconciliation_gap();
            error!(job_id = %reserved_id, "abandoned video reservation was not finalized");
        }
        Err(persistence_error) => {
            state.record_media_reconciliation_gap();
            error!(job_id = %reserved_id, %persistence_error, "failed to retire abandoned video reservation");
        }
    }
}

async fn prepare_video_create_attachment(
    state: &GatewayState,
    reserved_id: uuid::Uuid,
    required_target: &RequiredTarget,
    executed: &mut RoutedUnaryResult,
) -> Result<(VideoJobResult, String, MediaJobUpdate), InferenceError> {
    let result = match executed.result.as_ref() {
        CanonicalResult::VideoJob(result) => result.clone(),
        _ => {
            let failure = incompatible_result("video creation");
            if let Err(error) = state
                .store()
                .mark_media_job_create_ambiguous(
                    reserved_id,
                    "upstream_create_response_missing_job_identity",
                )
                .await
            {
                state.record_media_reconciliation_gap();
                error!(job_id = %reserved_id, %error, "failed to retire malformed video reservation");
            }
            executed.mark_failure(failure.accounting_outcome());
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
                reserved_id,
                "upstream_create_response_invalid_job_identity",
            )
            .await
        {
            state.record_media_reconciliation_gap();
            error!(job_id = %reserved_id, %error, "failed to retire invalid video reservation");
        }
        executed.mark_failure(failure.accounting_outcome());
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
                    reserved_id,
                    &upstream_job_id,
                    "upstream_create_response_invalid_status",
                )
                .await
            {
                state.record_media_reconciliation_gap();
                error!(job_id = %reserved_id, %error, "failed to schedule malformed video cleanup");
            }
            executed.mark_failure(failure.accounting_outcome());
            return Err(failure);
        }
    };
    let update = media_job_update(&result, state_update);
    Ok((result, upstream_job_id, update))
}

async fn persist_video_create_cleanup_intent(
    state: &GatewayState,
    reserved_id: uuid::Uuid,
    upstream_job_id: &str,
    identity_conflict: bool,
) -> bool {
    if identity_conflict {
        return false;
    }

    match state
        .store()
        .mark_media_job_create_cleanup_pending(
            reserved_id,
            upstream_job_id,
            "upstream_created_local_attach_failed",
        )
        .await
    {
        Ok(record)
            if record.lifecycle == MediaJobLifecycle::CreateCleanupPending
                && record.upstream_job_id.as_deref() == Some(upstream_job_id) =>
        {
            true
        }
        Ok(record) => {
            error!(
                job_id = %reserved_id,
                lifecycle = record.lifecycle.as_str(),
                "video cleanup intent did not retain the upstream identity"
            );
            false
        }
        Err(persistence_error) => {
            error!(job_id = %reserved_id, %persistence_error, "failed to persist video cleanup reconciliation metadata");
            false
        }
    }
}

async fn compensate_video_create(
    state: &GatewayState,
    principal: &HttpRequestAdmission,
    executed: &RoutedUnaryResult,
    upstream_job_id: &str,
    required_target: RequiredTarget,
) -> Result<bool, InferenceError> {
    let mut cleanup = decode_video_delete(upstream_job_id.to_owned());
    set_video_route(&mut cleanup, executed.route_slug.as_str())?;
    mark_missing_delete_as_success(&mut cleanup)?;
    let mut compensation = execute_internal_routed_result(
        state,
        principal,
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
            Ok(true)
        }
        Ok(compensation) => {
            let failure = incompatible_result("video deletion");
            compensation.mark_failure(failure.accounting_outcome());
            Ok(false)
        }
        Err(_) => Ok(false),
    }
}

async fn handle_failed_video_attachment(
    state: &GatewayState,
    principal: &HttpRequestAdmission,
    reserved_id: uuid::Uuid,
    upstream_job_id: &str,
    required_target: RequiredTarget,
    executed: &mut RoutedUnaryResult,
    attachment_error: MediaJobError,
) -> InferenceError {
    let cleanup_intent_persisted = persist_video_create_cleanup_intent(
        state,
        reserved_id,
        upstream_job_id,
        matches!(attachment_error, MediaJobError::UpstreamIdentityConflict),
    )
    .await;
    let compensation_confirmed = if cleanup_intent_persisted {
        match compensate_video_create(state, principal, executed, upstream_job_id, required_target)
            .await
        {
            Ok(confirmed) => confirmed,
            Err(failure) => {
                executed.mark_failure(failure.accounting_outcome());
                return failure;
            }
        }
    } else {
        false
    };

    if compensation_confirmed {
        match media_job_deletion_finalized(state.store(), reserved_id).await {
            Ok(true) => {}
            Ok(false) => {
                state.record_media_reconciliation_gap();
                error!(job_id = %reserved_id, "upstream cleanup succeeded but reconciliation tombstone was not finalized");
            }
            Err(persistence_error) => {
                state.record_media_reconciliation_gap();
                error!(job_id = %reserved_id, %persistence_error, "upstream cleanup succeeded but reconciliation tombstone failed");
            }
        }
    } else {
        state.record_media_reconciliation_gap();
        error!(
            job_id = %reserved_id,
            upstream_job_id,
            provider_id = %executed.provider_id,
            route = %executed.route_slug,
            "video create reconciliation gap requires operator attention"
        );
    }

    let failure = InferenceError::unavailable("media_job_create_reconciliation_pending");
    executed.mark_failure(failure.accounting_outcome());
    failure
}

async fn complete_video_create(
    state: GatewayState,
    principal: HttpRequestAdmission,
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
        Err(failure) => {
            retire_failed_video_create(&state, reserved.id, &failure).await;
            return Err(failure);
        }
    };
    let (mut result, upstream_job_id, update) =
        prepare_video_create_attachment(&state, reserved.id, &required_target, &mut executed)
            .await?;
    let record =
        match attach_media_job_with_retry(&state, reserved.id, &upstream_job_id, update).await {
            Ok(record) => record,
            Err(error) => {
                // A compensation DELETE is only safe after PostgreSQL records the
                // upstream identity and cleanup intent. An ambiguous attachment
                // outcome can already have committed the active row.
                return Err(handle_failed_video_attachment(
                    &state,
                    &principal,
                    reserved.id,
                    &upstream_job_id,
                    required_target,
                    &mut executed,
                    error,
                )
                .await);
            }
        };
    result.id = record.id.to_string();
    result.model = Some(executed.route_slug.to_string());
    let response = encode_video_object(&result, executed.route_slug.as_str())
        .map_err(|error| InferenceError::bad_gateway("provider_protocol_error", error.to_string()));
    mark_unary_outcome_with_status(&mut executed, &response, StatusCode::CREATED);
    let response = response?;
    Ok((StatusCode::CREATED, Json(response)).into_response())
}

pub(super) async fn video_list(
    State(state): State<GatewayState>,
    Extension(principal): Extension<HttpRequestAdmission>,
    Query(query): Query<OpenAiVideoListQuery>,
) -> Result<Response, InferenceError> {
    let key = authorize_principal(&state, &principal, GatewayCapability::Inference, None)?;
    let page_request = validate_video_list_query(&query)?;
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
            page_request.cursor,
            page_request.order,
            page_request.limit,
        )
        .await
        .map_err(media_job_error)?;
    let refreshed = stream::iter(page.items)
        .map(|record| refresh_video_list_record(&state, &principal, record))
        .buffered(4)
        .collect::<Vec<_>>()
        .await;
    let jobs = refreshed.iter().map(media_job_result).collect::<Vec<_>>();
    let result = olp_engine::domain::canonical::results::VideoListResult {
        first_id: jobs.first().map(|job| job.id.clone()),
        last_id: jobs.last().map(|job| job.id.clone()),
        jobs,
        has_more: page.next_cursor.is_some(),
        extensions: olp_engine::domain::canonical::requests::SourceExtensions::new(
            Surface::OpenAi,
            BTreeMap::new(),
        ),
    };
    let response = encode_video_list_response(&result, "video").map_err(|error| {
        InferenceError::bad_gateway("provider_protocol_error", error.to_string())
    })?;
    Ok((StatusCode::OK, Json(response)).into_response())
}

#[derive(Debug, Eq, PartialEq)]
struct ValidatedVideoListQuery {
    cursor: Option<uuid::Uuid>,
    order: MediaJobOrder,
    limit: u16,
}

fn validate_video_list_query(
    query: &OpenAiVideoListQuery,
) -> Result<ValidatedVideoListQuery, InferenceError> {
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
    Ok(ValidatedVideoListQuery {
        cursor,
        order,
        limit: query.limit.unwrap_or(20),
    })
}

pub(super) async fn video_get(
    State(state): State<GatewayState>,
    Extension(principal): Extension<HttpRequestAdmission>,
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
        _ => {
            executed.mark_provider_protocol_failure();
            return Err(incompatible_result("video status"));
        }
    };
    let state_update = match media_job_state(&result.status) {
        Ok(state) => state,
        Err(failure) => {
            executed.mark_failure(failure.accounting_outcome());
            return Err(failure);
        }
    };
    let updated = match state
        .store()
        .refresh_media_job(record.id, media_job_update(&result, state_update))
        .await
    {
        Ok(updated) => updated,
        Err(error) => {
            let failure = media_job_error(error);
            executed.mark_failure(failure.accounting_outcome());
            return Err(failure);
        }
    };
    result.id = updated.id.to_string();
    result.model = Some(updated.route_slug.clone());
    let response = encode_video_object(&result, &updated.route_slug)
        .map_err(|error| InferenceError::bad_gateway("provider_protocol_error", error.to_string()));
    mark_unary_outcome(&mut executed, &response);
    let response = response?;
    Ok((StatusCode::OK, Json(response)).into_response())
}

pub(super) async fn video_content(
    State(state): State<GatewayState>,
    Extension(principal): Extension<HttpRequestAdmission>,
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
        _ => {
            executed.mark_provider_protocol_failure();
            return Err(incompatible_result("video content"));
        }
    };
    let opened = match open_response_media(&state, &result.media.handle).await {
        Ok(opened) => opened,
        Err(failure) => {
            executed.mark_failure(failure.accounting_outcome());
            return Err(failure);
        }
    };
    let response = response_from_opened_media(
        opened,
        state.media_spool().clone(),
        "The provider returned an invalid video content type.",
        "The provider returned an invalid video length.",
    );
    defer_unary_outcome_to_body(&mut executed, response)
}

pub(super) async fn video_delete(
    State(state): State<GatewayState>,
    Extension(principal): Extension<HttpRequestAdmission>,
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
        let response = encode_video_delete_response(
            &olp_engine::domain::canonical::results::VideoDeleteResult {
                id: record.id.to_string(),
                deleted: true,
                extensions: olp_engine::domain::canonical::requests::SourceExtensions::new(
                    Surface::OpenAi,
                    BTreeMap::new(),
                ),
            },
        )
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
        _ => {
            executed.mark_provider_protocol_failure();
            return Err(incompatible_result("video deletion"));
        }
    };
    if !result.deleted {
        let failure = InferenceError::bad_gateway(
            "video_delete_not_confirmed",
            "The provider did not confirm video deletion.",
        );
        executed.mark_failure(failure.accounting_outcome());
        return Err(failure);
    }
    let finalized = match media_job_deletion_finalized(state.store(), record.id).await {
        Ok(finalized) => finalized,
        Err(error) => {
            let failure = media_job_error(error);
            executed.mark_failure(failure.accounting_outcome());
            return Err(failure);
        }
    };
    if !finalized {
        state.record_media_reconciliation_gap();
        let failure = InferenceError::unavailable("media_job_delete_reconciliation_pending");
        executed.mark_failure(failure.accounting_outcome());
        return Err(failure);
    }
    result.id = record.id.to_string();
    let response = encode_video_delete_response(&result)
        .map_err(|error| InferenceError::bad_gateway("provider_protocol_error", error.to_string()));
    mark_unary_outcome(&mut executed, &response);
    let response = response?;
    Ok((StatusCode::OK, Json(response)).into_response())
}

#[cfg(test)]
mod tests {
    use serde_json::Value;

    use super::*;

    fn list_query(
        after: Option<&str>,
        limit: Option<u16>,
        order: Option<&str>,
    ) -> OpenAiVideoListQuery {
        OpenAiVideoListQuery {
            after: after.map(str::to_owned),
            limit,
            order: order.map(str::to_owned),
            extra: BTreeMap::new(),
        }
    }

    #[test]
    fn video_list_query_defaults_are_explicit() {
        assert_eq!(
            validate_video_list_query(&list_query(None, None, None)).unwrap(),
            ValidatedVideoListQuery {
                cursor: None,
                order: MediaJobOrder::Descending,
                limit: 20,
            }
        );
    }

    #[test]
    fn video_list_query_accepts_supported_boundaries_and_cursor() {
        let cursor = uuid::Uuid::now_v7();
        for limit in [1, 100] {
            assert_eq!(
                validate_video_list_query(&list_query(
                    Some(&cursor.to_string()),
                    Some(limit),
                    Some("asc"),
                ))
                .unwrap(),
                ValidatedVideoListQuery {
                    cursor: Some(cursor),
                    order: MediaJobOrder::Ascending,
                    limit,
                }
            );
        }
    }

    #[test]
    fn video_list_query_rejects_each_unsupported_shape() {
        let cases = [
            (
                list_query(None, Some(0), None),
                "Video list limit must be between 1 and 100.",
            ),
            (
                list_query(None, Some(101), None),
                "Video list limit must be between 1 and 100.",
            ),
            (
                list_query(None, None, Some("newest")),
                "Video list order must be asc or desc.",
            ),
            (
                list_query(Some("not-a-uuid"), None, None),
                "The video cursor is invalid.",
            ),
        ];
        for (query, expected_message) in cases {
            let error = validate_video_list_query(&query).unwrap_err();
            assert_eq!(error.message(), expected_message);
        }

        let mut query = list_query(None, None, None);
        query.extra.insert("unknown".into(), Value::Bool(true));
        let error = validate_video_list_query(&query).unwrap_err();
        assert_eq!(
            error.message(),
            "Video list contains unsupported query parameters."
        );
    }
}

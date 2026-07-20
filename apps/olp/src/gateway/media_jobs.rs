use std::collections::BTreeMap;

use axum::http::{HeaderMap, StatusCode};
use chrono::Utc;
use futures::{StreamExt, stream};
use olp_domain::{
    ApiKey, CanonicalResult, MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION, Operation, OperationKind,
    RequestId, RequestMetadata, RouteSlug, Surface, TransportMode, authorize_api_key,
};
use olp_storage::{
    MediaJobError, MediaJobLifecycle, MediaJobRecord, MediaJobState, MediaJobUpdate,
    MediaReconciliationPass,
};
use serde_json::Value;
use tracing::{error, warn};

use crate::{ApiState, semantic_validation::select_representable_attempts_filtered};

use super::{
    error::InferenceError,
    execution::{RequiredTarget, authenticate_key, execute_routed_result},
    failover::{ExecutionOutput, execute_with_failover},
    telemetry::{UsageCapture, elapsed_ms, emit_request_event, usage_from_result},
};

pub(super) fn select_video_create_target(
    state: &ApiState,
    headers: &HeaderMap,
    operation: &Operation,
    local_job_id: uuid::Uuid,
) -> Result<(ApiKey, RouteSlug, RequiredTarget), InferenceError> {
    let route_slug = operation
        .route()
        .cloned()
        .ok_or_else(|| InferenceError::invalid_request("A route model is required."))?;
    let key = authenticate_key(
        state,
        headers,
        OperationKind::VideoCreate,
        Some(&route_slug),
    )?;
    let snapshot = crate::pin_inference_runtime(state);
    let attempt = select_representable_attempts_filtered(
        &snapshot,
        &route_slug,
        operation,
        Surface::OpenAi,
        TransportMode::Async,
        local_job_id.as_bytes(),
        |_, target| state.circuits.is_selectable(target.id),
    )?
    .into_iter()
    .next()
    .ok_or_else(|| InferenceError::unavailable("no_eligible_provider"))?;
    Ok((
        key,
        route_slug,
        RequiredTarget {
            provider_id: attempt.provider_id.as_uuid(),
            provider_model: attempt.provider_model,
        },
    ))
}

pub(super) async fn media_job_deletion_finalized(
    store: &olp_storage::PgStore,
    id: uuid::Uuid,
) -> Result<bool, MediaJobError> {
    if store.finalize_media_job_deletion(id).await? {
        return Ok(true);
    }
    Ok(store.media_job(id).await?.lifecycle == MediaJobLifecycle::Deleted)
}

pub(super) fn mark_missing_delete_as_success(
    operation: &mut Operation,
) -> Result<(), InferenceError> {
    let Operation::Video(olp_domain::VideoOperation::Delete(request)) = operation else {
        return Err(InferenceError::unavailable("media_job_operation_invalid"));
    };
    request.extensions.source = Some(Surface::OpenAi);
    request.extensions.values.insert(
        MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION.to_owned(),
        Value::Bool(true),
    );
    Ok(())
}

pub(super) fn valid_upstream_media_job_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 1_024
        && value.trim() == value
        && !value.chars().any(char::is_control)
}

/// Claims and reconciles a bounded metadata-only batch without authenticating
/// the API key that originally created each job. This is intentionally public
/// for the single-binary process supervisor; it is not an HTTP endpoint.
pub async fn reconcile_media_jobs_once(
    state: &ApiState,
    limit: u16,
) -> Result<MediaReconciliationPass, MediaJobError> {
    let records = require_inference_store(state)
        .map_err(|_| MediaJobError::Invalid("media persistence is not configured".to_owned()))?
        .claim_media_reconciliation_jobs(Utc::now(), limit)
        .await?;
    let claimed = u16::try_from(records.len()).unwrap_or(u16::MAX);
    let outcomes = stream::iter(records)
        .map(|record| reconcile_claimed_media_job(state, record))
        .buffer_unordered(4)
        .collect::<Vec<_>>()
        .await;
    let completed =
        u16::try_from(outcomes.iter().filter(|value| **value).count()).unwrap_or(u16::MAX);
    Ok(MediaReconciliationPass {
        claimed,
        completed,
        failed: claimed.saturating_sub(completed),
    })
}

async fn reconcile_claimed_media_job(state: &ApiState, mut record: MediaJobRecord) -> bool {
    let Some(claim_id) = record.reconciliation_claim_id else {
        state.record_media_reconciliation_gap();
        return false;
    };
    let store = require_inference_store(state)
        .expect("claimed media reconciliation always has a configured store");
    let outcome = reconcile_media_job_operation(state, &mut record).await;
    let now = Utc::now();
    let (next_attempt_at, error_class) = match outcome {
        Ok(()) => {
            let next = if matches!(record.state, MediaJobState::Queued | MediaJobState::Running)
                && record.lifecycle == MediaJobLifecycle::Active
            {
                now + chrono::Duration::seconds(5)
            } else {
                now + chrono::Duration::hours(24)
            };
            (next, None)
        }
        Err(code) => {
            let exponent = record.reconciliation_attempts.min(6);
            let seconds = 5_i64.saturating_mul(1_i64 << exponent).min(300);
            (now + chrono::Duration::seconds(seconds), Some(code))
        }
    };
    if let Err(error) = store
        .finish_media_reconciliation(record.id, claim_id, next_attempt_at, error_class)
        .await
    {
        state.record_media_reconciliation_gap();
        error!(job_id = %record.id, %error, "failed to checkpoint autonomous media reconciliation");
        return false;
    }
    if let Some(code) = error_class {
        warn!(job_id = %record.id, error_class = code, "autonomous media reconciliation will retry");
        false
    } else {
        true
    }
}

async fn reconcile_media_job_operation(
    state: &ApiState,
    record: &mut MediaJobRecord,
) -> Result<(), &'static str> {
    let store = require_inference_store(state).map_err(|_| "persistence_unavailable")?;
    match record.lifecycle {
        MediaJobLifecycle::Creating => {
            if let Some(upstream_id) = record.upstream_job_id.as_deref() {
                *record = store
                    .mark_media_job_create_cleanup_pending(
                        record.id,
                        upstream_id,
                        "stale_post_create_reservation",
                    )
                    .await
                    .map_err(|_| "persistence_unavailable")?;
            } else {
                *record = store
                    .mark_media_job_create_ambiguous(
                        record.id,
                        "upstream_create_outcome_unknown_after_restart",
                    )
                    .await
                    .map_err(|_| "persistence_unavailable")?;
                return Err("upstream_create_outcome_unknown");
            }
        }
        MediaJobLifecycle::CreateAmbiguous => {
            let Some(upstream_id) = record.upstream_job_id.as_deref() else {
                return Err("upstream_create_outcome_unknown");
            };
            *record = store
                .mark_media_job_create_cleanup_pending(
                    record.id,
                    upstream_id,
                    "ambiguous_create_has_cleanup_identity",
                )
                .await
                .map_err(|_| "persistence_unavailable")?;
        }
        MediaJobLifecycle::Deleted => return Ok(()),
        MediaJobLifecycle::Active
        | MediaJobLifecycle::CreateCleanupPending
        | MediaJobLifecycle::DeletePending => {}
    }

    if record.lifecycle == MediaJobLifecycle::Active
        && (record
            .expires_at
            .is_some_and(|expires_at| expires_at <= Utc::now())
            || record.created_at <= Utc::now() - chrono::Duration::days(30))
    {
        *record = store
            .begin_media_job_deletion(record.id)
            .await
            .map_err(|_| "persistence_unavailable")?;
    }

    let upstream_id = record
        .upstream_job_id
        .clone()
        .filter(|value| valid_upstream_media_job_id(value))
        .ok_or("media_job_upstream_id_unavailable")?;
    if record.lifecycle == MediaJobLifecycle::Active {
        let mut operation = olp_protocols::openai::decode_video_get(upstream_id);
        set_video_route(&mut operation, &record.route_slug).map_err(|error| error.code)?;
        let result = execute_media_reconciliation_result(state, record, operation).await?;
        let CanonicalResult::VideoJob(result) = result.as_ref() else {
            return Err("provider_protocol_error");
        };
        let state_update = media_job_state(&result.status).map_err(|error| error.code)?;
        *record = store
            .refresh_media_job(
                record.id,
                MediaJobUpdate {
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
                },
            )
            .await
            .map_err(|_| "persistence_unavailable")?;
        return Ok(());
    }

    let mut operation = olp_protocols::openai::decode_video_delete(upstream_id);
    set_video_route(&mut operation, &record.route_slug).map_err(|error| error.code)?;
    mark_missing_delete_as_success(&mut operation).map_err(|error| error.code)?;
    let result = execute_media_reconciliation_result(state, record, operation).await?;
    if !matches!(
        result.as_ref(),
        CanonicalResult::VideoDelete(deleted) if deleted.deleted
    ) {
        return Err("video_delete_not_confirmed");
    }
    let finalized = media_job_deletion_finalized(store, record.id)
        .await
        .map_err(|_| "persistence_unavailable")?;
    if !finalized {
        state.record_media_reconciliation_gap();
        return Err("persistence_unavailable");
    }
    record.lifecycle = MediaJobLifecycle::Deleted;
    Ok(())
}

async fn execute_media_reconciliation_result(
    state: &ApiState,
    record: &MediaJobRecord,
    operation: Operation,
) -> Result<Box<CanonicalResult>, &'static str> {
    let snapshot = state.runtime.pin();
    let route_slug = operation
        .route()
        .cloned()
        .ok_or("media_job_route_invalid")?;
    let request_id = RequestId::new();
    let request_started_at = Utc::now();
    let request_started = tokio::time::Instant::now();
    let operation_kind = operation.kind();
    let attempts = match select_representable_attempts_filtered(
        &snapshot,
        &route_slug,
        &operation,
        Surface::OpenAi,
        TransportMode::Unary,
        request_id.as_uuid().as_bytes(),
        |_, target| {
            target.provider_id.as_uuid() == record.provider_id
                && target.provider_model == record.provider_model
                && state.circuits.is_selectable(target.id)
        },
    ) {
        Ok(attempts) => attempts,
        Err(failure) => {
            emit_request_event(
                state,
                snapshot.generation.id.as_uuid(),
                record.api_key_id,
                request_id.as_uuid(),
                &route_slug,
                &[],
                request_started_at,
                request_started,
                None,
                None,
                Some(failure.status.as_u16()),
                Some(failure.code.to_owned()),
                false,
                &UsageCapture::default(),
                Surface::OpenAi,
                operation_kind.as_str(),
            );
            return Err(failure.code);
        }
    };
    let route = snapshot
        .routes
        .get(&route_slug)
        .ok_or("media_job_route_invalid")?;
    let execution = execute_with_failover(
        &snapshot,
        attempts,
        RequestMetadata {
            request_id,
            operation: operation_kind,
            surface: Surface::OpenAi,
            mode: TransportMode::Unary,
        },
        operation,
        route.overall_timeout.as_duration(),
        state.media_spool.clone(),
        &state.circuits,
    )
    .await;
    let success = match execution {
        Ok(success) => success,
        Err(failure) => {
            emit_request_event(
                state,
                snapshot.generation.id.as_uuid(),
                record.api_key_id,
                request_id.as_uuid(),
                &route_slug,
                &failure.attempts,
                request_started_at,
                request_started,
                None,
                None,
                Some(failure.error.status.as_u16()),
                Some(failure.error.code.to_owned()),
                false,
                &UsageCapture::default(),
                Surface::OpenAi,
                operation_kind.as_str(),
            );
            return Err(failure.error.code);
        }
    };
    let ExecutionOutput::Result(result) = success.output else {
        emit_request_event(
            state,
            snapshot.generation.id.as_uuid(),
            record.api_key_id,
            request_id.as_uuid(),
            &route_slug,
            &success.attempts,
            request_started_at,
            request_started,
            Some(success.attempt_started),
            Some(elapsed_ms(request_started.elapsed())),
            Some(StatusCode::BAD_GATEWAY.as_u16()),
            Some("provider_protocol_error".to_owned()),
            true,
            &UsageCapture::default(),
            Surface::OpenAi,
            operation_kind.as_str(),
        );
        return Err("provider_protocol_error");
    };
    let first_byte_ms = elapsed_ms(request_started.elapsed());
    emit_request_event(
        state,
        snapshot.generation.id.as_uuid(),
        record.api_key_id,
        request_id.as_uuid(),
        &route_slug,
        &success.attempts,
        request_started_at,
        request_started,
        Some(success.attempt_started),
        Some(first_byte_ms),
        Some(StatusCode::OK.as_u16()),
        None,
        true,
        &usage_from_result(&result),
        Surface::OpenAi,
        operation_kind.as_str(),
    );
    Ok(result)
}

pub(super) async fn refresh_video_list_record(
    state: &ApiState,
    headers: &HeaderMap,
    record: MediaJobRecord,
) -> MediaJobRecord {
    if !matches!(record.state, MediaJobState::Queued | MediaJobState::Running) {
        return record;
    }
    let Some(upstream_id) = record.upstream_job_id.clone() else {
        return record;
    };
    let mut operation = olp_protocols::openai::decode_video_get(upstream_id);
    if set_video_route(&mut operation, &record.route_slug).is_err() {
        return record;
    }
    let Ok(mut executed) = execute_routed_result(
        state,
        headers,
        operation,
        TransportMode::Unary,
        Some(RequiredTarget {
            provider_id: record.provider_id,
            provider_model: record.provider_model.clone(),
        }),
    )
    .await
    else {
        return record;
    };
    let result = match executed.result.as_ref() {
        CanonicalResult::VideoJob(result) => result.clone(),
        _ => return record,
    };
    let Ok(state_update) = media_job_state(&result.status) else {
        return record;
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
    let updated = require_inference_store(state)
        .expect("list refresh runs only with a configured store")
        .refresh_media_job(record.id, update)
        .await
        .unwrap_or(record);
    executed.mark_success();
    updated
}

pub(super) async fn owned_media_job(
    state: &ApiState,
    headers: &HeaderMap,
    video_id: &str,
    operation: OperationKind,
) -> Result<(ApiKey, MediaJobRecord), InferenceError> {
    let key = authenticate_key(state, headers, operation, None)?;
    let id = uuid::Uuid::parse_str(video_id)
        .map_err(|_| InferenceError::resource_not_found("video_not_found"))?;
    let record = require_inference_store(state)?
        .media_job(id)
        .await
        .map_err(media_job_error)?;
    if record.api_key_id != key.id.as_uuid() {
        return Err(InferenceError::resource_not_found("video_not_found"));
    }
    if record.lifecycle == MediaJobLifecycle::Deleted && operation != OperationKind::VideoDelete {
        return Err(InferenceError::resource_not_found("video_not_found"));
    }
    if !matches!(
        record.lifecycle,
        MediaJobLifecycle::Active | MediaJobLifecycle::DeletePending | MediaJobLifecycle::Deleted
    ) {
        return Err(InferenceError::unavailable(
            "media_job_reconciliation_pending",
        ));
    }
    let route = RouteSlug::parse(&record.route_slug)
        .map_err(|_| InferenceError::unavailable("media_job_route_invalid"))?;
    authorize_api_key(&key, Some(&route), operation, Utc::now())
        .map_err(|error| InferenceError::forbidden(error.to_string()))?;
    Ok((key, record))
}

pub(super) fn set_video_route(
    operation: &mut Operation,
    route: &str,
) -> Result<(), InferenceError> {
    let route = RouteSlug::parse(route)
        .map_err(|_| InferenceError::unavailable("media_job_route_invalid"))?;
    let Operation::Video(operation) = operation else {
        return Err(InferenceError::unavailable("media_job_operation_invalid"));
    };
    match operation {
        olp_domain::VideoOperation::Get(request)
        | olp_domain::VideoOperation::Content(request)
        | olp_domain::VideoOperation::Delete(request) => request.route = Some(route),
        _ => return Err(InferenceError::unavailable("media_job_operation_invalid")),
    }
    Ok(())
}

pub(super) fn require_inference_store(
    state: &ApiState,
) -> Result<&olp_storage::PgStore, InferenceError> {
    state
        .store
        .as_ref()
        .ok_or_else(|| InferenceError::unavailable("persistence_unavailable"))
}

pub(super) fn media_job_error(error: MediaJobError) -> InferenceError {
    match error {
        MediaJobError::NotFound => InferenceError::resource_not_found("video_not_found"),
        MediaJobError::PreconditionFailed => InferenceError {
            status: StatusCode::CONFLICT,
            code: "video_changed",
            kind: "conflict_error",
            message: "The video job changed; retry the request.".into(),
            retry_after: None,
        },
        MediaJobError::UpstreamIdentityConflict => {
            InferenceError::unavailable("media_job_upstream_identity_conflict")
        }
        MediaJobError::Invalid(message) => InferenceError::invalid_request(message),
        MediaJobError::Database(_) => InferenceError::unavailable("persistence_unavailable"),
    }
}

pub(super) fn media_job_state(
    status: &olp_domain::VideoStatus,
) -> Result<MediaJobState, InferenceError> {
    match status {
        olp_domain::VideoStatus::Queued => Ok(MediaJobState::Queued),
        olp_domain::VideoStatus::InProgress => Ok(MediaJobState::Running),
        olp_domain::VideoStatus::Completed => Ok(MediaJobState::Succeeded),
        olp_domain::VideoStatus::Failed => Ok(MediaJobState::Failed),
        olp_domain::VideoStatus::Other(status) => Err(InferenceError::bad_gateway(
            "provider_protocol_error",
            format!("The provider returned an unsupported video status: {status}."),
        )),
    }
}

pub(super) fn media_job_result(record: &MediaJobRecord) -> olp_domain::VideoJobResult {
    let status = match record.state {
        MediaJobState::Queued => olp_domain::VideoStatus::Queued,
        MediaJobState::Running => olp_domain::VideoStatus::InProgress,
        MediaJobState::Succeeded => olp_domain::VideoStatus::Completed,
        MediaJobState::Failed => olp_domain::VideoStatus::Failed,
        MediaJobState::Cancelled => olp_domain::VideoStatus::Other("cancelled".into()),
    };
    olp_domain::VideoJobResult {
        id: record.id.to_string(),
        model: Some(record.route_slug.clone()),
        status,
        progress_percent: record.progress_percent,
        created_at: Some(record.created_at.timestamp()),
        completed_at: record.completed_at.map(|value| value.timestamp()),
        expires_at: record.expires_at.map(|value| value.timestamp()),
        prompt: None,
        seconds: None,
        size: None,
        error: None,
        extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    }
}

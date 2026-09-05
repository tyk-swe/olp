use super::{
    MediaJobs, attach_media_job_with_retry, media_job_deletion_finalized,
    results::{
        mark_missing_delete_as_success, media_job_state, media_job_update, set_video_route,
        valid_upstream_media_job_id,
    },
};
use olp_db::media_jobs::{MediaJobError, MediaJobLifecycle, MediaJobRecord, MediaJobUpdate};
use olp_engine::{
    domain::canonical::{
        identity::TransportMode,
        requests::Operation,
        results::{CanonicalResult, VideoJobResult},
    },
    inference::{
        accounting::RequestOutcome,
        error::Error as InferenceError,
        execution::{RequestAdmission, RequiredTarget, RoutedUnaryResult},
        principal::Principal,
    },
    protocols::openai::video::decode_video_delete,
};
use tracing::error;

pub(crate) struct VideoCreateAdmission<'a> {
    pub(crate) principal: &'a Principal,
    pub(crate) request: RequestAdmission,
    pub(crate) compensation: RequestAdmission,
}

pub(crate) async fn complete_video_create(
    state: &MediaJobs,
    admission: VideoCreateAdmission<'_>,
    operation: Operation,
    reserved: MediaJobRecord,
    required_target: RequiredTarget,
) -> Result<(VideoJobResult, RoutedUnaryResult), InferenceError> {
    let mut executed = match state
        .inference
        .execute_result(
            admission.principal,
            operation,
            TransportMode::Async,
            Some(required_target.clone()),
            admission.request.clone(),
        )
        .await
    {
        Ok(executed) => executed,
        Err(failure) => {
            retire_failed_video_create(state, reserved.id, &failure).await;
            return Err(failure);
        }
    };
    let (mut result, upstream_job_id, update) =
        prepare_video_create_attachment(state, reserved.id, &required_target, &mut executed)
            .await?;
    let record = match attach_media_job_with_retry(
        &state.store,
        reserved.id,
        &upstream_job_id,
        update,
    )
    .await
    {
        Ok(record) => record,
        Err(error) => {
            // A compensation DELETE is only safe after PostgreSQL records the
            // upstream identity and cleanup intent. An ambiguous attachment
            // outcome can already have committed the active row.
            return Err(handle_failed_video_attachment(
                state,
                &admission,
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
    Ok((result, executed))
}

async fn retire_failed_video_create(
    state: &MediaJobs,
    reserved_id: uuid::Uuid,
    failure: &InferenceError,
) {
    if failure.code() == "ambiguous_upstream_result" {
        if let Err(persistence_error) = state
            .store
            .mark_media_job_create_ambiguous(reserved_id, "upstream_create_result_ambiguous")
            .await
        {
            error!(job_id = %reserved_id, %persistence_error, "failed to mark ambiguous video creation");
        }
        return;
    }

    match media_job_deletion_finalized(&state.store, reserved_id).await {
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
    state: &MediaJobs,
    reserved_id: uuid::Uuid,
    required_target: &RequiredTarget,
    executed: &mut RoutedUnaryResult,
) -> Result<(VideoJobResult, String, MediaJobUpdate), InferenceError> {
    let result = match executed.result.as_ref() {
        CanonicalResult::VideoJob(result) => result.clone(),
        _ => {
            let failure = InferenceError::bad_gateway(
                "provider_protocol_error",
                "The provider returned an incompatible video creation response.",
            );
            if let Err(error) = state
                .store
                .mark_media_job_create_ambiguous(
                    reserved_id,
                    "upstream_create_response_missing_job_identity",
                )
                .await
            {
                state.record_media_reconciliation_gap();
                error!(job_id = %reserved_id, %error, "failed to retire malformed video reservation");
            }
            executed.mark_failure(RequestOutcome::from_error(&failure));
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
            .store
            .mark_media_job_create_ambiguous(
                reserved_id,
                "upstream_create_response_invalid_job_identity",
            )
            .await
        {
            state.record_media_reconciliation_gap();
            error!(job_id = %reserved_id, %error, "failed to retire invalid video reservation");
        }
        executed.mark_failure(RequestOutcome::from_error(&failure));
        return Err(failure);
    }
    debug_assert_eq!(executed.provider_id, required_target.provider_id);
    debug_assert_eq!(executed.upstream_model, required_target.upstream_model);
    let state_update = match media_job_state(&result.status) {
        Ok(state_update) => state_update,
        Err(failure) => {
            if let Err(error) = state
                .store
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
            executed.mark_failure(RequestOutcome::from_error(&failure));
            return Err(failure);
        }
    };
    let update = media_job_update(&result, state_update);
    Ok((result, upstream_job_id, update))
}

async fn persist_video_create_cleanup_intent(
    state: &MediaJobs,
    reserved_id: uuid::Uuid,
    upstream_job_id: &str,
    identity_conflict: bool,
) -> bool {
    if identity_conflict {
        return false;
    }

    match state
        .store
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
    state: &MediaJobs,
    admission: &VideoCreateAdmission<'_>,
    executed: &RoutedUnaryResult,
    upstream_job_id: &str,
    required_target: RequiredTarget,
) -> Result<bool, InferenceError> {
    let mut cleanup = decode_video_delete(upstream_job_id.to_owned());
    set_video_route(&mut cleanup, executed.route_slug.as_str())?;
    mark_missing_delete_as_success(&mut cleanup)?;
    let mut compensation = state
        .inference
        .execute_result(
            admission.principal,
            cleanup,
            TransportMode::Unary,
            Some(required_target),
            admission.compensation.clone(),
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
            let failure = InferenceError::bad_gateway(
                "provider_protocol_error",
                "The provider returned an incompatible video deletion response.",
            );
            compensation.mark_failure(RequestOutcome::from_error(&failure));
            Ok(false)
        }
        Err(_) => Ok(false),
    }
}

async fn handle_failed_video_attachment(
    state: &MediaJobs,
    admission: &VideoCreateAdmission<'_>,
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
        match compensate_video_create(state, admission, executed, upstream_job_id, required_target)
            .await
        {
            Ok(confirmed) => confirmed,
            Err(failure) => {
                executed.mark_failure(RequestOutcome::from_error(&failure));
                return failure;
            }
        }
    } else {
        false
    };

    if compensation_confirmed {
        match media_job_deletion_finalized(&state.store, reserved_id).await {
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
    executed.mark_failure(RequestOutcome::from_error(&failure));
    failure
}

use super::{
    error::InferenceError,
    execution::{authorize_principal, execute_internal_routed_result},
    state::GatewayState,
};
use crate::{
    application::media_jobs::results::{media_job_state, media_job_update, set_video_route},
    public_http::request_admission::HttpRequestAdmission,
};
use chrono::Utc;
use olp_db::media_jobs::{MediaJobError, MediaJobLifecycle, MediaJobRecord, MediaJobState};
use olp_engine::{
    domain::{
        auth::{ApiKey, authorize_api_key, gateway_capability_for_operation},
        canonical::{
            identity::{OperationKind, TransportMode},
            results::CanonicalResult,
        },
        ids::RouteSlug,
    },
    inference::execution::RequiredTarget,
};
pub(super) async fn refresh_video_list_record(
    state: &GatewayState,
    principal: &HttpRequestAdmission,
    record: MediaJobRecord,
) -> MediaJobRecord {
    if !matches!(record.state, MediaJobState::Queued | MediaJobState::Running) {
        return record;
    }
    let Some(upstream_id) = record.upstream_job_id.clone() else {
        return record;
    };
    let mut operation = olp_engine::protocols::openai::video::decode_video_get(upstream_id);
    if set_video_route(&mut operation, &record.route_slug).is_err() {
        return record;
    }
    let Ok(mut executed) = execute_internal_routed_result(
        state,
        principal,
        operation,
        TransportMode::Unary,
        Some(RequiredTarget {
            provider_id: record.provider_id,
            upstream_model: record.upstream_model.clone(),
        }),
    )
    .await
    else {
        return record;
    };
    let result = match executed.result.as_ref() {
        CanonicalResult::VideoJob(result) => result.clone(),
        _ => {
            executed.mark_provider_protocol_failure();
            return record;
        }
    };
    let state_update = match media_job_state(&result.status) {
        Ok(state_update) => state_update,
        Err(failure) => {
            executed.mark_failure(InferenceError::from(failure).accounting_outcome());
            return record;
        }
    };
    let updated = state
        .store()
        .refresh_media_job(record.id, media_job_update(&result, state_update))
        .await
        .unwrap_or(record);
    executed.mark_success();
    updated
}

pub(super) async fn owned_media_job(
    state: &GatewayState,
    principal: &HttpRequestAdmission,
    video_id: &str,
    operation: OperationKind,
) -> Result<(ApiKey, MediaJobRecord), InferenceError> {
    let capability = gateway_capability_for_operation(operation);
    let key = authorize_principal(state, principal, capability, None)?;
    let id = uuid::Uuid::parse_str(video_id)
        .map_err(|_| InferenceError::resource_not_found("video_not_found"))?;
    let record = state.store().media_job(id).await.map_err(media_job_error)?;
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
    authorize_api_key(
        key,
        Some(&route),
        principal.gateway_capability(),
        capability,
        Utc::now(),
    )
    .map_err(|error| InferenceError::forbidden(error.to_string()))?;
    Ok((key.clone(), record))
}

pub(super) fn media_job_error(error: MediaJobError) -> InferenceError {
    match error {
        MediaJobError::NotFound => InferenceError::resource_not_found("video_not_found"),
        MediaJobError::PreconditionFailed => {
            InferenceError::conflict("video_changed", "The video job changed; retry the request.")
        }
        MediaJobError::UpstreamIdentityConflict => {
            InferenceError::unavailable("media_job_upstream_identity_conflict")
        }
        MediaJobError::Invalid(message) => InferenceError::invalid_request(message),
        MediaJobError::Database(_) => InferenceError::unavailable("persistence_unavailable"),
    }
}

#[cfg(test)]
mod tests {
    use chrono::TimeZone as _;
    use olp_engine::domain::canonical::{
        events::{Error, ErrorClass},
        results::{VideoJobResult, VideoStatus},
    };

    use super::*;
    use crate::application::media_jobs::results::{
        mark_missing_delete_as_success, media_job_result, valid_upstream_media_job_id,
    };
    use olp_engine::domain::canonical::{
        identity::Surface,
        requests::{MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION, Operation},
    };
    use serde_json::Value;
    use std::collections::BTreeMap;

    fn record(state: MediaJobState) -> MediaJobRecord {
        let created_at = Utc.with_ymd_and_hms(2025, 2, 3, 4, 5, 6).unwrap();
        MediaJobRecord {
            id: uuid::Uuid::from_u128(1),
            upstream_job_id: Some("upstream-1".to_owned()),
            api_key_id: uuid::Uuid::from_u128(2),
            provider_id: uuid::Uuid::from_u128(3),
            provider_name: "provider".to_owned(),
            upstream_model: "video-model".to_owned(),
            route_slug: "videos".to_owned(),
            operation: OperationKind::VideoGet,
            surface: Surface::OpenAi,
            state,
            lifecycle: MediaJobLifecycle::Active,
            progress_percent: Some(75.5),
            content_available: false,
            expires_at: Some(created_at + chrono::Duration::hours(1)),
            error_class: None,
            completed_at: Some(created_at + chrono::Duration::minutes(1)),
            last_polled_at: None,
            reconciliation_error: None,
            deleted_at: None,
            runtime_generation_id: None,
            provider_revision_id: None,
            reconciliation_claim_id: None,
            reconciliation_attempts: 0,
            next_reconciliation_at: created_at,
            last_reconciliation_at: None,
            etag: uuid::Uuid::from_u128(4),
            created_at,
            updated_at: created_at,
        }
    }

    #[test]
    fn upstream_job_ids_are_trimmed_bounded_and_control_free() {
        for (value, valid) in [
            ("job-1".to_owned(), true),
            ("".to_owned(), false),
            (" job-1".to_owned(), false),
            ("job-1 ".to_owned(), false),
            ("job\n1".to_owned(), false),
            ("x".repeat(1_024), true),
            ("x".repeat(1_025), false),
        ] {
            assert_eq!(valid_upstream_media_job_id(&value), valid, "{value:?}");
        }
    }

    #[test]
    fn route_and_delete_metadata_are_applied_only_to_supported_video_operations() {
        for mut operation in [
            olp_engine::protocols::openai::video::decode_video_get("upstream".to_owned()),
            olp_engine::protocols::openai::video::decode_video_content_with_query(
                "upstream".to_owned(),
                olp_engine::protocols::openai::video::OpenAiVideoContentQuery {
                    variant: None,
                    extra: std::collections::BTreeMap::new(),
                },
            )
            .unwrap(),
            olp_engine::protocols::openai::video::decode_video_delete("upstream".to_owned()),
        ] {
            set_video_route(&mut operation, "video-route").unwrap();
            assert_eq!(operation.route().unwrap().as_str(), "video-route");
        }

        let mut delete =
            olp_engine::protocols::openai::video::decode_video_delete("upstream".to_owned());
        mark_missing_delete_as_success(&mut delete).unwrap();
        let Operation::Video(olp_engine::domain::canonical::requests::VideoOperation::Delete(
            request,
        )) = delete
        else {
            panic!("expected video delete")
        };
        assert_eq!(request.extensions.source, Some(Surface::OpenAi));
        assert_eq!(
            request
                .extensions
                .values
                .get(MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION),
            Some(&Value::Bool(true))
        );

        let mut list = olp_engine::protocols::openai::video::decode_video_list(
            olp_engine::protocols::openai::video::OpenAiVideoListQuery {
                after: None,
                limit: None,
                order: None,
                extra: BTreeMap::new(),
            },
        )
        .unwrap();
        assert_eq!(
            set_video_route(&mut list, "video-route")
                .unwrap_err()
                .code(),
            "media_job_operation_invalid"
        );
        let mut invalid =
            olp_engine::protocols::openai::video::decode_video_get("upstream".to_owned());
        assert_eq!(
            set_video_route(&mut invalid, "Invalid Route")
                .unwrap_err()
                .code(),
            "media_job_route_invalid"
        );
    }

    #[test]
    fn provider_statuses_map_to_persistent_states() {
        for (status, expected) in [
            (VideoStatus::Queued, MediaJobState::Queued),
            (VideoStatus::InProgress, MediaJobState::Running),
            (VideoStatus::Completed, MediaJobState::Succeeded),
            (VideoStatus::Failed, MediaJobState::Failed),
        ] {
            assert_eq!(media_job_state(&status).unwrap(), expected);
        }
        assert_eq!(
            media_job_state(&VideoStatus::Other("paused".to_owned()))
                .unwrap_err()
                .code(),
            "provider_protocol_error"
        );
    }

    #[test]
    fn persistence_errors_keep_client_visible_failure_classes_stable() {
        let cases = [
            (MediaJobError::NotFound, 404, "video_not_found"),
            (MediaJobError::PreconditionFailed, 409, "video_changed"),
            (
                MediaJobError::UpstreamIdentityConflict,
                503,
                "media_job_upstream_identity_conflict",
            ),
            (
                MediaJobError::Invalid("invalid job".to_owned()),
                400,
                "invalid_request",
            ),
            (
                MediaJobError::Database(sqlx::Error::RowNotFound),
                503,
                "persistence_unavailable",
            ),
        ];
        for (error, status, code) in cases {
            let error = media_job_error(error);
            assert_eq!(error.status().as_u16(), status);
            assert_eq!(error.code(), code);
        }
    }

    #[test]
    fn provider_result_updates_preserve_progress_expiry_and_error_class() {
        let now = Utc::now();
        let result = VideoJobResult {
            id: "upstream".to_owned(),
            model: None,
            status: VideoStatus::Completed,
            progress_percent: Some(100.0),
            created_at: None,
            completed_at: Some(now.timestamp()),
            expires_at: Some((now + chrono::Duration::hours(1)).timestamp()),
            prompt: None,
            seconds: None,
            size: None,
            error: Some(Error {
                class: ErrorClass::RateLimit,
                message: "busy".to_owned(),
                provider_code: None,
                retryable: true,
            }),
            extensions: Default::default(),
        };
        let update = media_job_update(&result, MediaJobState::Succeeded);
        assert_eq!(update.state, MediaJobState::Succeeded);
        assert_eq!(update.progress_percent, Some(100.0));
        assert!(update.content_available);
        assert_eq!(
            update.expires_at.unwrap().timestamp(),
            result.expires_at.unwrap()
        );
        assert_eq!(update.error_class.as_deref(), Some("ratelimit"));
    }

    #[test]
    fn stored_states_round_trip_to_public_video_results() {
        for (state, expected) in [
            (MediaJobState::Queued, VideoStatus::Queued),
            (MediaJobState::Running, VideoStatus::InProgress),
            (MediaJobState::Succeeded, VideoStatus::Completed),
            (MediaJobState::Failed, VideoStatus::Failed),
            (
                MediaJobState::Cancelled,
                VideoStatus::Other("cancelled".to_owned()),
            ),
        ] {
            let record = record(state);
            let result = media_job_result(&record);
            assert_eq!(result.id, record.id.to_string());
            assert_eq!(result.model.as_deref(), Some("videos"));
            assert_eq!(result.status, expected);
            assert_eq!(result.progress_percent, record.progress_percent);
            assert_eq!(result.created_at, Some(record.created_at.timestamp()));
            assert_eq!(
                result.completed_at,
                record.completed_at.map(|value| value.timestamp())
            );
            assert_eq!(
                result.expires_at,
                record.expires_at.map(|value| value.timestamp())
            );
            assert_eq!(result.extensions.source, Some(Surface::OpenAi));
        }
    }
}

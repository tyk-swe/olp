use crate::access::policy::ApiKey;
use crate::access::policy::authorize_api_key;
use crate::access::policy::gateway_capability_for_operation;
use crate::http::request_admission::HttpRequestAdmission;
use crate::ids::RouteSlug;
use crate::inference::execution::RequiredTarget;
use crate::inference::http::error::InferenceError;
use crate::inference::http::execution::authorize_principal;
use crate::inference::http::state::GatewayState;
use crate::media::jobs::MediaJobError;
use crate::media::jobs::MediaJobLifecycle;
use crate::media::jobs::MediaJobRecord;
use crate::media::jobs::MediaJobState;
use crate::media::service::results::media_job_state;
use crate::media::service::results::media_job_update;
use crate::media::service::results::set_video_route;
use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::TransportMode;
use crate::protocols::canonical::results::CanonicalResult;
use chrono::Utc;
pub(crate) async fn refresh_video_list_record(
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
    let mut operation = crate::protocols::openai::video::decode_video_get(upstream_id);
    if set_video_route(&mut operation, &record.route_slug).is_err() {
        return record;
    }
    let Ok(mut executed) =
        execute_media_job_result(state, principal, &record, operation, true).await
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
    let updated = crate::media::jobs::lifecycle::refresh_media_job(
        &state.request_boundary.pool,
        record.id,
        media_job_update(&result, state_update),
    )
    .await
    .unwrap_or(record);
    executed.mark_success();
    updated
}

pub(crate) async fn execute_media_job_result(
    state: &GatewayState,
    admission: &HttpRequestAdmission,
    record: &MediaJobRecord,
    operation: crate::protocols::canonical::requests::Operation,
    internal: bool,
) -> Result<crate::inference::execution::RoutedUnaryResult, InferenceError> {
    let historical = crate::media::service::media_job_runtime(&state.media_jobs, record)
        .await
        .map_err(InferenceError::unavailable)?;
    let mut snapshot = crate::runtime::snapshot::Snapshot::clone(&historical);
    let current = admission.principal().runtime();
    snapshot.api_keys = current.api_keys.clone();
    snapshot.routing.installation = current.routing.installation.clone();
    snapshot.routing.routes = current.routing.routes.clone();
    let provider = crate::ids::ProviderId::from_uuid(record.provider_id);
    let transport = historical
        .transport(provider)
        .ok_or_else(|| InferenceError::unavailable("media_job_target_unavailable"))?;
    let runtime =
        crate::runtime::manager::Manager::reconciliation_bundle(snapshot, provider, transport)
            .map_err(|_| InferenceError::unavailable("media_job_runtime_unavailable"))?;
    let mut principal = crate::inference::principal::Principal::new(
        runtime,
        admission.principal().lookup_id().clone(),
        admission.principal().surface(),
        admission.principal().gateway_capability(),
    );
    principal.routing_preferences = admission.principal().routing_preferences.clone();
    state
        .request_boundary
        .inference
        .execute_result(
            &principal,
            operation,
            TransportMode::Unary,
            Some(RequiredTarget {
                credential_version_id: record.credential_version_id,
                provider_id: record.provider_id,
                upstream_model: record.upstream_model.clone(),
            }),
            if internal {
                admission.internal_engine_admission()
            } else {
                admission.engine_admission()
            },
        )
        .await
        .map_err(Into::into)
}

pub(crate) async fn owned_media_job(
    state: &GatewayState,
    principal: &HttpRequestAdmission,
    video_id: &str,
    operation: OperationKind,
) -> Result<(ApiKey, MediaJobRecord), InferenceError> {
    let capability = gateway_capability_for_operation(operation);
    let key = authorize_principal(state, principal, capability, None)?;
    let id = uuid::Uuid::parse_str(video_id)
        .map_err(|_| InferenceError::resource_not_found("video_not_found"))?;
    let record = crate::media::jobs::queries::media_job(&state.request_boundary.pool, id)
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

pub(crate) fn media_job_error(error: MediaJobError) -> InferenceError {
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
    use crate::protocols::canonical::events::Error;
    use crate::protocols::canonical::events::ErrorClass;
    use crate::protocols::canonical::results::VideoJobResult;
    use crate::protocols::canonical::results::VideoStatus;
    use chrono::TimeZone as _;

    use crate::inference::http::media_jobs::*;
    use crate::media::service::results::mark_missing_delete_as_success;
    use crate::media::service::results::media_job_result;
    use crate::media::service::results::valid_upstream_media_job_id;
    use crate::protocols::canonical::identity::Surface;
    use crate::protocols::canonical::requests::MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION;
    use crate::protocols::canonical::requests::Operation;
    use serde_json::Value;
    use std::collections::BTreeMap;

    fn record(state: MediaJobState) -> MediaJobRecord {
        let created_at = Utc.with_ymd_and_hms(2025, 2, 3, 4, 5, 6).unwrap();
        MediaJobRecord {
            credential_version_id: None,
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
            runtime_generation_id: uuid::Uuid::now_v7(),
            provider_revision_id: uuid::Uuid::now_v7(),
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
            crate::protocols::openai::video::decode_video_get("upstream".to_owned()),
            crate::protocols::openai::video::decode_video_content_with_query(
                "upstream".to_owned(),
                crate::protocols::openai::video::OpenAiVideoContentQuery {
                    variant: None,
                    extra: std::collections::BTreeMap::new(),
                },
            )
            .unwrap(),
            crate::protocols::openai::video::decode_video_delete("upstream".to_owned()),
        ] {
            set_video_route(&mut operation, "video-route").unwrap();
            assert_eq!(operation.route().unwrap().as_str(), "video-route");
        }

        let mut delete =
            crate::protocols::openai::video::decode_video_delete("upstream".to_owned());
        mark_missing_delete_as_success(&mut delete).unwrap();
        let Operation::Video(crate::protocols::canonical::requests::VideoOperation::Delete(
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

        let mut list = crate::protocols::openai::video::decode_video_list(
            crate::protocols::openai::video::OpenAiVideoListQuery {
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
        let mut invalid = crate::protocols::openai::video::decode_video_get("upstream".to_owned());
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

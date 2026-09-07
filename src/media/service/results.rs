use crate::ids::RouteSlug;
use crate::inference::error::Error as InferenceError;
use crate::media::jobs::MediaJobRecord;
use crate::media::jobs::MediaJobState;
use crate::media::jobs::MediaJobUpdate;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::requests::MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION;
use crate::protocols::canonical::requests::Operation;
use chrono::Utc;
use serde_json::Value;
use std::collections::BTreeMap;
pub(crate) fn mark_missing_delete_as_success(
    operation: &mut Operation,
) -> Result<(), InferenceError> {
    let Operation::Video(crate::protocols::canonical::requests::VideoOperation::Delete(request)) =
        operation
    else {
        return Err(InferenceError::unavailable("media_job_operation_invalid"));
    };
    request.extensions.source = Some(Surface::OpenAi);
    request.extensions.values.insert(
        MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION.to_owned(),
        Value::Bool(true),
    );
    Ok(())
}

pub(crate) fn valid_upstream_media_job_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 1_024
        && value.trim() == value
        && !value.chars().any(char::is_control)
}

pub(crate) fn set_video_route(
    operation: &mut Operation,
    route: &str,
) -> Result<(), InferenceError> {
    let route = RouteSlug::parse(route)
        .map_err(|_| InferenceError::unavailable("media_job_route_invalid"))?;
    let Operation::Video(operation) = operation else {
        return Err(InferenceError::unavailable("media_job_operation_invalid"));
    };
    match operation {
        crate::protocols::canonical::requests::VideoOperation::Get(request)
        | crate::protocols::canonical::requests::VideoOperation::Content(request)
        | crate::protocols::canonical::requests::VideoOperation::Delete(request) => {
            request.route = Some(route);
        }
        _ => return Err(InferenceError::unavailable("media_job_operation_invalid")),
    }
    Ok(())
}

pub(crate) fn media_job_state(
    status: &crate::protocols::canonical::results::VideoStatus,
) -> Result<MediaJobState, InferenceError> {
    match status {
        crate::protocols::canonical::results::VideoStatus::Queued => Ok(MediaJobState::Queued),
        crate::protocols::canonical::results::VideoStatus::InProgress => Ok(MediaJobState::Running),
        crate::protocols::canonical::results::VideoStatus::Completed => {
            Ok(MediaJobState::Succeeded)
        }
        crate::protocols::canonical::results::VideoStatus::Failed => Ok(MediaJobState::Failed),
        crate::protocols::canonical::results::VideoStatus::Other(status) => {
            Err(InferenceError::bad_gateway(
                "provider_protocol_error",
                format!("The provider returned an unsupported video status: {status}."),
            ))
        }
    }
}

pub(crate) fn media_job_update(
    result: &crate::protocols::canonical::results::VideoJobResult,
    state: MediaJobState,
) -> MediaJobUpdate {
    MediaJobUpdate {
        state,
        progress_percent: result.progress_percent,
        content_available: matches!(
            result.status,
            crate::protocols::canonical::results::VideoStatus::Completed
        ),
        expires_at: result
            .expires_at
            .and_then(chrono::DateTime::from_timestamp_secs),
        error_class: result
            .error
            .as_ref()
            .map(|error| format!("{:?}", error.class).to_lowercase()),
        last_polled_at: Utc::now(),
    }
}

pub(crate) fn media_job_result(
    record: &MediaJobRecord,
) -> crate::protocols::canonical::results::VideoJobResult {
    let status = match record.state {
        MediaJobState::Queued => crate::protocols::canonical::results::VideoStatus::Queued,
        MediaJobState::Running => crate::protocols::canonical::results::VideoStatus::InProgress,
        MediaJobState::Succeeded => crate::protocols::canonical::results::VideoStatus::Completed,
        MediaJobState::Failed => crate::protocols::canonical::results::VideoStatus::Failed,
        MediaJobState::Cancelled => {
            crate::protocols::canonical::results::VideoStatus::Other("cancelled".into())
        }
    };
    crate::protocols::canonical::results::VideoJobResult {
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
        extensions: crate::protocols::canonical::requests::SourceExtensions::new(
            Surface::OpenAi,
            BTreeMap::new(),
        ),
    }
}

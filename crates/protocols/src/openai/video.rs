use std::collections::BTreeMap;

use olp_domain::{
    CanonicalError, ErrorClass, MediaHandle, Operation, RouteSlug, RouteSlugError,
    SourceExtensions, Surface, VideoContentResult,
    VideoCreateRequest as CanonicalVideoCreateRequest, VideoDeleteResult, VideoJobRequest,
    VideoJobResult, VideoListRequest as CanonicalVideoListRequest, VideoListResult, VideoOperation,
    VideoStatus,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;

use super::extensions::{apply_pointer_extensions, collect_extra};
use super::media::{BinaryMediaBody, BoundedMediaPart};

pub const MAX_VIDEO_PROMPT_LENGTH: usize = 32_000;
pub const DEFAULT_VIDEO_REFERENCE_LIMIT: u64 = 20 * 1024 * 1024;

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiVideoCreateRequest {
    pub model: String,
    pub prompt: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub input_reference: Option<BoundedMediaPart>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub seconds: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub size: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_video_create(
    request: OpenAiVideoCreateRequest,
) -> Result<Operation, VideoCodecError> {
    if request.prompt.is_empty() || request.prompt.len() > MAX_VIDEO_PROMPT_LENGTH {
        return Err(VideoCodecError::InvalidPrompt);
    }
    validate_seconds(request.seconds.as_deref())?;
    validate_size(request.size.as_deref())?;
    let route = RouteSlug::parse(request.model)?;
    let mut extensions = BTreeMap::new();
    collect_extra("", &request.extra, &mut extensions);
    if let Some(seconds) = request.seconds {
        extensions.insert("/seconds".into(), Value::String(seconds));
    }
    if let Some(size) = request.size {
        extensions.insert("/size".into(), Value::String(size));
    }
    let input = request
        .input_reference
        .map(|reference| {
            if reference
                .content_type
                .as_deref()
                .is_some_and(|value| !value.starts_with("image/"))
            {
                return Err(VideoCodecError::InvalidInputReferenceMediaType);
            }
            Ok(reference.handle)
        })
        .transpose()?;
    Ok(Operation::Video(VideoOperation::Create(
        CanonicalVideoCreateRequest {
            route,
            prompt: request.prompt,
            input,
            extensions: SourceExtensions::new(Surface::OpenAi, extensions),
        },
    )))
}

pub fn encode_video_create(
    request: &CanonicalVideoCreateRequest,
    provider_model: &str,
    mut publish_reference: impl FnMut(&MediaHandle) -> Result<BoundedMediaPart, VideoCodecError>,
) -> Result<OpenAiVideoCreateRequest, VideoCodecError> {
    request
        .extensions
        .ensure_representable_on(Surface::OpenAi)?;
    let input_reference = request
        .input
        .as_ref()
        .map(&mut publish_reference)
        .transpose()?;
    apply_pointer_extensions(
        OpenAiVideoCreateRequest {
            model: provider_model.into(),
            prompt: request.prompt.clone(),
            input_reference,
            seconds: None,
            size: None,
            extra: BTreeMap::new(),
        },
        &request.extensions.values,
    )
    .map_err(VideoCodecError::InvalidExtension)
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiVideoListQuery {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub after: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub limit: Option<u16>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub order: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_video_list(query: OpenAiVideoListQuery) -> Result<Operation, VideoCodecError> {
    if query.limit == Some(0) || query.limit.is_some_and(|limit| limit > 100) {
        return Err(VideoCodecError::InvalidListLimit);
    }
    let mut extensions = BTreeMap::new();
    collect_extra("", &query.extra, &mut extensions);
    if let Some(order) = query.order {
        if order != "asc" && order != "desc" {
            return Err(VideoCodecError::InvalidOrder);
        }
        extensions.insert("/order".into(), Value::String(order));
    }
    Ok(Operation::Video(VideoOperation::List(
        CanonicalVideoListRequest {
            route: None,
            cursor: query.after,
            limit: query.limit,
            extensions: SourceExtensions::new(Surface::OpenAi, extensions),
        },
    )))
}

pub fn encode_video_list(
    request: &CanonicalVideoListRequest,
) -> Result<OpenAiVideoListQuery, VideoCodecError> {
    request
        .extensions
        .ensure_representable_on(Surface::OpenAi)?;
    if request.route.is_some() {
        return Err(VideoCodecError::RouteCannotBeEncoded);
    }
    apply_pointer_extensions(
        OpenAiVideoListQuery {
            after: request.cursor.clone(),
            limit: request.limit,
            order: None,
            extra: BTreeMap::new(),
        },
        &request.extensions.values,
    )
    .map_err(VideoCodecError::InvalidExtension)
}

pub fn decode_video_get(job_id: impl Into<String>) -> Operation {
    video_job_operation(job_id.into(), VideoJobKind::Get)
}

pub fn decode_video_content(job_id: impl Into<String>) -> Operation {
    video_job_operation(job_id.into(), VideoJobKind::Content)
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiVideoContentQuery {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub variant: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_video_content_with_query(
    job_id: impl Into<String>,
    query: OpenAiVideoContentQuery,
) -> Result<Operation, VideoCodecError> {
    if query
        .variant
        .as_deref()
        .is_some_and(|variant| !matches!(variant, "video" | "thumbnail" | "spritesheet"))
    {
        return Err(VideoCodecError::InvalidContentVariant);
    }
    let mut extensions = BTreeMap::new();
    collect_extra("", &query.extra, &mut extensions);
    if let Some(variant) = query.variant {
        extensions.insert("/variant".into(), Value::String(variant));
    }
    Ok(Operation::Video(VideoOperation::Content(VideoJobRequest {
        route: None,
        job_id: job_id.into(),
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    })))
}

pub fn decode_video_delete(job_id: impl Into<String>) -> Operation {
    video_job_operation(job_id.into(), VideoJobKind::Delete)
}

enum VideoJobKind {
    Get,
    Content,
    Delete,
}

fn video_job_operation(job_id: String, kind: VideoJobKind) -> Operation {
    let request = VideoJobRequest {
        route: None,
        job_id,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    };
    Operation::Video(match kind {
        VideoJobKind::Get => VideoOperation::Get(request),
        VideoJobKind::Content => VideoOperation::Content(request),
        VideoJobKind::Delete => VideoOperation::Delete(request),
    })
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiVideoObject {
    pub id: String,
    pub object: String,
    pub model: String,
    pub status: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub progress: Option<f32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub created_at: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub completed_at: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub expires_at: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub prompt: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub seconds: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub size: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub remixed_from_video_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub error: Option<OpenAiVideoError>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiVideoError {
    pub code: String,
    pub message: String,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_video_object(video: OpenAiVideoObject) -> Result<VideoJobResult, VideoCodecError> {
    if video.object != "video" {
        return Err(VideoCodecError::UnexpectedObject(video.object));
    }
    let status = match video.status.as_str() {
        "queued" => VideoStatus::Queued,
        "in_progress" => VideoStatus::InProgress,
        "completed" => VideoStatus::Completed,
        "failed" => VideoStatus::Failed,
        value => VideoStatus::Other(value.into()),
    };
    let mut extensions = BTreeMap::new();
    collect_extra("", &video.extra, &mut extensions);
    if let Some(source) = video.remixed_from_video_id {
        extensions.insert("/remixed_from_video_id".into(), Value::String(source));
    }
    let error = video.error.map(|error| {
        collect_extra("/error", &error.extra, &mut extensions);
        CanonicalError {
            class: ErrorClass::Upstream,
            message: error.message,
            provider_code: Some(error.code),
            retryable: false,
        }
    });
    Ok(VideoJobResult {
        id: video.id,
        model: Some(video.model),
        status,
        progress_percent: video.progress,
        created_at: video.created_at,
        completed_at: video.completed_at,
        expires_at: video.expires_at,
        prompt: video.prompt,
        seconds: video.seconds,
        size: video.size,
        error,
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    })
}

pub fn encode_video_object(
    result: &VideoJobResult,
    client_model: &str,
) -> Result<OpenAiVideoObject, VideoCodecError> {
    result.extensions.ensure_representable_on(Surface::OpenAi)?;
    let status = match &result.status {
        VideoStatus::Queued => "queued",
        VideoStatus::InProgress => "in_progress",
        VideoStatus::Completed => "completed",
        VideoStatus::Failed => "failed",
        VideoStatus::Other(value) => value,
    }
    .to_owned();
    let error = result.error.as_ref().map(|error| OpenAiVideoError {
        code: error
            .provider_code
            .clone()
            .unwrap_or_else(|| "video_error".into()),
        message: error.message.clone(),
        extra: BTreeMap::new(),
    });
    apply_pointer_extensions(
        OpenAiVideoObject {
            id: result.id.clone(),
            object: "video".into(),
            // Provider model identifiers stay in attempt metadata. The client
            // sees only the public route slug supplied by the HTTP boundary.
            model: client_model.into(),
            status,
            progress: result.progress_percent,
            created_at: result.created_at,
            completed_at: result.completed_at,
            expires_at: result.expires_at,
            prompt: result.prompt.clone(),
            seconds: result.seconds.clone(),
            size: result.size.clone(),
            remixed_from_video_id: None,
            error,
            extra: BTreeMap::new(),
        },
        &result.extensions.values,
    )
    .map_err(VideoCodecError::InvalidExtension)
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiVideoListResponse {
    pub object: String,
    pub data: Vec<OpenAiVideoObject>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub first_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_id: Option<String>,
    #[serde(default)]
    pub has_more: bool,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_video_list_response(
    response: OpenAiVideoListResponse,
) -> Result<VideoListResult, VideoCodecError> {
    if response.object != "list" {
        return Err(VideoCodecError::UnexpectedObject(response.object));
    }
    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);
    let jobs = response
        .data
        .into_iter()
        .map(decode_video_object)
        .collect::<Result<_, _>>()?;
    Ok(VideoListResult {
        jobs,
        first_id: response.first_id,
        last_id: response.last_id,
        has_more: response.has_more,
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    })
}

pub fn encode_video_list_response(
    result: &VideoListResult,
    fallback_model: &str,
) -> Result<OpenAiVideoListResponse, VideoCodecError> {
    result.extensions.ensure_representable_on(Surface::OpenAi)?;
    let data = result
        .jobs
        .iter()
        .map(|job| encode_video_object(job, job.model.as_deref().unwrap_or(fallback_model)))
        .collect::<Result<_, _>>()?;
    apply_pointer_extensions(
        OpenAiVideoListResponse {
            object: "list".into(),
            data,
            first_id: result.first_id.clone(),
            last_id: result.last_id.clone(),
            has_more: result.has_more,
            extra: BTreeMap::new(),
        },
        &result.extensions.values,
    )
    .map_err(VideoCodecError::InvalidExtension)
}

pub fn decode_video_content_body(body: BinaryMediaBody) -> VideoContentResult {
    VideoContentResult {
        media: body.media,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    }
}

pub fn encode_video_content_body(
    result: &VideoContentResult,
) -> Result<BinaryMediaBody, VideoCodecError> {
    result.extensions.ensure_representable_on(Surface::OpenAi)?;
    if !result.extensions.values.is_empty() {
        return Err(VideoCodecError::BinaryExtensionsUnsupported);
    }
    Ok(BinaryMediaBody {
        media: result.media.clone(),
    })
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiVideoDeleteResponse {
    pub id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub object: Option<String>,
    pub deleted: bool,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_video_delete_response(response: OpenAiVideoDeleteResponse) -> VideoDeleteResult {
    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);
    if let Some(object) = response.object {
        extensions.insert("/object".into(), Value::String(object));
    }
    VideoDeleteResult {
        id: response.id,
        deleted: response.deleted,
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    }
}

pub fn encode_video_delete_response(
    result: &VideoDeleteResult,
) -> Result<OpenAiVideoDeleteResponse, VideoCodecError> {
    result.extensions.ensure_representable_on(Surface::OpenAi)?;
    let mut extensions = result.extensions.values.clone();
    let object = extensions
        .remove("/object")
        .and_then(|value| value.as_str().map(str::to_owned))
        .or_else(|| Some("video.deleted".into()));
    apply_pointer_extensions(
        OpenAiVideoDeleteResponse {
            id: result.id.clone(),
            object,
            deleted: result.deleted,
            extra: BTreeMap::new(),
        },
        &extensions,
    )
    .map_err(VideoCodecError::InvalidExtension)
}

fn validate_seconds(value: Option<&str>) -> Result<(), VideoCodecError> {
    if value.is_some_and(|value| !matches!(value, "4" | "8" | "12")) {
        return Err(VideoCodecError::InvalidSeconds);
    }
    Ok(())
}

fn validate_size(value: Option<&str>) -> Result<(), VideoCodecError> {
    if value
        .is_some_and(|value| !matches!(value, "720x1280" | "1280x720" | "1024x1792" | "1792x1024"))
    {
        return Err(VideoCodecError::InvalidSize);
    }
    Ok(())
}

#[derive(Debug, Error)]
pub enum VideoCodecError {
    #[error(transparent)]
    InvalidRoute(#[from] RouteSlugError),
    #[error(transparent)]
    Extensions(#[from] olp_domain::ExtensionError),
    #[error("video prompt must contain 1 to 32000 bytes")]
    InvalidPrompt,
    #[error("video seconds must be one of 4, 8, or 12")]
    InvalidSeconds,
    #[error("unsupported video size")]
    InvalidSize,
    #[error("video input_reference must be an image file")]
    InvalidInputReferenceMediaType,
    #[error("video list limit must be between 1 and 100")]
    InvalidListLimit,
    #[error("video list order must be asc or desc")]
    InvalidOrder,
    #[error("video content variant must be video, thumbnail, or spritesheet")]
    InvalidContentVariant,
    #[error("a provider route cannot be encoded in the OpenAI video list query")]
    RouteCannotBeEncoded,
    #[error("unexpected OpenAI object type: {0}")]
    UnexpectedObject(String),
    #[error("invalid source extension path: {0}")]
    InvalidExtension(String),
    #[error("bounded reference staging failed: {0}")]
    Staging(String),
    #[error("binary video extensions require an HTTP header representation")]
    BinaryExtensionsUnsupported,
}

use std::collections::BTreeMap;

use olp_domain::{
    ImageArtifact, ImageEditRequest as CanonicalImageEditRequest,
    ImageGenerationRequest as CanonicalImageGenerationRequest, ImageOperation,
    ImageVariationRequest as CanonicalImageVariationRequest, ImagesResult, MediaHandle,
    MediaSource, Operation, RouteSlug, RouteSlugError, SourceExtensions, Surface, Usage,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;

use super::extensions::{apply_pointer_extensions, collect_extra};
use super::media::BoundedMediaPart;

pub const DEFAULT_IMAGE_UPLOAD_LIMIT: u64 = 50 * 1024 * 1024;

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiImageGenerationRequest {
    pub model: String,
    pub prompt: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub n: Option<u16>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub size: Option<String>,
    #[serde(default, skip_serializing_if = "is_false")]
    pub stream: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub quality: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub response_format: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub style: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub user: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub background: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub moderation: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub output_compression: Option<u8>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub output_format: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub partial_images: Option<u8>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

const fn is_false(value: &bool) -> bool {
    !*value
}

pub fn decode_image_generation(
    request: OpenAiImageGenerationRequest,
) -> Result<Operation, ImageCodecError> {
    validate_prompt_and_count(&request.prompt, request.n)?;
    let route = RouteSlug::parse(request.model)?;
    let mut extensions = BTreeMap::new();
    collect_extra("", &request.extra, &mut extensions);
    capture_optional(&mut extensions, "/quality", request.quality);
    capture_optional(&mut extensions, "/response_format", request.response_format);
    capture_optional(&mut extensions, "/style", request.style);
    capture_optional(&mut extensions, "/user", request.user);
    capture_optional(&mut extensions, "/background", request.background);
    capture_optional(&mut extensions, "/moderation", request.moderation);
    capture_number(
        &mut extensions,
        "/output_compression",
        request.output_compression,
    );
    capture_optional(&mut extensions, "/output_format", request.output_format);
    capture_number(&mut extensions, "/partial_images", request.partial_images);
    Ok(Operation::Images(ImageOperation::Generation(
        CanonicalImageGenerationRequest {
            route,
            prompt: request.prompt,
            count: request.n,
            size: request.size,
            stream: request.stream,
            extensions: SourceExtensions::new(Surface::OpenAi, extensions),
        },
    )))
}

pub fn encode_image_generation(
    request: &CanonicalImageGenerationRequest,
    provider_model: &str,
) -> Result<OpenAiImageGenerationRequest, ImageCodecError> {
    request
        .extensions
        .ensure_representable_on(Surface::OpenAi)?;
    apply_pointer_extensions(
        OpenAiImageGenerationRequest {
            model: provider_model.into(),
            prompt: request.prompt.clone(),
            n: request.count,
            size: request.size.clone(),
            stream: request.stream,
            quality: None,
            response_format: None,
            style: None,
            user: None,
            background: None,
            moderation: None,
            output_compression: None,
            output_format: None,
            partial_images: None,
            extra: BTreeMap::new(),
        },
        &request.extensions.values,
    )
    .map_err(ImageCodecError::InvalidExtension)
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiImageEditRequest {
    pub model: String,
    #[serde(rename = "image")]
    pub images: Vec<BoundedMediaPart>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub mask: Option<BoundedMediaPart>,
    pub prompt: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub n: Option<u16>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub size: Option<String>,
    #[serde(default, skip_serializing_if = "is_false")]
    pub stream: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub quality: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub response_format: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub user: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub background: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub input_fidelity: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub output_compression: Option<u8>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub output_format: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub partial_images: Option<u8>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_image_edit(request: OpenAiImageEditRequest) -> Result<Operation, ImageCodecError> {
    validate_prompt_and_count(&request.prompt, request.n)?;
    if request.images.is_empty() {
        return Err(ImageCodecError::MissingImage);
    }
    validate_media_parts(request.images.iter().chain(request.mask.iter()))?;
    let route = RouteSlug::parse(request.model)?;
    let mut extensions = BTreeMap::new();
    collect_extra("", &request.extra, &mut extensions);
    capture_number(&mut extensions, "/n", request.n);
    capture_optional(&mut extensions, "/size", request.size);
    capture_optional(&mut extensions, "/quality", request.quality);
    capture_optional(&mut extensions, "/response_format", request.response_format);
    capture_optional(&mut extensions, "/user", request.user);
    capture_optional(&mut extensions, "/background", request.background);
    capture_optional(&mut extensions, "/input_fidelity", request.input_fidelity);
    capture_number(
        &mut extensions,
        "/output_compression",
        request.output_compression,
    );
    capture_optional(&mut extensions, "/output_format", request.output_format);
    capture_number(&mut extensions, "/partial_images", request.partial_images);
    Ok(Operation::Images(ImageOperation::Edit(
        CanonicalImageEditRequest {
            route,
            images: request.images.into_iter().map(|part| part.handle).collect(),
            mask: request.mask.map(|part| part.handle),
            prompt: request.prompt,
            stream: request.stream,
            extensions: SourceExtensions::new(Surface::OpenAi, extensions),
        },
    )))
}

pub fn encode_image_edit(
    request: &CanonicalImageEditRequest,
    provider_model: &str,
    mut resolve_part: impl FnMut(&MediaHandle) -> Result<BoundedMediaPart, ImageCodecError>,
) -> Result<OpenAiImageEditRequest, ImageCodecError> {
    request
        .extensions
        .ensure_representable_on(Surface::OpenAi)?;
    let images = request
        .images
        .iter()
        .map(&mut resolve_part)
        .collect::<Result<Vec<_>, _>>()?;
    let mask = request.mask.as_ref().map(resolve_part).transpose()?;
    validate_media_parts(images.iter().chain(mask.iter()))?;
    apply_pointer_extensions(
        OpenAiImageEditRequest {
            model: provider_model.into(),
            images,
            mask,
            prompt: request.prompt.clone(),
            n: None,
            size: None,
            stream: request.stream,
            quality: None,
            response_format: None,
            user: None,
            background: None,
            input_fidelity: None,
            output_compression: None,
            output_format: None,
            partial_images: None,
            extra: BTreeMap::new(),
        },
        &request.extensions.values,
    )
    .map_err(ImageCodecError::InvalidExtension)
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiImageVariationRequest {
    pub model: String,
    pub image: BoundedMediaPart,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub n: Option<u16>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub size: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub response_format: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub user: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_image_variation(
    request: OpenAiImageVariationRequest,
) -> Result<Operation, ImageCodecError> {
    validate_count(request.n)?;
    validate_media_parts(std::iter::once(&request.image))?;
    let route = RouteSlug::parse(request.model)?;
    let mut extensions = BTreeMap::new();
    collect_extra("", &request.extra, &mut extensions);
    capture_optional(&mut extensions, "/response_format", request.response_format);
    capture_optional(&mut extensions, "/user", request.user);
    Ok(Operation::Images(ImageOperation::Variation(
        CanonicalImageVariationRequest {
            route,
            image: request.image.handle,
            count: request.n,
            size: request.size,
            extensions: SourceExtensions::new(Surface::OpenAi, extensions),
        },
    )))
}

pub fn encode_image_variation(
    request: &CanonicalImageVariationRequest,
    provider_model: &str,
    mut resolve_part: impl FnMut(&MediaHandle) -> Result<BoundedMediaPart, ImageCodecError>,
) -> Result<OpenAiImageVariationRequest, ImageCodecError> {
    request
        .extensions
        .ensure_representable_on(Surface::OpenAi)?;
    let image = resolve_part(&request.image)?;
    validate_media_parts(std::iter::once(&image))?;
    apply_pointer_extensions(
        OpenAiImageVariationRequest {
            model: provider_model.into(),
            image,
            n: request.count,
            size: request.size.clone(),
            response_format: None,
            user: None,
            extra: BTreeMap::new(),
        },
        &request.extensions.values,
    )
    .map_err(ImageCodecError::InvalidExtension)
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiImageResponse {
    pub created: i64,
    pub data: Vec<OpenAiImageData>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub usage: Option<OpenAiImageUsage>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiImageData {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub url: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub b64_json: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub revised_prompt: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiImageUsage {
    #[serde(default)]
    pub input_tokens: u64,
    #[serde(default)]
    pub output_tokens: u64,
    #[serde(default)]
    pub total_tokens: u64,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

/// Converts an image response after the adapter stages each base64 payload.
/// The closure must stream/decode into a bounded spool and return its handle.
pub fn decode_image_response(
    response: OpenAiImageResponse,
    mut stage_base64: impl FnMut(&str) -> Result<MediaHandle, ImageCodecError>,
) -> Result<ImagesResult, ImageCodecError> {
    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);
    let mut images = Vec::with_capacity(response.data.len());
    for (index, data) in response.data.into_iter().enumerate() {
        collect_extra(&format!("/data/{index}"), &data.extra, &mut extensions);
        let source = match (data.url, data.b64_json) {
            (Some(url), None) => MediaSource::Uri(url),
            (None, Some(encoded)) => MediaSource::Handle(stage_base64(&encoded)?),
            _ => return Err(ImageCodecError::AmbiguousImageResult),
        };
        images.push(ImageArtifact {
            source,
            revised_prompt: data.revised_prompt,
        });
    }
    let usage = response.usage.map(|usage| {
        collect_extra("/usage", &usage.extra, &mut extensions);
        Usage {
            input_tokens: usage.input_tokens,
            output_tokens: usage.output_tokens,
            total_tokens: usage.total_tokens,
            cached_input_tokens: None,
            reasoning_tokens: None,
        }
    });
    Ok(ImagesResult {
        created_at: Some(response.created),
        images,
        usage,
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    })
}

pub enum OpenAiImagePayload {
    Url(String),
    Base64Json(String),
}

pub fn encode_image_response(
    result: &ImagesResult,
    mut resolve_handle: impl FnMut(&MediaHandle) -> Result<OpenAiImagePayload, ImageCodecError>,
) -> Result<OpenAiImageResponse, ImageCodecError> {
    result.extensions.ensure_representable_on(Surface::OpenAi)?;
    let data = result
        .images
        .iter()
        .map(|image| {
            let payload = match &image.source {
                MediaSource::Uri(url) => OpenAiImagePayload::Url(url.clone()),
                MediaSource::Handle(handle) => resolve_handle(handle)?,
            };
            let (url, b64_json) = match payload {
                OpenAiImagePayload::Url(url) => (Some(url), None),
                OpenAiImagePayload::Base64Json(encoded) => (None, Some(encoded)),
            };
            Ok::<_, ImageCodecError>(OpenAiImageData {
                url,
                b64_json,
                revised_prompt: image.revised_prompt.clone(),
                extra: BTreeMap::new(),
            })
        })
        .collect::<Result<Vec<_>, _>>()?;
    let usage = result.usage.map(|usage| OpenAiImageUsage {
        input_tokens: usage.input_tokens,
        output_tokens: usage.output_tokens,
        total_tokens: usage.total_tokens,
        extra: BTreeMap::new(),
    });
    apply_pointer_extensions(
        OpenAiImageResponse {
            created: result.created_at.unwrap_or(0),
            data,
            usage,
            extra: BTreeMap::new(),
        },
        &result.extensions.values,
    )
    .map_err(ImageCodecError::InvalidExtension)
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiImageStreamEvent {
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub b64_json: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub partial_image_index: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub created_at: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub usage: Option<OpenAiImageUsage>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub enum ImageStreamUpdate {
    Partial {
        index: u32,
        image: ImageArtifact,
        extensions: SourceExtensions,
    },
    Completed {
        usage: Option<Usage>,
        extensions: SourceExtensions,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ImageStreamOperation {
    Generation,
    Edit,
}

pub fn decode_image_stream_event(
    event: OpenAiImageStreamEvent,
    mut stage_base64: impl FnMut(&str) -> Result<MediaHandle, ImageCodecError>,
) -> Result<ImageStreamUpdate, ImageCodecError> {
    let mut extensions = BTreeMap::new();
    collect_extra("", &event.extra, &mut extensions);
    if let Some(created_at) = event.created_at {
        extensions.insert("/created_at".into(), Value::from(created_at));
    }
    match event.kind.as_str() {
        "image_generation.partial_image" | "image_edit.partial_image" => {
            let encoded = event.b64_json.ok_or(ImageCodecError::MissingStreamImage)?;
            Ok(ImageStreamUpdate::Partial {
                index: event.partial_image_index.unwrap_or(0),
                image: ImageArtifact {
                    source: MediaSource::Handle(stage_base64(&encoded)?),
                    revised_prompt: None,
                },
                extensions: SourceExtensions::new(Surface::OpenAi, extensions),
            })
        }
        "image_generation.completed" | "image_edit.completed" => {
            let usage = event.usage.map(|usage| Usage {
                input_tokens: usage.input_tokens,
                output_tokens: usage.output_tokens,
                total_tokens: usage.total_tokens,
                cached_input_tokens: None,
                reasoning_tokens: None,
            });
            Ok(ImageStreamUpdate::Completed {
                usage,
                extensions: SourceExtensions::new(Surface::OpenAi, extensions),
            })
        }
        _ => Err(ImageCodecError::UnsupportedStreamEvent(event.kind)),
    }
}

pub fn encode_image_stream_update(
    update: &ImageStreamUpdate,
    operation: ImageStreamOperation,
    mut read_base64: impl FnMut(&MediaHandle) -> Result<String, ImageCodecError>,
) -> Result<OpenAiImageStreamEvent, ImageCodecError> {
    let (suffix, b64_json, partial_image_index, created_at, usage, extensions) = match update {
        ImageStreamUpdate::Partial {
            index,
            image,
            extensions,
        } => {
            extensions.ensure_representable_on(Surface::OpenAi)?;
            let MediaSource::Handle(handle) = &image.source else {
                return Err(ImageCodecError::StreamImageNeedsHandle);
            };
            (
                "partial_image",
                Some(read_base64(handle)?),
                Some(*index),
                None,
                None,
                extensions,
            )
        }
        ImageStreamUpdate::Completed { usage, extensions } => {
            extensions.ensure_representable_on(Surface::OpenAi)?;
            (
                "completed",
                None,
                None,
                None,
                usage.map(|usage| OpenAiImageUsage {
                    input_tokens: usage.input_tokens,
                    output_tokens: usage.output_tokens,
                    total_tokens: usage.total_tokens,
                    extra: BTreeMap::new(),
                }),
                extensions,
            )
        }
    };
    let prefix = match operation {
        ImageStreamOperation::Generation => "image_generation",
        ImageStreamOperation::Edit => "image_edit",
    };
    apply_pointer_extensions(
        OpenAiImageStreamEvent {
            kind: format!("{prefix}.{suffix}"),
            b64_json,
            partial_image_index,
            created_at,
            usage,
            extra: BTreeMap::new(),
        },
        &extensions.values,
    )
    .map_err(ImageCodecError::InvalidExtension)
}

fn validate_prompt_and_count(prompt: &str, count: Option<u16>) -> Result<(), ImageCodecError> {
    if prompt.trim().is_empty() {
        return Err(ImageCodecError::EmptyPrompt);
    }
    validate_count(count)
}

fn validate_count(count: Option<u16>) -> Result<(), ImageCodecError> {
    if count == Some(0) {
        return Err(ImageCodecError::ZeroCount);
    }
    Ok(())
}

fn validate_media_parts<'a>(
    parts: impl Iterator<Item = &'a BoundedMediaPart>,
) -> Result<(), ImageCodecError> {
    if let Some(part) = parts.into_iter().find(|part| {
        part.content_length > part.maximum_length
            || part.maximum_length > DEFAULT_IMAGE_UPLOAD_LIMIT
    }) {
        return Err(ImageCodecError::InvalidMediaPart(part.filename.clone()));
    }
    Ok(())
}

fn capture_optional(extensions: &mut BTreeMap<String, Value>, path: &str, value: Option<String>) {
    if let Some(value) = value {
        extensions.insert(path.into(), Value::String(value));
    }
}

fn capture_number<T: Into<u64>>(
    extensions: &mut BTreeMap<String, Value>,
    path: &str,
    value: Option<T>,
) {
    if let Some(value) = value {
        extensions.insert(path.into(), Value::from(value.into()));
    }
}

#[derive(Debug, Error)]
pub enum ImageCodecError {
    #[error(transparent)]
    InvalidRoute(#[from] RouteSlugError),
    #[error(transparent)]
    Extensions(#[from] olp_domain::ExtensionError),
    #[error("image prompt cannot be empty")]
    EmptyPrompt,
    #[error("image count must be greater than zero")]
    ZeroCount,
    #[error("at least one image upload is required")]
    MissingImage,
    #[error("invalid bounded image part: {0}")]
    InvalidMediaPart(String),
    #[error("image response must contain exactly one of url or b64_json")]
    AmbiguousImageResult,
    #[error("invalid source extension path: {0}")]
    InvalidExtension(String),
    #[error("base64 media staging failed: {0}")]
    Staging(String),
    #[error("image partial event is missing b64_json")]
    MissingStreamImage,
    #[error("unsupported image stream event: {0}")]
    UnsupportedStreamEvent(String),
    #[error("image stream update requires a bounded media handle")]
    StreamImageNeedsHandle,
}

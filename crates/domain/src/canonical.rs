use std::{collections::BTreeMap, fmt, str::FromStr};

use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;

use crate::{RequestId, RouteSlug};

#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Surface {
    OpenAi,
    Anthropic,
    Gemini,
}

impl Surface {
    pub const ALL: [Self; 3] = [Self::OpenAi, Self::Anthropic, Self::Gemini];

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::OpenAi => "open_ai",
            Self::Anthropic => "anthropic",
            Self::Gemini => "gemini",
        }
    }
}

impl FromStr for Surface {
    type Err = InvalidSurface;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        match value {
            "open_ai" => Ok(Self::OpenAi),
            "anthropic" => Ok(Self::Anthropic),
            "gemini" => Ok(Self::Gemini),
            _ => Err(InvalidSurface),
        }
    }
}

impl fmt::Display for Surface {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
#[error("invalid canonical surface")]
pub struct InvalidSurface;

#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum TransportMode {
    Unary,
    Streaming,
    Async,
}

impl TransportMode {
    pub const ALL: [Self; 3] = [Self::Unary, Self::Streaming, Self::Async];

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Unary => "unary",
            Self::Streaming => "streaming",
            Self::Async => "async",
        }
    }
}

impl FromStr for TransportMode {
    type Err = InvalidTransportMode;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        match value {
            "unary" => Ok(Self::Unary),
            "streaming" => Ok(Self::Streaming),
            "async" => Ok(Self::Async),
            _ => Err(InvalidTransportMode),
        }
    }
}

impl fmt::Display for TransportMode {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
#[error("invalid canonical transport mode")]
pub struct InvalidTransportMode;

#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum OperationKind {
    Generation,
    Embeddings,
    TokenCount,
    ImageGeneration,
    ImageEdit,
    ImageVariation,
    Speech,
    Transcription,
    VideoCreate,
    VideoList,
    VideoGet,
    VideoContent,
    VideoDelete,
    Moderation,
    ModelList,
    ModelGet,
}

impl OperationKind {
    pub const ALL: [Self; 16] = [
        Self::Generation,
        Self::Embeddings,
        Self::TokenCount,
        Self::ImageGeneration,
        Self::ImageEdit,
        Self::ImageVariation,
        Self::Speech,
        Self::Transcription,
        Self::VideoCreate,
        Self::VideoList,
        Self::VideoGet,
        Self::VideoContent,
        Self::VideoDelete,
        Self::Moderation,
        Self::ModelList,
        Self::ModelGet,
    ];

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Generation => "generation",
            Self::Embeddings => "embeddings",
            Self::TokenCount => "token_count",
            Self::ImageGeneration => "image_generation",
            Self::ImageEdit => "image_edit",
            Self::ImageVariation => "image_variation",
            Self::Speech => "speech",
            Self::Transcription => "transcription",
            Self::VideoCreate => "video_create",
            Self::VideoList => "video_list",
            Self::VideoGet => "video_get",
            Self::VideoContent => "video_content",
            Self::VideoDelete => "video_delete",
            Self::Moderation => "moderation",
            Self::ModelList => "model_list",
            Self::ModelGet => "model_get",
        }
    }
}

impl FromStr for OperationKind {
    type Err = InvalidOperationKind;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        match value {
            "generation" => Ok(Self::Generation),
            "embeddings" => Ok(Self::Embeddings),
            "token_count" => Ok(Self::TokenCount),
            "image_generation" => Ok(Self::ImageGeneration),
            "image_edit" => Ok(Self::ImageEdit),
            "image_variation" => Ok(Self::ImageVariation),
            "speech" => Ok(Self::Speech),
            "transcription" => Ok(Self::Transcription),
            "video_create" => Ok(Self::VideoCreate),
            "video_list" => Ok(Self::VideoList),
            "video_get" => Ok(Self::VideoGet),
            "video_content" => Ok(Self::VideoContent),
            "video_delete" => Ok(Self::VideoDelete),
            "moderation" => Ok(Self::Moderation),
            "model_list" => Ok(Self::ModelList),
            "model_get" => Ok(Self::ModelGet),
            _ => Err(InvalidOperationKind),
        }
    }
}

impl fmt::Display for OperationKind {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
#[error("invalid canonical operation kind")]
pub struct InvalidOperationKind;

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(tag = "operation", content = "request", rename_all = "snake_case")]
pub enum Operation {
    Generation(GenerationRequest),
    Embeddings(EmbeddingsRequest),
    TokenCount(TokenCountRequest),
    Images(ImageOperation),
    Speech(SpeechRequest),
    Transcription(TranscriptionRequest),
    Video(VideoOperation),
    Moderation(ModerationRequest),
    Models(ModelOperation),
}

impl Operation {
    #[must_use]
    pub const fn kind(&self) -> OperationKind {
        match self {
            Self::Generation(_) => OperationKind::Generation,
            Self::Embeddings(_) => OperationKind::Embeddings,
            Self::TokenCount(_) => OperationKind::TokenCount,
            Self::Images(operation) => operation.kind(),
            Self::Speech(_) => OperationKind::Speech,
            Self::Transcription(_) => OperationKind::Transcription,
            Self::Video(operation) => operation.kind(),
            Self::Moderation(_) => OperationKind::Moderation,
            Self::Models(operation) => operation.kind(),
        }
    }

    #[must_use]
    pub fn route(&self) -> Option<&RouteSlug> {
        match self {
            Self::Generation(request) => Some(&request.route),
            Self::Embeddings(request) => Some(&request.route),
            Self::TokenCount(request) => Some(&request.route),
            Self::Images(operation) => Some(operation.route()),
            Self::Speech(request) => Some(&request.route),
            Self::Transcription(request) => Some(&request.route),
            Self::Video(operation) => operation.route(),
            Self::Moderation(request) => Some(&request.route),
            Self::Models(operation) => operation.route(),
        }
    }

    #[must_use]
    pub fn extensions(&self) -> Option<&SourceExtensions> {
        match self {
            Self::Generation(request) => Some(&request.extensions),
            Self::Embeddings(request) => Some(&request.extensions),
            Self::TokenCount(request) => Some(&request.extensions),
            Self::Images(operation) => Some(operation.extensions()),
            Self::Speech(request) => Some(&request.extensions),
            Self::Transcription(request) => Some(&request.extensions),
            Self::Video(operation) => Some(operation.extensions()),
            Self::Moderation(request) => Some(&request.extensions),
            Self::Models(operation) => Some(operation.extensions()),
        }
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct GenerationRequest {
    pub route: RouteSlug,
    pub messages: Vec<Message>,
    #[serde(default)]
    pub parameters: GenerationParameters,
    #[serde(default)]
    pub tools: Vec<ToolDefinition>,
    pub tool_choice: Option<ToolChoice>,
    pub response_format: Option<ResponseFormat>,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Serialize)]
pub struct GenerationParameters {
    pub max_output_tokens: Option<u32>,
    pub temperature: Option<f32>,
    pub top_p: Option<f32>,
    #[serde(default)]
    pub stop_sequences: Vec<String>,
    pub candidate_count: Option<u16>,
    pub seed: Option<i64>,
    pub parallel_tool_calls: Option<bool>,
    pub stream: bool,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct Message {
    pub role: MessageRole,
    #[serde(default)]
    pub content: Vec<ContentPart>,
    pub name: Option<String>,
    pub tool_call_id: Option<String>,
    #[serde(default)]
    pub tool_calls: Vec<ToolCall>,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum MessageRole {
    System,
    Developer,
    User,
    Assistant,
    Tool,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ContentPart {
    Text {
        text: String,
    },
    Image {
        source: MediaSource,
        detail: Option<String>,
    },
    InputAudio {
        media: MediaHandle,
        format: String,
    },
    InputFile {
        media: MediaHandle,
        mime_type: String,
        filename: String,
    },
    Refusal {
        text: String,
    },
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(tag = "kind", content = "value", rename_all = "snake_case")]
pub enum MediaSource {
    Uri(String),
    Handle(MediaHandle),
}

#[derive(Clone, Debug, Deserialize, Eq, Hash, PartialEq, Serialize)]
#[serde(transparent)]
pub struct MediaHandle(String);

pub const INLINE_MEDIA_HANDLE_PREFIX: &str = "urn:olp:inline-media:";

impl MediaHandle {
    #[must_use]
    pub fn new(value: impl Into<String>) -> Self {
        Self(value.into())
    }

    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

#[must_use]
pub fn inline_media_marker(handle: &MediaHandle) -> String {
    format!("{INLINE_MEDIA_HANDLE_PREFIX}{}", handle.as_str())
}

#[must_use]
pub fn media_handle_from_inline_marker(value: &str) -> Option<MediaHandle> {
    value
        .strip_prefix(INLINE_MEDIA_HANDLE_PREFIX)
        .filter(|value| !value.is_empty())
        .map(MediaHandle::new)
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ToolDefinition {
    pub name: String,
    pub description: Option<String>,
    pub input_schema: Value,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ToolCall {
    pub id: String,
    pub name: String,
    pub arguments: String,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(tag = "mode", content = "name", rename_all = "snake_case")]
pub enum ToolChoice {
    Auto,
    None,
    Required,
    Named(String),
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ResponseFormat {
    Text,
    JsonObject,
    JsonSchema {
        name: String,
        description: Option<String>,
        schema: Value,
        strict: Option<bool>,
    },
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct EmbeddingsRequest {
    pub route: RouteSlug,
    pub input: Vec<EmbeddingInput>,
    pub dimensions: Option<u32>,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(untagged)]
pub enum EmbeddingInput {
    Text(String),
    Tokens(Vec<u32>),
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct TokenCountRequest {
    pub route: RouteSlug,
    pub input: Vec<ContentPart>,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(tag = "kind", content = "request", rename_all = "snake_case")]
pub enum ImageOperation {
    Generation(ImageGenerationRequest),
    Edit(ImageEditRequest),
    Variation(ImageVariationRequest),
}

impl ImageOperation {
    #[must_use]
    pub const fn kind(&self) -> OperationKind {
        match self {
            Self::Generation(_) => OperationKind::ImageGeneration,
            Self::Edit(_) => OperationKind::ImageEdit,
            Self::Variation(_) => OperationKind::ImageVariation,
        }
    }

    #[must_use]
    pub const fn route(&self) -> &RouteSlug {
        match self {
            Self::Generation(request) => &request.route,
            Self::Edit(request) => &request.route,
            Self::Variation(request) => &request.route,
        }
    }

    #[must_use]
    pub const fn extensions(&self) -> &SourceExtensions {
        match self {
            Self::Generation(request) => &request.extensions,
            Self::Edit(request) => &request.extensions,
            Self::Variation(request) => &request.extensions,
        }
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ImageGenerationRequest {
    pub route: RouteSlug,
    pub prompt: String,
    pub count: Option<u16>,
    pub size: Option<String>,
    pub stream: bool,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ImageEditRequest {
    pub route: RouteSlug,
    pub images: Vec<MediaHandle>,
    pub mask: Option<MediaHandle>,
    pub prompt: String,
    pub stream: bool,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ImageVariationRequest {
    pub route: RouteSlug,
    pub image: MediaHandle,
    pub count: Option<u16>,
    pub size: Option<String>,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct SpeechRequest {
    pub route: RouteSlug,
    pub input: String,
    pub voice: String,
    pub format: Option<String>,
    pub stream: bool,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct TranscriptionRequest {
    pub route: RouteSlug,
    pub audio: MediaHandle,
    pub language: Option<String>,
    pub prompt: Option<String>,
    pub stream: bool,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(tag = "kind", content = "request", rename_all = "snake_case")]
pub enum VideoOperation {
    Create(VideoCreateRequest),
    List(VideoListRequest),
    Get(VideoJobRequest),
    Content(VideoJobRequest),
    Delete(VideoJobRequest),
}

impl VideoOperation {
    #[must_use]
    pub const fn kind(&self) -> OperationKind {
        match self {
            Self::Create(_) => OperationKind::VideoCreate,
            Self::List(_) => OperationKind::VideoList,
            Self::Get(_) => OperationKind::VideoGet,
            Self::Content(_) => OperationKind::VideoContent,
            Self::Delete(_) => OperationKind::VideoDelete,
        }
    }

    #[must_use]
    pub const fn route(&self) -> Option<&RouteSlug> {
        match self {
            Self::Create(request) => Some(&request.route),
            Self::List(request) => request.route.as_ref(),
            Self::Get(request) | Self::Content(request) | Self::Delete(request) => {
                request.route.as_ref()
            }
        }
    }

    #[must_use]
    pub const fn extensions(&self) -> &SourceExtensions {
        match self {
            Self::Create(request) => &request.extensions,
            Self::List(request) => &request.extensions,
            Self::Get(request) | Self::Content(request) | Self::Delete(request) => {
                &request.extensions
            }
        }
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct VideoCreateRequest {
    pub route: RouteSlug,
    pub prompt: String,
    pub input: Option<MediaHandle>,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct VideoListRequest {
    pub route: Option<RouteSlug>,
    pub cursor: Option<String>,
    pub limit: Option<u16>,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct VideoJobRequest {
    pub route: Option<RouteSlug>,
    pub job_id: String,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

/// Internal, source-scoped marker used only after OLP has durably persisted a
/// delete/cleanup intent. Connectors may then treat an upstream missing result
/// as successful reconciliation instead of reviving an already-deleted job.
pub const MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION: &str =
    "/__olp/media/delete_missing_is_success";

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ModerationRequest {
    pub route: RouteSlug,
    pub input: Vec<ContentPart>,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum ModelOperation {
    List {
        #[serde(default)]
        extensions: SourceExtensions,
    },
    Get {
        route: RouteSlug,
        #[serde(default)]
        extensions: SourceExtensions,
    },
}

impl ModelOperation {
    #[must_use]
    pub const fn kind(&self) -> OperationKind {
        match self {
            Self::List { .. } => OperationKind::ModelList,
            Self::Get { .. } => OperationKind::ModelGet,
        }
    }

    #[must_use]
    pub const fn route(&self) -> Option<&RouteSlug> {
        match self {
            Self::List { .. } => None,
            Self::Get { route, .. } => Some(route),
        }
    }

    #[must_use]
    pub const fn extensions(&self) -> &SourceExtensions {
        match self {
            Self::List { extensions } | Self::Get { extensions, .. } => extensions,
        }
    }
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Serialize)]
pub struct SourceExtensions {
    pub source: Option<Surface>,
    #[serde(default)]
    pub values: BTreeMap<String, Value>,
}

impl SourceExtensions {
    #[must_use]
    pub fn new(source: Surface, values: BTreeMap<String, Value>) -> Self {
        Self {
            source: Some(source),
            values,
        }
    }

    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.values.is_empty()
    }

    pub fn ensure_representable_on(&self, target: Surface) -> Result<(), ExtensionError> {
        if self.values.is_empty() || self.source == Some(target) {
            return Ok(());
        }

        Err(ExtensionError::CrossProtocol {
            source_surface: self.source,
            target,
            fields: self.values.keys().cloned().collect(),
        })
    }
}

#[derive(Clone, Debug, Error, PartialEq)]
pub enum ExtensionError {
    #[error(
        "source-scoped fields {fields:?} from {source_surface:?} cannot be represented on {target:?}"
    )]
    CrossProtocol {
        source_surface: Option<Surface>,
        target: Surface,
        fields: Vec<String>,
    },
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct CanonicalEvent {
    pub sequence: u64,
    #[serde(flatten)]
    pub kind: CanonicalEventKind,
}

impl CanonicalEvent {
    #[must_use]
    pub const fn new(sequence: u64, kind: CanonicalEventKind) -> Self {
        Self { sequence, kind }
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(tag = "event", rename_all = "snake_case")]
pub enum CanonicalEventKind {
    ResponseStart {
        response_id: Option<String>,
        provider_model: Option<String>,
    },
    MessageStart {
        output_index: u32,
        role: MessageRole,
    },
    TextDelta {
        output_index: u32,
        text: String,
    },
    RefusalDelta {
        output_index: u32,
        text: String,
    },
    ToolCallDelta {
        output_index: u32,
        tool_index: u32,
        id: Option<String>,
        name: Option<String>,
        arguments_delta: String,
    },
    Usage {
        usage: Usage,
    },
    Finish {
        output_index: u32,
        reason: FinishReason,
    },
    Error {
        error: CanonicalError,
    },
    SourceExtension {
        extensions: SourceExtensions,
    },
    Done,
}

#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct Usage {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub total_tokens: u64,
    pub cached_input_tokens: Option<u64>,
    pub reasoning_tokens: Option<u64>,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum FinishReason {
    Stop,
    Length,
    ToolCalls,
    ContentFilter,
    Error,
    Other(String),
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct CanonicalError {
    pub class: ErrorClass,
    pub message: String,
    pub provider_code: Option<String>,
    pub retryable: bool,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ErrorClass {
    Authentication,
    Authorization,
    InvalidRequest,
    RateLimit,
    Timeout,
    Transport,
    Upstream,
    Internal,
}

/// A provider-neutral result from an embeddings operation.
#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct EmbeddingsResult {
    pub model: Option<String>,
    pub data: Vec<EmbeddingVector>,
    pub usage: Option<Usage>,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct EmbeddingVector {
    pub index: u32,
    pub values: Vec<f32>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct TokenCountResult {
    pub input_tokens: u64,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

/// Metadata describing media that has been placed in a bounded spool by an
/// adapter. Core never owns or copies the media bytes themselves.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct MediaArtifact {
    pub handle: MediaHandle,
    pub content_type: Option<String>,
    pub content_length: Option<u64>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ImagesResult {
    pub created_at: Option<i64>,
    pub images: Vec<ImageArtifact>,
    pub usage: Option<Usage>,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ImageArtifact {
    pub source: MediaSource,
    pub revised_prompt: Option<String>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct SpeechResult {
    pub audio: MediaArtifact,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct TranscriptionResult {
    pub text: String,
    pub language: Option<String>,
    pub duration_seconds: Option<f64>,
    #[serde(default)]
    pub segments: Vec<TranscriptionSegment>,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct TranscriptionSegment {
    pub id: Option<u32>,
    pub start_seconds: f64,
    pub end_seconds: f64,
    pub text: String,
    pub speaker: Option<String>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ModerationResult {
    pub id: Option<String>,
    pub model: Option<String>,
    pub results: Vec<ModerationItem>,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ModerationItem {
    pub flagged: bool,
    pub categories: BTreeMap<String, bool>,
    pub category_scores: BTreeMap<String, f64>,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum VideoStatus {
    Queued,
    InProgress,
    Completed,
    Failed,
    Other(String),
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct VideoJobResult {
    pub id: String,
    pub model: Option<String>,
    pub status: VideoStatus,
    pub progress_percent: Option<f32>,
    pub created_at: Option<i64>,
    pub completed_at: Option<i64>,
    pub expires_at: Option<i64>,
    pub prompt: Option<String>,
    pub seconds: Option<String>,
    pub size: Option<String>,
    pub error: Option<CanonicalError>,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct VideoListResult {
    pub jobs: Vec<VideoJobResult>,
    pub first_id: Option<String>,
    pub last_id: Option<String>,
    pub has_more: bool,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct VideoContentResult {
    pub media: MediaArtifact,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct VideoDeleteResult {
    pub id: String,
    pub deleted: bool,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ModelDescriptor {
    pub id: String,
    pub created_at: Option<i64>,
    pub owned_by: Option<String>,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ModelListResult {
    pub models: Vec<ModelDescriptor>,
    #[serde(default)]
    pub extensions: SourceExtensions,
}

/// Connector-neutral output for operations whose response is not a generation
/// event sequence. Content-bearing variants intentionally omit `Debug` so a
/// future diagnostic cannot accidentally print transcription text or prompts.
#[derive(Clone, Deserialize, PartialEq, Serialize)]
#[serde(tag = "result", content = "value", rename_all = "snake_case")]
pub enum CanonicalResult {
    TokenCount(TokenCountResult),
    Embeddings(EmbeddingsResult),
    Images(ImagesResult),
    Speech(SpeechResult),
    Transcription(TranscriptionResult),
    Moderation(ModerationResult),
    VideoJob(VideoJobResult),
    VideoList(VideoListResult),
    VideoContent(VideoContentResult),
    VideoDelete(VideoDeleteResult),
    ModelList(ModelListResult),
    Model(ModelDescriptor),
}

pub fn validate_event_sequence(events: &[CanonicalEvent]) -> Result<(), EventSequenceError> {
    let mut validator = EventSequenceValidator::new();
    for event in events {
        validator.push(event)?;
    }
    validator.finish()
}

/// Incrementally validates the ordering and terminal invariant of a canonical
/// event stream before protocol-specific encoders observe it.
#[derive(Clone, Copy, Debug, Default)]
pub struct EventSequenceValidator {
    expected: u64,
    done: bool,
}

impl EventSequenceValidator {
    #[must_use]
    pub const fn new() -> Self {
        Self {
            expected: 0,
            done: false,
        }
    }

    pub fn push(&mut self, event: &CanonicalEvent) -> Result<(), EventSequenceError> {
        if self.done {
            return Err(EventSequenceError::AfterDone {
                sequence: event.sequence,
            });
        }
        if event.sequence != self.expected {
            return Err(EventSequenceError::OutOfOrder {
                expected: self.expected,
                actual: event.sequence,
            });
        }
        self.done = matches!(event.kind, CanonicalEventKind::Done);
        self.expected = self.expected.saturating_add(1);
        Ok(())
    }

    pub fn finish(self) -> Result<(), EventSequenceError> {
        if self.done {
            Ok(())
        } else {
            Err(EventSequenceError::MissingDone {
                next_sequence: self.expected,
            })
        }
    }

    #[must_use]
    pub const fn is_complete(self) -> bool {
        self.done
    }
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
pub enum EventSequenceError {
    #[error("expected canonical event sequence {expected}, got {actual}")]
    OutOfOrder { expected: u64, actual: u64 },
    #[error("canonical event {sequence} appeared after the terminal done event")]
    AfterDone { sequence: u64 },
    #[error("canonical event stream ended before done; next sequence would be {next_sequence}")]
    MissingDone { next_sequence: u64 },
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct RequestMetadata {
    pub request_id: RequestId,
    pub operation: OperationKind,
    pub surface: Surface,
    pub mode: TransportMode,
}

use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;

use crate::RouteSlug;

use super::{OperationKind, Surface};

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

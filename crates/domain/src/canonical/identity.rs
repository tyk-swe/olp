use std::{fmt, str::FromStr};

use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::RequestId;

#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Surface {
    #[serde(rename = "openai")]
    OpenAi,
    Anthropic,
    Gemini,
}

impl Surface {
    pub const ALL: [Self; 3] = [Self::OpenAi, Self::Anthropic, Self::Gemini];

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::OpenAi => "openai",
            Self::Anthropic => "anthropic",
            Self::Gemini => "gemini",
        }
    }
}

impl FromStr for Surface {
    type Err = InvalidSurface;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        match value {
            "openai" => Ok(Self::OpenAi),
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

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct RequestMetadata {
    pub request_id: RequestId,
    pub operation: OperationKind,
    pub surface: Surface,
    pub mode: TransportMode,
}

use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::domain::RequestId;

closed_string_enum! {
    pub enum Surface {
        OpenAi => "openai",
        Anthropic => "anthropic",
        Gemini => "gemini",
    }
    parse_error InvalidSurface => |_| InvalidSurface;
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
#[error("invalid canonical surface")]
pub struct InvalidSurface;

closed_string_enum! {
    pub enum TransportMode {
        Unary => "unary",
        Streaming => "streaming",
        Async => "async",
    }
    parse_error InvalidTransportMode => |_| InvalidTransportMode;
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
#[error("invalid canonical transport mode")]
pub struct InvalidTransportMode;

closed_string_enum! {
    pub enum OperationKind {
        Generation => "generation",
        Embeddings => "embeddings",
        TokenCount => "token_count",
        ImageGeneration => "image_generation",
        ImageEdit => "image_edit",
        ImageVariation => "image_variation",
        Speech => "speech",
        Transcription => "transcription",
        VideoCreate => "video_create",
        VideoList => "video_list",
        VideoGet => "video_get",
        VideoContent => "video_content",
        VideoDelete => "video_delete",
        Moderation => "moderation",
        ModelList => "model_list",
        ModelGet => "model_get",
    }
    parse_error InvalidOperationKind => |_| InvalidOperationKind;
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

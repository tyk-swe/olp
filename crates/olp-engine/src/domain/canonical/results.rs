use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};

use super::{CanonicalError, MediaHandle, MediaSource, SourceExtensions, Usage};

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

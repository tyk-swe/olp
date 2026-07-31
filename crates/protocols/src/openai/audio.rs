use std::collections::BTreeMap;

use olp_domain::{
    Operation, RouteSlug, RouteSlugError, SourceExtensions,
    SpeechRequest as CanonicalSpeechRequest, SpeechResult, Surface,
    TranscriptionRequest as CanonicalTranscriptionRequest, TranscriptionResult,
    TranscriptionSegment,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;

use super::extensions::{apply_pointer_extensions, collect_extra};
use super::media::{BinaryMediaBody, BoundedMediaPart};

pub const DEFAULT_AUDIO_UPLOAD_LIMIT: u64 = 25 * 1024 * 1024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TranscriptionResponseFormat {
    Json,
    Text,
    Srt,
    VerboseJson,
    Vtt,
    DiarizedJson,
}

impl TranscriptionResponseFormat {
    pub fn parse(value: Option<&str>) -> Result<Self, AudioCodecError> {
        match value.unwrap_or("json") {
            "json" => Ok(Self::Json),
            "text" => Ok(Self::Text),
            "srt" => Ok(Self::Srt),
            "verbose_json" => Ok(Self::VerboseJson),
            "vtt" => Ok(Self::Vtt),
            "diarized_json" => Ok(Self::DiarizedJson),
            value => Err(AudioCodecError::UnsupportedTranscriptionFormat(
                value.to_owned(),
            )),
        }
    }

    #[must_use]
    pub const fn is_text(self) -> bool {
        matches!(self, Self::Text | Self::Srt | Self::Vtt)
    }
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiSpeechRequest {
    pub model: String,
    pub input: String,
    pub voice: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub response_format: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub instructions: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub speed: Option<f32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub stream_format: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_speech(request: OpenAiSpeechRequest) -> Result<Operation, AudioCodecError> {
    if request.input.is_empty() {
        return Err(AudioCodecError::EmptySpeechInput);
    }
    if request.voice.is_empty() {
        return Err(AudioCodecError::EmptyVoice);
    }
    if request
        .speed
        .is_some_and(|speed| !(0.25..=4.0).contains(&speed))
    {
        return Err(AudioCodecError::InvalidSpeed);
    }
    let route = RouteSlug::parse(request.model)?;
    let stream = request.stream_format.as_deref() == Some("sse");
    let mut extensions = BTreeMap::new();
    collect_extra("", &request.extra, &mut extensions);
    capture_string(&mut extensions, "/instructions", request.instructions);
    if let Some(speed) = request.speed {
        extensions.insert("/speed".into(), Value::from(f64::from(speed)));
    }
    // `sse` is represented canonically by `stream`; retaining it again as an
    // extension would collide when the request is encoded for the same
    // protocol. Preserve only future/non-streaming vendor values verbatim.
    if request.stream_format.as_deref() != Some("sse") {
        capture_string(&mut extensions, "/stream_format", request.stream_format);
    }
    Ok(Operation::Speech(CanonicalSpeechRequest {
        route,
        input: request.input,
        voice: request.voice,
        format: request.response_format,
        stream,
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    }))
}

pub fn encode_speech(
    request: &CanonicalSpeechRequest,
    upstream_model: &str,
) -> Result<OpenAiSpeechRequest, AudioCodecError> {
    request
        .extensions
        .ensure_representable_on(Surface::OpenAi)?;
    apply_pointer_extensions(
        OpenAiSpeechRequest {
            model: upstream_model.into(),
            input: request.input.clone(),
            voice: request.voice.clone(),
            response_format: request.format.clone(),
            instructions: None,
            speed: None,
            stream_format: request.stream.then(|| "sse".into()),
            extra: BTreeMap::new(),
        },
        &request.extensions.values,
    )
    .map_err(AudioCodecError::InvalidExtension)
}

pub fn decode_speech_body(body: BinaryMediaBody) -> SpeechResult {
    SpeechResult {
        audio: body.media,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    }
}

pub fn encode_speech_body(result: &SpeechResult) -> Result<BinaryMediaBody, AudioCodecError> {
    result.extensions.ensure_representable_on(Surface::OpenAi)?;
    if !result.extensions.values.is_empty() {
        return Err(AudioCodecError::BinaryExtensionsUnsupported);
    }
    Ok(BinaryMediaBody {
        media: result.audio.clone(),
    })
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiTranscriptionRequest {
    pub model: String,
    pub file: BoundedMediaPart,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub language: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub prompt: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub response_format: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub temperature: Option<f32>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub include: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub timestamp_granularities: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub chunking_strategy: Option<Value>,
    #[serde(default, skip_serializing_if = "is_false")]
    pub stream: bool,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

const fn is_false(value: &bool) -> bool {
    !*value
}

pub fn decode_transcription(
    mut request: OpenAiTranscriptionRequest,
) -> Result<Operation, AudioCodecError> {
    validate_audio_part(&request.file)?;
    if request
        .temperature
        .is_some_and(|value| !(0.0..=1.0).contains(&value))
    {
        return Err(AudioCodecError::InvalidTemperature);
    }
    let response_format = TranscriptionResponseFormat::parse(request.response_format.as_deref())?;
    validate_transcription_options(&request, response_format)?;
    let known_speakers = take_known_speakers(&mut request.extra, response_format)?;
    let route = RouteSlug::parse(request.model)?;
    let mut extensions = BTreeMap::new();
    collect_extra("", &request.extra, &mut extensions);
    capture_string(&mut extensions, "/response_format", request.response_format);
    if let Some(known_speakers) = known_speakers {
        extensions.insert(
            "/known_speaker_names".into(),
            serde_json::to_value(known_speakers.names)?,
        );
        extensions.insert(
            "/known_speaker_references".into(),
            serde_json::to_value(known_speakers.references)?,
        );
    }
    if let Some(temperature) = request.temperature {
        extensions.insert("/temperature".into(), Value::from(f64::from(temperature)));
    }
    if !request.include.is_empty() {
        extensions.insert("/include".into(), serde_json::to_value(request.include)?);
    }
    if !request.timestamp_granularities.is_empty() {
        extensions.insert(
            "/timestamp_granularities".into(),
            serde_json::to_value(request.timestamp_granularities)?,
        );
    }
    if let Some(strategy) = request.chunking_strategy {
        extensions.insert("/chunking_strategy".into(), strategy);
    }
    Ok(Operation::Transcription(CanonicalTranscriptionRequest {
        route,
        audio: request.file.handle,
        language: request.language,
        prompt: request.prompt,
        stream: request.stream,
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    }))
}

pub fn encode_transcription(
    request: &CanonicalTranscriptionRequest,
    upstream_model: &str,
    mut resolve_part: impl FnMut(&olp_domain::MediaHandle) -> Result<BoundedMediaPart, AudioCodecError>,
) -> Result<OpenAiTranscriptionRequest, AudioCodecError> {
    request
        .extensions
        .ensure_representable_on(Surface::OpenAi)?;
    let file = resolve_part(&request.audio)?;
    validate_audio_part(&file)?;
    let wire = apply_pointer_extensions(
        OpenAiTranscriptionRequest {
            model: upstream_model.into(),
            file,
            language: request.language.clone(),
            prompt: request.prompt.clone(),
            response_format: None,
            temperature: None,
            include: Vec::new(),
            timestamp_granularities: Vec::new(),
            chunking_strategy: None,
            stream: request.stream,
            extra: BTreeMap::new(),
        },
        &request.extensions.values,
    )
    .map_err(AudioCodecError::InvalidExtension)?;
    let response_format = TranscriptionResponseFormat::parse(wire.response_format.as_deref())?;
    validate_transcription_options(&wire, response_format)?;
    let mut extra = wire.extra.clone();
    take_known_speakers(&mut extra, response_format)?;
    Ok(wire)
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct KnownSpeakers {
    names: Vec<String>,
    references: Vec<String>,
}

fn take_known_speakers(
    extra: &mut BTreeMap<String, Value>,
    response_format: TranscriptionResponseFormat,
) -> Result<Option<KnownSpeakers>, AudioCodecError> {
    let names = take_string_array(extra, "known_speaker_names")?;
    let references = take_string_array(extra, "known_speaker_references")?;
    match (names, references) {
        (None, None) => Ok(None),
        (Some(names), Some(references))
            if response_format == TranscriptionResponseFormat::DiarizedJson
                && !names.is_empty()
                && names.len() <= 4
                && names.len() == references.len()
                && names
                    .iter()
                    .all(|name| !name.trim().is_empty() && name.len() <= 64)
                && references
                    .iter()
                    .all(|reference| reference.starts_with("data:audio/")) =>
        {
            Ok(Some(KnownSpeakers { names, references }))
        }
        _ => Err(AudioCodecError::InvalidKnownSpeakers),
    }
}

fn take_string_array(
    extra: &mut BTreeMap<String, Value>,
    field: &str,
) -> Result<Option<Vec<String>>, AudioCodecError> {
    let value = extra
        .remove(field)
        .or_else(|| extra.remove(&format!("{field}[]")));
    let Some(value) = value else { return Ok(None) };
    match value {
        Value::String(value) => Ok(Some(vec![value])),
        Value::Array(values) => values
            .into_iter()
            .map(|value| {
                value
                    .as_str()
                    .map(str::to_owned)
                    .ok_or(AudioCodecError::InvalidKnownSpeakers)
            })
            .collect::<Result<Vec<_>, _>>()
            .map(Some),
        _ => Err(AudioCodecError::InvalidKnownSpeakers),
    }
}

fn validate_transcription_options(
    request: &OpenAiTranscriptionRequest,
    response_format: TranscriptionResponseFormat,
) -> Result<(), AudioCodecError> {
    if !request.timestamp_granularities.is_empty()
        && (response_format != TranscriptionResponseFormat::VerboseJson
            || request
                .timestamp_granularities
                .iter()
                .any(|value| !matches!(value.as_str(), "word" | "segment")))
    {
        return Err(AudioCodecError::InvalidTimestampGranularities);
    }
    if !request.include.is_empty()
        && (response_format != TranscriptionResponseFormat::Json
            || request.include.iter().any(|value| value != "logprobs"))
    {
        return Err(AudioCodecError::InvalidTranscriptionInclude);
    }
    Ok(())
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
#[serde(untagged)]
pub enum OpenAiTranscriptionResponse {
    Json(OpenAiTranscriptionJson),
    Text(String),
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiTranscriptionJson {
    pub text: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub language: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub duration: Option<f64>,
    #[serde(default)]
    pub segments: Vec<OpenAiTranscriptionSegment>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiTranscriptionSegment {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub id: Option<u32>,
    pub start: f64,
    pub end: f64,
    pub text: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub speaker: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_transcription_response(
    response: OpenAiTranscriptionResponse,
) -> Result<TranscriptionResult, AudioCodecError> {
    Ok(match response {
        OpenAiTranscriptionResponse::Text(text) => TranscriptionResult {
            text,
            language: None,
            duration_seconds: None,
            segments: Vec::new(),
            extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        },
        OpenAiTranscriptionResponse::Json(response) => {
            validate_transcription_timing(&response)?;
            let mut extensions = BTreeMap::new();
            collect_extra("", &response.extra, &mut extensions);
            let segments = response
                .segments
                .into_iter()
                .enumerate()
                .map(|(index, segment)| {
                    collect_extra(
                        &format!("/segments/{index}"),
                        &segment.extra,
                        &mut extensions,
                    );
                    TranscriptionSegment {
                        id: segment.id,
                        start_seconds: segment.start,
                        end_seconds: segment.end,
                        text: segment.text,
                        speaker: segment.speaker,
                    }
                })
                .collect();
            TranscriptionResult {
                text: response.text,
                language: response.language,
                duration_seconds: response.duration,
                segments,
                extensions: SourceExtensions::new(Surface::OpenAi, extensions),
            }
        }
    })
}

pub fn encode_transcription_response(
    result: &TranscriptionResult,
) -> Result<OpenAiTranscriptionResponse, AudioCodecError> {
    result.extensions.ensure_representable_on(Surface::OpenAi)?;
    let segments = result
        .segments
        .iter()
        .map(|segment| OpenAiTranscriptionSegment {
            id: segment.id,
            start: segment.start_seconds,
            end: segment.end_seconds,
            text: segment.text.clone(),
            speaker: segment.speaker.clone(),
            extra: BTreeMap::new(),
        })
        .collect();
    let response = OpenAiTranscriptionJson {
        text: result.text.clone(),
        language: result.language.clone(),
        duration: result.duration_seconds,
        segments,
        extra: BTreeMap::new(),
    };
    validate_transcription_timing(&response)?;
    let wire = apply_pointer_extensions(
        OpenAiTranscriptionResponse::Json(response),
        &result.extensions.values,
    )
    .map_err(AudioCodecError::InvalidExtension)?;
    if let OpenAiTranscriptionResponse::Json(response) = &wire {
        validate_transcription_timing(response)?;
    }
    Ok(wire)
}

fn validate_transcription_timing(
    response: &OpenAiTranscriptionJson,
) -> Result<(), AudioCodecError> {
    if response
        .duration
        .is_some_and(|duration| !duration.is_finite() || duration < 0.0)
    {
        return Err(AudioCodecError::InvalidTranscriptionTiming);
    }
    let mut previous_start = None;
    for segment in &response.segments {
        if !segment.start.is_finite()
            || !segment.end.is_finite()
            || segment.start < 0.0
            || segment.end < segment.start
            || response
                .duration
                .is_some_and(|duration| segment.end > duration)
            || previous_start.is_some_and(|start| segment.start < start)
        {
            return Err(AudioCodecError::InvalidTranscriptionTiming);
        }
        previous_start = Some(segment.start);
    }
    Ok(())
}

fn validate_audio_part(part: &BoundedMediaPart) -> Result<(), AudioCodecError> {
    if part.content_length > part.maximum_length || part.maximum_length > DEFAULT_AUDIO_UPLOAD_LIMIT
    {
        return Err(AudioCodecError::InvalidMediaPart);
    }
    Ok(())
}

fn capture_string(extensions: &mut BTreeMap<String, Value>, path: &str, value: Option<String>) {
    if let Some(value) = value {
        extensions.insert(path.into(), Value::String(value));
    }
}

#[derive(Debug, Error)]
pub enum AudioCodecError {
    #[error(transparent)]
    InvalidRoute(#[from] RouteSlugError),
    #[error(transparent)]
    Extensions(#[from] olp_domain::ExtensionError),
    #[error(transparent)]
    Json(#[from] serde_json::Error),
    #[error("speech input cannot be empty")]
    EmptySpeechInput,
    #[error("speech voice cannot be empty")]
    EmptyVoice,
    #[error("speech speed must be between 0.25 and 4.0")]
    InvalidSpeed,
    #[error("transcription temperature must be between 0 and 1")]
    InvalidTemperature,
    #[error("unsupported transcription response format: {0}")]
    UnsupportedTranscriptionFormat(String),
    #[error("timestamp granularities require verbose_json and values word or segment")]
    InvalidTimestampGranularities,
    #[error("transcription include supports only logprobs with json responses")]
    InvalidTranscriptionInclude,
    #[error("known speakers require 1-4 paired names and audio data URLs with diarized_json")]
    InvalidKnownSpeakers,
    #[error("transcription file violates its bounded media limit")]
    InvalidMediaPart,
    #[error(
        "transcription duration and segment timestamps must be finite, nonnegative, and ordered"
    )]
    InvalidTranscriptionTiming,
    #[error("invalid source extension path: {0}")]
    InvalidExtension(String),
    #[error("binary speech extensions require an HTTP header representation")]
    BinaryExtensionsUnsupported,
    #[error("audio chunk staging failed: {0}")]
    Staging(String),
}

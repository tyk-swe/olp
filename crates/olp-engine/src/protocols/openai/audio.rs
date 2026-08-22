use std::collections::BTreeMap;

use crate::domain::{
    canonical::{
        events::{Event, Kind, Usage},
        identity::Surface,
        requests::{
            MessageRole, Operation, SourceExtensions, SpeechRequest as CanonicalSpeechRequest,
            TranscriptionRequest as CanonicalTranscriptionRequest,
        },
        results::{MediaArtifact, SpeechResult, TranscriptionResult, TranscriptionSegment},
    },
    ids::{RouteSlug, RouteSlugError},
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;

use super::extensions::{apply_pointer_extensions, collect_extra};
use super::media::{BinaryMediaBody, BoundedMediaPart};
use crate::protocols::sse::{DEFAULT_MAX_EVENT_BYTES, DecodeError, Decoder as SseDecoder, Frame};

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
    pub fn parse(value: Option<&str>) -> Result<Self, Error> {
        match value.unwrap_or("json") {
            "json" => Ok(Self::Json),
            "text" => Ok(Self::Text),
            "srt" => Ok(Self::Srt),
            "verbose_json" => Ok(Self::VerboseJson),
            "vtt" => Ok(Self::Vtt),
            "diarized_json" => Ok(Self::DiarizedJson),
            value => Err(Error::UnsupportedTranscriptionFormat(value.to_owned())),
        }
    }

    #[must_use]
    pub const fn is_text(self) -> bool {
        matches!(self, Self::Text | Self::Srt | Self::Vtt)
    }
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct SpeechRequest {
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

pub fn decode_speech(request: SpeechRequest) -> Result<Operation, Error> {
    if request.input.is_empty() {
        return Err(Error::EmptySpeechInput);
    }
    if request.voice.is_empty() {
        return Err(Error::EmptyVoice);
    }
    if request
        .speed
        .is_some_and(|speed| !(0.25..=4.0).contains(&speed))
    {
        return Err(Error::InvalidSpeed);
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
) -> Result<SpeechRequest, Error> {
    request
        .extensions
        .ensure_representable_on(Surface::OpenAi)?;
    apply_pointer_extensions(
        SpeechRequest {
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
    .map_err(Error::InvalidExtension)
}

pub fn decode_speech_body(body: BinaryMediaBody) -> SpeechResult {
    SpeechResult {
        audio: body.media,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    }
}

pub fn encode_speech_body(result: &SpeechResult) -> Result<BinaryMediaBody, Error> {
    result.extensions.ensure_representable_on(Surface::OpenAi)?;
    if !result.extensions.values.is_empty() {
        return Err(Error::BinaryExtensionsUnsupported);
    }
    Ok(BinaryMediaBody {
        media: result.audio.clone(),
    })
}

// ponytail: unwired in production — streamed audio flows through raw SSE
// passthrough (raw_sse_response / is_raw_media_terminal), not this semantic
// speech-stream codec. Kept only for the protocol_json fuzz target + unit tests.
// Delete this codec together with its fuzz arm if a passthrough→semantic switch
// is not on the roadmap.
#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct SpeechStreamEvent {
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub audio: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub delta: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub enum SpeechStreamUpdate {
    Audio {
        media: MediaArtifact,
        extensions: SourceExtensions,
    },
    Done {
        extensions: SourceExtensions,
    },
}

pub fn decode_speech_stream_event(
    event: SpeechStreamEvent,
    mut stage_base64: impl FnMut(&str) -> Result<MediaArtifact, Error>,
) -> Result<SpeechStreamUpdate, Error> {
    let mut extensions = BTreeMap::new();
    collect_extra("", &event.extra, &mut extensions);
    match event.kind.as_str() {
        "speech.audio.delta" => {
            let encoded = event
                .audio
                .or(event.delta)
                .ok_or(Error::MissingAudioDelta)?;
            Ok(SpeechStreamUpdate::Audio {
                media: stage_base64(&encoded)?,
                extensions: SourceExtensions::new(Surface::OpenAi, extensions),
            })
        }
        "speech.audio.done" => Ok(SpeechStreamUpdate::Done {
            extensions: SourceExtensions::new(Surface::OpenAi, extensions),
        }),
        _ => Err(Error::UnsupportedStreamEvent(event.kind)),
    }
}

pub fn encode_speech_stream_update(
    update: &SpeechStreamUpdate,
    mut read_base64: impl FnMut(&MediaArtifact) -> Result<String, Error>,
) -> Result<SpeechStreamEvent, Error> {
    let (kind, audio, extensions) = match update {
        SpeechStreamUpdate::Audio { media, extensions } => {
            extensions.ensure_representable_on(Surface::OpenAi)?;
            ("speech.audio.delta", Some(read_base64(media)?), extensions)
        }
        SpeechStreamUpdate::Done { extensions } => {
            extensions.ensure_representable_on(Surface::OpenAi)?;
            ("speech.audio.done", None, extensions)
        }
    };
    apply_pointer_extensions(
        SpeechStreamEvent {
            kind: kind.into(),
            audio,
            delta: None,
            extra: BTreeMap::new(),
        },
        &extensions.values,
    )
    .map_err(Error::InvalidExtension)
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct TranscriptionRequest {
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

pub fn decode_transcription(mut request: TranscriptionRequest) -> Result<Operation, Error> {
    validate_audio_part(&request.file)?;
    if request
        .temperature
        .is_some_and(|value| !(0.0..=1.0).contains(&value))
    {
        return Err(Error::InvalidTemperature);
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
    mut resolve_part: impl FnMut(
        &crate::domain::canonical::requests::MediaHandle,
    ) -> Result<BoundedMediaPart, Error>,
) -> Result<TranscriptionRequest, Error> {
    request
        .extensions
        .ensure_representable_on(Surface::OpenAi)?;
    let file = resolve_part(&request.audio)?;
    validate_audio_part(&file)?;
    let wire = apply_pointer_extensions(
        TranscriptionRequest {
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
    .map_err(Error::InvalidExtension)?;
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
) -> Result<Option<KnownSpeakers>, Error> {
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
        _ => Err(Error::InvalidKnownSpeakers),
    }
}

fn take_string_array(
    extra: &mut BTreeMap<String, Value>,
    field: &str,
) -> Result<Option<Vec<String>>, Error> {
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
                    .ok_or(Error::InvalidKnownSpeakers)
            })
            .collect::<Result<Vec<_>, _>>()
            .map(Some),
        _ => Err(Error::InvalidKnownSpeakers),
    }
}

fn validate_transcription_options(
    request: &TranscriptionRequest,
    response_format: TranscriptionResponseFormat,
) -> Result<(), Error> {
    if !request.timestamp_granularities.is_empty()
        && (response_format != TranscriptionResponseFormat::VerboseJson
            || request
                .timestamp_granularities
                .iter()
                .any(|value| !matches!(value.as_str(), "word" | "segment")))
    {
        return Err(Error::InvalidTimestampGranularities);
    }
    if !request.include.is_empty()
        && (response_format != TranscriptionResponseFormat::Json
            || request.include.iter().any(|value| value != "logprobs"))
    {
        return Err(Error::InvalidTranscriptionInclude);
    }
    Ok(())
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
#[serde(untagged)]
pub enum TranscriptionResponse {
    Json(Transcription),
    Text(String),
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct Transcription {
    pub text: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub language: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub duration: Option<f64>,
    #[serde(default)]
    pub segments: Vec<Segment>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct Segment {
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

pub fn decode_transcription_response(response: TranscriptionResponse) -> TranscriptionResult {
    match response {
        TranscriptionResponse::Text(text) => TranscriptionResult {
            text,
            language: None,
            duration_seconds: None,
            segments: Vec::new(),
            extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        },
        TranscriptionResponse::Json(response) => {
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
    }
}

pub fn encode_transcription_response(
    result: &TranscriptionResult,
) -> Result<TranscriptionResponse, Error> {
    result.extensions.ensure_representable_on(Surface::OpenAi)?;
    let segments = result
        .segments
        .iter()
        .map(|segment| Segment {
            id: segment.id,
            start: segment.start_seconds,
            end: segment.end_seconds,
            text: segment.text.clone(),
            speaker: segment.speaker.clone(),
            extra: BTreeMap::new(),
        })
        .collect();
    apply_pointer_extensions(
        TranscriptionResponse::Json(Transcription {
            text: result.text.clone(),
            language: result.language.clone(),
            duration: result.duration_seconds,
            segments,
            extra: BTreeMap::new(),
        }),
        &result.extensions.values,
    )
    .map_err(Error::InvalidExtension)
}

// ponytail: unwired in production — streamed transcription flows through raw
// SSE passthrough, not this semantic Decoder/Encoder. Kept only for the
// sse_decoder fuzz target + unit tests. Delete with its fuzz arm if semantic
// streaming stays unplanned.
pub struct Decoder {
    sse: SseDecoder,
    sequence: u64,
    started: bool,
    done: bool,
}

impl std::fmt::Debug for Decoder {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("Decoder")
            .field("next_sequence", &self.sequence)
            .field("started", &self.started)
            .field("done", &self.done)
            .finish_non_exhaustive()
    }
}

impl Default for Decoder {
    fn default() -> Self {
        Self::new()
    }
}

impl Decoder {
    #[must_use]
    pub fn new() -> Self {
        Self {
            sse: SseDecoder::new(DEFAULT_MAX_EVENT_BYTES),
            sequence: 0,
            started: false,
            done: false,
        }
    }

    pub fn push(&mut self, bytes: &[u8]) -> Result<Vec<Event>, Error> {
        let frames = self.sse.push(bytes)?;
        self.decode_frames(frames)
    }

    pub fn finish(&mut self) -> Result<Vec<Event>, Error> {
        let frames = self.sse.finish()?;
        let events = self.decode_frames(frames)?;
        if !self.done {
            return Err(Error::UnexpectedStreamEof);
        }
        Ok(events)
    }

    fn decode_frames(
        &mut self,
        frames: Vec<crate::protocols::sse::Frame>,
    ) -> Result<Vec<Event>, Error> {
        let mut output = Vec::new();
        for frame in frames {
            if self.done {
                return Err(Error::DataAfterDone);
            }
            let value: Value = serde_json::from_str(&frame.data)?;
            let kind = value
                .get("type")
                .and_then(Value::as_str)
                .or(frame.event.as_deref())
                .ok_or(Error::MissingStreamEventType)?;
            self.ensure_started(&mut output);
            match kind {
                "transcript.text.delta" => self.emit(
                    &mut output,
                    Kind::TextDelta {
                        output_index: 0,
                        text: value
                            .get("delta")
                            .and_then(Value::as_str)
                            .ok_or(Error::MissingTranscriptDelta)?
                            .to_owned(),
                    },
                ),
                "transcript.text.done" => {
                    if let Some(usage) = value.get("usage") {
                        let input_tokens = usage
                            .get("input_tokens")
                            .and_then(Value::as_u64)
                            .unwrap_or(0);
                        let output_tokens = usage
                            .get("output_tokens")
                            .and_then(Value::as_u64)
                            .unwrap_or(0);
                        self.emit(
                            &mut output,
                            Kind::Usage {
                                usage: Usage {
                                    input_tokens,
                                    output_tokens,
                                    total_tokens: usage
                                        .get("total_tokens")
                                        .and_then(Value::as_u64)
                                        .unwrap_or(input_tokens.saturating_add(output_tokens)),
                                    cached_input_tokens: None,
                                    reasoning_tokens: None,
                                },
                            },
                        );
                    }
                    self.emit(
                        &mut output,
                        Kind::Finish {
                            output_index: 0,
                            reason: crate::domain::canonical::events::FinishReason::Stop,
                        },
                    );
                    self.emit(&mut output, Kind::Done);
                    self.done = true;
                }
                _ => self.emit(
                    &mut output,
                    Kind::SourceExtension {
                        extensions: SourceExtensions::new(
                            Surface::OpenAi,
                            BTreeMap::from([(format!("/stream/{kind}"), value)]),
                        ),
                    },
                ),
            }
        }
        Ok(output)
    }

    fn ensure_started(&mut self, output: &mut Vec<Event>) {
        if self.started {
            return;
        }
        self.emit(
            output,
            Kind::ResponseStart {
                response_id: None,
                provider_model: None,
            },
        );
        self.emit(
            output,
            Kind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            },
        );
        self.started = true;
    }

    fn emit(&mut self, output: &mut Vec<Event>, kind: Kind) {
        output.push(Event::new(self.sequence, kind));
        self.sequence = self.sequence.saturating_add(1);
    }
}

pub struct Encoder {
    next_sequence: u64,
    usage: Option<Usage>,
    done: bool,
}

impl std::fmt::Debug for Encoder {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("Encoder")
            .field("next_sequence", &self.next_sequence)
            .field("has_usage", &self.usage.is_some())
            .field("done", &self.done)
            .finish()
    }
}

impl Default for Encoder {
    fn default() -> Self {
        Self::new()
    }
}

impl Encoder {
    #[must_use]
    pub const fn new() -> Self {
        Self {
            next_sequence: 0,
            usage: None,
            done: false,
        }
    }

    pub fn push(&mut self, event: &Event) -> Result<Vec<Frame>, Error> {
        if self.done {
            return Err(Error::DataAfterDone);
        }
        if event.sequence != self.next_sequence {
            return Err(Error::OutOfOrder {
                expected: self.next_sequence,
                actual: event.sequence,
            });
        }
        self.next_sequence = self.next_sequence.saturating_add(1);
        let frames = match &event.kind {
            Kind::ResponseStart { .. } | Kind::MessageStart { .. } | Kind::Finish { .. } => {
                Vec::new()
            }
            Kind::TextDelta { text, .. } => vec![transcription_sse_frame(
                "transcript.text.delta",
                serde_json::json!({"delta": text}),
            )?],
            Kind::Usage { usage } => {
                self.usage = Some(*usage);
                Vec::new()
            }
            Kind::SourceExtension { extensions } => {
                if extensions.source != Some(Surface::OpenAi) {
                    return Err(Error::CrossProtocolExtensions);
                }
                extensions
                    .values
                    .iter()
                    .filter(|(path, _)| path.starts_with("/stream/"))
                    .map(|(path, value)| {
                        let kind = value
                            .get("type")
                            .and_then(Value::as_str)
                            .ok_or_else(|| Error::InvalidExtension(path.clone()))?;
                        transcription_sse_frame(kind, value.clone())
                    })
                    .collect::<Result<Vec<_>, _>>()?
            }
            Kind::Error { error } => vec![transcription_sse_frame(
                "error",
                serde_json::json!({
                    "error": {"code": error.provider_code, "message": error.message}
                }),
            )?],
            Kind::Done => {
                self.done = true;
                let usage = self.usage.map(|usage| {
                    serde_json::json!({
                        "input_tokens": usage.input_tokens,
                        "output_tokens": usage.output_tokens,
                        "total_tokens": usage.total_tokens,
                    })
                });
                vec![transcription_sse_frame(
                    "transcript.text.done",
                    serde_json::json!({"usage": usage}),
                )?]
            }
            Kind::RefusalDelta { .. } | Kind::ToolCallDelta { .. } => {
                return Err(Error::UnrepresentableStreamEvent);
            }
        };
        Ok(frames)
    }
}

fn transcription_sse_frame(kind: &str, mut payload: Value) -> Result<Frame, Error> {
    let Value::Object(object) = &mut payload else {
        return Err(Error::InvalidStreamPayload);
    };
    object.insert("type".into(), Value::String(kind.into()));
    Ok(Frame {
        event: Some(kind.into()),
        data: serde_json::to_string(&payload)?,
        id: None,
        retry_ms: None,
    })
}

fn validate_audio_part(part: &BoundedMediaPart) -> Result<(), Error> {
    if part.content_length > part.maximum_length || part.maximum_length > DEFAULT_AUDIO_UPLOAD_LIMIT
    {
        return Err(Error::InvalidMediaPart);
    }
    Ok(())
}

fn capture_string(extensions: &mut BTreeMap<String, Value>, path: &str, value: Option<String>) {
    if let Some(value) = value {
        extensions.insert(path.into(), Value::String(value));
    }
}

#[derive(Debug, Error)]
pub enum Error {
    #[error(transparent)]
    InvalidRoute(#[from] RouteSlugError),
    #[error(transparent)]
    Extensions(#[from] crate::domain::canonical::requests::ExtensionError),
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
    #[error("invalid source extension path: {0}")]
    InvalidExtension(String),
    #[error(transparent)]
    Sse(#[from] DecodeError),
    #[error("binary speech extensions require an HTTP header representation")]
    BinaryExtensionsUnsupported,
    #[error("speech stream event is missing its audio delta")]
    MissingAudioDelta,
    #[error("unsupported audio stream event: {0}")]
    UnsupportedStreamEvent(String),
    #[error("audio chunk staging failed: {0}")]
    Staging(String),
    #[error("transcription stream event is missing type")]
    MissingStreamEventType,
    #[error("transcription stream event is missing delta")]
    MissingTranscriptDelta,
    #[error("transcription stream contained data after done")]
    DataAfterDone,
    #[error("transcription stream ended before done")]
    UnexpectedStreamEof,
    #[error("expected canonical event sequence {expected}, got {actual}")]
    OutOfOrder { expected: u64, actual: u64 },
    #[error("canonical source extensions came from a different protocol")]
    CrossProtocolExtensions,
    #[error("canonical event cannot be represented by transcription streaming")]
    UnrepresentableStreamEvent,
    #[error("transcription stream payload must be an object")]
    InvalidStreamPayload,
}

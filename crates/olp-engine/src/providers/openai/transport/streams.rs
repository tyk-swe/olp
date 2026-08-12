use std::{
    collections::VecDeque,
    pin::Pin,
    task::{Context, Poll},
    time::Duration,
};

use crate::domain::{
    AttemptFailureClass, CanonicalEvent, CanonicalEventKind, ProviderEventStream, Surface,
    TransportError, TransportPhase,
};
use crate::protocols::{
    openai::{
        ChatCompletionResponse, OpenAiChatStreamDecoder, OpenAiResponsesStreamDecoder,
        ResponseObject, decode_chat_completion_response, decode_response_object,
    },
    sse::{SseDecoder, SseFrame},
};
use futures::{Stream, stream};
use reqwest::Response;
use tokio::time::Instant;

use super::{
    OpenAiConnector,
    errors::*,
    wire::{
        BomChunk, StreamingBodyKind, StreamingBom, WireProfile, require_json_response,
        streaming_body_kind, strip_json_bom,
    },
};
use crate::providers::transport_io::{
    CanonicalEventDecoder, DeadlineByteStream, DecodedEventStream, ProviderResponseIo,
};

pub(super) const RESPONSE_IO: ProviderResponseIo = ProviderResponseIo::new("OpenAI");

pub(super) struct DeadlineResponse {
    pub(super) response: Response,
    pub(super) first_body_deadline: Instant,
    pub(super) attempt_deadline: Instant,
}

impl std::ops::Deref for DeadlineResponse {
    type Target = Response;

    fn deref(&self) -> &Self::Target {
        &self.response
    }
}

impl DeadlineResponse {
    pub(super) fn new(
        response: Response,
        first_byte_timeout: Duration,
        attempt_deadline: Instant,
    ) -> Self {
        Self {
            response,
            first_body_deadline: Instant::now() + first_byte_timeout,
            attempt_deadline,
        }
    }
}

pub(in crate::providers::openai::transport) fn require_content_type(
    response: &Response,
    expected: &'static str,
) -> Result<(), TransportError> {
    RESPONSE_IO.require_content_type(response, expected)
}

impl OpenAiConnector {
    pub(super) fn raw_sse_response(&self, response: DeadlineResponse) -> ProviderEventStream {
        let bytes = RESPONSE_IO.response_stream(
            response.response,
            response.first_body_deadline,
            self.config.timeouts.idle,
            response.attempt_deadline,
        );
        Box::pin(RawSseEventStream::new(bytes, self.config.max_event_bytes))
    }

    pub(super) async fn unary_response(
        &self,
        response: Response,
        first_byte_deadline: Instant,
        attempt_deadline: Instant,
        responses_endpoint: bool,
        wire_profile: WireProfile,
    ) -> Result<ProviderEventStream, TransportError> {
        require_json_response(&response, wire_profile)?;
        let body = RESPONSE_IO
            .read_bounded_body(
                response,
                first_byte_deadline,
                attempt_deadline,
                self.config.timeouts.idle,
                self.config.max_response_bytes,
            )
            .await?;
        let body = strip_json_bom(&body, wire_profile);
        let events = if responses_endpoint {
            let response: ResponseObject = parse_wire("Responses", body)?;
            decode_response_object(response)
                .map_err(|error| protocol_decode_error("Responses", error))?
        } else if wire_profile.is_compatible() {
            crate::protocols::openai::decode_compatible_chat_completion_response(body)
                .map_err(|error| protocol_decode_error("chat", error))?
        } else {
            let response: ChatCompletionResponse = parse_wire("chat", body)?;
            decode_chat_completion_response(response)
                .map_err(|error| protocol_decode_error("chat", error))?
        };
        Ok(Box::pin(stream::iter(events.into_iter().map(Ok))))
    }

    pub(super) async fn streaming_response(
        &self,
        response: Response,
        first_byte_deadline: Instant,
        attempt_deadline: Instant,
        responses_endpoint: bool,
        wire_profile: WireProfile,
    ) -> Result<ProviderEventStream, TransportError> {
        let kind = streaming_body_kind(&response, wire_profile, !responses_endpoint)?;
        if matches!(kind, StreamingBodyKind::UnaryJson) {
            // Keep fallback sniffing bounded by the existing unary response cap.
            // A generic response is accepted only if it decodes as the expected
            // JSON protocol; otherwise the request fails as a protocol error.
            return self
                .unary_response(
                    response,
                    first_byte_deadline,
                    attempt_deadline,
                    responses_endpoint,
                    wire_profile,
                )
                .await;
        }
        let bytes = RESPONSE_IO.response_stream(
            response,
            first_byte_deadline,
            self.config.timeouts.idle,
            attempt_deadline,
        );
        let decoder = self.event_decoder(responses_endpoint, wire_profile, kind);
        Ok(Box::pin(DecodedEventStream::new(
            RESPONSE_IO,
            bytes,
            decoder,
        )))
    }

    fn event_decoder(
        &self,
        responses_endpoint: bool,
        wire_profile: WireProfile,
        body_kind: StreamingBodyKind,
    ) -> OpenAiEventDecoder {
        let inner = if responses_endpoint {
            OpenAiEventDecoderInner::Responses(OpenAiResponsesStreamDecoder::with_max_event_bytes(
                self.config.max_event_bytes,
            ))
        } else if matches!(body_kind, StreamingBodyKind::Sniff) {
            OpenAiEventDecoderInner::CompatibleChat(CompatibleChatBodyDecoder::new(
                self.config.max_response_bytes,
                self.config.max_event_bytes,
            ))
        } else if wire_profile.is_compatible() {
            OpenAiEventDecoderInner::Chat(OpenAiChatStreamDecoder::with_compatible_profile(
                self.config.max_event_bytes,
            ))
        } else {
            OpenAiEventDecoderInner::Chat(OpenAiChatStreamDecoder::with_max_event_bytes(
                self.config.max_event_bytes,
            ))
        };
        OpenAiEventDecoder {
            inner,
            bom: StreamingBom::new(wire_profile),
        }
    }
}

pub(super) async fn read_bounded_body(
    response: Response,
    first_byte_deadline: Instant,
    attempt_deadline: Instant,
    idle_timeout: Duration,
    maximum: usize,
) -> Result<Vec<u8>, TransportError> {
    RESPONSE_IO
        .read_bounded_body(
            response,
            first_byte_deadline,
            attempt_deadline,
            idle_timeout,
            maximum,
        )
        .await
}

pub(super) async fn read_deadline_body(
    response: DeadlineResponse,
    idle_timeout: Duration,
    maximum: usize,
) -> Result<Vec<u8>, TransportError> {
    RESPONSE_IO
        .read_bounded_body(
            response.response,
            response.first_body_deadline,
            response.attempt_deadline,
            idle_timeout,
            maximum,
        )
        .await
}

pub(super) struct OpenAiEventDecoder {
    inner: OpenAiEventDecoderInner,
    bom: StreamingBom,
}

enum OpenAiEventDecoderInner {
    Chat(OpenAiChatStreamDecoder),
    CompatibleChat(CompatibleChatBodyDecoder),
    Responses(OpenAiResponsesStreamDecoder),
}

impl CanonicalEventDecoder for OpenAiEventDecoder {
    type Error = String;

    fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, String> {
        match self.bom.push(bytes) {
            BomChunk::Pending => Ok(Vec::new()),
            BomChunk::Rejected => Err("OpenAI stream must not begin with a UTF-8 BOM".to_owned()),
            BomChunk::Borrowed(bytes) => self.inner.push(bytes),
            BomChunk::Buffered(bytes) => self.inner.push(&bytes),
        }
    }

    fn finish(&mut self) -> Result<Vec<CanonicalEvent>, String> {
        let mut events = self
            .bom
            .finish()
            .map_or_else(|| Ok(Vec::new()), |prefix| self.inner.push(&prefix))?;
        events.extend(self.inner.finish()?);
        Ok(events)
    }
}

impl OpenAiEventDecoderInner {
    fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, String> {
        match self {
            Self::Chat(decoder) => decoder.push(bytes).map_err(|error| error.to_string()),
            Self::CompatibleChat(decoder) => decoder.push(bytes),
            Self::Responses(decoder) => decoder.push(bytes).map_err(|error| error.to_string()),
        }
    }

    fn finish(&mut self) -> Result<Vec<CanonicalEvent>, String> {
        match self {
            Self::Chat(decoder) => decoder.finish().map_err(|error| error.to_string()),
            Self::CompatibleChat(decoder) => decoder.finish(),
            Self::Responses(decoder) => decoder.finish().map_err(|error| error.to_string()),
        }
    }
}

struct CompatibleChatBodyDecoder {
    mode: CompatibleChatBodyMode,
    maximum_response_bytes: usize,
    maximum_event_bytes: usize,
}

enum CompatibleChatBodyMode {
    Undecided(Vec<u8>),
    EventStream(Box<OpenAiChatStreamDecoder>),
    UnaryJson(Vec<u8>),
}

impl CompatibleChatBodyDecoder {
    fn new(maximum_response_bytes: usize, maximum_event_bytes: usize) -> Self {
        Self {
            mode: CompatibleChatBodyMode::Undecided(Vec::new()),
            maximum_response_bytes,
            maximum_event_bytes,
        }
    }

    fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, String> {
        match &mut self.mode {
            CompatibleChatBodyMode::EventStream(decoder) => {
                decoder.push(bytes).map_err(|error| error.to_string())
            }
            CompatibleChatBodyMode::UnaryJson(body) => {
                extend_bounded(body, bytes, self.maximum_response_bytes)?;
                Ok(Vec::new())
            }
            CompatibleChatBodyMode::Undecided(prefix) => {
                extend_bounded(prefix, bytes, self.maximum_response_bytes)?;
                let Some(first) = prefix
                    .iter()
                    .copied()
                    .find(|byte| !matches!(byte, b' ' | b'\t' | b'\r' | b'\n'))
                else {
                    return Ok(Vec::new());
                };
                let prefix = std::mem::take(prefix);
                if first == b'{' {
                    self.mode = CompatibleChatBodyMode::UnaryJson(prefix);
                    Ok(Vec::new())
                } else {
                    let mut decoder =
                        OpenAiChatStreamDecoder::with_compatible_profile(self.maximum_event_bytes);
                    let events = decoder.push(&prefix).map_err(|error| error.to_string())?;
                    self.mode = CompatibleChatBodyMode::EventStream(Box::new(decoder));
                    Ok(events)
                }
            }
        }
    }

    fn finish(&mut self) -> Result<Vec<CanonicalEvent>, String> {
        match std::mem::replace(
            &mut self.mode,
            CompatibleChatBodyMode::Undecided(Vec::new()),
        ) {
            CompatibleChatBodyMode::EventStream(mut decoder) => {
                decoder.finish().map_err(|error| error.to_string())
            }
            CompatibleChatBodyMode::UnaryJson(body) => {
                crate::protocols::openai::decode_compatible_chat_completion_response(&body)
                    .map_err(|error| error.to_string())
            }
            CompatibleChatBodyMode::Undecided(prefix) => {
                let mut decoder =
                    OpenAiChatStreamDecoder::with_compatible_profile(self.maximum_event_bytes);
                decoder.push(&prefix).map_err(|error| error.to_string())?;
                decoder.finish().map_err(|error| error.to_string())
            }
        }
    }
}

fn extend_bounded(body: &mut Vec<u8>, bytes: &[u8], maximum: usize) -> Result<(), String> {
    if bytes.len() > maximum.saturating_sub(body.len()) {
        return Err("OpenAI compatible response exceeded the configured maximum".to_owned());
    }
    body.extend_from_slice(bytes);
    Ok(())
}

pub(super) struct RawSseEventStream {
    bytes: DeadlineByteStream,
    decoder: SseDecoder,
    queued: VecDeque<CanonicalEvent>,
    sequence: u64,
    committed: bool,
    terminal: bool,
    usage: RawUsage,
}

impl RawSseEventStream {
    pub(super) fn new(bytes: DeadlineByteStream, maximum_event_bytes: usize) -> Self {
        Self {
            bytes,
            decoder: SseDecoder::new(maximum_event_bytes),
            queued: VecDeque::new(),
            sequence: 0,
            committed: false,
            terminal: false,
            usage: RawUsage::default(),
        }
    }

    fn queue_frames(&mut self, frames: Vec<SseFrame>) -> Result<(), TransportError> {
        for frame in frames {
            if self.terminal {
                return Err(self.protocol_error("OpenAI sent media events after completion"));
            }
            if frame.data.trim() == "[DONE]" {
                self.push(CanonicalEventKind::Done);
                self.terminal = true;
                continue;
            }
            let value: serde_json::Value = serde_json::from_str(&frame.data).map_err(|error| {
                self.protocol_error(format!("OpenAI media event is invalid JSON: {error}"))
            })?;
            self.usage.observe(&value).map_err(|()| {
                self.protocol_error("OpenAI media event contains invalid usage metadata")
            })?;
            let kind = value
                .get("type")
                .and_then(serde_json::Value::as_str)
                .or(frame.event.as_deref())
                .unwrap_or("message")
                .to_owned();
            let extensions = crate::domain::SourceExtensions::new(
                Surface::OpenAi,
                std::collections::BTreeMap::from([
                    ("/__olp/raw_sse/data".into(), value),
                    (
                        "/__olp/raw_sse/event".into(),
                        serde_json::Value::String(kind.clone()),
                    ),
                ]),
            );
            self.push(CanonicalEventKind::SourceExtension { extensions });
            if is_raw_media_terminal(&kind) {
                self.push(CanonicalEventKind::Done);
                self.terminal = true;
            }
        }
        Ok(())
    }

    fn push(&mut self, kind: crate::domain::CanonicalEventKind) {
        self.queued
            .push_back(CanonicalEvent::new(self.sequence, kind));
        self.sequence = self.sequence.saturating_add(1);
    }

    fn protocol_error(&self, message: impl Into<String>) -> TransportError {
        transport_error(
            TransportPhase::Body,
            AttemptFailureClass::Protocol,
            self.committed,
            message,
        )
    }
}

#[derive(Default)]
struct RawUsage {
    input: Option<u64>,
    output: Option<u64>,
    total: Option<u64>,
    cached: Option<u64>,
}

impl RawUsage {
    fn observe(&mut self, value: &serde_json::Value) -> Result<(), ()> {
        let Some(usage) = value.get("usage") else {
            return Ok(());
        };
        if usage.is_null() {
            return Ok(());
        }
        let usage = usage.as_object().ok_or(())?;
        let input = aliased_counter(usage, "input_tokens", "prompt_tokens")?;
        let output = aliased_counter(usage, "output_tokens", "completion_tokens")?;
        let total = optional_counter(usage.get("total_tokens"))?;
        let cached =
            aliased_cached_counter(usage, "input_tokens_details", "prompt_tokens_details")?;

        merge_counter(&mut self.input, input)?;
        merge_counter(&mut self.output, output)?;
        merge_counter(&mut self.total, total)?;
        merge_counter(&mut self.cached, cached)?;

        if let (Some(cached), Some(input)) = (self.cached, self.input)
            && cached > input
        {
            return Err(());
        }

        if let (Some(input), Some(output), Some(total)) = (self.input, self.output, self.total)
            && input.checked_add(output) != Some(total)
        {
            return Err(());
        }
        Ok(())
    }
}

fn merge_counter(target: &mut Option<u64>, observed: Option<u64>) -> Result<(), ()> {
    match (*target, observed) {
        (Some(existing), Some(value)) if existing != value => Err(()),
        (None, Some(value)) => {
            *target = Some(value);
            Ok(())
        }
        _ => Ok(()),
    }
}

fn aliased_counter(
    usage: &serde_json::Map<String, serde_json::Value>,
    primary: &str,
    alias: &str,
) -> Result<Option<u64>, ()> {
    let primary_value = optional_counter(usage.get(primary))?;
    let alias_value = optional_counter(usage.get(alias))?;
    if let (Some(primary_value), Some(alias_value)) = (primary_value, alias_value)
        && primary_value != alias_value
    {
        return Err(());
    }
    Ok(primary_value.or(alias_value))
}

fn aliased_cached_counter(
    usage: &serde_json::Map<String, serde_json::Value>,
    primary: &str,
    alias: &str,
) -> Result<Option<u64>, ()> {
    let primary_value = cached_counter(usage.get(primary))?;
    let alias_value = cached_counter(usage.get(alias))?;
    if let (Some(primary_value), Some(alias_value)) = (primary_value, alias_value)
        && primary_value != alias_value
    {
        return Err(());
    }
    Ok(primary_value.or(alias_value))
}

fn cached_counter(details: Option<&serde_json::Value>) -> Result<Option<u64>, ()> {
    let Some(details) = details else {
        return Ok(None);
    };
    if details.is_null() {
        return Ok(None);
    }
    optional_counter(details.as_object().ok_or(())?.get("cached_tokens"))
}

fn optional_counter(value: Option<&serde_json::Value>) -> Result<Option<u64>, ()> {
    let Some(value) = value else {
        return Ok(None);
    };
    if value.is_null() {
        return Ok(None);
    }
    value
        .as_u64()
        .filter(|value| *value <= i64::MAX as u64)
        .map(Some)
        .ok_or(())
}

impl Stream for RawSseEventStream {
    type Item = Result<CanonicalEvent, TransportError>;

    fn poll_next(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<Option<Self::Item>> {
        loop {
            if let Some(event) = self.queued.pop_front() {
                self.committed = true;
                return Poll::Ready(Some(Ok(event)));
            }
            if self.terminal {
                return Poll::Ready(None);
            }
            match Pin::new(&mut self.bytes).poll_next(context) {
                Poll::Ready(Some(Ok(chunk))) => {
                    let frames = match self.decoder.push(&chunk) {
                        Ok(frames) => frames,
                        Err(error) => {
                            self.terminal = true;
                            return Poll::Ready(Some(Err(self.protocol_error(format!(
                                "invalid OpenAI media event stream: {error}"
                            )))));
                        }
                    };
                    if let Err(error) = self.queue_frames(frames) {
                        self.terminal = true;
                        return Poll::Ready(Some(Err(error)));
                    }
                }
                Poll::Ready(Some(Err(mut error))) => {
                    self.terminal = true;
                    error.response_committed = self.committed;
                    return Poll::Ready(Some(Err(error)));
                }
                Poll::Ready(None) => {
                    let frames = match self.decoder.finish() {
                        Ok(frames) => frames,
                        Err(error) => {
                            self.terminal = true;
                            return Poll::Ready(Some(Err(self.protocol_error(format!(
                                "truncated OpenAI media event stream: {error}"
                            )))));
                        }
                    };
                    if let Err(error) = self.queue_frames(frames) {
                        self.terminal = true;
                        return Poll::Ready(Some(Err(error)));
                    }
                    if !self.terminal {
                        self.terminal = true;
                        return Poll::Ready(Some(Err(self.protocol_error(
                            "OpenAI media event stream ended without completion",
                        ))));
                    }
                }
                Poll::Pending => return Poll::Pending,
            }
        }
    }
}

fn is_raw_media_terminal(kind: &str) -> bool {
    matches!(
        kind,
        "image_generation.completed"
            | "image_edit.completed"
            | "speech.audio.done"
            | "transcript.text.done"
            | "transcription.done"
            | "transcription.completed"
    ) || kind.ends_with(".failed")
}

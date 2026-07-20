use std::{
    collections::VecDeque,
    pin::Pin,
    task::{Context, Poll},
    time::Duration,
};

use bytes::Bytes;
use futures::{Stream, StreamExt, stream};
use olp_domain::{
    AttemptFailureClass, CanonicalEvent, CanonicalEventKind, ProviderEventStream, Surface,
    TransportError, TransportPhase,
};
use olp_protocols::{
    openai::{
        ChatCompletionResponse, OpenAiChatStreamDecoder, OpenAiResponsesStreamDecoder,
        ResponseObject, decode_chat_completion_response, decode_response_object,
    },
    sse::{SseDecoder, SseFrame},
};
use reqwest::{Response, header};
use tokio::time::{Instant, Sleep, timeout};

use super::{OpenAiConnector, errors::*};

pub(super) type ReqwestByteStream =
    Pin<Box<dyn Stream<Item = Result<Bytes, reqwest::Error>> + Send + 'static>>;

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

pub(in crate::openai::transport) fn require_content_type(
    response: &Response,
    expected: &'static str,
) -> Result<(), TransportError> {
    let valid = response
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.split(';').next())
        .is_some_and(|value| value.trim().eq_ignore_ascii_case(expected));
    if valid {
        return Ok(());
    }
    Err(transport_error(
        TransportPhase::FirstByte,
        AttemptFailureClass::Protocol,
        false,
        format!("OpenAI response must use content type {expected}"),
    ))
}

impl OpenAiConnector {
    pub(super) fn raw_sse_response(
        &self,
        response: DeadlineResponse,
    ) -> Result<ProviderEventStream, TransportError> {
        let source: ReqwestByteStream = Box::pin(response.response.bytes_stream());
        let bytes = DeadlineByteStream::new(
            source,
            response.first_body_deadline,
            self.config.timeouts.idle,
            response.attempt_deadline,
        );
        Ok(Box::pin(RawSseEventStream::new(
            bytes,
            self.config.max_event_bytes,
        )))
    }

    pub(super) async fn unary_response(
        &self,
        response: Response,
        first_byte_deadline: Instant,
        attempt_deadline: Instant,
        responses_endpoint: bool,
    ) -> Result<ProviderEventStream, TransportError> {
        require_content_type(&response, "application/json")?;
        let body = read_bounded_body(
            response,
            first_byte_deadline,
            attempt_deadline,
            self.config.timeouts.idle,
            self.config.max_response_bytes,
        )
        .await?;
        let events = if responses_endpoint {
            let response: ResponseObject = parse_wire("Responses", &body)?;
            decode_response_object(response)
                .map_err(|error| protocol_decode_error("Responses", error))?
        } else {
            let response: ChatCompletionResponse = parse_wire("chat", &body)?;
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
    ) -> Result<ProviderEventStream, TransportError> {
        require_content_type(&response, "text/event-stream")?;
        let source: ReqwestByteStream = Box::pin(response.bytes_stream());
        let bytes = DeadlineByteStream::new(
            source,
            first_byte_deadline,
            self.config.timeouts.idle,
            attempt_deadline,
        );
        let decoder = if responses_endpoint {
            OpenAiEventDecoder::Responses(OpenAiResponsesStreamDecoder::with_max_event_bytes(
                self.config.max_event_bytes,
            ))
        } else {
            OpenAiEventDecoder::Chat(OpenAiChatStreamDecoder::with_max_event_bytes(
                self.config.max_event_bytes,
            ))
        };
        Ok(Box::pin(DecodedEventStream::new(bytes, decoder)))
    }
}

pub(super) async fn read_bounded_body(
    response: Response,
    first_byte_deadline: Instant,
    attempt_deadline: Instant,
    idle_timeout: Duration,
    maximum: usize,
) -> Result<Vec<u8>, TransportError> {
    let mut source = response.bytes_stream();
    let mut output = Vec::new();
    let mut first = true;
    loop {
        let wait = if first {
            remaining_until(first_byte_deadline, attempt_deadline).ok_or_else(first_byte_timeout)?
        } else {
            bounded_duration(
                idle_timeout,
                remaining(attempt_deadline, TransportPhase::Body)?,
            )
        };
        let next = timeout(wait, source.next()).await.map_err(|_| {
            if first {
                first_byte_timeout()
            } else {
                body_idle_timeout()
            }
        })?;
        let Some(chunk) = next else {
            break;
        };
        let chunk = chunk.map_err(|error| {
            if first {
                map_first_body_error(error)
            } else {
                map_body_error(error, false)
            }
        })?;
        first = false;
        if output.len().saturating_add(chunk.len()) > maximum {
            return Err(transport_error(
                TransportPhase::Body,
                AttemptFailureClass::Protocol,
                false,
                format!("OpenAI response exceeded the {maximum} byte limit"),
            ));
        }
        output.extend_from_slice(&chunk);
    }
    if first {
        return Err(transport_error(
            TransportPhase::FirstByte,
            AttemptFailureClass::Protocol,
            false,
            "OpenAI response body was empty",
        ));
    }
    Ok(output)
}

pub(super) async fn read_deadline_body(
    response: DeadlineResponse,
    idle_timeout: Duration,
    maximum: usize,
) -> Result<Vec<u8>, TransportError> {
    read_bounded_body(
        response.response,
        response.first_body_deadline,
        response.attempt_deadline,
        idle_timeout,
        maximum,
    )
    .await
}

pub(super) struct DeadlineByteStream {
    source: ReqwestByteStream,
    first: bool,
    idle_timeout: Duration,
    idle_sleep: Pin<Box<Sleep>>,
    attempt_deadline: Instant,
    terminal: bool,
}

impl DeadlineByteStream {
    pub(super) fn new(
        source: ReqwestByteStream,
        first_body_deadline: Instant,
        idle_timeout: Duration,
        attempt_deadline: Instant,
    ) -> Self {
        let wake_at = bounded_instant(first_body_deadline, attempt_deadline);
        Self {
            source,
            first: true,
            idle_timeout,
            idle_sleep: Box::pin(tokio::time::sleep_until(wake_at)),
            attempt_deadline,
            terminal: false,
        }
    }
}

impl Stream for DeadlineByteStream {
    type Item = Result<Bytes, TransportError>;

    fn poll_next(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<Option<Self::Item>> {
        if self.terminal {
            return Poll::Ready(None);
        }
        if Instant::now() >= self.attempt_deadline {
            self.terminal = true;
            let error = if self.first {
                first_byte_timeout()
            } else {
                attempt_body_timeout()
            };
            return Poll::Ready(Some(Err(error)));
        }

        match self.source.as_mut().poll_next(context) {
            Poll::Ready(Some(Ok(chunk))) => {
                self.first = false;
                let wake_at =
                    bounded_instant(Instant::now() + self.idle_timeout, self.attempt_deadline);
                self.idle_sleep.as_mut().reset(wake_at);
                return Poll::Ready(Some(Ok(chunk)));
            }
            Poll::Ready(Some(Err(error))) => {
                self.terminal = true;
                let error = if self.first {
                    map_first_body_error(error)
                } else {
                    map_body_error(error, false)
                };
                return Poll::Ready(Some(Err(error)));
            }
            Poll::Ready(None) => {
                self.terminal = true;
                if self.first {
                    return Poll::Ready(Some(Err(transport_error(
                        TransportPhase::FirstByte,
                        AttemptFailureClass::Protocol,
                        false,
                        "OpenAI response body was empty",
                    ))));
                }
                return Poll::Ready(None);
            }
            Poll::Pending => {}
        }

        if self.idle_sleep.as_mut().poll(context).is_ready() {
            self.terminal = true;
            let error = if Instant::now() >= self.attempt_deadline {
                if self.first {
                    first_byte_timeout()
                } else {
                    attempt_body_timeout()
                }
            } else if self.first {
                first_byte_timeout()
            } else {
                body_idle_timeout()
            };
            return Poll::Ready(Some(Err(error)));
        }
        Poll::Pending
    }
}

pub(super) struct DecodedEventStream {
    bytes: DeadlineByteStream,
    decoder: OpenAiEventDecoder,
    queued: VecDeque<CanonicalEvent>,
    committed: bool,
    terminal: bool,
}

impl DecodedEventStream {
    pub(super) fn new(bytes: DeadlineByteStream, decoder: OpenAiEventDecoder) -> Self {
        Self {
            bytes,
            decoder,
            queued: VecDeque::new(),
            committed: false,
            terminal: false,
        }
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

pub(super) enum OpenAiEventDecoder {
    Chat(OpenAiChatStreamDecoder),
    Responses(OpenAiResponsesStreamDecoder),
}

impl OpenAiEventDecoder {
    fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, String> {
        match self {
            Self::Chat(decoder) => decoder.push(bytes).map_err(|error| error.to_string()),
            Self::Responses(decoder) => decoder.push(bytes).map_err(|error| error.to_string()),
        }
    }

    fn finish(&mut self) -> Result<Vec<CanonicalEvent>, String> {
        match self {
            Self::Chat(decoder) => decoder.finish().map_err(|error| error.to_string()),
            Self::Responses(decoder) => decoder.finish().map_err(|error| error.to_string()),
        }
    }
}

impl Stream for DecodedEventStream {
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
                Poll::Ready(Some(Ok(chunk))) => match self.decoder.push(&chunk) {
                    Ok(events) => self.queued.extend(events),
                    Err(error) => {
                        self.terminal = true;
                        return Poll::Ready(Some(Err(
                            self.protocol_error(format!("invalid OpenAI event stream: {error}"))
                        )));
                    }
                },
                Poll::Ready(Some(Err(mut error))) => {
                    self.terminal = true;
                    error.response_committed = self.committed;
                    return Poll::Ready(Some(Err(error)));
                }
                Poll::Ready(None) => {
                    self.terminal = true;
                    match self.decoder.finish() {
                        Ok(events) => self.queued.extend(events),
                        Err(error) => {
                            return Poll::Ready(Some(Err(self.protocol_error(format!(
                                "truncated OpenAI event stream: {error}"
                            )))));
                        }
                    }
                }
                Poll::Pending => return Poll::Pending,
            }
        }
    }
}

pub(super) struct RawSseEventStream {
    bytes: DeadlineByteStream,
    decoder: SseDecoder,
    queued: VecDeque<CanonicalEvent>,
    sequence: u64,
    committed: bool,
    terminal: bool,
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
            let kind = value
                .get("type")
                .and_then(serde_json::Value::as_str)
                .or(frame.event.as_deref())
                .unwrap_or("message")
                .to_owned();
            let extensions = olp_domain::SourceExtensions::new(
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

    fn push(&mut self, kind: olp_domain::CanonicalEventKind) {
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

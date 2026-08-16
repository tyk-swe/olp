use std::{
    collections::VecDeque,
    pin::Pin,
    task::{Context, Poll},
    time::Duration,
};

use crate::domain::{
    canonical::{
        events::{Event, Kind},
        identity::Surface,
    },
    ports::{AttemptFailureClass, ProviderEventStream, TransportError, TransportPhase},
};
use crate::protocols::{
    openai::{
        response::{Completion, Decoder as ChatDecoder, decode},
        responses::{
            response::{Object, decode_response_object},
            stream::Decoder as ResponsesDecoder,
        },
    },
    sse::{Decoder, Frame},
};
use futures::{Stream, stream};
use reqwest::Response;
use tokio::time::Instant;

use super::{Connector, errors::*};
use crate::providers::transport_common::transport_error;
use crate::providers::transport_io::{
    ProviderResponseIo,
    event_stream::{CanonicalEventDecoder, DeadlineByteStream, DecodedEventStream},
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

impl Connector {
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
    ) -> Result<ProviderEventStream, TransportError> {
        require_content_type(&response, "application/json")?;
        let body = RESPONSE_IO
            .read_bounded_body(
                response,
                first_byte_deadline,
                attempt_deadline,
                self.config.timeouts.idle,
                self.config.max_response_bytes,
            )
            .await?;
        let events = if responses_endpoint {
            let response: Object = parse_wire("Responses", &body)?;
            decode_response_object(response)
                .map_err(|error| protocol_decode_error("Responses", error))?
        } else {
            let response: Completion = parse_wire("chat", &body)?;
            decode(response).map_err(|error| protocol_decode_error("chat", error))?
        };
        Ok(Box::pin(stream::iter(events.into_iter().map(Ok))))
    }

    pub(super) fn streaming_response(
        &self,
        response: Response,
        first_byte_deadline: Instant,
        attempt_deadline: Instant,
        responses_endpoint: bool,
    ) -> Result<ProviderEventStream, TransportError> {
        require_content_type(&response, "text/event-stream")?;
        let bytes = RESPONSE_IO.response_stream(
            response,
            first_byte_deadline,
            self.config.timeouts.idle,
            attempt_deadline,
        );
        let decoder = if responses_endpoint {
            OpenAiEventDecoder::Responses(ResponsesDecoder::with_max_event_bytes(
                self.config.max_event_bytes,
            ))
        } else {
            OpenAiEventDecoder::Chat(ChatDecoder::with_max_event_bytes(
                self.config.max_event_bytes,
            ))
        };
        Ok(Box::pin(DecodedEventStream::new(
            RESPONSE_IO,
            bytes,
            decoder,
        )))
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

pub(super) enum OpenAiEventDecoder {
    Chat(ChatDecoder),
    Responses(ResponsesDecoder),
}

impl CanonicalEventDecoder for OpenAiEventDecoder {
    type Error = String;

    fn push(&mut self, bytes: &[u8]) -> Result<Vec<Event>, String> {
        match self {
            Self::Chat(decoder) => decoder.push(bytes).map_err(|error| error.to_string()),
            Self::Responses(decoder) => decoder.push(bytes).map_err(|error| error.to_string()),
        }
    }

    fn finish(&mut self) -> Result<Vec<Event>, String> {
        match self {
            Self::Chat(decoder) => decoder.finish().map_err(|error| error.to_string()),
            Self::Responses(decoder) => decoder.finish().map_err(|error| error.to_string()),
        }
    }
}

pub(super) struct RawSseEventStream {
    bytes: DeadlineByteStream,
    decoder: Decoder,
    queued: VecDeque<Event>,
    sequence: u64,
    committed: bool,
    terminal: bool,
}

impl RawSseEventStream {
    pub(super) fn new(bytes: DeadlineByteStream, maximum_event_bytes: usize) -> Self {
        Self {
            bytes,
            decoder: Decoder::new(maximum_event_bytes),
            queued: VecDeque::new(),
            sequence: 0,
            committed: false,
            terminal: false,
        }
    }

    fn queue_frames(&mut self, frames: Vec<Frame>) -> Result<(), TransportError> {
        for frame in frames {
            if self.terminal {
                return Err(self.protocol_error("OpenAI sent media events after completion"));
            }
            if frame.data.trim() == "[DONE]" {
                self.push(Kind::Done);
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
            let extensions = crate::domain::canonical::requests::SourceExtensions::new(
                Surface::OpenAi,
                std::collections::BTreeMap::from([
                    ("/__olp/raw_sse/data".into(), value),
                    (
                        "/__olp/raw_sse/event".into(),
                        serde_json::Value::String(kind.clone()),
                    ),
                ]),
            );
            self.push(Kind::SourceExtension { extensions });
            if is_raw_media_terminal(&kind) {
                self.push(Kind::Done);
                self.terminal = true;
            }
        }
        Ok(())
    }

    fn push(&mut self, kind: crate::domain::canonical::events::Kind) {
        self.queued.push_back(Event::new(self.sequence, kind));
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
    type Item = Result<Event, TransportError>;

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

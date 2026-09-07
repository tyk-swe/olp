use std::collections::VecDeque;
use std::pin::Pin;
use std::task::Context;
use std::task::Poll;
use std::time::Duration;

use crate::inference::transport::AttemptFailureClass;
use crate::inference::transport::ProviderEventStream;
use crate::inference::transport::TransportError;
use crate::inference::transport::TransportPhase;
use crate::protocols::canonical::events::Event;
use crate::protocols::canonical::events::Kind;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::openai::response::Completion;
use crate::protocols::openai::response::Decoder as ChatDecoder;
use crate::protocols::openai::response::decode;
use crate::protocols::openai::responses::response::Object;
use crate::protocols::openai::responses::response::decode_response_object;
use crate::protocols::openai::responses::stream::Decoder as ResponsesDecoder;
use crate::protocols::sse::Decoder;
use crate::protocols::sse::Frame;
use futures::Stream;
use futures::stream;
use reqwest::Response;
use tokio::time::Instant;

use crate::providers::openai::transport::Connector;
use crate::providers::openai::transport::errors::*;
use crate::providers::transport_common::transport_error;
use crate::providers::transport_io::ProviderResponseIo;
use crate::providers::transport_io::event_stream::CanonicalEventDecoder;
use crate::providers::transport_io::event_stream::DeadlineByteStream;
use crate::providers::transport_io::event_stream::DecodedEventStream;

pub(crate) const RESPONSE_IO: ProviderResponseIo = ProviderResponseIo::new("OpenAI");

pub(crate) struct DeadlineResponse {
    pub(crate) response: Response,
    pub(crate) first_body_deadline: Instant,
    pub(crate) attempt_deadline: Instant,
}

impl std::ops::Deref for DeadlineResponse {
    type Target = Response;

    fn deref(&self) -> &Self::Target {
        &self.response
    }
}

impl DeadlineResponse {
    pub(crate) fn new(
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

pub(crate) fn require_content_type(
    response: &Response,
    expected: &'static str,
) -> Result<(), TransportError> {
    RESPONSE_IO.require_content_type(response, expected)
}

impl Connector {
    pub(crate) fn raw_sse_response(&self, response: DeadlineResponse) -> ProviderEventStream {
        let bytes = RESPONSE_IO.response_stream(
            response.response,
            response.first_body_deadline,
            self.config.timeouts.idle,
            response.attempt_deadline,
        );
        Box::pin(RawSseEventStream::new(bytes, self.config.max_event_bytes))
    }

    pub(crate) async fn unary_response(
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

    pub(crate) fn streaming_response(
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

pub(crate) async fn read_bounded_body(
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

pub(crate) async fn read_deadline_body(
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

pub(crate) enum OpenAiEventDecoder {
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

pub(crate) struct RawSseEventStream {
    bytes: DeadlineByteStream,
    decoder: Decoder,
    queued: VecDeque<Event>,
    sequence: u64,
    committed: bool,
    terminal: bool,
}

impl RawSseEventStream {
    pub(crate) fn new(bytes: DeadlineByteStream, maximum_event_bytes: usize) -> Self {
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
            let extensions = crate::protocols::canonical::requests::SourceExtensions::new(
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

    fn push(&mut self, kind: crate::protocols::canonical::events::Kind) {
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

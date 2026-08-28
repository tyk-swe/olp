use std::{collections::VecDeque, convert::Infallible, fmt::Display};

use axum::{
    body::{Body, Bytes},
    http::{HeaderValue, header},
    response::Response,
};
use futures::{StreamExt, stream};
use olp_engine::domain::canonical::events::{Event, Kind};
use olp_engine::protocols::sse::{EncodeError, Frame, encode_frame};

use crate::gateway::error::InferenceError;
use olp_engine::inference::{
    accounting::{RequestOutcome, UsageCapture},
    execution::{RoutedEvents, RoutedStream},
};

pub(crate) fn encode_sse_frame(frame: &Frame) -> Result<Bytes, EncodeError> {
    encode_frame(frame).map(Bytes::from)
}

pub(crate) fn encode_protocol_sse_frames<E: Display>(
    frames: Result<Vec<Frame>, E>,
) -> Result<Vec<Bytes>, InferenceError> {
    let protocol_error = |error: &dyn Display| {
        InferenceError::bad_gateway("provider_protocol_error", error.to_string())
    };
    frames
        .map_err(|error| protocol_error(&error))?
        .iter()
        .map(encode_sse_frame)
        .collect::<Result<_, _>>()
        .map_err(|error| protocol_error(&error))
}

pub(crate) fn encode_server_sse_frame(frame: &Frame) -> Bytes {
    encode_sse_frame(frame).expect("server-generated SSE event fields are valid")
}

const STREAM_BUFFER_CAPACITY: usize = 32;
const MAX_TERMINAL_FRAMES: usize = 2;

#[derive(Debug, Eq, PartialEq)]
pub(crate) enum StreamSendFailure {
    ClientClosed,
    DeadlineElapsed,
}

impl StreamSendFailure {
    /// Maps a send failure into the inference failure that ends the stream.
    pub(crate) fn into_inference_error(self) -> InferenceError {
        match self {
            StreamSendFailure::ClientClosed => InferenceError::client_cancelled(),
            StreamSendFailure::DeadlineElapsed => InferenceError::timeout(),
        }
    }
}

#[derive(Debug, Eq, PartialEq)]
pub(crate) enum StreamFinishOutcome {
    Queued,
    ClientClosed,
}

#[derive(Default)]
pub(crate) struct TerminalFrames {
    frames: Vec<Bytes>,
}

impl TerminalFrames {
    pub(crate) fn empty() -> Self {
        Self::default()
    }

    pub(crate) fn one(frame: Bytes) -> Self {
        Self::new(vec![frame])
    }

    pub(crate) fn new(frames: Vec<Bytes>) -> Self {
        assert!(
            frames.len() <= MAX_TERMINAL_FRAMES,
            "terminal SSE tails are limited to two frames"
        );
        Self { frames }
    }

    fn into_queue(self) -> VecDeque<Bytes> {
        self.frames.into()
    }
}

pub(crate) struct SseResponseWriter {
    ordinary: tokio::sync::mpsc::Sender<Result<Bytes, Infallible>>,
    terminal: Option<tokio::sync::oneshot::Sender<TerminalFrames>>,
}

impl SseResponseWriter {
    pub(crate) async fn send(
        &self,
        bytes: Bytes,
        deadline: tokio::time::Instant,
    ) -> Result<(), StreamSendFailure> {
        tokio::select! {
            biased;

            () = self.ordinary.closed() => Err(StreamSendFailure::ClientClosed),
            () = tokio::time::sleep_until(deadline) => Err(StreamSendFailure::DeadlineElapsed),
            result = self.ordinary.send(Ok(bytes)) => result
                .map_err(|_| StreamSendFailure::ClientClosed),
        }
    }

    /// Sends an ordinary frame, returning the inference failure that ended the
    /// stream when the client disconnected or the deadline elapsed.
    pub(crate) async fn send_or_fail(
        &self,
        bytes: Bytes,
        deadline: tokio::time::Instant,
    ) -> Result<(), InferenceError> {
        self.send(bytes, deadline)
            .await
            .map_err(StreamSendFailure::into_inference_error)
    }

    pub(crate) async fn closed(&self) {
        self.ordinary.closed().await;
    }

    pub(crate) fn finish(mut self, terminal: TerminalFrames) -> StreamFinishOutcome {
        let outcome = self
            .terminal
            .take()
            .expect("an SSE writer is finished exactly once")
            .send(terminal);
        // The body deliberately waits for ordinary channel closure before it
        // observes the ready terminal tail, so this preserves wire ordering.
        drop(self.ordinary);
        if outcome.is_ok() {
            StreamFinishOutcome::Queued
        } else {
            StreamFinishOutcome::ClientClosed
        }
    }

    /// Finalizes the stream, queuing terminal frames derived from `terminal`
    /// or, when no terminal event was observed, from `failure` via
    /// `encode_error`. Sets `failure` to `client_cancelled` if the client
    /// closed during finalization and no other failure was already recorded.
    pub(crate) fn finish_stream(
        self,
        terminal: Option<TerminalFrames>,
        failure: &mut Option<InferenceError>,
        encode_error: impl FnOnce(&InferenceError) -> TerminalFrames,
    ) {
        let terminal = terminal.unwrap_or_else(|| match failure.as_ref() {
            Some(error) if error.code() == "client_cancelled" => TerminalFrames::empty(),
            Some(error) => encode_error(error),
            None => TerminalFrames::empty(),
        });
        if matches!(self.finish(terminal), StreamFinishOutcome::ClientClosed) && failure.is_none() {
            *failure = Some(InferenceError::client_cancelled());
        }
    }
}

enum SseBodyState {
    Ordinary {
        receiver: tokio::sync::mpsc::Receiver<Result<Bytes, Infallible>>,
        terminal: tokio::sync::oneshot::Receiver<TerminalFrames>,
    },
    Terminal(VecDeque<Bytes>),
}

pub(crate) fn sse_stream() -> (SseResponseWriter, Response) {
    sse_stream_with_capacity(STREAM_BUFFER_CAPACITY)
}

fn sse_stream_with_capacity(capacity: usize) -> (SseResponseWriter, Response) {
    let (ordinary, receiver) = tokio::sync::mpsc::channel(capacity);
    let (terminal, terminal_receiver) = tokio::sync::oneshot::channel();
    let body_stream = stream::unfold(
        SseBodyState::Ordinary {
            receiver,
            terminal: terminal_receiver,
        },
        |state| async move {
            match state {
                SseBodyState::Ordinary {
                    mut receiver,
                    terminal,
                } => {
                    if let Some(item) = receiver.recv().await {
                        Some((item, SseBodyState::Ordinary { receiver, terminal }))
                    } else {
                        let mut frames = terminal.await.unwrap_or_default().into_queue();
                        frames
                            .pop_front()
                            .map(|frame| (Ok(frame), SseBodyState::Terminal(frames)))
                    }
                }
                SseBodyState::Terminal(mut frames) => frames
                    .pop_front()
                    .map(|frame| (Ok(frame), SseBodyState::Terminal(frames))),
            }
        },
    );
    let mut response = Response::new(Body::from_stream(body_stream));
    response.headers_mut().insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("text/event-stream; charset=utf-8"),
    );
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-cache"));
    (
        SseResponseWriter {
            ordinary,
            terminal: Some(terminal),
        },
        response,
    )
}

/// A provider failure the gateway already knows about before a single byte is
/// written must answer with a real HTTP status. Committing `200 OK
/// text/event-stream` and then describing a 429 inside the body defeats every
/// status-driven retry in an SDK, load balancer, or proxy — and the unary path
/// on the identical failure already returns the right status. Returns the
/// failure after closing out accounting for it.
pub(crate) fn precommit_stream_failure(
    execution: RoutedEvents,
) -> Result<RoutedEvents, InferenceError> {
    let Kind::Error { error } = &execution.first.kind else {
        return Ok(execution);
    };
    let failure = InferenceError::from_canonical(error);
    let (stream, mut accounting) = execution.into_parts();
    accounting.usage_mut().observe(&stream.first);
    accounting.finish(failure.accounting_outcome());
    Err(failure)
}

/// How one wire protocol turns canonical events into SSE bytes. The pump in
/// [`protocol_streaming_response`] owns everything protocol-neutral: usage
/// observation, deadlines, client disconnects, terminal detection, and
/// accounting; an encoder only supplies the bytes.
pub(crate) trait ProtocolStreamEncoder: Send + 'static {
    fn push(&mut self, event: Event) -> Result<Vec<Bytes>, InferenceError>;

    fn encode_error(&self, error: &InferenceError) -> Bytes;

    /// Frames appended after the encoded terminal event. `failure` is the
    /// canonical error the stream ended on, if any.
    fn terminal_tail(&self, failure: Option<&InferenceError>) -> Vec<Bytes> {
        let _ = failure;
        Vec::new()
    }

    /// Frames closing a stream that ended on a gateway-side failure rather
    /// than a provider terminal event.
    fn error_frames(&self, error: &InferenceError) -> TerminalFrames {
        TerminalFrames::one(self.encode_error(error))
    }

    fn observe(&self, usage: &mut UsageCapture, event: &Event) {
        usage.observe(event);
    }

    /// Called once the provider's `Done` has been encoded.
    fn settle(&self, usage: &mut UsageCapture) {
        let _ = usage;
    }
}

pub(crate) fn protocol_streaming_response<E>(execution: RoutedEvents, mut encoder: E) -> Response
where
    E: ProtocolStreamEncoder,
{
    let (writer, response) = sse_stream();
    tokio::spawn(async move {
        let (stream, mut accounting) = execution.into_parts();
        let RoutedStream {
            first,
            mut events,
            deadline,
            ..
        } = stream;
        let mut next = Some(Ok(first));
        let mut failure = None;
        let mut terminal = None;
        while let Some(item) = next {
            let event = match item {
                Ok(event) => event,
                Err(error) => {
                    failure = Some(InferenceError::from_transport(error));
                    break;
                }
            };
            encoder.observe(accounting.usage_mut(), &event);
            let is_done = matches!(event.kind, Kind::Done);
            let canonical_failure = match &event.kind {
                Kind::Error { error } => Some(InferenceError::from_canonical(error)),
                _ => None,
            };
            let is_terminal = is_done || canonical_failure.is_some();
            match encoder.push(event) {
                Ok(mut chunks) => {
                    if is_terminal {
                        chunks.extend(encoder.terminal_tail(canonical_failure.as_ref()));
                        if is_done {
                            encoder.settle(accounting.usage_mut());
                        }
                        terminal = Some(TerminalFrames::new(chunks));
                        failure = canonical_failure;
                        break;
                    }
                    for chunk in chunks {
                        if let Err(error) = writer.send_or_fail(chunk, deadline).await {
                            failure = Some(error);
                            break;
                        }
                    }
                }
                Err(error) => failure = Some(error),
            }
            if failure.is_some() {
                break;
            }
            next = tokio::select! {
                () = writer.closed() => {
                    failure = Some(InferenceError::client_cancelled());
                    None
                }
                () = tokio::time::sleep_until(deadline) => {
                    failure = Some(InferenceError::timeout());
                    None
                }
                next = events.next() => next,
            };
        }
        if terminal.is_none() && failure.is_none() {
            failure = Some(InferenceError::bad_gateway(
                "provider_protocol_error",
                "The provider stream ended without a terminal event.",
            ));
        }
        drop(events);
        writer.finish_stream(terminal, &mut failure, |error| encoder.error_frames(error));
        let outcome = failure
            .as_ref()
            .map_or_else(RequestOutcome::success, InferenceError::accounting_outcome);
        accounting.finish(outcome);
    });
    response
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use axum::{
        body::Bytes,
        http::{StatusCode, header},
    };
    use http_body_util::BodyExt as _;

    use super::{
        StreamFinishOutcome, StreamSendFailure, TerminalFrames, encode_sse_frame,
        sse_stream_with_capacity,
    };

    #[test]
    fn encode_sse_frame_preserves_event_id_and_data_line_bytes() {
        let encoded = encode_sse_frame(&olp_engine::protocols::sse::Frame {
            event: Some("message".to_owned()),
            data: "first\nsecond".to_owned(),
            id: Some("event-7".to_owned()),
            retry_ms: None,
        })
        .unwrap();

        assert_eq!(
            encoded.as_ref(),
            b"event: message\nid: event-7\ndata: first\ndata: second\n\n"
        );
    }

    #[test]
    fn encode_sse_frame_includes_retry_and_empty_data() {
        let encoded = encode_sse_frame(&olp_engine::protocols::sse::Frame {
            event: None,
            data: String::new(),
            id: Some("event-8".to_owned()),
            retry_ms: Some(250),
        })
        .unwrap();

        assert_eq!(encoded.as_ref(), b"id: event-8\nretry: 250\ndata: \n\n");
    }

    #[tokio::test]
    async fn sse_response_preserves_streamed_bytes_and_headers() {
        let (writer, response) = sse_stream_with_capacity(1);
        assert_eq!(
            writer
                .send(
                    Bytes::from_static(b"data: payload\n\n"),
                    tokio::time::Instant::now() + Duration::from_secs(1),
                )
                .await,
            Ok(())
        );
        assert_eq!(
            writer.finish(TerminalFrames::empty()),
            StreamFinishOutcome::Queued
        );

        assert_eq!(response.status(), StatusCode::OK);
        assert_eq!(
            response.headers()[header::CONTENT_TYPE],
            "text/event-stream; charset=utf-8"
        );
        assert_eq!(response.headers()[header::CACHE_CONTROL], "no-cache");
        assert_eq!(
            response
                .into_body()
                .collect()
                .await
                .unwrap()
                .to_bytes()
                .as_ref(),
            b"data: payload\n\n"
        );
    }

    #[tokio::test]
    async fn ordinary_frames_precede_a_ready_terminal_tail() {
        let (writer, response) = sse_stream_with_capacity(2);
        let deadline = tokio::time::Instant::now() + Duration::from_secs(1);
        writer
            .send(Bytes::from_static(b"data: one\n\n"), deadline)
            .await
            .unwrap();
        writer
            .send(Bytes::from_static(b"data: two\n\n"), deadline)
            .await
            .unwrap();
        assert_eq!(
            writer.finish(TerminalFrames::new(vec![
                Bytes::from_static(b"data: error\n\n"),
                Bytes::from_static(b"data: [DONE]\n\n"),
            ])),
            StreamFinishOutcome::Queued
        );
        assert_eq!(
            response
                .into_body()
                .collect()
                .await
                .unwrap()
                .to_bytes()
                .as_ref(),
            b"data: one\n\ndata: two\n\ndata: error\n\ndata: [DONE]\n\n"
        );
    }

    #[tokio::test]
    async fn terminal_finalization_does_not_wait_for_a_full_ordinary_queue() {
        let (writer, response) = sse_stream_with_capacity(1);
        writer
            .send(
                Bytes::from_static(b"data: ordinary\n\n"),
                tokio::time::Instant::now() + Duration::from_secs(1),
            )
            .await
            .unwrap();
        assert_eq!(
            writer.finish(TerminalFrames::new(vec![
                Bytes::from_static(b"data: error\n\n"),
                Bytes::from_static(b"data: [DONE]\n\n"),
            ])),
            StreamFinishOutcome::Queued
        );
        assert_eq!(
            response
                .into_body()
                .collect()
                .await
                .unwrap()
                .to_bytes()
                .as_ref(),
            b"data: ordinary\n\ndata: error\n\ndata: [DONE]\n\n"
        );
    }

    #[tokio::test]
    async fn body_drop_unblocks_a_full_ordinary_send_as_client_closed() {
        let (writer, response) = sse_stream_with_capacity(1);
        writer
            .send(
                Bytes::from_static(b"data: ordinary\n\n"),
                tokio::time::Instant::now() + Duration::from_secs(1),
            )
            .await
            .unwrap();
        let send = writer.send(
            Bytes::from_static(b"data: blocked\n\n"),
            tokio::time::Instant::now() + Duration::from_secs(60),
        );
        drop(response);
        assert_eq!(send.await, Err(StreamSendFailure::ClientClosed));
    }

    #[tokio::test]
    async fn full_open_ordinary_queue_reports_a_deadline() {
        let (writer, _response) = sse_stream_with_capacity(1);
        writer
            .send(
                Bytes::from_static(b"data: ordinary\n\n"),
                tokio::time::Instant::now() + Duration::from_secs(1),
            )
            .await
            .unwrap();
        assert_eq!(
            writer
                .send(
                    Bytes::from_static(b"data: blocked\n\n"),
                    tokio::time::Instant::now() + Duration::from_millis(20),
                )
                .await,
            Err(StreamSendFailure::DeadlineElapsed)
        );
    }

    #[test]
    #[should_panic(expected = "terminal SSE tails are limited to two frames")]
    fn terminal_tail_is_count_bounded() {
        let _ = TerminalFrames::new(vec![Bytes::new(), Bytes::new(), Bytes::new()]);
    }
}

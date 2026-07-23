use std::{collections::VecDeque, convert::Infallible};

use axum::{
    body::{Body, Bytes},
    http::{HeaderValue, header},
    response::Response,
};
use futures::{StreamExt, stream};
use olp_domain::{CanonicalEvent, CanonicalEventKind};
use olp_protocols::sse::{SseEncodeError, SseFrame, encode_frame};

use crate::{
    GatewayState,
    gateway::{
        InferenceError, RoutedEventExecution, UsageCapture, emit_event_execution_metadata,
        release_limits,
    },
};

pub(crate) fn encode_sse_frame(frame: &SseFrame) -> Result<Bytes, SseEncodeError> {
    encode_frame(frame).map(Bytes::from)
}

pub(crate) fn encode_server_sse_frame(frame: &SseFrame) -> Bytes {
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

pub(crate) trait ProtocolStreamEncoder: Send + 'static {
    fn push(&mut self, event: CanonicalEvent) -> Result<Vec<Bytes>, String>;
    fn encode_error(&self, error: &InferenceError) -> Bytes;
}

pub(crate) fn protocol_streaming_response<E>(
    state: GatewayState,
    mut execution: RoutedEventExecution,
    mut encoder: E,
) -> Response
where
    E: ProtocolStreamEncoder,
{
    let (writer, response) = sse_stream();
    tokio::spawn(async move {
        let mut events = std::mem::replace(&mut execution.events, Box::pin(stream::empty()));
        let mut next = Some(Ok(execution.first.clone()));
        let mut usage = UsageCapture::default();
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
            usage.observe(&event);
            let is_done = matches!(event.kind, CanonicalEventKind::Done);
            let canonical_failure = match &event.kind {
                CanonicalEventKind::Error { error } => Some(InferenceError::from_canonical(error)),
                _ => None,
            };
            let is_terminal = is_done || canonical_failure.is_some();
            match encoder.push(event) {
                Ok(chunks) => {
                    if is_terminal {
                        terminal = Some(TerminalFrames::new(chunks));
                        if let Some(canonical_failure) = canonical_failure {
                            failure = Some(canonical_failure);
                        }
                        break;
                    }
                    for chunk in chunks {
                        if let Err(error) = writer.send_or_fail(chunk, execution.deadline).await {
                            failure = Some(error);
                            break;
                        }
                    }
                }
                Err(message) => {
                    failure = Some(InferenceError::bad_gateway(
                        "provider_protocol_error",
                        message,
                    ));
                }
            }
            if failure.is_some() {
                break;
            }
            next = tokio::select! {
                () = writer.closed() => {
                    failure = Some(InferenceError::client_cancelled());
                    None
                }
                () = tokio::time::sleep_until(execution.deadline) => {
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
        writer.finish_stream(terminal, &mut failure, |error| {
            TerminalFrames::one(encoder.encode_error(error))
        });
        drop(events);
        emit_event_execution_metadata(&state, &execution, &usage, failure.as_ref());
        release_limits(&state, execution.lease.as_ref()).await;
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
        let encoded = encode_sse_frame(&olp_protocols::sse::SseFrame {
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
        let encoded = encode_sse_frame(&olp_protocols::sse::SseFrame {
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

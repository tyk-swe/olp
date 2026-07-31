//! Shared response-body and canonical-event streaming machinery for provider
//! transports.
//!
//! The Anthropic and Gemini connectors obtain their first response-body chunk
//! before returning a stream to the gateway. This module deliberately starts
//! its idle watchdog only after that handoff, so first-byte failures remain
//! pre-commit and eligible for the existing retry policy.

use std::{
    collections::VecDeque,
    fmt,
    future::ready,
    pin::Pin,
    task::{Context, Poll},
    time::Duration,
};

use bytes::Bytes;
use futures::{Stream, StreamExt, stream};
use olp_domain::{
    AttemptFailureClass, CanonicalEvent, ProviderEventStream, TransportError, TransportPhase,
};
use reqwest::Response;
use tokio::time::{Instant, Sleep};

pub(crate) type ReqwestByteStream =
    Pin<Box<dyn Stream<Item = Result<Bytes, reqwest::Error>> + Send + 'static>>;

/// Provider-labelled response I/O policy.
///
/// Keeping the label here preserves the connector-specific diagnostic text
/// while sharing timeout and commitment behavior.
#[derive(Clone, Copy)]
pub(crate) struct ProviderResponseIo {
    provider: &'static str,
}

impl ProviderResponseIo {
    #[must_use]
    pub(crate) const fn new(provider: &'static str) -> Self {
        Self { provider }
    }

    pub(crate) fn require_content_type(
        self,
        response: &Response,
        expected: &'static str,
    ) -> Result<(), TransportError> {
        if crate::transport_common::has_content_type(response.headers(), expected) {
            Ok(())
        } else {
            Err(self.protocol_error(
                TransportPhase::FirstByte,
                false,
                format!(
                    "{} response must use content type {expected}",
                    self.provider
                ),
            ))
        }
    }

    pub(crate) async fn read_bounded_body(
        self,
        response: Response,
        first_byte_deadline: Instant,
        attempt_deadline: Instant,
        idle_timeout: Duration,
        maximum: usize,
    ) -> Result<Vec<u8>, TransportError> {
        self.read_bounded_stream(
            Box::pin(response.bytes_stream()),
            first_byte_deadline,
            attempt_deadline,
            idle_timeout,
            maximum,
        )
        .await
    }

    pub(crate) async fn decoded_event_stream<D>(
        self,
        response: Response,
        first_byte_deadline: Instant,
        attempt_deadline: Instant,
        idle_timeout: Duration,
        decoder: D,
    ) -> Result<ProviderEventStream, TransportError>
    where
        D: CanonicalEventDecoder,
    {
        self.require_content_type(&response, "text/event-stream")?;
        let mut source: ReqwestByteStream = Box::pin(response.bytes_stream());
        let first_deadline = first_byte_deadline.min(attempt_deadline);
        let first = loop {
            let next = tokio::select! {
                biased;
                () = tokio::time::sleep_until(first_deadline) => {
                    return Err(self.first_byte_timeout());
                }
                next = source.next() => next,
            };
            let chunk = next
                .ok_or_else(|| {
                    self.protocol_error(
                        TransportPhase::FirstByte,
                        false,
                        format!("{} stream ended before its first body byte", self.provider),
                    )
                })?
                .map_err(|error| self.map_first_body_error(error))?;
            if !chunk.is_empty() {
                break chunk;
            }
        };
        let source = Box::pin(stream::once(ready(Ok(first))).chain(source));
        let bytes = self.after_first_byte_stream(source, idle_timeout, attempt_deadline);
        Ok(Box::pin(DecodedEventStream::new(self, bytes, decoder)))
    }

    /// Bounds a stream whose first byte has already been obtained by the
    /// caller. The source includes that buffered byte so downstream decoders
    /// observe it exactly once.
    #[must_use]
    pub(crate) fn after_first_byte_stream(
        self,
        source: ReqwestByteStream,
        idle_timeout: Duration,
        attempt_deadline: Instant,
    ) -> DeadlineByteStream {
        DeadlineByteStream::new(self, source, None, idle_timeout, attempt_deadline)
    }

    #[must_use]
    pub(crate) fn response_stream(
        self,
        response: Response,
        first_byte_deadline: Instant,
        idle_timeout: Duration,
        attempt_deadline: Instant,
    ) -> DeadlineByteStream {
        DeadlineByteStream::new(
            self,
            Box::pin(response.bytes_stream()),
            Some(first_byte_deadline),
            idle_timeout,
            attempt_deadline,
        )
    }

    pub(crate) fn first_byte_timeout(self) -> TransportError {
        self.transport_error(
            TransportPhase::FirstByte,
            AttemptFailureClass::Timeout,
            false,
            format!("{} first-byte deadline elapsed", self.provider),
        )
    }

    pub(crate) fn map_first_body_error(self, error: reqwest::Error) -> TransportError {
        self.transport_error(
            TransportPhase::FirstByte,
            if error.is_timeout() {
                AttemptFailureClass::Timeout
            } else {
                AttemptFailureClass::Connect
            },
            false,
            format!(
                "{} response body failed before its first byte",
                self.provider
            ),
        )
    }

    pub(crate) fn remaining(
        self,
        deadline: Instant,
        phase: TransportPhase,
    ) -> Result<Duration, TransportError> {
        deadline
            .checked_duration_since(Instant::now())
            .filter(|remaining| !remaining.is_zero())
            .ok_or_else(|| {
                self.transport_error(
                    phase,
                    AttemptFailureClass::Timeout,
                    false,
                    format!("{} attempt deadline elapsed", self.provider),
                )
            })
    }

    #[must_use]
    pub(crate) fn remaining_until(
        self,
        phase_deadline: Instant,
        attempt_deadline: Instant,
    ) -> Option<Duration> {
        phase_deadline
            .min(attempt_deadline)
            .checked_duration_since(Instant::now())
            .filter(|remaining| !remaining.is_zero())
    }

    async fn read_bounded_stream(
        self,
        mut source: ReqwestByteStream,
        first_byte_deadline: Instant,
        attempt_deadline: Instant,
        idle_timeout: Duration,
        maximum: usize,
    ) -> Result<Vec<u8>, TransportError> {
        let mut output = Vec::new();
        let mut first = true;
        let mut idle_deadline = attempt_deadline;
        loop {
            let (deadline, timeout_error) = if first {
                (
                    first_byte_deadline.min(attempt_deadline),
                    self.first_byte_timeout(),
                )
            } else {
                let deadline = idle_deadline.min(attempt_deadline);
                let error = if attempt_deadline <= idle_deadline {
                    self.attempt_body_timeout()
                } else {
                    self.body_idle_timeout()
                };
                (deadline, error)
            };
            let next = tokio::select! {
                biased;
                () = tokio::time::sleep_until(deadline) => {
                    return Err(timeout_error);
                }
                next = source.next() => next,
            };
            let Some(chunk) = next else { break };
            let chunk = chunk.map_err(|error| {
                if first {
                    self.map_first_body_error(error)
                } else {
                    self.map_body_error(error, false)
                }
            })?;
            if chunk.is_empty() {
                continue;
            }
            first = false;
            idle_deadline = Instant::now() + idle_timeout;
            if output.len().saturating_add(chunk.len()) > maximum {
                return Err(self.protocol_error(
                    TransportPhase::Body,
                    false,
                    format!(
                        "{} response exceeded the {maximum} byte limit",
                        self.provider
                    ),
                ));
            }
            output.extend_from_slice(&chunk);
        }
        if first {
            return Err(self.protocol_error(
                TransportPhase::FirstByte,
                false,
                format!("{} response body was empty", self.provider),
            ));
        }
        Ok(output)
    }

    fn body_idle_timeout(self) -> TransportError {
        self.transport_error(
            TransportPhase::Body,
            AttemptFailureClass::Timeout,
            false,
            format!("{} response idle deadline elapsed", self.provider),
        )
    }

    fn attempt_body_timeout(self) -> TransportError {
        self.transport_error(
            TransportPhase::Body,
            AttemptFailureClass::Timeout,
            false,
            format!(
                "{} attempt deadline elapsed while reading the response",
                self.provider
            ),
        )
    }

    fn map_body_error(self, error: reqwest::Error, response_committed: bool) -> TransportError {
        self.transport_error(
            TransportPhase::Body,
            if error.is_timeout() {
                AttemptFailureClass::Timeout
            } else {
                AttemptFailureClass::Connect
            },
            response_committed,
            format!("{} response body failed", self.provider),
        )
    }

    fn protocol_error(
        self,
        phase: TransportPhase,
        response_committed: bool,
        message: impl Into<String>,
    ) -> TransportError {
        self.transport_error(
            phase,
            AttemptFailureClass::Protocol,
            response_committed,
            message,
        )
    }

    fn transport_error(
        self,
        phase: TransportPhase,
        class: AttemptFailureClass,
        response_committed: bool,
        message: impl Into<String>,
    ) -> TransportError {
        TransportError {
            phase,
            class,
            response_committed,
            retry_after: None,
            message: message.into(),
        }
    }
}

#[must_use]
pub(crate) fn bounded_duration(configured: Duration, remaining: Duration) -> Duration {
    configured.min(remaining)
}

pub(crate) trait CanonicalEventDecoder: Send + Unpin + 'static {
    type Error: fmt::Display;

    fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, Self::Error>;
    fn finish(&mut self) -> Result<Vec<CanonicalEvent>, Self::Error>;
}

pub(crate) struct DeadlineByteStream {
    source: ReqwestByteStream,
    io: ProviderResponseIo,
    first: bool,
    idle_timeout: Duration,
    idle_sleep: Pin<Box<Sleep>>,
    attempt_deadline: Instant,
    terminal: bool,
}

impl DeadlineByteStream {
    fn new(
        io: ProviderResponseIo,
        source: ReqwestByteStream,
        first_byte_deadline: Option<Instant>,
        idle_timeout: Duration,
        attempt_deadline: Instant,
    ) -> Self {
        let wake = first_byte_deadline
            .unwrap_or_else(|| Instant::now() + idle_timeout)
            .min(attempt_deadline);
        Self {
            source,
            io,
            first: first_byte_deadline.is_some(),
            idle_timeout,
            idle_sleep: Box::pin(tokio::time::sleep_until(wake)),
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
                self.io.first_byte_timeout()
            } else {
                self.io.attempt_body_timeout()
            };
            return Poll::Ready(Some(Err(error)));
        }
        if self.idle_sleep.as_mut().poll(context).is_ready() {
            self.terminal = true;
            return Poll::Ready(Some(Err(if self.first {
                self.io.first_byte_timeout()
            } else {
                self.io.body_idle_timeout()
            })));
        }
        match self.source.as_mut().poll_next(context) {
            Poll::Ready(Some(Ok(chunk))) if chunk.is_empty() => {
                context.waker().wake_by_ref();
                Poll::Pending
            }
            Poll::Ready(Some(Ok(chunk))) => {
                self.first = false;
                let wake = (Instant::now() + self.idle_timeout).min(self.attempt_deadline);
                self.idle_sleep.as_mut().reset(wake);
                Poll::Ready(Some(Ok(chunk)))
            }
            Poll::Ready(Some(Err(error))) => {
                self.terminal = true;
                let error = if self.first {
                    self.io.map_first_body_error(error)
                } else {
                    self.io.map_body_error(error, false)
                };
                Poll::Ready(Some(Err(error)))
            }
            Poll::Ready(None) => {
                self.terminal = true;
                if self.first {
                    return Poll::Ready(Some(Err(self.io.protocol_error(
                        TransportPhase::FirstByte,
                        false,
                        format!("{} response body was empty", self.io.provider),
                    ))));
                }
                Poll::Ready(None)
            }
            Poll::Pending => Poll::Pending,
        }
    }
}

pub(crate) struct DecodedEventStream<D> {
    bytes: DeadlineByteStream,
    decoder: D,
    io: ProviderResponseIo,
    queued: VecDeque<CanonicalEvent>,
    committed: bool,
    terminal: bool,
}

impl<D> DecodedEventStream<D> {
    #[must_use]
    pub(crate) fn new(io: ProviderResponseIo, bytes: DeadlineByteStream, decoder: D) -> Self {
        Self {
            bytes,
            decoder,
            io,
            queued: VecDeque::new(),
            committed: false,
            terminal: false,
        }
    }
}

impl<D> Stream for DecodedEventStream<D>
where
    D: CanonicalEventDecoder,
{
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
                        return Poll::Ready(Some(Err(self.io.protocol_error(
                            TransportPhase::Body,
                            self.committed,
                            format!("invalid {} event stream: {error}", self.io.provider),
                        ))));
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
                            return Poll::Ready(Some(Err(self.io.protocol_error(
                                TransportPhase::Body,
                                self.committed,
                                format!("truncated {} event stream: {error}", self.io.provider),
                            ))));
                        }
                    }
                }
                Poll::Pending => return Poll::Pending,
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use futures::stream;
    use http::header;
    use olp_domain::CanonicalEventKind;
    use tokio::time::timeout;

    use super::*;

    struct TestDecoder;

    impl CanonicalEventDecoder for TestDecoder {
        type Error = String;

        fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, Self::Error> {
            match bytes {
                b"event" => Ok(vec![CanonicalEvent::new(0, CanonicalEventKind::Done)]),
                _ => Err("invalid frame".into()),
            }
        }

        fn finish(&mut self) -> Result<Vec<CanonicalEvent>, Self::Error> {
            Ok(Vec::new())
        }
    }

    fn source(items: impl IntoIterator<Item = Bytes>) -> ReqwestByteStream {
        Box::pin(stream::iter(
            items
                .into_iter()
                .map(Ok::<Bytes, reqwest::Error>)
                .collect::<Vec<_>>(),
        ))
    }

    fn response_with_content_types(values: &[&str]) -> Response {
        let mut builder = http::Response::builder().status(200);
        for value in values {
            builder = builder.header(header::CONTENT_TYPE, *value);
        }
        builder.body(reqwest::Body::default()).unwrap().into()
    }

    #[test]
    fn content_type_must_be_an_unambiguous_singleton() {
        let io = ProviderResponseIo::new("Test");
        assert!(
            io.require_content_type(
                &response_with_content_types(&["application/json; charset=utf-8"]),
                "application/json",
            )
            .is_ok()
        );
        assert!(
            io.require_content_type(
                &response_with_content_types(&["application/json", "text/plain"]),
                "application/json",
            )
            .is_err()
        );
    }

    #[tokio::test]
    async fn bounded_body_rejects_empty_and_oversized_streams() {
        let io = ProviderResponseIo::new("Test");
        let deadline = Instant::now() + Duration::from_secs(1);
        let empty = io
            .read_bounded_stream(
                Box::pin(stream::empty()),
                deadline,
                deadline,
                Duration::from_secs(1),
                8,
            )
            .await
            .unwrap_err();
        assert_eq!(empty.phase, TransportPhase::FirstByte);
        assert_eq!(empty.class, AttemptFailureClass::Protocol);
        assert_eq!(empty.message, "Test response body was empty");

        let oversized = io
            .read_bounded_stream(
                source([Bytes::from_static(b"oversized")]),
                deadline,
                deadline,
                Duration::from_secs(1),
                3,
            )
            .await
            .unwrap_err();
        assert_eq!(oversized.phase, TransportPhase::Body);
        assert_eq!(oversized.class, AttemptFailureClass::Protocol);
        assert_eq!(oversized.message, "Test response exceeded the 3 byte limit");

        let body = io
            .read_bounded_stream(
                source([Bytes::new(), Bytes::from_static(b"body")]),
                deadline,
                deadline,
                Duration::from_secs(1),
                8,
            )
            .await
            .unwrap();
        assert_eq!(body, b"body");
    }

    #[tokio::test]
    async fn bounded_body_reports_the_absolute_attempt_deadline() {
        let io = ProviderResponseIo::new("Test");
        let now = Instant::now();
        let source = Box::pin(
            stream::once(ready(Ok::<Bytes, reqwest::Error>(Bytes::from_static(
                b"body",
            ))))
            .chain(stream::pending()),
        );
        let error = io
            .read_bounded_stream(
                source,
                now + Duration::from_secs(1),
                now + Duration::from_millis(20),
                Duration::from_secs(1),
                8,
            )
            .await
            .unwrap_err();
        assert_eq!(
            error.message,
            "Test attempt deadline elapsed while reading the response"
        );
    }

    #[tokio::test]
    async fn decoder_error_after_an_event_is_committed() {
        let io = ProviderResponseIo::new("Test");
        let deadline = Instant::now() + Duration::from_secs(1);
        let bytes = io.after_first_byte_stream(
            source([Bytes::from_static(b"event"), Bytes::from_static(b"invalid")]),
            Duration::from_secs(1),
            deadline,
        );
        let mut events = DecodedEventStream::new(io, bytes, TestDecoder);

        assert!(events.next().await.unwrap().is_ok());
        let error = events.next().await.unwrap().unwrap_err();
        assert_eq!(error.phase, TransportPhase::Body);
        assert_eq!(error.class, AttemptFailureClass::Protocol);
        assert!(error.response_committed);
        assert_eq!(error.message, "invalid Test event stream: invalid frame");
        assert!(events.next().await.is_none());
    }

    #[tokio::test]
    async fn deadline_stream_enforces_idle_and_attempt_deadlines() {
        let io = ProviderResponseIo::new("Test");
        let mut empty = DeadlineByteStream::new(
            io,
            source([]),
            Some(Instant::now() + Duration::from_secs(1)),
            Duration::from_secs(1),
            Instant::now() + Duration::from_secs(1),
        );
        let empty_error = empty.next().await.unwrap().unwrap_err();
        assert_eq!(empty_error.phase, TransportPhase::FirstByte);
        assert_eq!(empty_error.class, AttemptFailureClass::Protocol);
        assert_eq!(empty_error.message, "Test response body was empty");

        let first_then_pending = || {
            Box::pin(
                stream::iter([Ok::<Bytes, reqwest::Error>(Bytes::from_static(b"event"))])
                    .chain(stream::pending()),
            ) as ReqwestByteStream
        };

        let mut idle = io.after_first_byte_stream(
            first_then_pending(),
            Duration::from_millis(20),
            Instant::now() + Duration::from_secs(1),
        );
        assert!(idle.next().await.unwrap().is_ok());
        let idle_error = timeout(Duration::from_secs(1), idle.next())
            .await
            .unwrap()
            .unwrap()
            .unwrap_err();
        assert_eq!(idle_error.phase, TransportPhase::Body);
        assert_eq!(idle_error.class, AttemptFailureClass::Timeout);
        assert_eq!(idle_error.message, "Test response idle deadline elapsed");
        assert!(idle.next().await.is_none());

        let mut attempt = io.after_first_byte_stream(
            first_then_pending(),
            Duration::from_secs(1),
            Instant::now() + Duration::from_millis(20),
        );
        assert!(attempt.next().await.unwrap().is_ok());
        let attempt_error = timeout(Duration::from_secs(1), attempt.next())
            .await
            .unwrap()
            .unwrap()
            .unwrap_err();
        assert_eq!(attempt_error.phase, TransportPhase::Body);
        assert_eq!(attempt_error.class, AttemptFailureClass::Timeout);
        assert_eq!(
            attempt_error.message,
            "Test attempt deadline elapsed while reading the response"
        );
        assert!(attempt.next().await.is_none());

        let mut zero_then_body = io.after_first_byte_stream(
            source([Bytes::new(), Bytes::from_static(b"event")]),
            Duration::from_secs(1),
            Instant::now() + Duration::from_secs(1),
        );
        assert_eq!(
            zero_then_body.next().await.unwrap().unwrap(),
            Bytes::from_static(b"event")
        );

        let mut late = io.after_first_byte_stream(
            source([Bytes::from_static(b"late")]),
            Duration::from_millis(10),
            Instant::now() + Duration::from_secs(1),
        );
        tokio::time::sleep(Duration::from_millis(30)).await;
        let late_error = late.next().await.unwrap().unwrap_err();
        assert_eq!(late_error.message, "Test response idle deadline elapsed");
    }

    #[test]
    fn bounded_duration_uses_the_tighter_limit() {
        assert_eq!(
            bounded_duration(Duration::from_secs(2), Duration::from_secs(1)),
            Duration::from_secs(1)
        );
    }
}

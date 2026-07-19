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
    pin::Pin,
    task::{Context, Poll},
    time::Duration,
};

use bytes::Bytes;
use futures::{Stream, StreamExt};
use http::header;
use olp_domain::{AttemptFailureClass, CanonicalEvent, TransportError, TransportPhase};
use reqwest::Response;
use tokio::time::{Instant, Sleep, timeout};

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
        let valid = response
            .headers()
            .get(header::CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.split(';').next())
            .is_some_and(|value| value.trim().eq_ignore_ascii_case(expected));
        if valid {
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
        DeadlineByteStream::new(self, source, idle_timeout, attempt_deadline)
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
        loop {
            let wait = if first {
                self.remaining_until(first_byte_deadline, attempt_deadline)
                    .ok_or_else(|| self.first_byte_timeout())?
            } else {
                bounded_duration(
                    idle_timeout,
                    self.remaining(attempt_deadline, TransportPhase::Body)?,
                )
            };
            let next = timeout(wait, source.next()).await.map_err(|_| {
                if first {
                    self.first_byte_timeout()
                } else {
                    self.body_idle_timeout()
                }
            })?;
            let Some(chunk) = next else { break };
            let chunk = chunk.map_err(|error| {
                if first {
                    self.map_first_body_error(error)
                } else {
                    self.map_body_error(error, false)
                }
            })?;
            first = false;
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
    idle_timeout: Duration,
    idle_sleep: Pin<Box<Sleep>>,
    attempt_deadline: Instant,
    terminal: bool,
}

impl DeadlineByteStream {
    fn new(
        io: ProviderResponseIo,
        source: ReqwestByteStream,
        idle_timeout: Duration,
        attempt_deadline: Instant,
    ) -> Self {
        Self {
            source,
            io,
            idle_timeout,
            idle_sleep: Box::pin(tokio::time::sleep_until(
                (Instant::now() + idle_timeout).min(attempt_deadline),
            )),
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
            return Poll::Ready(Some(Err(self.io.attempt_body_timeout())));
        }
        match self.source.as_mut().poll_next(context) {
            Poll::Ready(Some(Ok(chunk))) => {
                let wake = (Instant::now() + self.idle_timeout).min(self.attempt_deadline);
                self.idle_sleep.as_mut().reset(wake);
                return Poll::Ready(Some(Ok(chunk)));
            }
            Poll::Ready(Some(Err(error))) => {
                self.terminal = true;
                return Poll::Ready(Some(Err(self.io.map_body_error(error, false))));
            }
            Poll::Ready(None) => {
                self.terminal = true;
                return Poll::Ready(None);
            }
            Poll::Pending => {}
        }
        if self.idle_sleep.as_mut().poll(context).is_ready() {
            self.terminal = true;
            return Poll::Ready(Some(Err(if Instant::now() >= self.attempt_deadline {
                self.io.attempt_body_timeout()
            } else {
                self.io.body_idle_timeout()
            })));
        }
        Poll::Pending
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
    use olp_domain::CanonicalEventKind;

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
    }

    #[test]
    fn bounded_duration_uses_the_tighter_limit() {
        assert_eq!(
            bounded_duration(Duration::from_secs(2), Duration::from_secs(1)),
            Duration::from_secs(1)
        );
    }
}

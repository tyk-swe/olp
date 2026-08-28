//! Shared response-body and canonical-event streaming machinery for provider
//! transports.
//!
//! The Anthropic and Gemini connectors obtain their first response-body chunk
//! before returning a stream to the gateway. This module deliberately starts
//! its idle watchdog only after that handoff, so first-byte failures remain
//! pre-commit and eligible for the existing retry policy.

use std::{future::ready, pin::Pin, time::Duration};

use crate::domain::ports::{
    AttemptFailureClass, ProviderEventStream, TransportError, TransportPhase,
};
use bytes::Bytes;
use futures::{Stream, StreamExt as _, stream};
use http::header;
use reqwest::Response;
use tokio::time::{Instant, timeout};

pub(in crate::providers) mod event_stream;
use self::event_stream::{CanonicalEventDecoder, DeadlineByteStream, DecodedEventStream};

pub(in crate::providers) type ReqwestByteStream =
    Pin<Box<dyn Stream<Item = Result<Bytes, reqwest::Error>> + Send + 'static>>;

#[derive(Clone, Copy)]
enum DeadlineKind {
    Phase,
    Attempt,
}

fn select_deadline(phase_deadline: Instant, attempt_deadline: Instant) -> (Instant, DeadlineKind) {
    if attempt_deadline <= phase_deadline {
        (attempt_deadline, DeadlineKind::Attempt)
    } else {
        (phase_deadline, DeadlineKind::Phase)
    }
}

/// The first-byte default is patience of last resort, never a cap.
///
/// For a unary completion the first byte *is* the whole generation, so a
/// connector default that precedes the route's configured `overall_timeout`
/// silently overrode it — every unary attempt on a 300s route died at 30s, and
/// the resulting `Timeout` then burned each remaining target the same way. The
/// operator's deadline governs; the idle watchdog, which detects a genuinely
/// stalled stream, keeps its own shorter window.
pub(in crate::providers) fn first_byte_deadline_within_attempt(
    configured: Instant,
    attempt_deadline: Instant,
) -> Instant {
    configured.max(attempt_deadline)
}

/// Provider-labelled response I/O policy.
///
/// Keeping the label here preserves the connector-specific diagnostic text
/// while sharing timeout and commitment behavior.
#[derive(Clone, Copy)]
pub(in crate::providers) struct ProviderResponseIo {
    provider: &'static str,
}

impl ProviderResponseIo {
    #[must_use]
    pub(in crate::providers) const fn new(provider: &'static str) -> Self {
        Self { provider }
    }

    pub(in crate::providers) fn require_content_type(
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

    pub(in crate::providers) async fn read_bounded_body(
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

    pub(in crate::providers) async fn decoded_event_stream<D>(
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
        let (first_deadline, deadline_kind) = select_deadline(
            first_byte_deadline_within_attempt(first_byte_deadline, attempt_deadline),
            attempt_deadline,
        );
        let first_wait = first_deadline
            .checked_duration_since(Instant::now())
            .ok_or_else(|| self.first_byte_wait_timeout(deadline_kind))?;
        let first = timeout(first_wait, source.next())
            .await
            .map_err(|_| self.first_byte_wait_timeout(deadline_kind))?
            .ok_or_else(|| {
                self.protocol_error(
                    TransportPhase::FirstByte,
                    false,
                    format!("{} stream ended before its first body byte", self.provider),
                )
            })?
            .map_err(|error| self.map_first_body_error(error))?;
        let source = Box::pin(stream::once(ready(Ok(first))).chain(source));
        let bytes = self.after_first_byte_stream(source, idle_timeout, attempt_deadline);
        Ok(Box::pin(DecodedEventStream::new(self, bytes, decoder)))
    }

    /// Bounds a stream whose first byte has already been obtained by the
    /// caller. The source includes that buffered byte so downstream decoders
    /// observe it exactly once.
    #[must_use]
    pub(in crate::providers) fn after_first_byte_stream(
        self,
        source: ReqwestByteStream,
        idle_timeout: Duration,
        attempt_deadline: Instant,
    ) -> DeadlineByteStream {
        DeadlineByteStream::new(self, source, None, idle_timeout, attempt_deadline)
    }

    #[must_use]
    pub(in crate::providers) fn response_stream(
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

    pub(in crate::providers) fn first_byte_timeout(self) -> TransportError {
        self.transport_error(
            TransportPhase::FirstByte,
            AttemptFailureClass::Timeout,
            false,
            format!("{} first-byte deadline elapsed", self.provider),
        )
    }

    fn attempt_first_byte_timeout(self) -> TransportError {
        self.transport_error(
            TransportPhase::FirstByte,
            AttemptFailureClass::Timeout,
            false,
            format!(
                "{} attempt deadline elapsed before the first response byte",
                self.provider
            ),
        )
    }

    fn first_byte_wait_timeout(self, deadline_kind: DeadlineKind) -> TransportError {
        match deadline_kind {
            DeadlineKind::Phase => self.first_byte_timeout(),
            DeadlineKind::Attempt => self.attempt_first_byte_timeout(),
        }
    }

    pub(in crate::providers) fn map_first_body_error(
        self,
        error: reqwest::Error,
    ) -> TransportError {
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

    pub(in crate::providers) fn remaining(
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
    pub(in crate::providers) fn remaining_until(
        self,
        phase_deadline: Instant,
        attempt_deadline: Instant,
    ) -> Option<Duration> {
        phase_deadline
            .min(attempt_deadline)
            .checked_duration_since(Instant::now())
    }

    /// Sends a request whose response headers must arrive before the earlier
    /// of the first-byte and attempt deadlines. Every connector's send used to
    /// spell out this wait/timeout/map sequence itself.
    pub(in crate::providers) async fn send_before(
        self,
        request: reqwest::RequestBuilder,
        first_byte_deadline: Instant,
        attempt_deadline: Instant,
        map_send_error: impl FnOnce(reqwest::Error) -> TransportError,
    ) -> Result<reqwest::Response, TransportError> {
        let send_wait = self
            .remaining_until(first_byte_deadline, attempt_deadline)
            .ok_or_else(|| self.first_byte_timeout())?;
        timeout(send_wait, request.send())
            .await
            .map_err(|_| self.first_byte_timeout())?
            .map_err(map_send_error)
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
            let (wait, attempt_deadline_is_tighter) = if first {
                let (deadline, deadline_kind) = select_deadline(
                    first_byte_deadline_within_attempt(first_byte_deadline, attempt_deadline),
                    attempt_deadline,
                );
                let wait = deadline
                    .checked_duration_since(Instant::now())
                    .ok_or_else(|| self.first_byte_wait_timeout(deadline_kind))?;
                (wait, matches!(deadline_kind, DeadlineKind::Attempt))
            } else {
                let attempt_remaining = self.remaining(attempt_deadline, TransportPhase::Body)?;
                (
                    bounded_duration(idle_timeout, attempt_remaining),
                    attempt_remaining <= idle_timeout,
                )
            };
            let next = timeout(wait, source.next()).await.map_err(|_| {
                if first && attempt_deadline_is_tighter {
                    self.attempt_first_byte_timeout()
                } else if first {
                    self.first_byte_timeout()
                } else if attempt_deadline_is_tighter {
                    self.attempt_body_timeout()
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
            upstream: Default::default(),
            phase,
            class,
            response_committed,
            message: message.into(),
        }
    }
}

#[must_use]
pub(in crate::providers) fn bounded_duration(
    configured: Duration,
    remaining: Duration,
) -> Duration {
    configured.min(remaining)
}

#[cfg(test)]
mod tests;

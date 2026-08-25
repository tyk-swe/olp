use std::{
    collections::VecDeque,
    fmt,
    pin::Pin,
    task::{Context, Poll},
    time::Duration,
};

use crate::domain::{
    canonical::events::Event,
    ports::{TransportError, TransportPhase},
};
use bytes::Bytes;
use futures::Stream;
use tokio::time::{Instant, Sleep};

use super::{DeadlineKind, ProviderResponseIo, ReqwestByteStream, select_deadline};

pub(in crate::providers) trait CanonicalEventDecoder:
    Send + Unpin + 'static
{
    type Error: fmt::Display;

    fn push(&mut self, bytes: &[u8]) -> Result<Vec<Event>, Self::Error>;
    fn finish(&mut self) -> Result<Vec<Event>, Self::Error>;
}

pub(in crate::providers) struct DeadlineByteStream {
    source: ReqwestByteStream,
    io: ProviderResponseIo,
    first: bool,
    idle_timeout: Duration,
    deadline_sleep: Pin<Box<Sleep>>,
    deadline_kind: DeadlineKind,
    attempt_deadline: Instant,
    terminal: bool,
}

impl DeadlineByteStream {
    pub(super) fn new(
        io: ProviderResponseIo,
        source: ReqwestByteStream,
        first_byte_deadline: Option<Instant>,
        idle_timeout: Duration,
        attempt_deadline: Instant,
    ) -> Self {
        let phase_deadline = first_byte_deadline.map_or_else(
            || Instant::now() + idle_timeout,
            |configured| super::first_byte_deadline_within_attempt(configured, attempt_deadline),
        );
        let (wake, deadline_kind) = select_deadline(phase_deadline, attempt_deadline);
        Self {
            source,
            io,
            first: first_byte_deadline.is_some(),
            idle_timeout,
            deadline_sleep: Box::pin(tokio::time::sleep_until(wake)),
            deadline_kind,
            attempt_deadline,
            terminal: false,
        }
    }

    fn timeout_error(&self) -> TransportError {
        match (self.first, self.deadline_kind) {
            (true, DeadlineKind::Attempt) => self.io.attempt_first_byte_timeout(),
            (true, DeadlineKind::Phase) => self.io.first_byte_timeout(),
            (false, DeadlineKind::Attempt) => self.io.attempt_body_timeout(),
            (false, DeadlineKind::Phase) => self.io.body_idle_timeout(),
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
            return Poll::Ready(Some(Err(self.timeout_error())));
        }
        if self.deadline_sleep.as_mut().poll(context).is_ready() {
            self.terminal = true;
            return Poll::Ready(Some(Err(self.timeout_error())));
        }
        match self.source.as_mut().poll_next(context) {
            Poll::Ready(Some(Ok(chunk))) => {
                self.first = false;
                let idle_deadline = Instant::now() + self.idle_timeout;
                let (wake, deadline_kind) = select_deadline(idle_deadline, self.attempt_deadline);
                self.deadline_kind = deadline_kind;
                self.deadline_sleep.as_mut().reset(wake);
                return Poll::Ready(Some(Ok(chunk)));
            }
            Poll::Ready(Some(Err(error))) => {
                self.terminal = true;
                let error = if self.first {
                    self.io.map_first_body_error(error)
                } else {
                    self.io.map_body_error(error, false)
                };
                return Poll::Ready(Some(Err(error)));
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
                return Poll::Ready(None);
            }
            Poll::Pending => {}
        }
        Poll::Pending
    }
}

pub(in crate::providers) struct DecodedEventStream<D> {
    bytes: DeadlineByteStream,
    decoder: D,
    io: ProviderResponseIo,
    queued: VecDeque<Event>,
    committed: bool,
    terminal: bool,
}

impl<D> DecodedEventStream<D> {
    #[must_use]
    pub(in crate::providers) fn new(
        io: ProviderResponseIo,
        bytes: DeadlineByteStream,
        decoder: D,
    ) -> Self {
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

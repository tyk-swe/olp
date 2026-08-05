use std::time::Duration;

use bytes::Bytes;
use futures::{StreamExt as _, stream};
use olp_domain::{AttemptFailureClass, CanonicalEvent, CanonicalEventKind, TransportPhase};
use tokio::time::{Instant, timeout};

use super::{
    CanonicalEventDecoder, DeadlineByteStream, DecodedEventStream, ProviderResponseIo,
    ReqwestByteStream, bounded_duration,
};

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
}

#[test]
fn bounded_duration_uses_the_tighter_limit() {
    assert_eq!(
        bounded_duration(Duration::from_secs(2), Duration::from_secs(1)),
        Duration::from_secs(1)
    );
}

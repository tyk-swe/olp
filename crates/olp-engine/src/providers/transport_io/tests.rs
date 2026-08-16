use std::time::Duration;

use crate::domain::{
    canonical::events::{Event, Kind},
    ports::{AttemptFailureClass, TransportPhase},
};
use bytes::Bytes;
use futures::{StreamExt as _, stream};
use tokio::time::{Instant, advance, sleep, timeout};

use super::{
    CanonicalEventDecoder, DeadlineByteStream, DecodedEventStream, ProviderResponseIo,
    ReqwestByteStream, bounded_duration,
};

struct TestDecoder;

impl CanonicalEventDecoder for TestDecoder {
    type Error = String;

    fn push(&mut self, bytes: &[u8]) -> Result<Vec<Event>, Self::Error> {
        match bytes {
            b"event" => Ok(vec![Event::new(0, Kind::Done)]),
            _ => Err("invalid frame".into()),
        }
    }

    fn finish(&mut self) -> Result<Vec<Event>, Self::Error> {
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

#[tokio::test(start_paused = true)]
async fn deadline_stream_prefers_phase_timeout_over_simultaneously_ready_bytes() {
    let io = ProviderResponseIo::new("Test");
    let phase_timeout = Duration::from_secs(1);
    let attempt_deadline = Instant::now() + Duration::from_secs(10);
    let late_first_byte = Box::pin(stream::once(async move {
        sleep(phase_timeout).await;
        Ok::<Bytes, reqwest::Error>(Bytes::from_static(b"late"))
    })) as ReqwestByteStream;
    let mut first = DeadlineByteStream::new(
        io,
        late_first_byte,
        Some(Instant::now() + phase_timeout),
        Duration::from_secs(10),
        attempt_deadline,
    );

    let first_error = first.next().await.unwrap().unwrap_err();
    assert_eq!(first_error.phase, TransportPhase::FirstByte);
    assert_eq!(first_error.class, AttemptFailureClass::Timeout);
    assert_eq!(first_error.message, "Test first-byte deadline elapsed");
    assert!(first.next().await.is_none());

    let late_body = Box::pin(
        stream::iter([Ok::<Bytes, reqwest::Error>(Bytes::from_static(b"first"))]).chain(
            stream::once(async move {
                sleep(phase_timeout).await;
                Ok::<Bytes, reqwest::Error>(Bytes::from_static(b"late"))
            }),
        ),
    ) as ReqwestByteStream;
    let mut body = io.after_first_byte_stream(
        late_body,
        phase_timeout,
        Instant::now() + Duration::from_secs(10),
    );

    assert_eq!(
        body.next().await.unwrap().unwrap(),
        Bytes::from_static(b"first")
    );
    let body_error = body.next().await.unwrap().unwrap_err();
    assert_eq!(body_error.phase, TransportPhase::Body);
    assert_eq!(body_error.class, AttemptFailureClass::Timeout);
    assert_eq!(body_error.message, "Test response idle deadline elapsed");
    assert!(body.next().await.is_none());
}

#[tokio::test(start_paused = true)]
async fn deadline_stream_reports_attempt_deadline_before_the_first_byte() {
    let io = ProviderResponseIo::new("Test");
    let now = Instant::now();
    let mut stream = DeadlineByteStream::new(
        io,
        Box::pin(stream::pending()),
        Some(now + Duration::from_secs(10)),
        Duration::from_secs(30),
        now + Duration::from_secs(1),
    );

    let error = stream.next().await.unwrap().unwrap_err();
    assert_eq!(error.phase, TransportPhase::FirstByte);
    assert_eq!(error.class, AttemptFailureClass::Timeout);
    assert_eq!(
        error.message,
        "Test attempt deadline elapsed before the first response byte"
    );
    assert!(stream.next().await.is_none());
}

#[tokio::test(start_paused = true)]
async fn deadline_stream_preserves_earlier_idle_cause_after_delayed_poll() {
    let io = ProviderResponseIo::new("Test");
    let mut body = io.after_first_byte_stream(
        Box::pin(
            stream::iter([Ok::<Bytes, reqwest::Error>(Bytes::from_static(b"first"))])
                .chain(stream::pending()),
        ),
        Duration::from_secs(1),
        Instant::now() + Duration::from_secs(10),
    );

    assert_eq!(
        body.next().await.unwrap().unwrap(),
        Bytes::from_static(b"first")
    );
    advance(Duration::from_secs(11)).await;

    let body_error = body.next().await.unwrap().unwrap_err();
    assert_eq!(body_error.phase, TransportPhase::Body);
    assert_eq!(body_error.class, AttemptFailureClass::Timeout);
    assert_eq!(body_error.message, "Test response idle deadline elapsed");
    assert!(body.next().await.is_none());
}

#[tokio::test(start_paused = true)]
async fn bounded_body_reports_the_tighter_attempt_deadline() {
    let io = ProviderResponseIo::new("Test");
    let now = Instant::now();
    let error = io
        .read_bounded_stream(
            Box::pin(
                stream::iter([Ok::<Bytes, reqwest::Error>(Bytes::from_static(b"first"))])
                    .chain(stream::pending()),
            ),
            now + Duration::from_secs(1),
            now + Duration::from_secs(5),
            Duration::from_secs(30),
            16,
        )
        .await
        .unwrap_err();

    assert_eq!(error.phase, TransportPhase::Body);
    assert_eq!(error.class, AttemptFailureClass::Timeout);
    assert_eq!(
        error.message,
        "Test attempt deadline elapsed while reading the response"
    );
}

#[tokio::test(start_paused = true)]
async fn bounded_body_reports_attempt_deadline_before_the_first_byte() {
    let io = ProviderResponseIo::new("Test");
    let now = Instant::now();
    let error = io
        .read_bounded_stream(
            Box::pin(stream::pending()),
            now + Duration::from_secs(10),
            now + Duration::from_secs(1),
            Duration::from_secs(30),
            16,
        )
        .await
        .unwrap_err();

    assert_eq!(error.phase, TransportPhase::FirstByte);
    assert_eq!(error.class, AttemptFailureClass::Timeout);
    assert_eq!(
        error.message,
        "Test attempt deadline elapsed before the first response byte"
    );
}

#[test]
fn bounded_duration_uses_the_tighter_limit() {
    assert_eq!(
        bounded_duration(Duration::from_secs(2), Duration::from_secs(1)),
        Duration::from_secs(1)
    );
}

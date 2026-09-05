use super::*;
use crate::domain::canonical::events::Event;
#[tokio::test]
async fn canonical_event_stream_wrapper_rejects_gaps_and_missing_done() {
    let first = Event::new(
        0,
        Kind::ResponseStart {
            response_id: None,
            provider_model: None,
        },
    );
    let mut validator = EventSequenceValidator::new();
    validator.push(&first).unwrap();
    let events: ProviderEventStream = Box::pin(stream::iter([Ok(Event::new(2, Kind::Done))]));
    let error = match validated_event_stream(events, validator).next().await {
        Some(Err(error)) => error,
        _ => panic!("sequence gap must become a protocol error"),
    };
    assert_eq!(error.class, AttemptFailureClass::Protocol);
    assert!(error.response_committed);
    assert!(
        error
            .message
            .contains("expected canonical event sequence 1")
    );

    let mut validator = EventSequenceValidator::new();
    validator.push(&first).unwrap();
    let events: ProviderEventStream = Box::pin(stream::empty());
    let error = match validated_event_stream(events, validator).next().await {
        Some(Err(error)) => error,
        _ => panic!("missing done must become a protocol error"),
    };
    assert_eq!(error.class, AttemptFailureClass::Protocol);
    assert!(error.message.contains("ended before done"));

    let mut validator = EventSequenceValidator::new();
    validator.push(&first).unwrap();
    let events: ProviderEventStream = Box::pin(stream::iter([Ok(Event::new(1, Kind::Done))]));
    let mut events = validated_event_stream(events, validator);
    assert!(matches!(
        events.next().await,
        Some(Ok(Event {
            kind: Kind::Done,
            ..
        }))
    ));
    assert!(events.next().await.is_none());
}

#[tokio::test]
async fn committed_stream_failures_trip_circuit_only_after_terminal_accounting() {
    let circuits = Breaker::default();
    let target = TargetId::new();
    let first = Event::new(
        0,
        Kind::ResponseStart {
            response_id: None,
            provider_model: None,
        },
    );

    for _ in 0..5 {
        assert!(circuits.try_acquire(target));
        let mut validator = EventSequenceValidator::new();
        validator.push(&first).unwrap();
        let provider: ProviderEventStream = Box::pin(stream::iter([Err(TransportError {
            upstream: Default::default(),
            phase: crate::domain::ports::TransportPhase::Body,
            class: AttemptFailureClass::UpstreamServer,
            response_committed: false,
            message: "stream failed after its first event".to_owned(),
        })]));
        let mut events = circuit_accounted_event_stream(
            validated_event_stream(provider, validator),
            circuits.clone(),
            target,
            false,
        );
        let error = events.next().await.unwrap().unwrap_err();
        assert!(error.response_committed);
    }
    assert!(!circuits.is_selectable(target));

    let recovered_target = TargetId::new();
    circuits.record_failure(recovered_target, AttemptFailureClass::UpstreamServer);
    let mut validator = EventSequenceValidator::new();
    validator.push(&first).unwrap();
    let provider: ProviderEventStream = Box::pin(stream::iter([Ok(Event::new(1, Kind::Done))]));
    let mut events = circuit_accounted_event_stream(
        validated_event_stream(provider, validator),
        circuits.clone(),
        recovered_target,
        false,
    );
    assert!(matches!(
        events.next().await,
        Some(Ok(Event {
            kind: Kind::Done,
            ..
        }))
    ));
    for _ in 0..4 {
        circuits.record_failure(recovered_target, AttemptFailureClass::UpstreamServer);
    }
    assert!(circuits.is_selectable(recovered_target));
}

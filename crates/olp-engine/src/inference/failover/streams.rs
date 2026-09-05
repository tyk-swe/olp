use crate::domain::{
    canonical::events::{ErrorClass, EventSequenceError, EventSequenceValidator, Kind},
    ids::TargetId,
    ports::{AttemptFailureClass, TransportError, TransportPhase},
};
use crate::inference::circuit::{Breaker, CircuitPermit};
use futures::{StreamExt, stream};

use crate::domain::ports::ProviderEventStream;

pub(super) fn canonical_event_protocol_error(
    error: EventSequenceError,
    response_committed: bool,
) -> TransportError {
    TransportError {
        upstream: Default::default(),
        phase: TransportPhase::Body,
        class: AttemptFailureClass::Protocol,
        response_committed,
        message: format!("invalid canonical event stream: {error}"),
    }
}

pub(super) fn validated_event_stream(
    events: ProviderEventStream,
    validator: EventSequenceValidator,
) -> ProviderEventStream {
    Box::pin(stream::unfold(
        (events, validator, false),
        |(mut events, mut validator, terminal)| async move {
            if terminal || validator.is_complete() {
                return None;
            }
            match events.next().await {
                Some(Ok(event)) => match validator.push(&event) {
                    Ok(()) => Some((Ok(event), (events, validator, false))),
                    Err(error) => Some((
                        Err(canonical_event_protocol_error(error, true)),
                        (events, validator, true),
                    )),
                },
                Some(Err(error)) => Some((Err(error), (events, validator, true))),
                None => validator.finish().err().map(|error| {
                    (
                        Err(canonical_event_protocol_error(error, true)),
                        (events, validator, true),
                    )
                }),
            }
        },
    ))
}

#[cfg(test)]
fn circuit_accounted_event_stream(
    events: ProviderEventStream,
    circuits: Breaker,
    target: TargetId,
    initial_failure: bool,
) -> ProviderEventStream {
    circuit_accounted_event_stream_with_permit(events, circuits, target, initial_failure, None)
}

pub(super) fn circuit_accounted_event_stream_with_permit(
    events: ProviderEventStream,
    circuits: Breaker,
    target: TargetId,
    initial_failure: bool,
    permit: Option<CircuitPermit>,
) -> ProviderEventStream {
    Box::pin(stream::unfold(
        (events, circuits, initial_failure),
        move |(mut events, circuits, mut failed)| async move {
            let item = events.next().await?;
            let item = match item {
                Ok(event) => {
                    match &event.kind {
                        Kind::Error { error } => {
                            if let Some(class) = canonical_error_circuit_class(error.class) {
                                circuits.record_failure_for_optional_permit(
                                    target,
                                    permit.as_ref(),
                                    class,
                                    None,
                                );
                            }
                            failed = true;
                        }
                        Kind::Done if !failed => {
                            circuits.record_success_for_optional_permit(target, permit.as_ref())
                        }
                        _ => {}
                    }
                    Ok(event)
                }
                Err(mut error) => {
                    error.response_committed = true;
                    circuits.record_failure_for_optional_permit(
                        target,
                        permit.as_ref(),
                        error.class,
                        error.upstream.retry_after,
                    );
                    failed = true;
                    Err(error)
                }
            };
            Some((item, (events, circuits, failed)))
        },
    ))
}

pub(super) const fn canonical_error_circuit_class(
    class: ErrorClass,
) -> Option<AttemptFailureClass> {
    match class {
        ErrorClass::RateLimit => Some(AttemptFailureClass::RateLimit),
        ErrorClass::Timeout => Some(AttemptFailureClass::Timeout),
        ErrorClass::Transport => Some(AttemptFailureClass::Connect),
        ErrorClass::Upstream => Some(AttemptFailureClass::UpstreamServer),
        ErrorClass::Authentication
        | ErrorClass::Authorization
        | ErrorClass::InvalidRequest
        | ErrorClass::Internal => None,
    }
}

#[cfg(test)]
mod tests;

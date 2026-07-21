use std::{sync::Arc, time::Duration};

use axum::http::StatusCode;
use chrono::Utc;
use futures::{StreamExt, stream};
use olp_domain::{
    AttemptFailureClass, AttemptPlan, CanonicalEvent, CanonicalEventKind, CanonicalResult,
    ErrorClass, EventSequenceError, EventSequenceValidator, MediaSpool, Operation, OperationKind,
    ProviderOutput, ProviderRequest, RequestMetadata, TargetId, TransportError,
};
use olp_storage::RequestAttemptMetadata;

use crate::semantic_validation::operation_for_provider;

use super::{error::InferenceError, telemetry::elapsed_ms};

pub(super) type EventStream = olp_domain::ProviderEventStream;

fn canonical_event_protocol_error(
    error: EventSequenceError,
    response_committed: bool,
) -> TransportError {
    TransportError {
        phase: olp_domain::TransportPhase::Body,
        class: AttemptFailureClass::Protocol,
        response_committed,
        message: format!("invalid canonical event stream: {error}"),
    }
}

pub(super) fn validated_event_stream(
    events: EventStream,
    validator: EventSequenceValidator,
) -> EventStream {
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

pub(super) fn circuit_accounted_event_stream(
    events: EventStream,
    circuits: crate::circuit::CircuitBreaker,
    target: TargetId,
    initial_failure: bool,
) -> EventStream {
    Box::pin(stream::unfold(
        (events, circuits, initial_failure),
        move |(mut events, circuits, mut failed)| async move {
            let item = events.next().await?;
            let item = match item {
                Ok(event) => {
                    match &event.kind {
                        CanonicalEventKind::Error { error } => {
                            if let Some(class) = canonical_error_circuit_class(error.class) {
                                circuits.record_failure(target, class);
                            }
                            failed = true;
                        }
                        CanonicalEventKind::Done if !failed => circuits.record_success(target),
                        _ => {}
                    }
                    Ok(event)
                }
                Err(mut error) => {
                    // A provider stream has already committed once this wrapper
                    // owns it. Terminal transport failures still affect target
                    // health, but must never trigger request failover.
                    error.response_committed = true;
                    circuits.record_failure(target, error.class);
                    failed = true;
                    Err(error)
                }
            };
            Some((item, (events, circuits, failed)))
        },
    ))
}

const fn canonical_error_circuit_class(class: ErrorClass) -> Option<AttemptFailureClass> {
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

pub(super) struct ExecutionSuccess {
    pub(super) output: ExecutionOutput,
    pub(super) deadline: tokio::time::Instant,
    pub(super) attempts: Vec<RequestAttemptMetadata>,
    pub(super) attempt_started: tokio::time::Instant,
}

pub(super) enum ExecutionOutput {
    Events {
        first: CanonicalEvent,
        events: EventStream,
    },
    Result(Box<CanonicalResult>),
}

pub(super) struct ExecutionFailure {
    pub(super) error: InferenceError,
    pub(super) attempts: Vec<RequestAttemptMetadata>,
}

pub(super) async fn execute_with_failover(
    runtime: &crate::RuntimeBundle,
    attempts: Vec<AttemptPlan>,
    metadata: RequestMetadata,
    operation: Operation,
    overall_timeout: Duration,
    media_spool: Arc<dyn MediaSpool>,
    circuits: &crate::circuit::CircuitBreaker,
) -> Result<ExecutionSuccess, ExecutionFailure> {
    let deadline = tokio::time::Instant::now() + overall_timeout;
    let mut last_error = None;
    let mut traces = Vec::with_capacity(attempts.len());
    for attempt in attempts {
        if !circuits.try_acquire(attempt.target_id) {
            continue;
        }
        let ordinal = u16::try_from(traces.len() + 1).unwrap_or(u16::MAX);
        let attempt_started_at = Utc::now();
        let attempt_started = tokio::time::Instant::now();
        let attempt_deadline = deadline.min(attempt_started + attempt.timeout.as_duration());
        let Some(transport) = runtime.transport(attempt.provider_id) else {
            let error = TransportError {
                phase: olp_domain::TransportPhase::Connect,
                class: AttemptFailureClass::Connect,
                response_committed: false,
                message: "provider transport is not loaded".to_owned(),
            };
            traces.push(failed_attempt(
                &attempt,
                ordinal,
                attempt_started_at,
                attempt_started,
                &error,
            ));
            circuits.record_failure(attempt.target_id, error.class);
            last_error = Some(error);
            continue;
        };
        let remaining = attempt_deadline.saturating_duration_since(tokio::time::Instant::now());
        if remaining.is_zero() {
            return Err(ExecutionFailure {
                error: InferenceError::timeout(),
                attempts: traces,
            });
        }
        let provider_request = ProviderRequest {
            metadata: metadata.clone(),
            attempt: attempt.clone(),
            operation: operation_for_provider(&operation, attempt.provider_kind),
            media: Some(media_spool.clone()),
        };
        let output =
            match tokio::time::timeout(remaining, transport.execute(provider_request)).await {
                Ok(Ok(events)) => events,
                Ok(Err(error)) if error.allows_failover() => {
                    traces.push(failed_attempt(
                        &attempt,
                        ordinal,
                        attempt_started_at,
                        attempt_started,
                        &error,
                    ));
                    circuits.record_failure(attempt.target_id, error.class);
                    last_error = Some(error);
                    continue;
                }
                Ok(Err(error)) => {
                    traces.push(failed_attempt(
                        &attempt,
                        ordinal,
                        attempt_started_at,
                        attempt_started,
                        &error,
                    ));
                    circuits.record_failure(attempt.target_id, error.class);
                    return Err(ExecutionFailure {
                        error: InferenceError::from_transport(error),
                        attempts: traces,
                    });
                }
                Err(_) => {
                    let ambiguous = operation_timeout_is_ambiguous(operation.kind());
                    let error = TransportError {
                        phase: olp_domain::TransportPhase::FirstByte,
                        class: if ambiguous {
                            AttemptFailureClass::Ambiguous
                        } else {
                            AttemptFailureClass::Timeout
                        },
                        response_committed: ambiguous,
                        message: "route deadline elapsed before provider response".to_owned(),
                    };
                    traces.push(failed_attempt(
                        &attempt,
                        ordinal,
                        attempt_started_at,
                        attempt_started,
                        &error,
                    ));
                    circuits.record_failure(attempt.target_id, error.class);
                    if error.allows_failover() {
                        last_error = Some(error);
                        continue;
                    }
                    return Err(ExecutionFailure {
                        error: InferenceError::from_transport(error),
                        attempts: traces,
                    });
                }
            };
        let mut events = match output {
            ProviderOutput::Events(events) => events,
            ProviderOutput::Result(result) => {
                circuits.record_success(attempt.target_id);
                traces.push(successful_attempt(
                    &attempt,
                    ordinal,
                    attempt_started_at,
                    attempt_started,
                ));
                return Ok(ExecutionSuccess {
                    output: ExecutionOutput::Result(result),
                    deadline: attempt_deadline,
                    attempts: traces,
                    attempt_started,
                });
            }
        };
        let remaining = attempt_deadline.saturating_duration_since(tokio::time::Instant::now());
        let first = match tokio::time::timeout(remaining, events.next()).await {
            Ok(Some(Ok(event))) => event,
            Ok(Some(Err(error))) if error.allows_failover() => {
                traces.push(failed_attempt(
                    &attempt,
                    ordinal,
                    attempt_started_at,
                    attempt_started,
                    &error,
                ));
                circuits.record_failure(attempt.target_id, error.class);
                last_error = Some(error);
                continue;
            }
            Ok(Some(Err(error))) => {
                traces.push(failed_attempt(
                    &attempt,
                    ordinal,
                    attempt_started_at,
                    attempt_started,
                    &error,
                ));
                circuits.record_failure(attempt.target_id, error.class);
                return Err(ExecutionFailure {
                    error: InferenceError::from_transport(error),
                    attempts: traces,
                });
            }
            Ok(None) => {
                let error = TransportError {
                    phase: olp_domain::TransportPhase::FirstByte,
                    class: AttemptFailureClass::Protocol,
                    response_committed: false,
                    message: "the provider returned an empty response".to_owned(),
                };
                traces.push(failed_attempt(
                    &attempt,
                    ordinal,
                    attempt_started_at,
                    attempt_started,
                    &error,
                ));
                circuits.record_failure(attempt.target_id, error.class);
                return Err(ExecutionFailure {
                    error: InferenceError::bad_gateway(
                        "provider_protocol_error",
                        "The provider returned an empty response.",
                    ),
                    attempts: traces,
                });
            }
            Err(_) => {
                let error = TransportError {
                    phase: olp_domain::TransportPhase::FirstByte,
                    class: AttemptFailureClass::Timeout,
                    response_committed: false,
                    message: "route deadline elapsed before a canonical event".to_owned(),
                };
                traces.push(failed_attempt(
                    &attempt,
                    ordinal,
                    attempt_started_at,
                    attempt_started,
                    &error,
                ));
                circuits.record_failure(attempt.target_id, error.class);
                last_error = Some(error);
                continue;
            }
        };
        let mut event_sequence = EventSequenceValidator::new();
        if let Err(sequence_error) = event_sequence.push(&first) {
            let error = canonical_event_protocol_error(sequence_error, false);
            traces.push(failed_attempt(
                &attempt,
                ordinal,
                attempt_started_at,
                attempt_started,
                &error,
            ));
            circuits.record_failure(attempt.target_id, error.class);
            return Err(ExecutionFailure {
                error: InferenceError::from_transport(error),
                attempts: traces,
            });
        }
        let initial_failure = if let CanonicalEventKind::Error { error } = &first.kind {
            if let Some(class) = canonical_error_circuit_class(error.class) {
                circuits.record_failure(attempt.target_id, class);
            }
            true
        } else {
            false
        };
        if matches!(first.kind, CanonicalEventKind::Done) && !initial_failure {
            circuits.record_success(attempt.target_id);
        }
        let events = circuit_accounted_event_stream(
            validated_event_stream(events, event_sequence),
            circuits.clone(),
            attempt.target_id,
            initial_failure,
        );
        traces.push(successful_attempt(
            &attempt,
            ordinal,
            attempt_started_at,
            attempt_started,
        ));
        return Ok(ExecutionSuccess {
            output: ExecutionOutput::Events { first, events },
            deadline: attempt_deadline,
            attempts: traces,
            attempt_started,
        });
    }
    Err(ExecutionFailure {
        error: last_error.map_or_else(
            || InferenceError::unavailable("no_eligible_provider"),
            InferenceError::from_transport,
        ),
        attempts: traces,
    })
}

const fn operation_timeout_is_ambiguous(operation: OperationKind) -> bool {
    matches!(
        operation,
        OperationKind::ImageGeneration
            | OperationKind::ImageEdit
            | OperationKind::ImageVariation
            | OperationKind::Speech
            | OperationKind::Transcription
            | OperationKind::VideoCreate
            | OperationKind::VideoDelete
    )
}

fn successful_attempt(
    attempt: &AttemptPlan,
    ordinal: u16,
    started_at: chrono::DateTime<Utc>,
    started: tokio::time::Instant,
) -> RequestAttemptMetadata {
    RequestAttemptMetadata {
        id: uuid::Uuid::now_v7(),
        ordinal,
        provider_id: attempt.provider_id.as_uuid(),
        upstream_model: attempt.upstream_model.clone(),
        started_at,
        completed_at: Utc::now(),
        status_code: Some(StatusCode::OK.as_u16()),
        error_class: None,
        committed: true,
        latency_ms: elapsed_ms(started.elapsed()),
        first_byte_ms: Some(elapsed_ms(started.elapsed())),
    }
}

fn failed_attempt(
    attempt: &AttemptPlan,
    ordinal: u16,
    started_at: chrono::DateTime<Utc>,
    started: tokio::time::Instant,
    error: &TransportError,
) -> RequestAttemptMetadata {
    let mapped = InferenceError::from_transport(error.clone());
    RequestAttemptMetadata {
        id: uuid::Uuid::now_v7(),
        ordinal,
        provider_id: attempt.provider_id.as_uuid(),
        upstream_model: attempt.upstream_model.clone(),
        started_at,
        completed_at: Utc::now(),
        status_code: Some(mapped.status.as_u16()),
        error_class: Some(attempt_failure_name(error.class).to_owned()),
        committed: error.response_committed,
        latency_ms: elapsed_ms(started.elapsed()),
        first_byte_ms: None,
    }
}

const fn attempt_failure_name(class: AttemptFailureClass) -> &'static str {
    match class {
        AttemptFailureClass::Connect => "connect",
        AttemptFailureClass::Timeout => "timeout",
        AttemptFailureClass::RateLimit => "rate_limit",
        AttemptFailureClass::UpstreamServer => "upstream_server",
        AttemptFailureClass::UpstreamClient => "upstream_client",
        AttemptFailureClass::Protocol => "protocol",
        AttemptFailureClass::Cancelled => "cancelled",
        AttemptFailureClass::Ambiguous => "ambiguous",
    }
}

use std::{sync::Arc, time::Duration};

use axum::http::StatusCode;
use chrono::Utc;
use futures::{StreamExt, stream};
use olp_domain::{
    AttemptFailureClass, AttemptPlan, CanonicalEvent, CanonicalEventKind, CanonicalResult,
    ErrorClass, EventSequenceError, EventSequenceValidator, MediaSpool, Operation, OperationKind,
    ProviderOutput, ProviderRequest, RequestMetadata, TransportError,
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
    initial_failure: bool,
    permit: crate::circuit::CircuitPermit,
) -> EventStream {
    Box::pin(stream::unfold(
        (events, initial_failure, permit),
        move |(mut events, mut failed, mut permit)| async move {
            let item = events.next().await?;
            let item = match item {
                Ok(event) => {
                    match &event.kind {
                        CanonicalEventKind::Error { error } => {
                            if let Some(class) = canonical_error_circuit_class(error.class) {
                                permit.record_failure(class);
                            }
                            failed = true;
                        }
                        CanonicalEventKind::Done if !failed => permit.record_success(),
                        _ => {}
                    }
                    Ok(event)
                }
                Err(mut error) => {
                    // A provider stream has already committed once this wrapper
                    // owns it. Terminal transport failures still affect target
                    // health, but must never trigger request failover.
                    error.response_committed = true;
                    permit.record_failure(error.class);
                    failed = true;
                    Err(error)
                }
            };
            Some((item, (events, failed, permit)))
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

type AttemptStartedObserver<'a> = dyn FnMut(&[RequestAttemptMetadata], &AttemptPlan, u16, chrono::DateTime<Utc>, tokio::time::Instant)
    + Send
    + 'a;

pub(super) struct FailoverContext<'a> {
    pub(super) runtime: &'a crate::RuntimeBundle,
    pub(super) overall_timeout: Duration,
    pub(super) media_spool: Arc<dyn MediaSpool>,
    pub(super) circuits: &'a crate::circuit::CircuitBreaker,
    pub(super) on_attempt_started: Option<&'a mut AttemptStartedObserver<'a>>,
}

pub(super) async fn execute_with_failover(
    context: FailoverContext<'_>,
    attempts: Vec<AttemptPlan>,
    metadata: RequestMetadata,
    operation: Operation,
) -> Result<ExecutionSuccess, ExecutionFailure> {
    let FailoverContext {
        runtime,
        overall_timeout,
        media_spool,
        circuits,
        mut on_attempt_started,
    } = context;
    let deadline = tokio::time::Instant::now() + overall_timeout;
    let mut last_error = None;
    // A retryable canonical provider error, with the trace count at the time
    // it was recorded. When no later attempt runs, the client receives the
    // provider's own error instead of a synthesized transport failure.
    let mut last_canonical_error: Option<(usize, olp_domain::CanonicalError)> = None;
    let mut traces = Vec::with_capacity(attempts.len());
    let attempt_count = attempts.len();
    for (attempt_index, attempt) in attempts.into_iter().enumerate() {
        let Some(mut circuit_permit) = circuits.try_acquire(attempt.target_id) else {
            continue;
        };
        let ordinal = u16::try_from(traces.len() + 1).unwrap_or(u16::MAX);
        let attempt_started_at = Utc::now();
        let attempt_started = tokio::time::Instant::now();
        if let Some(observer) = on_attempt_started.as_mut() {
            observer(
                &traces,
                &attempt,
                ordinal,
                attempt_started_at,
                attempt_started,
            );
        }
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
            circuit_permit.record_failure(error.class);
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
                Ok(Err(error)) => {
                    let error = reclassify_ambiguous_transport_failure(error, operation.kind());
                    traces.push(failed_attempt(
                        &attempt,
                        ordinal,
                        attempt_started_at,
                        attempt_started,
                        &error,
                    ));
                    circuit_permit.record_failure(error.class);
                    if error.allows_failover() {
                        last_error = Some(error);
                        continue;
                    }
                    return Err(ExecutionFailure {
                        error: InferenceError::from_transport(error),
                        attempts: traces,
                    });
                }
                Err(_) => {
                    let error = reclassify_ambiguous_transport_failure(
                        TransportError {
                            phase: olp_domain::TransportPhase::FirstByte,
                            class: AttemptFailureClass::Timeout,
                            response_committed: false,
                            message: "route deadline elapsed before provider response".to_owned(),
                        },
                        operation.kind(),
                    );
                    traces.push(failed_attempt(
                        &attempt,
                        ordinal,
                        attempt_started_at,
                        attempt_started,
                        &error,
                    ));
                    circuit_permit.record_failure(error.class);
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
                let usage = match result.as_ref() {
                    CanonicalResult::Embeddings(result) => result.usage,
                    CanonicalResult::Images(result) => result.usage,
                    _ => None,
                };
                if usage.is_some_and(|usage| !usage.is_valid()) {
                    if let CanonicalResult::Images(images) = result.as_ref() {
                        for image in &images.images {
                            if let olp_domain::MediaSource::Handle(artifact) = &image.source {
                                let _ = media_spool.remove(artifact).await;
                            }
                        }
                    }
                    let error = TransportError {
                        phase: olp_domain::TransportPhase::Body,
                        class: AttemptFailureClass::Protocol,
                        response_committed: false,
                        message: "the provider returned invalid usage".to_owned(),
                    };
                    traces.push(failed_attempt(
                        &attempt,
                        ordinal,
                        attempt_started_at,
                        attempt_started,
                        &error,
                    ));
                    circuit_permit.record_failure(error.class);
                    return Err(ExecutionFailure {
                        error: InferenceError::from_transport(error),
                        attempts: traces,
                    });
                }
                circuit_permit.record_success();
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
            Ok(Some(Err(error))) => {
                let error = reclassify_ambiguous_transport_failure(error, operation.kind());
                traces.push(failed_attempt(
                    &attempt,
                    ordinal,
                    attempt_started_at,
                    attempt_started,
                    &error,
                ));
                circuit_permit.record_failure(error.class);
                if error.allows_failover() {
                    last_error = Some(error);
                    continue;
                }
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
                circuit_permit.record_failure(error.class);
                return Err(ExecutionFailure {
                    error: InferenceError::bad_gateway(
                        "provider_protocol_error",
                        "The provider returned an empty response.",
                    ),
                    attempts: traces,
                });
            }
            Err(_) => {
                let error = reclassify_ambiguous_transport_failure(
                    TransportError {
                        phase: olp_domain::TransportPhase::FirstByte,
                        class: AttemptFailureClass::Timeout,
                        response_committed: false,
                        message: "route deadline elapsed before a canonical event".to_owned(),
                    },
                    operation.kind(),
                );
                traces.push(failed_attempt(
                    &attempt,
                    ordinal,
                    attempt_started_at,
                    attempt_started,
                    &error,
                ));
                circuit_permit.record_failure(error.class);
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
            circuit_permit.record_failure(error.class);
            return Err(ExecutionFailure {
                error: InferenceError::from_transport(error),
                attempts: traces,
            });
        }
        let initial_failure = if let CanonicalEventKind::Error { error } = &first.kind {
            if error.retryable
                && let Some(class) = canonical_error_circuit_class(error.class)
            {
                let transport_error = reclassify_ambiguous_transport_failure(
                    TransportError {
                        phase: olp_domain::TransportPhase::FirstByte,
                        class,
                        response_committed: false,
                        message: error.message.clone(),
                    },
                    operation.kind(),
                );
                let allows_failover = transport_error.allows_failover();
                if !allows_failover || attempt_index + 1 < attempt_count {
                    traces.push(failed_attempt(
                        &attempt,
                        ordinal,
                        attempt_started_at,
                        attempt_started,
                        &transport_error,
                    ));
                    circuit_permit.record_failure(transport_error.class);
                    if !allows_failover {
                        return Err(ExecutionFailure {
                            error: InferenceError::from_transport(transport_error),
                            attempts: traces,
                        });
                    }
                    last_error = Some(transport_error);
                    last_canonical_error = Some((traces.len(), error.clone()));
                    continue;
                }
            }
            if let Some(class) = canonical_error_circuit_class(error.class) {
                circuit_permit.record_failure(class);
            }
            true
        } else {
            false
        };
        if matches!(first.kind, CanonicalEventKind::Done) && !initial_failure {
            circuit_permit.record_success();
        }
        let events = circuit_accounted_event_stream(
            validated_event_stream(events, event_sequence),
            initial_failure,
            circuit_permit,
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
        error: match last_canonical_error {
            Some((failed_at, canonical)) if failed_at == traces.len() => {
                InferenceError::from_canonical(&canonical)
            }
            _ => last_error.map_or_else(
                || InferenceError::unavailable("no_eligible_provider"),
                InferenceError::from_transport,
            ),
        },
        attempts: traces,
    })
}

/// A transport failure after the request may have reached the provider is
/// ambiguous for side-effecting operations: the upstream may have executed
/// (and billed) the work, so failing over could duplicate it. Failures during
/// the connection phase remain retryable.
pub(super) fn reclassify_ambiguous_transport_failure(
    mut error: TransportError,
    operation: OperationKind,
) -> TransportError {
    if operation_is_side_effecting(operation)
        && matches!(
            error.class,
            AttemptFailureClass::Connect
                | AttemptFailureClass::Timeout
                | AttemptFailureClass::UpstreamServer
        )
        && !matches!(error.phase, olp_domain::TransportPhase::Connect)
    {
        error.class = AttemptFailureClass::Ambiguous;
        error.response_committed = true;
    }
    error
}

const fn operation_is_side_effecting(operation: OperationKind) -> bool {
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

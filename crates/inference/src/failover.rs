use std::{sync::Arc, time::Duration};

use chrono::Utc;
use futures::{StreamExt, stream};
use olp_domain::{
    AttemptFailureClass, AttemptPlan, CanonicalEvent, CanonicalEventKind, CanonicalResult,
    ErrorClass, EventSequenceError, EventSequenceValidator, MediaSpool, Operation, OperationKind,
    ProviderOutput, ProviderRequest, RequestMetadata, TransportError,
};
use olp_storage::request_metadata::{RequestAttemptMetadata, RequestAttemptUsageMetadata};

use crate::{
    InferenceError,
    circuit::{CircuitBreaker, CircuitPermit},
    runtime::RuntimeBundle,
    selection::operation_for_provider,
    telemetry::{elapsed_ms, metadata_status_code},
};

pub type EventStream = olp_domain::ProviderEventStream;

fn canonical_event_protocol_error(
    error: EventSequenceError,
    response_committed: bool,
) -> TransportError {
    TransportError {
        phase: olp_domain::TransportPhase::Body,
        class: AttemptFailureClass::Protocol,
        response_committed,
        retry_after: None,
        message: format!("invalid canonical event stream: {error}"),
    }
}

pub fn validated_event_stream(
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

pub fn circuit_accounted_event_stream(
    events: EventStream,
    circuits: CircuitBreaker,
    permit: CircuitPermit,
    initial_failure: bool,
) -> EventStream {
    Box::pin(stream::unfold(
        (events, circuits, permit, initial_failure),
        move |(mut events, circuits, permit, mut failed)| async move {
            let item = events.next().await?;
            let item = match item {
                Ok(event) => {
                    match &event.kind {
                        CanonicalEventKind::Error { error } => {
                            if let Some(class) = canonical_error_circuit_class(error.class) {
                                circuits.record_failure(&permit, class, None).await;
                            }
                            failed = true;
                        }
                        CanonicalEventKind::Done if !failed => {
                            circuits.record_success(&permit).await;
                        }
                        _ => {}
                    }
                    Ok(event)
                }
                Err(mut error) => {
                    // A provider stream has already committed once this wrapper
                    // owns it. Terminal transport failures still affect target
                    // health, but must never trigger request failover.
                    error.response_committed = true;
                    circuits
                        .record_failure(&permit, error.class, error.retry_after)
                        .await;
                    failed = true;
                    Err(error)
                }
            };
            Some((item, (events, circuits, permit, failed)))
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

pub struct ExecutionSuccess {
    pub output: ExecutionOutput,
    pub deadline: tokio::time::Instant,
    pub attempts: Vec<RequestAttemptMetadata>,
    pub attempt_started: tokio::time::Instant,
}

pub enum ExecutionOutput {
    Events {
        first: CanonicalEvent,
        events: EventStream,
    },
    Result(Box<CanonicalResult>),
}

pub struct ExecutionFailure {
    pub error: InferenceError,
    pub attempts: Vec<RequestAttemptMetadata>,
}

enum ProviderAttemptFailure {
    Classify(TransportError),
    Terminal {
        transport: TransportError,
        gateway: InferenceError,
    },
}

enum ProviderAttemptOutcome {
    Retryable(TransportError),
    Terminal(ExecutionFailure),
}

struct AttemptRecord<'a> {
    plan: &'a AttemptPlan,
    ordinal: u16,
    started_at: chrono::DateTime<Utc>,
    started: tokio::time::Instant,
    circuits: &'a CircuitBreaker,
    permit: &'a CircuitPermit,
}

impl AttemptRecord<'_> {
    async fn record_failure(
        &self,
        attempts: &mut AttemptLog<'_>,
        failure: ProviderAttemptFailure,
    ) -> ProviderAttemptOutcome {
        let (transport, terminal) = match failure {
            ProviderAttemptFailure::Classify(transport) => (transport, None),
            ProviderAttemptFailure::Terminal { transport, gateway } => (transport, Some(gateway)),
        };
        attempts.complete(failed_attempt(
            self.plan,
            self.ordinal,
            self.started_at,
            self.started,
            &transport,
        ));
        self.circuits
            .record_failure(self.permit, transport.class, transport.retry_after)
            .await;
        if terminal.is_none() && transport.allows_failover() {
            ProviderAttemptOutcome::Retryable(transport)
        } else {
            ProviderAttemptOutcome::Terminal(ExecutionFailure {
                error: terminal.unwrap_or_else(|| InferenceError::from_transport(transport)),
                attempts: attempts.take(),
            })
        }
    }

    async fn record_success(&self, attempts: &mut AttemptLog<'_>) {
        attempts.complete(successful_attempt(
            self.plan,
            self.ordinal,
            self.started_at,
            self.started,
        ));
        self.circuits.record_success(self.permit).await;
    }
}

pub trait AttemptLifecycleObserver: Send {
    fn on_attempt_started(
        &mut self,
        completed: &[RequestAttemptMetadata],
        attempt: &AttemptPlan,
        ordinal: u16,
        started_at: chrono::DateTime<Utc>,
        started: tokio::time::Instant,
    );

    fn on_attempt_completed(&mut self, attempt: &RequestAttemptMetadata);
}

struct AttemptLog<'a> {
    traces: Vec<RequestAttemptMetadata>,
    observer: Option<&'a mut dyn AttemptLifecycleObserver>,
}

impl AttemptLog<'_> {
    fn start(
        &mut self,
        attempt: &AttemptPlan,
        ordinal: u16,
        started_at: chrono::DateTime<Utc>,
        started: tokio::time::Instant,
    ) {
        if let Some(observer) = self.observer.as_deref_mut() {
            observer.on_attempt_started(&self.traces, attempt, ordinal, started_at, started);
        }
    }

    fn complete(&mut self, attempt: RequestAttemptMetadata) {
        self.traces.push(attempt);
        if let Some(observer) = self.observer.as_deref_mut() {
            observer.on_attempt_completed(self.traces.last().expect("attempt was just added"));
        }
    }

    fn take(&mut self) -> Vec<RequestAttemptMetadata> {
        std::mem::take(&mut self.traces)
    }
}

pub struct FailoverContext<'a> {
    pub runtime: &'a RuntimeBundle,
    pub overall_timeout: Duration,
    pub media_spool: Arc<dyn MediaSpool>,
    pub circuits: &'a CircuitBreaker,
    pub attempt_observer: Option<&'a mut dyn AttemptLifecycleObserver>,
}

pub async fn execute_with_failover(
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
        attempt_observer,
    } = context;
    let deadline = tokio::time::Instant::now() + overall_timeout;
    let mut last_error = None;
    // A retryable canonical provider error, with the trace count at the time
    // it was recorded. When no later attempt runs, the client receives the
    // provider's own error instead of a synthesized transport failure.
    let mut last_canonical_error: Option<(usize, olp_domain::CanonicalError)> = None;
    let attempt_count = attempts.len();
    let mut attempt_log = AttemptLog {
        traces: Vec::with_capacity(attempt_count),
        observer: attempt_observer,
    };
    for (attempt_index, attempt) in attempts.into_iter().enumerate() {
        let Some(permit) = circuits.acquire(attempt.target_routing_id).await else {
            continue;
        };
        let ordinal = u16::try_from(attempt_log.traces.len() + 1).unwrap_or(u16::MAX);
        let attempt_started_at = Utc::now();
        let attempt_started = tokio::time::Instant::now();
        attempt_log.start(&attempt, ordinal, attempt_started_at, attempt_started);
        let record = AttemptRecord {
            plan: &attempt,
            ordinal,
            started_at: attempt_started_at,
            started: attempt_started,
            circuits,
            permit: &permit,
        };
        let attempt_deadline = deadline.min(attempt_started + attempt.timeout.as_duration());
        let Some(transport) = runtime.transport(attempt.provider_id) else {
            let error = TransportError {
                phase: olp_domain::TransportPhase::Connect,
                class: AttemptFailureClass::Connect,
                response_committed: false,
                retry_after: None,
                message: "provider transport is not loaded".to_owned(),
            };
            match record
                .record_failure(&mut attempt_log, ProviderAttemptFailure::Classify(error))
                .await
            {
                ProviderAttemptOutcome::Retryable(error) => last_error = Some(error),
                ProviderAttemptOutcome::Terminal(failure) => return Err(failure),
            }
            continue;
        };
        let remaining = attempt_deadline.saturating_duration_since(tokio::time::Instant::now());
        if remaining.is_zero() {
            return Err(ExecutionFailure {
                error: InferenceError::timeout(),
                attempts: attempt_log.take(),
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
                    match record
                        .record_failure(&mut attempt_log, ProviderAttemptFailure::Classify(error))
                        .await
                    {
                        ProviderAttemptOutcome::Retryable(error) => {
                            last_error = Some(error);
                            continue;
                        }
                        ProviderAttemptOutcome::Terminal(failure) => return Err(failure),
                    }
                }
                Err(_) => {
                    let error = reclassify_ambiguous_transport_failure(
                        TransportError {
                            phase: olp_domain::TransportPhase::FirstByte,
                            class: AttemptFailureClass::Timeout,
                            response_committed: false,
                            retry_after: None,
                            message: "route deadline elapsed before provider response".to_owned(),
                        },
                        operation.kind(),
                    );
                    match record
                        .record_failure(&mut attempt_log, ProviderAttemptFailure::Classify(error))
                        .await
                    {
                        ProviderAttemptOutcome::Retryable(error) => {
                            last_error = Some(error);
                            continue;
                        }
                        ProviderAttemptOutcome::Terminal(failure) => return Err(failure),
                    }
                }
            };
        let mut events = match output {
            ProviderOutput::Events(events) => events,
            ProviderOutput::Result(result) => {
                record.record_success(&mut attempt_log).await;
                return Ok(ExecutionSuccess {
                    output: ExecutionOutput::Result(result),
                    deadline: attempt_deadline,
                    attempts: attempt_log.take(),
                    attempt_started,
                });
            }
        };
        let remaining = attempt_deadline.saturating_duration_since(tokio::time::Instant::now());
        let first = match tokio::time::timeout(remaining, events.next()).await {
            Ok(Some(Ok(event))) => event,
            Ok(Some(Err(error))) => {
                let error = reclassify_ambiguous_transport_failure(error, operation.kind());
                match record
                    .record_failure(&mut attempt_log, ProviderAttemptFailure::Classify(error))
                    .await
                {
                    ProviderAttemptOutcome::Retryable(error) => {
                        last_error = Some(error);
                        continue;
                    }
                    ProviderAttemptOutcome::Terminal(failure) => return Err(failure),
                }
            }
            Ok(None) => {
                let error = TransportError {
                    phase: olp_domain::TransportPhase::FirstByte,
                    class: AttemptFailureClass::Protocol,
                    response_committed: false,
                    retry_after: None,
                    message: "the provider returned an empty response".to_owned(),
                };
                let gateway = InferenceError::bad_gateway(
                    "provider_protocol_error",
                    "The provider returned an empty response.",
                );
                match record
                    .record_failure(
                        &mut attempt_log,
                        ProviderAttemptFailure::Terminal {
                            transport: error,
                            gateway,
                        },
                    )
                    .await
                {
                    ProviderAttemptOutcome::Terminal(failure) => return Err(failure),
                    ProviderAttemptOutcome::Retryable(_) => unreachable!("terminal failure"),
                }
            }
            Err(_) => {
                let error = reclassify_ambiguous_transport_failure(
                    TransportError {
                        phase: olp_domain::TransportPhase::FirstByte,
                        class: AttemptFailureClass::Timeout,
                        response_committed: false,
                        retry_after: None,
                        message: "route deadline elapsed before a canonical event".to_owned(),
                    },
                    operation.kind(),
                );
                match record
                    .record_failure(&mut attempt_log, ProviderAttemptFailure::Classify(error))
                    .await
                {
                    ProviderAttemptOutcome::Retryable(error) => {
                        last_error = Some(error);
                        continue;
                    }
                    ProviderAttemptOutcome::Terminal(failure) => return Err(failure),
                }
            }
        };
        let mut event_sequence = EventSequenceValidator::new();
        if let Err(sequence_error) = event_sequence.push(&first) {
            let error = canonical_event_protocol_error(sequence_error, false);
            let gateway = InferenceError::from_transport(error.clone());
            match record
                .record_failure(
                    &mut attempt_log,
                    ProviderAttemptFailure::Terminal {
                        transport: error,
                        gateway,
                    },
                )
                .await
            {
                ProviderAttemptOutcome::Terminal(failure) => return Err(failure),
                ProviderAttemptOutcome::Retryable(_) => unreachable!("terminal failure"),
            }
        }
        let (initial_failure, initial_failure_class) =
            if let CanonicalEventKind::Error { error } = &first.kind {
                if error.retryable
                    && attempt_index + 1 < attempt_count
                    && let Some(class) = canonical_error_circuit_class(error.class)
                {
                    let transport_error = TransportError {
                        phase: olp_domain::TransportPhase::FirstByte,
                        class,
                        response_committed: false,
                        retry_after: None,
                        message: error.message.clone(),
                    };
                    match record
                        .record_failure(
                            &mut attempt_log,
                            ProviderAttemptFailure::Classify(transport_error),
                        )
                        .await
                    {
                        ProviderAttemptOutcome::Retryable(error) => last_error = Some(error),
                        ProviderAttemptOutcome::Terminal(_) => {
                            unreachable!("canonical retryable error permits failover")
                        }
                    }
                    last_canonical_error = Some((attempt_log.traces.len(), error.clone()));
                    continue;
                }
                (true, canonical_error_circuit_class(error.class))
            } else {
                (false, None)
            };
        attempt_log.complete(successful_attempt(
            &attempt,
            ordinal,
            attempt_started_at,
            attempt_started,
        ));
        if let Some(class) = initial_failure_class {
            circuits.record_failure(&permit, class, None).await;
        } else if matches!(first.kind, CanonicalEventKind::Done) {
            circuits.record_success(&permit).await;
        }
        let events = circuit_accounted_event_stream(
            validated_event_stream(events, event_sequence),
            circuits.clone(),
            permit,
            initial_failure,
        );
        return Ok(ExecutionSuccess {
            output: ExecutionOutput::Events { first, events },
            deadline: attempt_deadline,
            attempts: attempt_log.take(),
            attempt_started,
        });
    }
    Err(ExecutionFailure {
        error: match last_canonical_error {
            Some((failed_at, canonical)) if failed_at == attempt_log.traces.len() => {
                InferenceError::from_canonical(&canonical)
            }
            _ => last_error.map_or_else(
                || InferenceError::unavailable("no_eligible_provider"),
                InferenceError::from_transport,
            ),
        },
        attempts: attempt_log.take(),
    })
}

/// A transport failure after the request may have reached the provider is
/// ambiguous for side-effecting operations: the upstream may have executed
/// (and billed) the work, so failing over could duplicate it. Failures during
/// the connection phase remain retryable.
pub fn reclassify_ambiguous_transport_failure(
    mut error: TransportError,
    operation: OperationKind,
) -> TransportError {
    if operation_is_side_effecting(operation)
        && matches!(
            error.class,
            AttemptFailureClass::Connect | AttemptFailureClass::Timeout
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
        status_code: Some(200),
        error_class: None,
        committed: true,
        latency_ms: elapsed_ms(started.elapsed()),
        first_byte_ms: Some(elapsed_ms(started.elapsed())),
        usage: Some(RequestAttemptUsageMetadata {
            observed: false,
            complete: false,
            billing_uncertain: true,
            input_tokens: None,
            output_tokens: None,
            cached_input_tokens: None,
            media_units: None,
        }),
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
        status_code: Some(metadata_status_code(&mapped)),
        error_class: Some(attempt_failure_name(error.class).to_owned()),
        committed: error.response_committed,
        latency_ms: elapsed_ms(started.elapsed()),
        first_byte_ms: None,
        usage: Some(RequestAttemptUsageMetadata {
            observed: false,
            complete: !attempt_billing_is_uncertain(error),
            billing_uncertain: attempt_billing_is_uncertain(error),
            input_tokens: None,
            output_tokens: None,
            cached_input_tokens: None,
            media_units: None,
        }),
    }
}

const fn attempt_billing_is_uncertain(error: &TransportError) -> bool {
    error.response_committed
        || matches!(error.class, AttemptFailureClass::Ambiguous)
        || (matches!(
            error.class,
            AttemptFailureClass::Timeout
                | AttemptFailureClass::UpstreamServer
                | AttemptFailureClass::Protocol
                | AttemptFailureClass::Cancelled
        ) && !matches!(error.phase, olp_domain::TransportPhase::Connect))
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

#[cfg(test)]
mod tests {
    use olp_domain::{AttemptFailureClass, TransportError, TransportPhase};

    use super::attempt_billing_is_uncertain;

    #[test]
    fn billing_uncertainty_starts_after_a_request_may_reach_the_provider() {
        assert!(!attempt_billing_is_uncertain(&failure(
            TransportPhase::Connect,
            AttemptFailureClass::Connect,
        )));
        assert!(!attempt_billing_is_uncertain(&failure(
            TransportPhase::FirstByte,
            AttemptFailureClass::RateLimit,
        )));
        assert!(attempt_billing_is_uncertain(&failure(
            TransportPhase::FirstByte,
            AttemptFailureClass::UpstreamServer,
        )));
        assert!(attempt_billing_is_uncertain(&failure(
            TransportPhase::Body,
            AttemptFailureClass::Protocol,
        )));
        assert!(attempt_billing_is_uncertain(&failure(
            TransportPhase::FirstByte,
            AttemptFailureClass::Timeout,
        )));
    }

    fn failure(phase: TransportPhase, class: AttemptFailureClass) -> TransportError {
        TransportError {
            phase,
            class,
            response_committed: false,
            retry_after: None,
            message: "metadata-free fixture".to_owned(),
        }
    }
}

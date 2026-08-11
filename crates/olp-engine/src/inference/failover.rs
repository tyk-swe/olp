use std::{sync::Arc, time::Duration};

use crate::domain::{
    AttemptFailureClass, AttemptPlan, CanonicalError, CanonicalEvent, CanonicalEventKind,
    CanonicalResult, ErrorClass, EventSequenceError, EventSequenceValidator, MediaSpool, Operation,
    OperationKind, ProviderOutput, ProviderRequest, RequestMetadata, TargetId, TransportError,
};
use crate::inference::{
    InferenceError,
    circuit::{CircuitBreaker, CircuitPermit},
    request_metadata::{RequestAttemptMetadata, RequestAttemptUsageMetadata},
    runtime::RuntimeBundle,
    selection::operation_for_provider,
    telemetry::{elapsed_ms, metadata_status_code},
};
use chrono::Utc;
use futures::{StreamExt, stream};

pub type EventStream = crate::domain::ProviderEventStream;

fn canonical_event_protocol_error(
    error: EventSequenceError,
    response_committed: bool,
) -> TransportError {
    TransportError {
        phase: crate::domain::TransportPhase::Body,
        class: AttemptFailureClass::Protocol,
        response_committed,
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

struct AttemptRecord<'a> {
    plan: &'a AttemptPlan,
    circuit_permit: CircuitPermit,
    ordinal: u16,
    started_at: chrono::DateTime<Utc>,
    started: tokio::time::Instant,
}

impl AttemptRecord<'_> {
    fn finish_failure(
        &self,
        traces: &mut Vec<RequestAttemptMetadata>,
        circuits: &CircuitBreaker,
        transport: TransportError,
        terminal: Option<InferenceError>,
    ) -> Result<TransportError, ExecutionFailure> {
        traces.push(failed_attempt(
            self.plan,
            self.ordinal,
            self.started_at,
            self.started,
            &transport,
        ));
        circuits.record_failure(self.plan.target_id, transport.class);
        if terminal.is_none() && transport.allows_failover() {
            Ok(transport)
        } else {
            Err(ExecutionFailure {
                error: terminal.unwrap_or_else(|| InferenceError::from_transport(transport)),
                attempts: std::mem::take(traces),
            })
        }
    }

    fn record_failure(
        &self,
        traces: &mut Vec<RequestAttemptMetadata>,
        circuits: &CircuitBreaker,
        transport: TransportError,
    ) -> Result<TransportError, ExecutionFailure> {
        self.finish_failure(traces, circuits, transport, None)
    }

    fn record_terminal_failure(
        &self,
        traces: &mut Vec<RequestAttemptMetadata>,
        circuits: &CircuitBreaker,
        transport: TransportError,
        gateway: InferenceError,
    ) -> ExecutionFailure {
        self.finish_failure(traces, circuits, transport, Some(gateway))
            .expect_err("an explicit gateway failure is terminal")
    }

    fn record_success(&self, traces: &mut Vec<RequestAttemptMetadata>, circuits: &CircuitBreaker) {
        circuits.record_success(self.plan.target_id);
        self.record_accepted_output(traces);
    }

    fn record_accepted_output(&self, traces: &mut Vec<RequestAttemptMetadata>) {
        traces.push(successful_attempt(
            self.plan,
            self.ordinal,
            self.started_at,
            self.started,
        ));
    }

    fn record_deadline_elapsed(
        &self,
        traces: &mut Vec<RequestAttemptMetadata>,
        circuits: &CircuitBreaker,
    ) -> ExecutionFailure {
        let timeout = TransportError {
            phase: crate::domain::TransportPhase::Connect,
            class: AttemptFailureClass::Timeout,
            response_committed: false,
            message: "route deadline elapsed before provider execution".to_owned(),
        };
        traces.push(failed_attempt(
            self.plan,
            self.ordinal,
            self.started_at,
            self.started,
            &timeout,
        ));
        circuits.abandon_probe(self.plan.target_id, self.circuit_permit);
        ExecutionFailure {
            error: InferenceError::timeout(),
            attempts: std::mem::take(traces),
        }
    }
}

pub type AttemptStartedObserver<'a> = dyn FnMut(&[RequestAttemptMetadata], &AttemptPlan, u16, chrono::DateTime<Utc>, tokio::time::Instant)
    + Send
    + 'a;

pub struct FailoverContext<'a> {
    pub runtime: &'a RuntimeBundle,
    pub overall_timeout: Duration,
    pub media_spool: Arc<dyn MediaSpool>,
    pub circuits: &'a CircuitBreaker,
    pub on_attempt_started: Option<&'a mut AttemptStartedObserver<'a>>,
}

/// Failure state that crosses attempt boundaries.
///
/// Retryable canonical errors are retained only while no later attempt has
/// actually run. This preserves the provider error when every remaining
/// target is circuit-open without allowing it to mask a later transport
/// failure.
#[derive(Default)]
struct FailureHistory {
    last_transport: Option<TransportError>,
    last_canonical: Option<(usize, CanonicalError)>,
}

impl FailureHistory {
    fn record_retry(
        &mut self,
        transport: TransportError,
        canonical: Option<CanonicalError>,
        completed_attempts: usize,
    ) {
        self.last_transport = Some(transport);
        if let Some(canonical) = canonical {
            self.last_canonical = Some((completed_attempts, canonical));
        }
    }

    fn into_error(self, completed_attempts: usize) -> InferenceError {
        match self.last_canonical {
            Some((failed_at, canonical)) if failed_at == completed_attempts => {
                InferenceError::from_canonical(&canonical)
            }
            _ => self.last_transport.map_or_else(
                || InferenceError::unavailable("no_eligible_provider"),
                InferenceError::from_transport,
            ),
        }
    }
}

struct AttemptExecutionContext<'a> {
    runtime: &'a RuntimeBundle,
    media_spool: &'a Arc<dyn MediaSpool>,
    circuits: &'a CircuitBreaker,
    metadata: &'a RequestMetadata,
    operation: &'a Operation,
    route_deadline: tokio::time::Instant,
    can_retry_canonical: bool,
}

enum AttemptDisposition {
    Retry {
        transport: TransportError,
        canonical: Option<CanonicalError>,
    },
    Success {
        output: ExecutionOutput,
        deadline: tokio::time::Instant,
    },
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
        mut on_attempt_started,
    } = context;
    let route_deadline = tokio::time::Instant::now() + overall_timeout;
    let mut failures = FailureHistory::default();
    let mut traces = Vec::with_capacity(attempts.len());
    let attempt_count = attempts.len();
    for (attempt_index, attempt) in attempts.into_iter().enumerate() {
        let Some(circuit_permit) = circuits.try_acquire_permit(attempt.target_id) else {
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
        let record = AttemptRecord {
            plan: &attempt,
            circuit_permit,
            ordinal,
            started_at: attempt_started_at,
            started: attempt_started,
        };
        match execute_attempt(
            AttemptExecutionContext {
                runtime,
                media_spool: &media_spool,
                circuits,
                metadata: &metadata,
                operation: &operation,
                route_deadline,
                can_retry_canonical: attempt_index + 1 < attempt_count,
            },
            &record,
            &mut traces,
        )
        .await?
        {
            AttemptDisposition::Retry {
                transport,
                canonical,
            } => failures.record_retry(transport, canonical, traces.len()),
            AttemptDisposition::Success { output, deadline } => {
                return Ok(ExecutionSuccess {
                    output,
                    deadline,
                    attempts: traces,
                    attempt_started,
                });
            }
        }
    }
    Err(ExecutionFailure {
        error: failures.into_error(traces.len()),
        attempts: traces,
    })
}

async fn execute_attempt(
    context: AttemptExecutionContext<'_>,
    record: &AttemptRecord<'_>,
    traces: &mut Vec<RequestAttemptMetadata>,
) -> Result<AttemptDisposition, ExecutionFailure> {
    let AttemptExecutionContext {
        runtime,
        media_spool,
        circuits,
        metadata,
        operation,
        route_deadline,
        can_retry_canonical,
    } = context;
    let attempt = record.plan;
    let attempt_deadline = route_deadline.min(record.started + attempt.timeout.as_duration());
    let Some(transport) = runtime.transport(attempt.provider_id) else {
        let error = TransportError {
            phase: crate::domain::TransportPhase::Connect,
            class: AttemptFailureClass::Connect,
            response_committed: false,
            message: "provider transport is not loaded".to_owned(),
        };
        return Ok(AttemptDisposition::Retry {
            transport: record.record_failure(traces, circuits, error)?,
            canonical: None,
        });
    };
    let remaining = attempt_deadline.saturating_duration_since(tokio::time::Instant::now());
    if remaining.is_zero() {
        return Err(record.record_deadline_elapsed(traces, circuits));
    }
    let provider_request = ProviderRequest {
        metadata: metadata.clone(),
        attempt: attempt.clone(),
        operation: operation_for_provider(operation, attempt.provider_kind),
        media: Some(Arc::clone(media_spool)),
    };
    let output = match tokio::time::timeout(remaining, transport.execute(provider_request)).await {
        Ok(Ok(output)) => output,
        Ok(Err(error)) => {
            let error = reclassify_ambiguous_transport_failure(error, operation.kind());
            return Ok(AttemptDisposition::Retry {
                transport: record.record_failure(traces, circuits, error)?,
                canonical: None,
            });
        }
        Err(_) => {
            let error = reclassify_ambiguous_transport_failure(
                TransportError {
                    phase: crate::domain::TransportPhase::FirstByte,
                    class: AttemptFailureClass::Timeout,
                    response_committed: false,
                    message: "route deadline elapsed before provider response".to_owned(),
                },
                operation.kind(),
            );
            return Ok(AttemptDisposition::Retry {
                transport: record.record_failure(traces, circuits, error)?,
                canonical: None,
            });
        }
    };
    let mut events = match output {
        ProviderOutput::Events(events) => events,
        ProviderOutput::Result(result) => {
            record.record_success(traces, circuits);
            return Ok(AttemptDisposition::Success {
                output: ExecutionOutput::Result(result),
                deadline: attempt_deadline,
            });
        }
    };
    let remaining = attempt_deadline.saturating_duration_since(tokio::time::Instant::now());
    let first = match tokio::time::timeout(remaining, events.next()).await {
        Ok(Some(Ok(event))) => event,
        Ok(Some(Err(error))) => {
            let error = reclassify_ambiguous_transport_failure(error, operation.kind());
            return Ok(AttemptDisposition::Retry {
                transport: record.record_failure(traces, circuits, error)?,
                canonical: None,
            });
        }
        Ok(None) => {
            let error = TransportError {
                phase: crate::domain::TransportPhase::FirstByte,
                class: AttemptFailureClass::Protocol,
                response_committed: false,
                message: "the provider returned an empty response".to_owned(),
            };
            let gateway = InferenceError::bad_gateway(
                "provider_protocol_error",
                "The provider returned an empty response.",
            );
            return Err(record.record_terminal_failure(traces, circuits, error, gateway));
        }
        Err(_) => {
            let error = reclassify_ambiguous_transport_failure(
                TransportError {
                    phase: crate::domain::TransportPhase::FirstByte,
                    class: AttemptFailureClass::Timeout,
                    response_committed: false,
                    message: "route deadline elapsed before a canonical event".to_owned(),
                },
                operation.kind(),
            );
            return Ok(AttemptDisposition::Retry {
                transport: record.record_failure(traces, circuits, error)?,
                canonical: None,
            });
        }
    };
    let mut event_sequence = EventSequenceValidator::new();
    if let Err(sequence_error) = event_sequence.push(&first) {
        let error = canonical_event_protocol_error(sequence_error, false);
        let gateway = InferenceError::from_transport(error.clone());
        return Err(record.record_terminal_failure(traces, circuits, error, gateway));
    }
    let initial_failure = if let CanonicalEventKind::Error { error } = &first.kind {
        if error.retryable
            && can_retry_canonical
            && let Some(class) = canonical_error_circuit_class(error.class)
        {
            let transport_error = TransportError {
                phase: crate::domain::TransportPhase::FirstByte,
                class,
                response_committed: false,
                message: error.message.clone(),
            };
            return Ok(AttemptDisposition::Retry {
                transport: record.record_failure(traces, circuits, transport_error)?,
                canonical: Some(error.clone()),
            });
        }
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
    record.record_accepted_output(traces);
    Ok(AttemptDisposition::Success {
        output: ExecutionOutput::Events { first, events },
        deadline: attempt_deadline,
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
        && !matches!(error.phase, crate::domain::TransportPhase::Connect)
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
        ) && !matches!(error.phase, crate::domain::TransportPhase::Connect))
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
    use crate::domain::{
        AttemptFailureClass, AttemptPlan, CanonicalError, DurationMs, ErrorClass, ProviderId,
        ProviderKind, RouteId, RuntimeGenerationId, TargetId, TransportError, TransportPhase,
    };
    use chrono::Utc;

    use super::{AttemptRecord, FailureHistory, attempt_billing_is_uncertain};
    use crate::inference::{InferenceErrorKind, circuit::CircuitBreaker};

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

    #[test]
    fn elapsed_deadline_records_attempt_without_penalizing_closed_circuit() {
        let target_id = TargetId::new();
        let attempt = AttemptPlan {
            generation_id: RuntimeGenerationId::new(),
            route_id: RouteId::new(),
            target_id,
            provider_id: ProviderId::new(),
            provider_kind: ProviderKind::OpenAi,
            upstream_model: "deadline-test".to_owned(),
            timeout: DurationMs::new(1_000),
            priority: 0,
        };
        let circuits = CircuitBreaker::default();
        let record = AttemptRecord {
            plan: &attempt,
            circuit_permit: circuits
                .try_acquire_permit(target_id)
                .expect("closed circuit admits an attempt"),
            ordinal: 1,
            started_at: Utc::now(),
            started: tokio::time::Instant::now(),
        };

        let mut traces = Vec::new();
        let failure = record.record_deadline_elapsed(&mut traces, &circuits);

        assert_eq!(failure.error.code(), "gateway_timeout");
        assert_eq!(failure.attempts.len(), 1);
        let failed_attempt = &failure.attempts[0];
        assert_eq!(failed_attempt.ordinal, 1);
        assert_eq!(failed_attempt.error_class.as_deref(), Some("timeout"));
        assert_eq!(failed_attempt.status_code, Some(504));
        assert!(!failed_attempt.committed);
        let usage = failed_attempt
            .usage
            .as_ref()
            .expect("timeout attempt records billing certainty");
        assert!(usage.complete);
        assert!(!usage.billing_uncertain);
        assert_eq!(circuits.open_count(), 0);
        for _ in 0..4 {
            circuits.record_failure(target_id, AttemptFailureClass::Connect);
        }
        assert_eq!(circuits.open_count(), 0);
        assert!(circuits.is_selectable(target_id));
        circuits.record_failure(target_id, AttemptFailureClass::Connect);
        assert_eq!(circuits.open_count(), 1);
    }

    #[test]
    fn final_retryable_canonical_error_is_preserved() {
        let mut failures = FailureHistory::default();
        failures.record_retry(
            failure(TransportPhase::FirstByte, AttemptFailureClass::RateLimit),
            Some(CanonicalError {
                class: ErrorClass::RateLimit,
                message: "provider asked the client to retry".to_owned(),
                provider_code: Some("busy".to_owned()),
                retryable: true,
            }),
            1,
        );

        let error = failures.into_error(1);

        assert_eq!(
            error.kind(),
            InferenceErrorKind::Canonical(ErrorClass::RateLimit)
        );
        assert_eq!(error.message(), "provider asked the client to retry");
    }

    #[test]
    fn later_transport_failure_supersedes_a_canonical_error() {
        let mut failures = FailureHistory::default();
        failures.record_retry(
            failure(TransportPhase::FirstByte, AttemptFailureClass::RateLimit),
            Some(CanonicalError {
                class: ErrorClass::RateLimit,
                message: "first failure".to_owned(),
                provider_code: None,
                retryable: true,
            }),
            1,
        );
        failures.record_retry(
            failure(TransportPhase::Connect, AttemptFailureClass::Connect),
            None,
            2,
        );

        let error = failures.into_error(2);

        assert_eq!(error.code(), "upstream_unavailable");
    }

    fn failure(phase: TransportPhase, class: AttemptFailureClass) -> TransportError {
        TransportError {
            phase,
            class,
            response_committed: false,
            message: "metadata-free fixture".to_owned(),
        }
    }
}

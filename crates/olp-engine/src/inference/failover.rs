use std::{num::NonZeroU16, sync::Arc, time::Duration};

use crate::domain::{
    canonical::{
        events::{Error, Event, EventSequenceValidator, Kind},
        identity::{OperationKind, RequestMetadata},
        requests::Operation,
        results::CanonicalResult,
    },
    ports::{AttemptFailureClass, MediaSpool, ProviderOutput, ProviderRequest, TransportError},
    routing::selection::AttemptPlan,
};
use crate::inference::{
    circuit::{Breaker, CircuitPermit},
    error::Error as InferenceError,
    request_metadata::{RequestAttemptMetadata, RequestAttemptUsageMetadata},
    runtime::Bundle,
    selection::operation_for_provider,
    telemetry::{elapsed_ms, metadata_status_code},
};
use chrono::Utc;
use futures::StreamExt;
use tracing::Instrument as _;

use super::tracing::{AttemptTrace, RequestTrace};

mod streams;

#[cfg(any(test, feature = "test-util"))]
pub use streams::circuit_accounted_event_stream;
pub use streams::validated_event_stream;
use streams::{
    canonical_error_circuit_class, canonical_event_protocol_error,
    circuit_accounted_event_stream_with_permit,
};

pub type EventStream = crate::domain::ports::ProviderEventStream;

pub struct ExecutionSuccess {
    pub output: ExecutionOutput,
    pub deadline: tokio::time::Instant,
    pub attempts: Vec<RequestAttemptMetadata>,
    pub attempt_started: tokio::time::Instant,
    pub(in crate::inference) attempt_trace: Option<AttemptTrace>,
}

pub enum ExecutionOutput {
    Events { first: Event, events: EventStream },
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
    trace: Option<AttemptTrace>,
}

impl AttemptRecord<'_> {
    fn finish_failure(
        &mut self,
        traces: &mut Vec<RequestAttemptMetadata>,
        circuits: &Breaker,
        transport: TransportError,
        terminal: Option<InferenceError>,
    ) -> Result<TransportError, ExecutionFailure> {
        if let Some(trace) = self.trace.as_mut() {
            trace.record_transport_failure(&transport);
        }
        traces.push(failed_attempt(
            self.plan,
            self.ordinal,
            self.started_at,
            self.started,
            &transport,
        ));
        circuits.record_failure_for_optional_permit(
            self.plan.routing_id,
            Some(&self.circuit_permit),
            transport.class,
            transport.upstream.retry_after,
        );
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
        &mut self,
        traces: &mut Vec<RequestAttemptMetadata>,
        circuits: &Breaker,
        transport: TransportError,
    ) -> Result<TransportError, ExecutionFailure> {
        self.finish_failure(traces, circuits, transport, None)
    }

    fn record_terminal_failure(
        &mut self,
        traces: &mut Vec<RequestAttemptMetadata>,
        circuits: &Breaker,
        transport: TransportError,
        gateway: InferenceError,
    ) -> ExecutionFailure {
        self.finish_failure(traces, circuits, transport, Some(gateway))
            .expect_err("an explicit gateway failure is terminal")
    }

    fn record_success(&mut self, traces: &mut Vec<RequestAttemptMetadata>, circuits: &Breaker) {
        circuits
            .record_success_for_optional_permit(self.plan.routing_id, Some(&self.circuit_permit));
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
        &mut self,
        traces: &mut Vec<RequestAttemptMetadata>,
        circuits: &Breaker,
    ) -> ExecutionFailure {
        let timeout = TransportError {
            upstream: Default::default(),
            phase: crate::domain::ports::TransportPhase::Connect,
            class: AttemptFailureClass::Timeout,
            response_committed: false,
            message: "route deadline elapsed before provider execution".to_owned(),
        };
        if let Some(trace) = self.trace.as_mut() {
            trace.record_transport_failure(&timeout);
        }
        traces.push(failed_attempt(
            self.plan,
            self.ordinal,
            self.started_at,
            self.started,
            &timeout,
        ));
        circuits.abandon_probe(self.plan.routing_id, self.circuit_permit);
        ExecutionFailure {
            error: InferenceError::timeout(),
            attempts: std::mem::take(traces),
        }
    }

    fn take_trace(&mut self) -> Option<AttemptTrace> {
        self.trace.take()
    }
}

fn notify_attempt_started(
    observer: Option<&mut AttemptStartedObserver<'_>>,
    completed: &[RequestAttemptMetadata],
    record: &AttemptRecord<'_>,
) {
    if let Some(observer) = observer {
        observer(
            completed,
            record.plan,
            record.ordinal,
            record.started_at,
            record.started,
        );
    }
}

pub type AttemptStartedObserver<'a> = dyn FnMut(&[RequestAttemptMetadata], &AttemptPlan, u16, chrono::DateTime<Utc>, tokio::time::Instant)
    + Send
    + 'a;

pub struct Context<'a> {
    pub runtime: &'a Bundle,
    pub overall_timeout: Duration,
    pub max_attempts: NonZeroU16,
    pub media_spool: Arc<dyn MediaSpool>,
    pub max_inline_media_bytes: usize,
    pub circuits: &'a Breaker,
    pub on_attempt_started: Option<&'a mut AttemptStartedObserver<'a>>,
    pub trace: Option<&'a RequestTrace>,
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
    last_canonical: Option<(usize, Error)>,
}

/// First retry waits about this long; each further retry doubles it.
const BASE_RETRY_BACKOFF: Duration = Duration::from_millis(100);
/// Ceiling for the computed backoff.
const MAX_RETRY_BACKOFF: Duration = Duration::from_secs(5);
/// Longest an upstream `Retry-After` may hold the caller's connection,
/// concurrency slot and token reservation before we fall back to our own
/// backoff. Matches the circuit's open window, so a hint never outlives it.
const MAX_RETRY_AFTER_DELAY: Duration = Duration::from_secs(30);

/// Delay before re-attempting after a retryable failure. Exponential with full
/// jitter so a fleet does not resynchronize on a provider blip, floored at any
/// `Retry-After` the provider sent. `jitter` is a fraction in `[0, 1]`.
fn retry_backoff(retry_index: u32, retry_after: Option<Duration>, jitter: f64) -> Duration {
    let exponential = BASE_RETRY_BACKOFF
        .saturating_mul(1_u32 << retry_index.min(6))
        .min(MAX_RETRY_BACKOFF);
    let jittered = exponential.mul_f64(0.5 + jitter.clamp(0.0, 1.0) / 2.0);
    jittered.max(retry_after.unwrap_or(Duration::ZERO))
}

impl FailureHistory {
    fn record_retry(
        &mut self,
        transport: TransportError,
        canonical: Option<Error>,
        completed_attempts: usize,
    ) {
        self.last_transport = Some(transport);
        if let Some(canonical) = canonical {
            self.last_canonical = Some((completed_attempts, canonical));
        }
    }

    /// What the last failing provider asked us to wait, if it said anything.
    fn retry_after(&self) -> Option<Duration> {
        self.last_transport
            .as_ref()
            .and_then(|transport| transport.upstream.retry_after)
    }

    /// Re-sending to the target that just failed is only safe when that
    /// attempt certainly never reached generation; otherwise the caller could
    /// be billed twice for one request.
    fn permits_same_target_retry(&self) -> bool {
        self.last_transport
            .as_ref()
            .is_some_and(|transport| !attempt_billing_is_uncertain(transport))
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
    runtime: &'a Bundle,
    media_spool: &'a Arc<dyn MediaSpool>,
    max_inline_media_bytes: usize,
    circuits: &'a Breaker,
    metadata: &'a RequestMetadata,
    operation: &'a Arc<Operation>,
    route_deadline: tokio::time::Instant,
    can_retry_canonical: bool,
    propagate_trace_context: bool,
}

enum AttemptDisposition {
    Retry {
        transport: TransportError,
        canonical: Option<Error>,
    },
    Success {
        output: ExecutionOutput,
        deadline: tokio::time::Instant,
        trace: Option<AttemptTrace>,
    },
}

pub async fn execute(
    context: Context<'_>,
    attempts: Vec<AttemptPlan>,
    metadata: RequestMetadata,
    operation: Operation,
) -> Result<ExecutionSuccess, ExecutionFailure> {
    let Context {
        runtime,
        overall_timeout,
        max_attempts,
        media_spool,
        max_inline_media_bytes,
        circuits,
        mut on_attempt_started,
        trace,
    } = context;
    let route_deadline = tokio::time::Instant::now() + overall_timeout;
    let operation = Arc::new(operation);
    let mut failures = FailureHistory::default();
    let mut traces = Vec::with_capacity(attempts.len());
    let attempts = with_sole_target_retry(attempts, max_attempts);
    for attempt_index in 0..attempts.len() {
        let attempt = &attempts[attempt_index];
        if let Some(previous) = attempt_index.checked_sub(1) {
            let Some(delay) = plan_retry(
                &attempts[previous],
                attempt,
                u32::try_from(previous).unwrap_or(u32::MAX),
                &failures,
                route_deadline,
            ) else {
                break;
            };
            if !delay.is_zero() && circuits.is_selectable(attempt.routing_id) {
                tokio::time::sleep(delay).await;
            }
        }
        let Some(circuit_permit) = circuits.try_acquire_permit(attempt.routing_id) else {
            continue;
        };
        let ordinal = u16::try_from(traces.len() + 1).unwrap_or(u16::MAX);
        let attempt_started_at = Utc::now();
        let attempt_started = tokio::time::Instant::now();
        let mut record = AttemptRecord {
            plan: attempt,
            circuit_permit,
            ordinal,
            started_at: attempt_started_at,
            started: attempt_started,
            trace: trace.map(|trace| trace.attempt(attempt)),
        };
        notify_attempt_started(on_attempt_started.as_deref_mut(), &traces, &record);
        match execute_attempt(
            AttemptExecutionContext {
                runtime,
                media_spool: &media_spool,
                max_inline_media_bytes,
                circuits,
                metadata: &metadata,
                operation: &operation,
                route_deadline,
                can_retry_canonical: attempt_index + 1 < attempts.len(),
                propagate_trace_context: trace.is_some_and(RequestTrace::propagate_upstream),
            },
            &mut record,
            &mut traces,
        )
        .await?
        {
            AttemptDisposition::Retry {
                transport,
                canonical,
            } => failures.record_retry(transport, canonical, traces.len()),
            AttemptDisposition::Success {
                output,
                deadline,
                trace,
            } => {
                return Ok(ExecutionSuccess {
                    output,
                    deadline,
                    attempts: traces,
                    attempt_started,
                    attempt_trace: trace,
                });
            }
        }
    }
    Err(ExecutionFailure {
        error: failures.into_error(traces.len()),
        attempts: traces,
    })
}

/// A route whose other targets are all unavailable would otherwise perform
/// zero retries: one transient 503 goes straight to the client. One extra pass
/// over the same target stays inside the route's `max_attempts`, the deadline
/// and the circuit, and `plan_retry` still refuses it when the first attempt
/// may already have been billed.
fn with_sole_target_retry(
    mut attempts: Vec<AttemptPlan>,
    max_attempts: NonZeroU16,
) -> Vec<AttemptPlan> {
    if let [only] = attempts.as_slice()
        && max_attempts.get() > 1
    {
        let retry = only.clone();
        attempts.push(retry);
    }
    attempts
}

/// Cheap decorrelating jitter in `[0, 1)`. Backoff spreading does not need a
/// cryptographic source, only enough entropy that concurrent retries on the
/// same host do not resynchronize.
fn jitter_fraction() -> f64 {
    use std::sync::atomic::{AtomicU64, Ordering};

    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |elapsed| u64::from(elapsed.subsec_nanos()));
    let mut value = COUNTER.fetch_add(1, Ordering::Relaxed) ^ nanos;
    value = value.wrapping_mul(0x2545_f491_4f6c_dd1d) | 1;
    value ^= value >> 33;
    value = value.wrapping_mul(0xff51_afd7_ed55_8ccd);
    value ^= value >> 33;
    #[expect(
        clippy::cast_precision_loss,
        reason = "53 bits is exactly f64's mantissa"
    )]
    let fraction = (value >> 11) as f64 / (1_u64 << 53) as f64;
    fraction
}

/// Decides whether `next` is worth attempting after `previous` failed, and how
/// long to wait first. A provider's `Retry-After` only applies to that same
/// provider: it must not delay failover to an unrelated target. Returns `None`
/// when the deadline leaves no room for another attempt, and when a same-target
/// retry could double-bill the caller.
fn plan_retry(
    previous: &AttemptPlan,
    next: &AttemptPlan,
    retry_index: u32,
    failures: &FailureHistory,
    route_deadline: tokio::time::Instant,
) -> Option<Duration> {
    let same_target = previous.routing_id == next.routing_id;
    if same_target && !failures.permits_same_target_retry() {
        return None;
    }
    let retry_after = same_target
        .then(|| failures.retry_after())
        .flatten()
        .map(|hint| hint.min(MAX_RETRY_AFTER_DELAY));
    let backoff = retry_backoff(retry_index, retry_after, jitter_fraction());
    let remaining = route_deadline.saturating_duration_since(tokio::time::Instant::now());
    if backoff >= remaining {
        return None;
    }
    Some(backoff)
}

async fn execute_attempt(
    context: AttemptExecutionContext<'_>,
    record: &mut AttemptRecord<'_>,
    traces: &mut Vec<RequestAttemptMetadata>,
) -> Result<AttemptDisposition, ExecutionFailure> {
    let AttemptExecutionContext {
        runtime,
        media_spool,
        max_inline_media_bytes,
        circuits,
        metadata,
        operation,
        route_deadline,
        can_retry_canonical,
        propagate_trace_context,
    } = context;
    let attempt = record.plan;
    let attempt_deadline = route_deadline.min(record.started + attempt.timeout.as_duration());
    let Some(transport) = runtime.transport(attempt.provider_id) else {
        let error = TransportError {
            upstream: Default::default(),
            phase: crate::domain::ports::TransportPhase::Connect,
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
        max_inline_media_bytes,
        propagate_trace_context,
    };
    let output = if let Some(trace) = record.trace.as_ref() {
        tokio::time::timeout(
            remaining,
            transport.execute(provider_request).instrument(trace.span()),
        )
        .await
    } else {
        tokio::time::timeout(remaining, transport.execute(provider_request)).await
    };
    let output = match output {
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
                    upstream: Default::default(),
                    phase: crate::domain::ports::TransportPhase::FirstByte,
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
                trace: record.take_trace(),
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
                upstream: Default::default(),
                phase: crate::domain::ports::TransportPhase::FirstByte,
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
                    upstream: Default::default(),
                    phase: crate::domain::ports::TransportPhase::FirstByte,
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
    let initial_failure = if let Kind::Error { error } = &first.kind {
        if error.retryable
            && can_retry_canonical
            && let Some(class) = canonical_error_circuit_class(error.class)
        {
            let transport_error = TransportError {
                upstream: Default::default(),
                phase: crate::domain::ports::TransportPhase::FirstByte,
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
            circuits.record_failure_for_optional_permit(
                attempt.routing_id,
                Some(&record.circuit_permit),
                class,
                None,
            );
        }
        true
    } else {
        false
    };
    if matches!(first.kind, Kind::Done) && !initial_failure {
        circuits
            .record_success_for_optional_permit(attempt.routing_id, Some(&record.circuit_permit));
    }
    // The half-open probe outlives this function: a stream is still the same
    // probe, so it reports its outcome under the same lease.
    let events = circuit_accounted_event_stream_with_permit(
        validated_event_stream(events, event_sequence),
        circuits.clone(),
        attempt.routing_id,
        initial_failure,
        Some(record.circuit_permit),
    );
    record.record_accepted_output(traces);
    Ok(AttemptDisposition::Success {
        output: ExecutionOutput::Events { first, events },
        deadline: attempt_deadline,
        trace: record.take_trace(),
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
            AttemptFailureClass::Connect
                | AttemptFailureClass::Timeout
                | AttemptFailureClass::UpstreamServer
                | AttemptFailureClass::Protocol
                | AttemptFailureClass::Cancelled
        )
        && !matches!(error.phase, crate::domain::ports::TransportPhase::Connect)
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
        ) && !matches!(error.phase, crate::domain::ports::TransportPhase::Connect))
}

pub(in crate::inference) const fn attempt_failure_name(class: AttemptFailureClass) -> &'static str {
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
mod tests;

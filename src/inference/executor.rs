use std::sync::Arc;

use crate::ids::RequestId;
use crate::ids::RouteSlug;
use crate::inference::circuit::Breaker;
use crate::inference::error::Error as InferenceError;
use crate::inference::events::MAX_COLLECTED_CANONICAL_EVENT_BYTES;
use crate::inference::events::collect_provider_events_with_observer;
use crate::inference::failover::Context;
use crate::inference::failover::ExecutionOutput;
use crate::inference::failover::execute;
use crate::inference::lifecycle::UsageCapture;
use crate::inference::selection::select_representable_attempts_filtered;
use crate::inference::telemetry::elapsed_ms;
use crate::inference::tracing::AttemptTrace;
use crate::inference::tracing::RequestTrace;
use crate::inference::transport::MediaSpool;
use crate::inference::transport::ProviderEventStream;
use crate::limits::admission::ReloadableLimiter;
use crate::media::lifecycle::RequestMediaGuard;
use crate::media::lifecycle::operation_media_handles;
use crate::protocols::canonical::events::Event;
use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::RequestMetadata;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::identity::TransportMode;
use crate::protocols::canonical::requests::Operation;
use crate::runtime::manager::Manager;
use crate::usage::emitter::Emitter;

const PLAYGROUND_GENERATION_ONLY: &str = "The playground supports generation only.";

pub struct SessionGenerationExecution {
    pub events: Vec<Event>,
    pub request_id: RequestId,
    pub route_slug: RouteSlug,
    pub latency_ms: u64,
}

struct SessionTrace {
    request: Option<RequestTrace>,
    attempt: Option<AttemptTrace>,
    started: tokio::time::Instant,
    attempt_count: usize,
    first_byte_ms: u64,
    usage: UsageCapture,
    finished: bool,
}

/// How a session ended. The three variants differ only in the outcome
/// literals they contribute, so they share one terminal body.
enum SessionOutcome<'a> {
    Success,
    Failed(&'a InferenceError),
    Cancelled,
}

impl SessionTrace {
    fn finish(&mut self, error: Option<&InferenceError>) {
        self.record_terminal(error.map_or(SessionOutcome::Success, SessionOutcome::Failed));
    }

    fn record_terminal(&mut self, outcome: SessionOutcome<'_>) {
        if self.finished {
            return;
        }
        let (attempt_outcome, status, error_class) = match outcome {
            SessionOutcome::Success => ("success", Some(200), None),
            SessionOutcome::Failed(error) => (
                error.code(),
                Some(crate::inference::telemetry::metadata_status_code(error)),
                Some(error.code()),
            ),
            SessionOutcome::Cancelled => ("cancelled", None, Some("client_cancelled")),
        };
        if let Some(attempt) = self.attempt.as_mut() {
            self.usage.record_trace(attempt);
            attempt.finish(attempt_outcome, Some("2xx"));
        }
        if let Some(request) = &self.request {
            request.record_terminal(
                status,
                error_class,
                self.attempt_count,
                Some(self.first_byte_ms),
                elapsed_ms(self.started.elapsed()),
            );
        }
        self.finished = true;
    }
}

impl Drop for SessionTrace {
    fn drop(&mut self) {
        self.record_terminal(SessionOutcome::Cancelled);
    }
}

fn record_session_execution_failure(
    trace: Option<&RequestTrace>,
    failure: &crate::inference::failover::ExecutionFailure,
    started: tokio::time::Instant,
) {
    if let Some(trace) = trace {
        trace.record_terminal(
            Some(crate::inference::telemetry::metadata_status_code(
                &failure.error,
            )),
            Some(failure.error.code()),
            failure.attempts.len(),
            None,
            elapsed_ms(started.elapsed()),
        );
    }
}

async fn collect_session_events(
    first: Event,
    events: &mut ProviderEventStream,
    deadline: tokio::time::Instant,
    maximum_bytes: usize,
    trace: &mut SessionTrace,
) -> Result<Vec<Event>, InferenceError> {
    let result = collect_provider_events_with_observer(
        first,
        events,
        deadline,
        maximum_bytes,
        &mut |event| trace.usage.observe(event),
    )
    .await;
    if let Err(error) = &result {
        trace.finish(Some(error));
    }
    result
}

/// Shared transport-neutral inference capability installed into each delivery
/// surface that is allowed to execute or observe inference work.
#[derive(Clone)]
pub struct Executor {
    pub(crate) runtime: Arc<Manager>,
    pub(crate) limiter: ReloadableLimiter,
    pub(crate) request_metadata: Option<Emitter>,
    pub(crate) circuits: Breaker,
    pub(crate) media_spool: Arc<dyn MediaSpool>,
    pub(crate) max_collected_event_bytes: usize,
    pub(crate) max_inline_media_bytes: usize,
}

impl Executor {
    #[must_use]
    pub fn new(
        runtime: Arc<Manager>,
        limiter: ReloadableLimiter,
        request_metadata: Option<Emitter>,
        circuits: Breaker,
        media_spool: Arc<dyn MediaSpool>,
    ) -> Self {
        Self {
            runtime,
            limiter,
            request_metadata,
            circuits,
            media_spool,
            max_collected_event_bytes: MAX_COLLECTED_CANONICAL_EVENT_BYTES,
            max_inline_media_bytes: 1024 * 1024,
        }
    }

    /// Caps the bytes buffered while collecting a non-streaming generation;
    /// operators align it with the provider response cap.
    #[must_use]
    pub fn with_max_collected_event_bytes(mut self, bytes: usize) -> Self {
        self.max_collected_event_bytes = bytes.max(1);
        self
    }

    #[must_use]
    pub fn with_max_inline_media_bytes(mut self, bytes: usize) -> Self {
        self.max_inline_media_bytes = bytes.max(1);
        self
    }

    #[cfg(any(test, feature = "test-util"))]
    pub fn replace_request_metadata(&mut self, emitter: Option<Emitter>) {
        self.request_metadata = emitter;
    }

    #[cfg(any(test, feature = "test-util"))]
    pub fn replace_media_spool(&mut self, media_spool: Arc<dyn MediaSpool>) {
        self.media_spool = media_spool;
    }

    /// Executes an authenticated control-plane playground request through the
    /// same pinned runtime, selection, timeout, circuit, and failover path as
    /// public inference. Session and RBAC authorization remain in delivery.
    pub async fn execute_session_generation(
        &self,
        operation: Operation,
        surface: Surface,
        trace: Option<RequestTrace>,
    ) -> Result<SessionGenerationExecution, InferenceError> {
        let request_media = RequestMediaGuard::new(
            Arc::clone(&self.media_spool),
            operation_media_handles(&operation),
        );
        let result = self
            .execute_session_generation_inner(operation, surface, trace)
            .await;
        request_media.cleanup().await;
        result
    }

    async fn execute_session_generation_inner(
        &self,
        operation: Operation,
        surface: Surface,
        trace: Option<RequestTrace>,
    ) -> Result<SessionGenerationExecution, InferenceError> {
        if operation.kind() != OperationKind::Generation {
            return Err(InferenceError::invalid_request(PLAYGROUND_GENERATION_ONLY));
        }
        let snapshot = self.runtime.pin();
        let route_slug = operation
            .route()
            .cloned()
            .ok_or_else(|| InferenceError::invalid_request("A route model is required."))?;
        let request_id = RequestId::new();
        if let Some(trace) = &trace {
            trace.record_session_context(
                OperationKind::Generation,
                &route_slug,
                snapshot.generation.id.as_uuid(),
            );
        }
        let attempts = select_representable_attempts_filtered(
            &snapshot,
            &route_slug,
            &operation,
            surface,
            TransportMode::Unary,
            request_id.as_uuid().as_bytes(),
            |_, target| self.circuits.is_selectable(target.routing_id),
        )?;
        let route = snapshot
            .routes
            .get(&route_slug)
            .expect("attempt selection returned a known route");
        let started = tokio::time::Instant::now();
        let execution = execute(
            Context {
                runtime: &snapshot,
                overall_timeout: route.overall_timeout.as_duration(),
                max_attempts: route.max_attempts,
                media_spool: Arc::clone(&self.media_spool),
                max_inline_media_bytes: self.max_inline_media_bytes,
                circuits: &self.circuits,
                on_attempt_started: None,
                trace: trace.as_ref(),
            },
            attempts,
            RequestMetadata {
                request_id,
                operation: OperationKind::Generation,
                surface,
                mode: TransportMode::Unary,
            },
            operation,
        )
        .await;
        let success = match execution {
            Ok(success) => success,
            Err(failure) => {
                record_session_execution_failure(trace.as_ref(), &failure, started);
                return Err(failure.error);
            }
        };
        let mut session_trace = SessionTrace {
            request: trace,
            attempt: success.attempt_trace,
            started,
            attempt_count: success.attempts.len(),
            first_byte_ms: elapsed_ms(started.elapsed()),
            usage: UsageCapture::default(),
            finished: false,
        };
        let ExecutionOutput::Events { first, mut events } = success.output else {
            let error = InferenceError::bad_gateway(
                "provider_protocol_error",
                "The provider returned an incompatible generation response.",
            );
            session_trace.finish(Some(&error));
            return Err(error);
        };
        let events = collect_session_events(
            first,
            &mut events,
            success.deadline,
            self.max_collected_event_bytes,
            &mut session_trace,
        )
        .await?;
        session_trace.finish(None);
        Ok(SessionGenerationExecution {
            events,
            request_id,
            route_slug,
            latency_ms: elapsed_ms(started.elapsed()),
        })
    }
}

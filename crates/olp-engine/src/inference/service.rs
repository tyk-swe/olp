use std::sync::Arc;

use crate::domain::{
    canonical::{
        events::Event,
        identity::{OperationKind, RequestMetadata, Surface, TransportMode},
        requests::Operation,
    },
    ids::{RequestId, RouteSlug},
    ports::MediaSpool,
};
use crate::inference::{
    circuit::Breaker,
    error::Error as InferenceError,
    events::collect,
    failover::{Context, ExecutionOutput, execute},
    limits::ReloadableLimiter,
    media_lifecycle::{RequestMediaGuard, operation_media_handles},
    request_metadata::Emitter,
    runtime::Manager,
    selection::select_representable_attempts_filtered,
    telemetry::elapsed_ms,
};

pub struct SessionGenerationExecution {
    pub events: Vec<Event>,
    pub request_id: RequestId,
    pub route_slug: RouteSlug,
    pub latency_ms: u64,
}

/// Shared transport-neutral inference capability installed into each delivery
/// surface that is allowed to execute or observe inference work.
#[derive(Clone)]
pub struct Service {
    runtime: Arc<Manager>,
    limiter: ReloadableLimiter,
    request_metadata: Option<Emitter>,
    circuits: Breaker,
    media_spool: Arc<dyn MediaSpool>,
}

impl Service {
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
        }
    }

    #[must_use]
    pub fn runtime(&self) -> &Manager {
        &self.runtime
    }

    #[must_use]
    pub const fn limiter(&self) -> &ReloadableLimiter {
        &self.limiter
    }

    #[must_use]
    pub const fn request_metadata(&self) -> Option<&Emitter> {
        self.request_metadata.as_ref()
    }

    #[must_use]
    pub const fn circuits(&self) -> &Breaker {
        &self.circuits
    }

    #[must_use]
    pub fn media_spool(&self) -> &Arc<dyn MediaSpool> {
        &self.media_spool
    }

    pub fn replace_request_metadata(&mut self, emitter: Option<Emitter>) {
        self.request_metadata = emitter;
    }

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
    ) -> Result<SessionGenerationExecution, InferenceError> {
        let request_media = RequestMediaGuard::new(
            Arc::clone(&self.media_spool),
            operation_media_handles(&operation),
        );
        let result = self
            .execute_session_generation_inner(operation, surface)
            .await;
        request_media.cleanup().await;
        result
    }

    async fn execute_session_generation_inner(
        &self,
        operation: Operation,
        surface: Surface,
    ) -> Result<SessionGenerationExecution, InferenceError> {
        if operation.kind() != OperationKind::Generation {
            return Err(InferenceError::invalid_request(
                "The playground supports generation only.",
            ));
        }
        let snapshot = self.runtime.pin();
        let route_slug = operation
            .route()
            .cloned()
            .ok_or_else(|| InferenceError::invalid_request("A route model is required."))?;
        let request_id = RequestId::new();
        let attempts = select_representable_attempts_filtered(
            &snapshot,
            &route_slug,
            &operation,
            surface,
            TransportMode::Unary,
            request_id.as_uuid().as_bytes(),
            |_, target| {
                self.circuits
                    .is_selectable(target.routing_id.unwrap_or(target.id))
            },
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
                media_spool: Arc::clone(&self.media_spool),
                circuits: &self.circuits,
                on_attempt_started: None,
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
        let success = execution.map_err(|failure| failure.error)?;
        let ExecutionOutput::Events { first, mut events } = success.output else {
            return Err(InferenceError::bad_gateway(
                "provider_protocol_error",
                "The provider returned an incompatible generation response.",
            ));
        };
        let events = collect(first, &mut events, success.deadline).await?;
        Ok(SessionGenerationExecution {
            events,
            request_id,
            route_slug,
            latency_ms: elapsed_ms(started.elapsed()),
        })
    }
}

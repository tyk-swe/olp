//! Shared authentication and execution setup for inference surfaces.

use std::{collections::BTreeMap, time::Duration};

use chrono::Utc;
use olp_domain::{
    ApiKey, CanonicalEvent, CanonicalResult, Operation, OperationKind, RequestId, RequestMetadata,
    RouteSlug, Surface, TransportMode, authorize_api_key,
};
use olp_storage::{LimitLease, RequestAttemptMetadata};

use crate::{
    GatewayState, InferencePrincipal, event_completion::collect_provider_events,
    semantic_validation::select_representable_attempts_filtered,
};

use super::{
    error::InferenceError,
    failover::{ExecutionOutput, ExecutionSuccess, execute_with_failover},
    limits::{RequestMediaGuard, operation_media_handles, release_limits, reserve_limits},
    telemetry::{
        UnaryRequestMetadataFinalizer, UsageCapture, elapsed_ms, emit_request_metadata_event,
        usage_from_result,
    },
};

pub(crate) struct RoutedEventExecution {
    pub(crate) first: CanonicalEvent,
    pub(crate) events: olp_domain::ProviderEventStream,
    pub(crate) deadline: tokio::time::Instant,
    pub(crate) lease: Option<LimitLease>,
    pub(super) generation_id: uuid::Uuid,
    pub(super) api_key_id: uuid::Uuid,
    pub request_id: uuid::Uuid,
    pub route_slug: RouteSlug,
    pub(super) surface: Surface,
    pub(super) operation_kind: OperationKind,
    pub(super) request_started_at: chrono::DateTime<Utc>,
    pub(super) request_started: tokio::time::Instant,
    pub(super) attempt_started: tokio::time::Instant,
    pub(super) attempts: Vec<RequestAttemptMetadata>,
    pub(super) first_byte_ms: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct RequiredTarget {
    pub provider_id: uuid::Uuid,
    pub upstream_model: String,
}

pub(super) fn authorize_principal<'a>(
    principal: &'a InferencePrincipal,
    operation: OperationKind,
    route: Option<&RouteSlug>,
) -> Result<&'a ApiKey, InferenceError> {
    authorize_api_key(principal.key(), route, operation, Utc::now())
        .map_err(|error| InferenceError::forbidden(error.to_string()))?;
    Ok(principal.key())
}

pub(super) fn incompatible_result(operation: &'static str) -> InferenceError {
    InferenceError::bad_gateway(
        "provider_protocol_error",
        format!("The provider returned an incompatible {operation} response."),
    )
}

struct ExecutionContext {
    generation_id: uuid::Uuid,
    api_key_id: uuid::Uuid,
    request_id: RequestId,
    route_slug: RouteSlug,
    operation_kind: OperationKind,
    request_started_at: chrono::DateTime<Utc>,
    request_started: tokio::time::Instant,
    lease: Option<LimitLease>,
    surface: Surface,
}

struct CompletedExecution {
    context: ExecutionContext,
    success: ExecutionSuccess,
}

async fn execute_operation(
    state: &GatewayState,
    principal: &InferencePrincipal,
    operation: Operation,
    mode: TransportMode,
    required_target: Option<RequiredTarget>,
) -> Result<CompletedExecution, InferenceError> {
    let route_slug = operation
        .route()
        .cloned()
        .ok_or_else(|| InferenceError::invalid_request("A route model is required."))?;
    let operation_kind = operation.kind();
    authorize_api_key(
        principal.key(),
        Some(&route_slug),
        operation_kind,
        Utc::now(),
    )
    .map_err(|error| InferenceError::forbidden(error.to_string()))?;

    let request_id = RequestId::new();
    let request_started_at = Utc::now();
    let request_started = tokio::time::Instant::now();
    let lease = reserve_limits(
        state,
        principal.key(),
        &operation,
        principal.lookup_id().as_str(),
        principal
            .runtime()
            .routes
            .get(&route_slug)
            .map(|route| route.overall_timeout.as_duration())
            .unwrap_or(Duration::from_secs(30))
            .saturating_add(Duration::from_secs(30)),
    )
    .await?;
    let context = ExecutionContext {
        generation_id: principal.runtime().generation.id.as_uuid(),
        api_key_id: principal.key().id.as_uuid(),
        request_id,
        route_slug,
        operation_kind,
        request_started_at,
        request_started,
        lease,
        surface: principal.surface(),
    };

    let attempts = match select_representable_attempts_filtered(
        principal.runtime(),
        &context.route_slug,
        &operation,
        principal.surface(),
        mode,
        context.request_id.as_uuid().as_bytes(),
        |_, target| {
            state.circuits.is_selectable(target.id)
                && required_target.as_ref().is_none_or(|required| {
                    target.provider_id.as_uuid() == required.provider_id
                        && target.upstream_model == required.upstream_model
                })
        },
    ) {
        Ok(attempts) => attempts,
        Err(error) => {
            let failure = if required_target.is_some() && error.code == "no_eligible_provider" {
                InferenceError::unavailable("media_job_target_unavailable")
            } else {
                error
            };
            emit_early_failure(state, &context, &[], &failure);
            release_limits(state, context.lease.as_ref()).await;
            return Err(failure);
        }
    };
    let route = principal
        .runtime()
        .routes
        .get(&context.route_slug)
        .expect("attempt selection returned a known route");
    let execution = execute_with_failover(
        principal.runtime(),
        attempts,
        RequestMetadata {
            request_id: context.request_id,
            operation: context.operation_kind,
            surface: principal.surface(),
            mode,
        },
        operation,
        route.overall_timeout.as_duration(),
        state.media_spool.clone(),
        &state.circuits,
    )
    .await;
    match execution {
        Ok(success) => Ok(CompletedExecution { context, success }),
        Err(failure) => {
            emit_early_failure(state, &context, &failure.attempts, &failure.error);
            release_limits(state, context.lease.as_ref()).await;
            Err(failure.error)
        }
    }
}

fn emit_early_failure(
    state: &GatewayState,
    context: &ExecutionContext,
    attempts: &[RequestAttemptMetadata],
    failure: &InferenceError,
) {
    emit_request_metadata_event(
        state,
        context.generation_id,
        context.api_key_id,
        context.request_id.as_uuid(),
        &context.route_slug,
        attempts,
        context.request_started_at,
        context.request_started,
        None,
        None,
        Some(failure.status.as_u16()),
        Some(failure.code.to_owned()),
        false,
        &UsageCapture::default(),
        context.surface,
        context.operation_kind,
    );
}

pub(super) async fn execute_event_operation(
    state: &GatewayState,
    principal: &InferencePrincipal,
    operation: Operation,
    mode: TransportMode,
) -> Result<RoutedEventExecution, InferenceError> {
    execute_event_operation_for_surface(state, principal, operation, mode).await
}

pub(crate) async fn execute_event_operation_for_surface(
    state: &GatewayState,
    principal: &InferencePrincipal,
    operation: Operation,
    mode: TransportMode,
) -> Result<RoutedEventExecution, InferenceError> {
    let request_media = RequestMediaGuard::new(
        state.media_spool.clone(),
        operation_media_handles(&operation),
    );
    let result = execute_event_operation_for_surface_inner(state, principal, operation, mode).await;
    request_media.cleanup().await;
    result
}

pub(super) async fn execute_event_operation_for_surface_inner(
    state: &GatewayState,
    principal: &InferencePrincipal,
    operation: Operation,
    mode: TransportMode,
) -> Result<RoutedEventExecution, InferenceError> {
    let CompletedExecution { context, success } =
        execute_operation(state, principal, operation, mode, None).await?;
    let ExecutionSuccess {
        output,
        deadline,
        attempts,
        attempt_started,
    } = success;
    let ExecutionOutput::Events { first, events } = output else {
        release_limits(state, context.lease.as_ref()).await;
        return Err(incompatible_result("generation"));
    };
    crate::claim_http_inference_metadata();
    let first_byte_ms = elapsed_ms(context.request_started.elapsed());
    Ok(RoutedEventExecution {
        first,
        events,
        deadline,
        lease: context.lease,
        generation_id: context.generation_id,
        api_key_id: context.api_key_id,
        request_id: context.request_id.as_uuid(),
        route_slug: context.route_slug,
        surface: context.surface,
        operation_kind: context.operation_kind,
        request_started_at: context.request_started_at,
        request_started: context.request_started,
        attempt_started,
        attempts,
        first_byte_ms,
    })
}

pub(crate) struct RoutedUnaryResult {
    pub result: Box<CanonicalResult>,
    pub request_id: RequestId,
    pub api_key_id: uuid::Uuid,
    pub route_slug: RouteSlug,
    pub provider_id: uuid::Uuid,
    pub upstream_model: String,
    request_metadata_finalizer: Option<UnaryRequestMetadataFinalizer>,
}

impl RoutedUnaryResult {
    pub(crate) fn mark_success(&mut self) {
        if let Some(finalizer) = self.request_metadata_finalizer.take() {
            finalizer.finalize(None);
        }
    }

    pub(crate) fn mark_failure(&mut self, failure: &InferenceError) {
        if let Some(finalizer) = self.request_metadata_finalizer.take() {
            finalizer.finalize(Some(failure));
        }
    }

    pub(super) fn mark_outcome<T>(&mut self, outcome: &Result<T, InferenceError>) {
        match outcome {
            Ok(_) => self.mark_success(),
            Err(failure) => self.mark_failure(failure),
        }
    }
}

impl Drop for RoutedUnaryResult {
    fn drop(&mut self) {
        let Some(finalizer) = self.request_metadata_finalizer.take() else {
            return;
        };
        let failure = InferenceError::bad_gateway(
            "provider_protocol_error",
            "The provider result was not representable on the client protocol.",
        );
        finalizer.finalize(Some(&failure));
    }
}

pub(super) async fn execute_unary_result(
    state: &GatewayState,
    principal: &InferencePrincipal,
    operation: Operation,
) -> Result<RoutedUnaryResult, InferenceError> {
    execute_routed_result(state, principal, operation, TransportMode::Unary, None).await
}

pub(super) async fn execute_routed_result(
    state: &GatewayState,
    principal: &InferencePrincipal,
    operation: Operation,
    mode: TransportMode,
    required_target: Option<RequiredTarget>,
) -> Result<RoutedUnaryResult, InferenceError> {
    execute_routed_result_for_surface(state, principal, operation, mode, required_target).await
}

pub(crate) async fn execute_routed_result_for_surface(
    state: &GatewayState,
    principal: &InferencePrincipal,
    operation: Operation,
    mode: TransportMode,
    required_target: Option<RequiredTarget>,
) -> Result<RoutedUnaryResult, InferenceError> {
    let request_media = RequestMediaGuard::new(
        state.media_spool.clone(),
        operation_media_handles(&operation),
    );
    let result =
        execute_routed_result_for_surface_inner(state, principal, operation, mode, required_target)
            .await;
    request_media.cleanup().await;
    result
}

pub(super) async fn execute_routed_result_for_surface_inner(
    state: &GatewayState,
    principal: &InferencePrincipal,
    operation: Operation,
    mode: TransportMode,
    required_target: Option<RequiredTarget>,
) -> Result<RoutedUnaryResult, InferenceError> {
    let CompletedExecution { context, success } =
        execute_operation(state, principal, operation, mode, required_target).await?;
    let ExecutionSuccess {
        output,
        attempts,
        attempt_started,
        ..
    } = success;
    let ExecutionOutput::Result(result) = output else {
        let failure = InferenceError::bad_gateway(
            "provider_protocol_error",
            "The provider returned an event stream for a unary result operation.",
        );
        emit_request_metadata_event(
            state,
            context.generation_id,
            context.api_key_id,
            context.request_id.as_uuid(),
            &context.route_slug,
            &attempts,
            context.request_started_at,
            context.request_started,
            Some(attempt_started),
            Some(elapsed_ms(context.request_started.elapsed())),
            Some(failure.status.as_u16()),
            Some(failure.code.to_owned()),
            true,
            &UsageCapture::default(),
            context.surface,
            context.operation_kind,
        );
        release_limits(state, context.lease.as_ref()).await;
        return Err(failure);
    };
    let usage = usage_from_result(&result);
    let first_byte_ms = elapsed_ms(context.request_started.elapsed());
    release_limits(state, context.lease.as_ref()).await;
    let final_attempt = attempts
        .last()
        .expect("a successful execution has one provider attempt");
    crate::claim_http_inference_metadata();
    Ok(RoutedUnaryResult {
        result,
        request_id: context.request_id,
        api_key_id: context.api_key_id,
        route_slug: context.route_slug.clone(),
        provider_id: final_attempt.provider_id,
        upstream_model: final_attempt.upstream_model.clone(),
        request_metadata_finalizer: Some(UnaryRequestMetadataFinalizer {
            state: state.clone(),
            generation_id: context.generation_id,
            api_key_id: context.api_key_id,
            request_id: context.request_id.as_uuid(),
            route_slug: context.route_slug,
            attempts,
            request_started_at: context.request_started_at,
            request_started: context.request_started,
            attempt_started,
            first_byte_ms,
            usage,
            surface: context.surface,
            operation: context.operation_kind,
        }),
    })
}

pub(crate) fn authorize_model_access(
    principal: &InferencePrincipal,
    operation: OperationKind,
) -> Result<(&crate::RuntimeBundle, &ApiKey), InferenceError> {
    let key = authorize_principal(principal, operation, None)?;
    Ok((principal.runtime(), key))
}

pub(crate) async fn reserve_model_limits(
    state: &GatewayState,
    principal: &InferencePrincipal,
) -> Result<Option<LimitLease>, InferenceError> {
    let operation = Operation::Models(olp_domain::ModelOperation::List {
        extensions: olp_domain::SourceExtensions::new(principal.surface(), BTreeMap::new()),
    });
    reserve_limits(
        state,
        principal.key(),
        &operation,
        principal.lookup_id().as_str(),
        Duration::from_secs(30),
    )
    .await
}

pub(crate) async fn release_model_limits(state: &GatewayState, lease: Option<&LimitLease>) {
    release_limits(state, lease).await;
}

pub(crate) struct SessionGenerationExecution {
    pub events: Vec<CanonicalEvent>,
    pub request_id: RequestId,
    pub route_slug: RouteSlug,
    pub latency_ms: u64,
}

/// Executes a management-session playground request through the exact same
/// capability selection, provider transport, deadline, cancellation, and
/// failover machinery as API-key inference. The caller owns session/RBAC
/// authorization. Content and usage are intentionally not emitted or stored.
pub(crate) async fn execute_session_generation(
    state: &GatewayState,
    operation: Operation,
    surface: Surface,
) -> Result<SessionGenerationExecution, InferenceError> {
    let request_media = RequestMediaGuard::new(
        state.media_spool.clone(),
        operation_media_handles(&operation),
    );
    let result = execute_session_generation_inner(state, operation, surface).await;
    request_media.cleanup().await;
    result
}

async fn execute_session_generation_inner(
    state: &GatewayState,
    operation: Operation,
    surface: Surface,
) -> Result<SessionGenerationExecution, InferenceError> {
    if operation.kind() != OperationKind::Generation {
        return Err(InferenceError::invalid_request(
            "The playground supports generation only.",
        ));
    }
    let snapshot = state.runtime.pin();
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
        |_, target| state.circuits.is_selectable(target.id),
    )?;
    let route = snapshot
        .routes
        .get(&route_slug)
        .expect("attempt selection returned a known route");
    let started = tokio::time::Instant::now();
    let execution = execute_with_failover(
        &snapshot,
        attempts,
        RequestMetadata {
            request_id,
            operation: OperationKind::Generation,
            surface,
            mode: TransportMode::Unary,
        },
        operation,
        route.overall_timeout.as_duration(),
        state.media_spool.clone(),
        &state.circuits,
    )
    .await;
    let success = execution.map_err(|failure| failure.error)?;
    let ExecutionOutput::Events { first, mut events } = success.output else {
        return Err(incompatible_result("generation"));
    };
    let events = collect_provider_events(first, &mut events, success.deadline).await?;
    Ok(SessionGenerationExecution {
        events,
        request_id,
        route_slug,
        latency_ms: elapsed_ms(started.elapsed()),
    })
}

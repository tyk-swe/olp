//! Common authenticated execution setup shared by event and unary routes.
//!
//! This keeps authorization, rate-limit reservations, attempt selection, and
//! early failure telemetry on one path. The parent module remains responsible
//! for adapting a successful provider output to the appropriate HTTP shape.

use std::time::Duration;

use chrono::Utc;
use olp_domain::{
    Operation, OperationKind, RequestId, RequestMetadata, RouteSlug, Surface, TransportMode,
    authorize_api_key,
};
use olp_storage::{LimitLease, UsageAttempt};

use crate::{ApiState, semantic_validation::select_representable_attempts_filtered};

use super::{
    ExecutionSuccess, InferenceError, RequiredTarget, UsageCapture, authenticate_proxy_key,
    emit_request_event, execute_with_failover, release_limits, reserve_limits,
};

pub(super) struct ExecutionContext {
    pub(super) generation_id: uuid::Uuid,
    pub(super) api_key_id: uuid::Uuid,
    pub(super) request_id: RequestId,
    pub(super) route_slug: RouteSlug,
    pub(super) operation_kind: OperationKind,
    pub(super) request_started_at: chrono::DateTime<Utc>,
    pub(super) request_started: tokio::time::Instant,
    pub(super) lease: Option<LimitLease>,
    pub(super) surface: Surface,
}

pub(super) struct CompletedExecution {
    pub(super) context: ExecutionContext,
    pub(super) success: ExecutionSuccess,
}

pub(super) async fn execute_operation(
    state: &ApiState,
    plaintext_key: &str,
    operation: Operation,
    surface: Surface,
    mode: TransportMode,
    required_target: Option<RequiredTarget>,
) -> Result<CompletedExecution, InferenceError> {
    let authenticated = authenticate_proxy_key(state, plaintext_key)?;
    let route_slug = operation
        .route()
        .cloned()
        .ok_or_else(|| InferenceError::invalid_request("A route model is required."))?;
    let operation_kind = operation.kind();
    authorize_api_key(
        &authenticated.key,
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
        &authenticated.key,
        &operation,
        authenticated.lookup_id.as_str(),
        authenticated
            .runtime
            .routes
            .get(&route_slug)
            .map(|route| route.overall_timeout.as_duration())
            .unwrap_or(Duration::from_secs(30))
            .saturating_add(Duration::from_secs(30)),
    )
    .await?;
    let context = ExecutionContext {
        generation_id: authenticated.runtime.generation.id.as_uuid(),
        api_key_id: authenticated.key.id.as_uuid(),
        request_id,
        route_slug,
        operation_kind,
        request_started_at,
        request_started,
        lease,
        surface,
    };

    let attempts = match select_representable_attempts_filtered(
        &authenticated.runtime,
        &context.route_slug,
        &operation,
        surface,
        mode,
        context.request_id.as_uuid().as_bytes(),
        |_, target| {
            state.circuits.is_selectable(target.id)
                && required_target.as_ref().is_none_or(|required| {
                    target.provider_id.as_uuid() == required.provider_id
                        && target.provider_model == required.provider_model
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
    let route = authenticated
        .runtime
        .routes
        .get(&context.route_slug)
        .expect("attempt selection returned a known route");
    let execution = execute_with_failover(
        &authenticated.runtime,
        attempts,
        RequestMetadata {
            request_id: context.request_id,
            operation: context.operation_kind,
            surface,
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
    state: &ApiState,
    context: &ExecutionContext,
    attempts: &[UsageAttempt],
    failure: &InferenceError,
) {
    emit_request_event(
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
        context.operation_kind.as_str(),
    );
}

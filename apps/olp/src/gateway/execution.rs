//! HTTP-delivery adapter for the cohesive transport-neutral inference service.

use olp_domain::{ApiKey, Operation, OperationKind, TransportMode};
use olp_inference::{RequestAdmission, runtime::RuntimeBundle};
use olp_storage::limits::LimitLease;

use crate::{GatewayState, InferencePrincipal};

use super::error::InferenceError;

pub(crate) use olp_inference::{RequiredTarget, RoutedEventExecution, RoutedUnaryResult};

fn admitted_request() -> RequestAdmission {
    RequestAdmission::new(
        crate::http_inference_reservation(),
        crate::http_inference_reserved_tokens(),
        crate::http_inference_metadata_claim(),
    )
}

pub(super) fn authorize_principal<'a>(
    state: &GatewayState,
    principal: &'a InferencePrincipal,
    operation: OperationKind,
    route: Option<&olp_domain::RouteSlug>,
) -> Result<&'a ApiKey, InferenceError> {
    state
        .inference()
        .authorize_principal(principal, operation, route)
        .map_err(Into::into)
}

pub(super) fn incompatible_result(operation: &'static str) -> InferenceError {
    InferenceError::bad_gateway(
        "provider_protocol_error",
        format!("The provider returned an incompatible {operation} response."),
    )
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
    state
        .inference()
        .execute_event(principal, operation, mode, admitted_request())
        .await
        .map_err(Into::into)
}

#[cfg(test)]
pub(super) async fn execute_event_operation_for_surface_inner(
    state: &GatewayState,
    principal: &InferencePrincipal,
    operation: Operation,
    mode: TransportMode,
) -> Result<RoutedEventExecution, InferenceError> {
    state
        .inference()
        .execute_event(principal, operation, mode, RequestAdmission::default())
        .await
        .map_err(Into::into)
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
    state
        .inference()
        .execute_result(
            principal,
            operation,
            mode,
            required_target,
            admitted_request(),
        )
        .await
        .map_err(Into::into)
}

#[cfg(test)]
pub(super) async fn execute_routed_result_for_surface_inner(
    state: &GatewayState,
    principal: &InferencePrincipal,
    operation: Operation,
    mode: TransportMode,
    required_target: Option<RequiredTarget>,
) -> Result<RoutedUnaryResult, InferenceError> {
    state
        .inference()
        .execute_result(
            principal,
            operation,
            mode,
            required_target,
            RequestAdmission::default(),
        )
        .await
        .map_err(Into::into)
}

pub(crate) fn authorize_model_access<'a>(
    state: &GatewayState,
    principal: &'a InferencePrincipal,
    operation: OperationKind,
) -> Result<(&'a RuntimeBundle, &'a ApiKey), InferenceError> {
    state
        .inference()
        .authorize_model_access(principal, operation)
        .map_err(Into::into)
}

pub(crate) async fn reserve_model_limits(
    state: &GatewayState,
    principal: &InferencePrincipal,
) -> Result<Option<LimitLease>, InferenceError> {
    state
        .inference()
        .reserve_model_limits(principal, crate::http_inference_reserved_tokens())
        .await
        .map_err(Into::into)
}

pub(crate) async fn release_model_limits(state: &GatewayState, lease: Option<&LimitLease>) {
    state.inference().release_model_limits(lease).await;
}

pub(crate) fn mark_unary_outcome<T>(
    execution: &mut RoutedUnaryResult,
    outcome: &Result<T, InferenceError>,
) {
    match outcome {
        Ok(_) => execution.mark_success(),
        Err(failure) => execution.mark_failure(failure.accounting_outcome()),
    }
}

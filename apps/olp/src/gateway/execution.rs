//! HTTP-delivery adapter for the cohesive transport-neutral inference service.

use std::{
    pin::Pin,
    task::{Context, Poll},
};

use axum::{
    body::{Body, HttpBody},
    http::StatusCode,
    response::Response,
};
use olp_engine::domain::{
    auth::{ApiKey, GatewayCapability},
    canonical::{identity::TransportMode, requests::Operation},
};
use olp_engine::inference::{
    execution::{RequestAdmission, RoutedUnaryFinalizer},
    limits::Reservation,
    runtime::Bundle,
};

use crate::bootstrap::mode_dependencies::GatewayState;
use crate::public_http::request_admission::HttpRequestAdmission;
use olp_engine::inference::principal::Principal;

use super::error::InferenceError;

use olp_engine::inference::execution::{RequiredTarget, RoutedEvents, RoutedUnaryResult};

pub(super) fn authorize_principal<'a>(
    state: &GatewayState,
    principal: &'a Principal,
    capability: GatewayCapability,
    route: Option<&olp_engine::domain::ids::RouteSlug>,
) -> Result<&'a ApiKey, InferenceError> {
    state
        .inference()
        .authorize_principal(principal, capability, route)
        .map_err(Into::into)
}

pub(super) fn incompatible_result(operation: &'static str) -> InferenceError {
    InferenceError::bad_gateway(
        "provider_protocol_error",
        format!("The provider returned an incompatible {operation} response."),
    )
}

pub(crate) async fn execute_event_operation(
    state: &GatewayState,
    admission: &HttpRequestAdmission,
    operation: Operation,
    mode: TransportMode,
) -> Result<RoutedEvents, InferenceError> {
    state
        .inference()
        .execute_event(
            admission.principal(),
            operation,
            mode,
            admission.engine_admission(),
        )
        .await
        .map_err(Into::into)
}

pub(super) async fn execute_unary_result(
    state: &GatewayState,
    admission: &HttpRequestAdmission,
    operation: Operation,
) -> Result<RoutedUnaryResult, InferenceError> {
    execute_routed_result(state, admission, operation, TransportMode::Unary, None).await
}

pub(super) async fn execute_routed_result(
    state: &GatewayState,
    admission: &HttpRequestAdmission,
    operation: Operation,
    mode: TransportMode,
    required_target: Option<RequiredTarget>,
) -> Result<RoutedUnaryResult, InferenceError> {
    execute_result_with_admission(
        state,
        admission,
        operation,
        mode,
        required_target,
        admission.engine_admission(),
    )
    .await
}

pub(super) async fn execute_internal_routed_result(
    state: &GatewayState,
    admission: &HttpRequestAdmission,
    operation: Operation,
    mode: TransportMode,
    required_target: Option<RequiredTarget>,
) -> Result<RoutedUnaryResult, InferenceError> {
    execute_result_with_admission(
        state,
        admission,
        operation,
        mode,
        required_target,
        admission.internal_engine_admission(),
    )
    .await
}

async fn execute_result_with_admission(
    state: &GatewayState,
    admission: &HttpRequestAdmission,
    operation: Operation,
    mode: TransportMode,
    required_target: Option<RequiredTarget>,
    engine_admission: RequestAdmission,
) -> Result<RoutedUnaryResult, InferenceError> {
    state
        .inference()
        .execute_result(
            admission.principal(),
            operation,
            mode,
            required_target,
            engine_admission,
        )
        .await
        .map_err(Into::into)
}

pub(crate) fn authorize_model_access<'a>(
    state: &GatewayState,
    principal: &'a Principal,
) -> Result<(&'a Bundle, &'a ApiKey), InferenceError> {
    state
        .inference()
        .authorize_model_access(principal)
        .map_err(Into::into)
}

pub(crate) async fn reserve_model_limits(
    state: &GatewayState,
    admission: &HttpRequestAdmission,
) -> Result<Option<Reservation>, InferenceError> {
    state
        .inference()
        .reserve_model_limits(admission.principal(), admission.reserved_tokens())
        .await
        .map_err(Into::into)
}

pub(crate) async fn release_model_limits(state: &GatewayState, lease: Option<Reservation>) {
    state.inference().release_model_limits(lease).await;
}

pub(crate) fn mark_unary_outcome<T>(
    execution: &mut RoutedUnaryResult,
    outcome: &Result<T, InferenceError>,
) {
    mark_unary_outcome_with_status(execution, outcome, StatusCode::OK);
}

pub(crate) fn mark_unary_outcome_with_status<T>(
    execution: &mut RoutedUnaryResult,
    outcome: &Result<T, InferenceError>,
    success_status: StatusCode,
) {
    match outcome {
        Ok(_) => execution.mark_success_with_status(success_status.as_u16()),
        Err(failure) => execution.mark_failure(failure.accounting_outcome()),
    }
}

trait UnaryBodyFinalizer: Send + Unpin + 'static {
    fn success(self, status_code: u16);
    fn provider_protocol_failure(self);
}

impl UnaryBodyFinalizer for RoutedUnaryFinalizer {
    fn success(self, status_code: u16) {
        self.mark_success(status_code);
    }

    fn provider_protocol_failure(self) {
        self.mark_provider_protocol_failure();
    }
}

struct FinalizeUnaryBody<F: UnaryBodyFinalizer> {
    inner: Body,
    finalizer: Option<F>,
    success_status: u16,
}

impl<F: UnaryBodyFinalizer> HttpBody for FinalizeUnaryBody<F> {
    type Data = bytes::Bytes;
    type Error = axum::Error;

    fn poll_frame(
        self: Pin<&mut Self>,
        context: &mut Context<'_>,
    ) -> Poll<Option<Result<http_body::Frame<Self::Data>, Self::Error>>> {
        let this = self.get_mut();
        match Pin::new(&mut this.inner).poll_frame(context) {
            Poll::Ready(None) => {
                if let Some(finalizer) = this.finalizer.take() {
                    finalizer.success(this.success_status);
                }
                Poll::Ready(None)
            }
            Poll::Ready(Some(Err(error))) => {
                if let Some(finalizer) = this.finalizer.take() {
                    finalizer.provider_protocol_failure();
                }
                Poll::Ready(Some(Err(error)))
            }
            other => other,
        }
    }

    fn is_end_stream(&self) -> bool {
        self.finalizer.is_none() && self.inner.is_end_stream()
    }

    fn size_hint(&self) -> http_body::SizeHint {
        self.inner.size_hint()
    }
}

fn wrap_unary_body<F: UnaryBodyFinalizer>(response: Response, finalizer: F) -> Response {
    let success_status = response.status().as_u16();
    let (parts, inner) = response.into_parts();
    let body = Body::new(FinalizeUnaryBody {
        inner,
        finalizer: Some(finalizer),
        success_status,
    });
    Response::from_parts(parts, body)
}

pub(crate) fn defer_unary_outcome_to_body(
    execution: &mut RoutedUnaryResult,
    outcome: Result<Response, InferenceError>,
) -> Result<Response, InferenceError> {
    match outcome {
        Ok(response) => Ok(wrap_unary_body(response, execution.take_body_finalizer())),
        Err(failure) => {
            execution.mark_failure(failure.accounting_outcome());
            Err(failure)
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::{Arc, Mutex};

    use bytes::Bytes;
    use futures::stream;
    use http_body_util::BodyExt as _;

    use super::*;

    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    enum Finalization {
        Success(u16),
        ProviderProtocolFailure,
        ClientCancelled,
    }

    struct TestFinalizer {
        events: Arc<Mutex<Vec<Finalization>>>,
        armed: bool,
    }

    impl TestFinalizer {
        fn new(events: Arc<Mutex<Vec<Finalization>>>) -> Self {
            Self {
                events,
                armed: true,
            }
        }

        fn record(&mut self, event: Finalization) {
            self.events.lock().unwrap().push(event);
            self.armed = false;
        }
    }

    impl UnaryBodyFinalizer for TestFinalizer {
        fn success(mut self, status_code: u16) {
            self.record(Finalization::Success(status_code));
        }

        fn provider_protocol_failure(mut self) {
            self.record(Finalization::ProviderProtocolFailure);
        }
    }

    impl Drop for TestFinalizer {
        fn drop(&mut self) {
            if self.armed {
                self.events
                    .lock()
                    .unwrap()
                    .push(Finalization::ClientCancelled);
            }
        }
    }

    #[tokio::test]
    async fn lazy_unary_body_records_success_only_after_eof() {
        let events = Arc::new(Mutex::new(Vec::new()));
        let response = Response::builder()
            .status(StatusCode::PARTIAL_CONTENT)
            .body(Body::from_stream(stream::iter([Ok::<_, std::io::Error>(
                Bytes::from_static(b"payload"),
            )])))
            .unwrap();
        let response = wrap_unary_body(response, TestFinalizer::new(Arc::clone(&events)));
        assert!(events.lock().unwrap().is_empty());

        assert_eq!(
            response.into_body().collect().await.unwrap().to_bytes(),
            Bytes::from_static(b"payload")
        );
        assert_eq!(
            events.lock().unwrap().as_slice(),
            &[Finalization::Success(StatusCode::PARTIAL_CONTENT.as_u16())]
        );
    }

    #[tokio::test]
    async fn lazy_unary_body_records_late_body_failure() {
        let events = Arc::new(Mutex::new(Vec::new()));
        let body = stream::iter([
            Ok(Bytes::from_static(b"partial")),
            Err(std::io::Error::other("late spool read failure")),
        ]);
        let response = wrap_unary_body(
            Response::new(Body::from_stream(body)),
            TestFinalizer::new(Arc::clone(&events)),
        );

        assert!(response.into_body().collect().await.is_err());
        assert_eq!(
            events.lock().unwrap().as_slice(),
            &[Finalization::ProviderProtocolFailure]
        );
    }

    #[tokio::test]
    async fn dropping_lazy_unary_body_records_client_cancellation() {
        let events = Arc::new(Mutex::new(Vec::new()));
        let pending = stream::pending::<Result<Bytes, std::io::Error>>();
        let response = wrap_unary_body(
            Response::new(Body::from_stream(pending)),
            TestFinalizer::new(Arc::clone(&events)),
        );

        drop(response);
        assert_eq!(
            events.lock().unwrap().as_slice(),
            &[Finalization::ClientCancelled]
        );
    }
}

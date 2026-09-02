use axum::{
    Json,
    extract::rejection::JsonRejection,
    http::StatusCode,
    response::{IntoResponse, Response},
};
use olp_engine::domain::canonical::identity::Surface;
use serde_json::json;

use crate::public_http::problem::Problem;

use super::error::{InferenceError, insert_retry_after_header};

#[derive(Debug)]
pub(super) struct ProtocolError {
    surface: Surface,
    error: InferenceError,
}

impl ProtocolError {
    pub(super) fn anthropic(error: InferenceError) -> Self {
        Self {
            surface: Surface::Anthropic,
            error,
        }
    }

    pub(super) fn gemini(error: InferenceError) -> Self {
        Self {
            surface: Surface::Gemini,
            error,
        }
    }

    pub(super) fn invalid(surface: Surface, message: impl Into<String>) -> Self {
        Self {
            surface,
            error: InferenceError::invalid_request(message),
        }
    }

    pub(super) fn not_found(surface: Surface, message: impl Into<String>) -> Self {
        Self {
            surface,
            error: InferenceError::not_found(message.into()),
        }
    }

    pub(super) fn upstream(surface: Surface, message: impl Into<String>) -> Self {
        Self {
            surface,
            error: InferenceError::bad_gateway("provider_protocol_error", message),
        }
    }
}

impl IntoResponse for ProtocolError {
    fn into_response(self) -> Response {
        let status = self.error.status();
        let retry_after = self.error.retry_after();
        let mut response = match self.surface {
            Surface::Anthropic => (
                status,
                Json(anthropic_error_body(status, self.error.message())),
            )
                .into_response(),
            Surface::Gemini => (
                status,
                Json(gemini_error_body(status, self.error.message())),
            )
                .into_response(),
            Surface::OpenAi => return self.error.into_response(),
        };
        insert_retry_after_header(&mut response, retry_after);
        response
    }
}

pub(crate) fn problem_response(surface: Surface, problem: Problem) -> Response {
    let status = StatusCode::from_u16(problem.status).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
    let code = if status == StatusCode::UNAUTHORIZED {
        "invalid_api_key".to_owned()
    } else {
        problem
            .problem_type
            .rsplit('/')
            .next()
            .unwrap_or("request_failed")
            .to_owned()
    };
    match surface {
        Surface::OpenAi => super::error::openai_error_response(
            status,
            &code,
            match status {
                StatusCode::UNAUTHORIZED => "authentication_error",
                StatusCode::FORBIDDEN => "permission_error",
                StatusCode::TOO_MANY_REQUESTS => "rate_limit_error",
                status if status.is_client_error() => "invalid_request_error",
                _ => "server_error",
            },
            &problem.detail,
            None,
            status == StatusCode::UNAUTHORIZED,
        ),
        Surface::Anthropic => {
            (status, Json(anthropic_error_body(status, &problem.detail))).into_response()
        }
        Surface::Gemini => {
            (status, Json(gemini_error_body(status, &problem.detail))).into_response()
        }
    }
}

pub(crate) fn inference_error_response(surface: Surface, error: InferenceError) -> Response {
    ProtocolError { surface, error }.into_response()
}

pub(super) fn valid_json<T>(
    payload: Result<Json<T>, JsonRejection>,
    surface: Surface,
) -> Result<Json<T>, ProtocolError> {
    payload
        .map_err(|error| ProtocolError::invalid(surface, format!("Invalid JSON request: {error}")))
}

pub(super) fn anthropic_error_body(status: StatusCode, message: &str) -> serde_json::Value {
    json!({
        "type": "error",
        "error": {"type": anthropic_error_kind(status), "message": message}
    })
}

fn anthropic_error_kind(status: StatusCode) -> &'static str {
    match status {
        StatusCode::UNAUTHORIZED => "authentication_error",
        StatusCode::FORBIDDEN => "permission_error",
        StatusCode::NOT_FOUND => "not_found_error",
        StatusCode::TOO_MANY_REQUESTS => "rate_limit_error",
        status if status.is_client_error() => "invalid_request_error",
        _ => "api_error",
    }
}

pub(super) fn gemini_error_body(status: StatusCode, message: &str) -> serde_json::Value {
    json!({
        "error": {
            "code": status.as_u16(),
            "message": message,
            "status": gemini_error_status(status)
        }
    })
}

fn gemini_error_status(status: StatusCode) -> &'static str {
    match status {
        StatusCode::BAD_REQUEST => "INVALID_ARGUMENT",
        StatusCode::UNAUTHORIZED => "UNAUTHENTICATED",
        StatusCode::FORBIDDEN => "PERMISSION_DENIED",
        StatusCode::NOT_FOUND => "NOT_FOUND",
        StatusCode::TOO_MANY_REQUESTS => "RESOURCE_EXHAUSTED",
        StatusCode::GATEWAY_TIMEOUT => "DEADLINE_EXCEEDED",
        StatusCode::SERVICE_UNAVAILABLE => "UNAVAILABLE",
        _ => "INTERNAL",
    }
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use axum::http::header;
    use olp_engine::inference::limits::LimitDimension;

    use super::*;

    #[test]
    fn fractional_retry_after_rounds_up_for_every_protocol_surface() {
        for surface in [Surface::OpenAi, Surface::Anthropic, Surface::Gemini] {
            let response = inference_error_response(
                surface,
                InferenceError::rate_limited(
                    LimitDimension::Requests,
                    Duration::from_millis(1_001),
                ),
            );

            assert_eq!(response.status(), StatusCode::TOO_MANY_REQUESTS);
            assert_eq!(
                response.headers().get(header::RETRY_AFTER).unwrap(),
                "2",
                "surface: {surface:?}"
            );
        }
    }

    #[tokio::test]
    async fn a_rejected_provider_credential_is_a_server_fault_on_every_surface() {
        use olp_engine::domain::ports::{
            AttemptFailureClass, TransportError, TransportPhase, UpstreamSignal,
        };
        use olp_engine::inference::error::Error as CoreInferenceError;

        for (surface, pointer, expected) in [
            (Surface::OpenAi, "/error/type", "server_error"),
            (Surface::Anthropic, "/error/type", "api_error"),
            (Surface::Gemini, "/error/status", "INTERNAL"),
        ] {
            for status in [401, 403] {
                let error = CoreInferenceError::from_transport(TransportError {
                    phase: TransportPhase::FirstByte,
                    class: AttemptFailureClass::UpstreamClient,
                    response_committed: false,
                    message: "invalid api key".to_owned(),
                    upstream: UpstreamSignal::from_status(status),
                });
                let response = inference_error_response(surface, InferenceError::from(error));
                assert_eq!(
                    response.status(),
                    StatusCode::BAD_GATEWAY,
                    "surface: {surface:?} status {status}"
                );
                let body = axum::body::to_bytes(response.into_body(), usize::MAX)
                    .await
                    .unwrap();
                let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
                assert_eq!(
                    body.pointer(pointer).and_then(serde_json::Value::as_str),
                    Some(expected),
                    "surface: {surface:?} status {status}: {body}"
                );
            }
        }
    }

    #[tokio::test]
    async fn budget_exhaustion_keeps_each_protocol_shape_and_publishes_unpriced_semantics() {
        for (surface, code_pointer, kind_pointer, expected_kind) in [
            (
                Surface::OpenAi,
                Some("/error/code"),
                "/error/type",
                "rate_limit_error",
            ),
            (Surface::Anthropic, None, "/error/type", "rate_limit_error"),
            (Surface::Gemini, None, "/error/status", "RESOURCE_EXHAUSTED"),
        ] {
            let response = inference_error_response(
                surface,
                InferenceError::rate_limited(
                    LimitDimension::DailyCost,
                    Duration::from_secs(86_400),
                ),
            );
            assert_eq!(response.status(), StatusCode::TOO_MANY_REQUESTS);
            assert_eq!(response.headers()[header::RETRY_AFTER], "86400");
            let body = axum::body::to_bytes(response.into_body(), usize::MAX)
                .await
                .unwrap();
            let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
            assert_eq!(
                body.pointer(kind_pointer)
                    .and_then(serde_json::Value::as_str),
                Some(expected_kind),
                "surface: {surface:?}"
            );
            if let Some(pointer) = code_pointer {
                assert_eq!(
                    body.pointer(pointer).and_then(serde_json::Value::as_str),
                    Some("budget_exhausted")
                );
            }
            assert!(body.to_string().contains("Unpriced attempts accrue 0."));
        }
    }
}

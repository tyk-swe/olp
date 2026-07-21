use axum::{
    Json,
    extract::rejection::JsonRejection,
    http::{HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use olp_domain::Surface;
use serde_json::json;

use crate::Problem;

use super::InferenceError;

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
                Json(json!({
                    "type": "error",
                    "error": {
                        "type": anthropic_error_kind(&self.error),
                        "message": self.error.message()
                    }
                })),
            )
                .into_response(),
            Surface::Gemini => (status, Json(gemini_error_body(&self.error))).into_response(),
            Surface::OpenAi => self.error.into_response(),
        };
        if let Some(retry_after) = retry_after
            && let Ok(value) = HeaderValue::from_str(&retry_after.as_secs().max(1).to_string())
        {
            response.headers_mut().insert(header::RETRY_AFTER, value);
        }
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
    let mut response = match surface {
        Surface::OpenAi => (
            status,
            Json(json!({
                "error": {
                    "message": problem.detail,
                    "type": match status {
                        StatusCode::UNAUTHORIZED => "authentication_error",
                        StatusCode::FORBIDDEN => "permission_error",
                        StatusCode::TOO_MANY_REQUESTS => "rate_limit_error",
                        status if status.is_client_error() => "invalid_request_error",
                        _ => "server_error"
                    },
                    "param": null,
                    "code": code
                }
            })),
        )
            .into_response(),
        Surface::Anthropic => (
            status,
            Json(json!({
                "type": "error",
                "error": {
                    "type": match status {
                        StatusCode::UNAUTHORIZED => "authentication_error",
                        StatusCode::FORBIDDEN => "permission_error",
                        StatusCode::NOT_FOUND => "not_found_error",
                        StatusCode::TOO_MANY_REQUESTS => "rate_limit_error",
                        status if status.is_client_error() => "invalid_request_error",
                        _ => "api_error"
                    },
                    "message": problem.detail
                }
            })),
        )
            .into_response(),
        Surface::Gemini => (
            status,
            Json(json!({
                "error": {
                    "code": status.as_u16(),
                    "message": problem.detail,
                    "status": gemini_error_status(status)
                }
            })),
        )
            .into_response(),
    };
    if matches!(surface, Surface::OpenAi) && status == StatusCode::UNAUTHORIZED {
        response
            .headers_mut()
            .insert(header::WWW_AUTHENTICATE, HeaderValue::from_static("Bearer"));
    }
    response
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

pub(super) fn anthropic_error_kind(error: &InferenceError) -> &'static str {
    match error.status() {
        StatusCode::UNAUTHORIZED => "authentication_error",
        StatusCode::FORBIDDEN => "permission_error",
        StatusCode::NOT_FOUND => "not_found_error",
        StatusCode::TOO_MANY_REQUESTS => "rate_limit_error",
        status if status.is_client_error() => "invalid_request_error",
        _ => "api_error",
    }
}

pub(super) fn gemini_error_body(error: &InferenceError) -> serde_json::Value {
    json!({
        "error": {
            "code": error.status().as_u16(),
            "message": error.message(),
            "status": gemini_error_status(error.status())
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

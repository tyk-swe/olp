use std::{fmt, time::Duration};

use axum::{
    Json,
    extract::rejection::JsonRejection,
    http::{HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use olp_domain::{AttemptFailureClass, CanonicalError, ErrorClass, TransportError};
use olp_storage::LimitDimension;
use serde::Serialize;

use crate::Problem;

pub(crate) struct InferenceError {
    pub(super) status: StatusCode,
    pub(super) code: &'static str,
    pub(super) kind: &'static str,
    pub(super) message: String,
    pub(super) retry_after: Option<Duration>,
}

pub(super) fn valid_json<T>(
    payload: Result<Json<T>, JsonRejection>,
) -> Result<Json<T>, InferenceError> {
    payload.map_err(|error| {
        InferenceError::invalid_request(format!("The JSON request is invalid: {error}"))
    })
}

impl fmt::Debug for InferenceError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("InferenceError")
            .field("status", &self.status)
            .field("code", &self.code)
            .field("kind", &self.kind)
            .field("message", &"[REDACTED]")
            .field("retry_after", &self.retry_after)
            .finish()
    }
}

impl InferenceError {
    pub(crate) fn unauthorized() -> Self {
        Self {
            status: StatusCode::UNAUTHORIZED,
            code: "invalid_api_key",
            kind: "authentication_error",
            message: "The API key is invalid or unavailable.".to_owned(),
            retry_after: None,
        }
    }

    pub(crate) fn forbidden(message: String) -> Self {
        Self {
            status: StatusCode::FORBIDDEN,
            code: "permission_denied",
            kind: "permission_error",
            message,
            retry_after: None,
        }
    }

    pub(crate) fn invalid_request(message: impl Into<String>) -> Self {
        Self {
            status: StatusCode::BAD_REQUEST,
            code: "invalid_request",
            kind: "invalid_request_error",
            message: message.into(),
            retry_after: None,
        }
    }

    pub(super) fn payload_too_large(code: &'static str) -> Self {
        Self {
            status: StatusCode::PAYLOAD_TOO_LARGE,
            code,
            kind: "invalid_request_error",
            message: "The uploaded media exceeds the configured limit.".to_owned(),
            retry_after: None,
        }
    }

    pub(crate) fn not_found(message: String) -> Self {
        Self {
            status: StatusCode::NOT_FOUND,
            code: "route_not_found",
            kind: "invalid_request_error",
            message,
            retry_after: None,
        }
    }

    pub(super) fn resource_not_found(code: &'static str) -> Self {
        Self {
            status: StatusCode::NOT_FOUND,
            code,
            kind: "invalid_request_error",
            message: "The requested resource was not found.".to_owned(),
            retry_after: None,
        }
    }

    pub(crate) fn rate_limited(dimension: LimitDimension, retry_after: Duration) -> Self {
        let name = match dimension {
            LimitDimension::Requests => "requests per minute",
            LimitDimension::Tokens => "tokens per minute",
            LimitDimension::Concurrency => "concurrency",
            LimitDimension::Unknown => "configured",
        };
        Self {
            status: StatusCode::TOO_MANY_REQUESTS,
            code: "rate_limit_exceeded",
            kind: "rate_limit_error",
            message: format!("The API key {name} limit was exceeded."),
            retry_after: Some(retry_after),
        }
    }

    pub(crate) fn unavailable(code: &'static str) -> Self {
        Self {
            status: StatusCode::SERVICE_UNAVAILABLE,
            code,
            kind: "service_unavailable_error",
            message: "The gateway is temporarily unavailable.".to_owned(),
            retry_after: None,
        }
    }

    pub(super) fn multipart_parser_timeout() -> Self {
        Self {
            status: StatusCode::REQUEST_TIMEOUT,
            code: "multipart_parser_timeout",
            kind: "timeout_error",
            message: "The multipart upload exceeded its parser deadline.".to_owned(),
            retry_after: None,
        }
    }

    pub(crate) fn timeout() -> Self {
        Self {
            status: StatusCode::GATEWAY_TIMEOUT,
            code: "gateway_timeout",
            kind: "timeout_error",
            message: "The route deadline elapsed.".to_owned(),
            retry_after: None,
        }
    }

    pub(crate) fn bad_gateway(code: &'static str, message: impl Into<String>) -> Self {
        Self {
            status: StatusCode::BAD_GATEWAY,
            code,
            kind: "upstream_error",
            message: message.into(),
            retry_after: None,
        }
    }

    pub(crate) fn client_cancelled() -> Self {
        Self {
            status: StatusCode::BAD_GATEWAY,
            code: "client_cancelled",
            kind: "cancelled_error",
            message: "The client disconnected.".to_owned(),
            retry_after: None,
        }
    }

    pub(crate) const fn status(&self) -> StatusCode {
        self.status
    }

    pub(crate) const fn code(&self) -> &'static str {
        self.code
    }

    pub(crate) const fn kind(&self) -> &'static str {
        self.kind
    }

    pub(crate) fn message(&self) -> &str {
        &self.message
    }

    pub(crate) const fn retry_after(&self) -> Option<Duration> {
        self.retry_after
    }

    pub(crate) fn into_problem(self) -> Problem {
        self.into()
    }

    pub(crate) fn from_transport(error: TransportError) -> Self {
        match error.class {
            AttemptFailureClass::RateLimit => Self {
                status: StatusCode::TOO_MANY_REQUESTS,
                code: "upstream_rate_limit",
                kind: "rate_limit_error",
                message: error.message,
                retry_after: None,
            },
            AttemptFailureClass::Timeout => Self::timeout(),
            AttemptFailureClass::UpstreamClient => {
                Self::bad_gateway("upstream_rejected", error.message)
            }
            AttemptFailureClass::Connect | AttemptFailureClass::UpstreamServer => {
                Self::bad_gateway("upstream_unavailable", error.message)
            }
            AttemptFailureClass::Protocol => {
                Self::bad_gateway("provider_protocol_error", error.message)
            }
            AttemptFailureClass::Cancelled => {
                Self::bad_gateway("provider_cancelled", error.message)
            }
            AttemptFailureClass::Ambiguous => {
                Self::bad_gateway("ambiguous_upstream_result", error.message)
            }
        }
    }

    pub(crate) fn from_canonical(error: &CanonicalError) -> Self {
        let status = match error.class {
            ErrorClass::Authentication => StatusCode::BAD_GATEWAY,
            ErrorClass::Authorization => StatusCode::BAD_GATEWAY,
            ErrorClass::InvalidRequest => StatusCode::BAD_GATEWAY,
            ErrorClass::RateLimit => StatusCode::TOO_MANY_REQUESTS,
            ErrorClass::Timeout => StatusCode::GATEWAY_TIMEOUT,
            ErrorClass::Transport | ErrorClass::Upstream | ErrorClass::Internal => {
                StatusCode::BAD_GATEWAY
            }
        };
        Self {
            status,
            code: "upstream_error",
            kind: crate::openai_response::error_type(error.class),
            message: error.message.clone(),
            retry_after: None,
        }
    }
}

#[derive(Serialize)]
struct OpenAiErrorEnvelope<'a> {
    error: OpenAiErrorBody<'a>,
}

#[derive(Serialize)]
struct OpenAiErrorBody<'a> {
    message: &'a str,
    #[serde(rename = "type")]
    kind: &'a str,
    param: Option<&'a str>,
    code: &'a str,
}

impl IntoResponse for InferenceError {
    fn into_response(self) -> Response {
        let mut response = (
            self.status,
            Json(OpenAiErrorEnvelope {
                error: OpenAiErrorBody {
                    message: &self.message,
                    kind: self.kind,
                    param: None,
                    code: self.code,
                },
            }),
        )
            .into_response();
        if let Some(retry_after) = self.retry_after {
            let seconds = retry_after.as_secs().max(1).to_string();
            if let Ok(value) = HeaderValue::from_str(&seconds) {
                response.headers_mut().insert(header::RETRY_AFTER, value);
            }
        }
        response
    }
}

impl From<InferenceError> for Problem {
    fn from(error: InferenceError) -> Self {
        Problem::new(error.status, error.code, error.kind, error.message)
    }
}

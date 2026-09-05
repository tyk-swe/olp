use std::time::Duration;

use axum::{
    Json,
    extract::rejection::JsonRejection,
    http::{HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use olp_engine::domain::{
    canonical::events::{Error, ErrorClass},
    ports::TransportError,
};
use olp_engine::inference::{
    error::{Error as CoreInferenceError, Kind as InferenceErrorKind},
    limits::LimitDimension,
};
use serde::Serialize;

use crate::public_http::problem::Problem;

/// Delivery adapter for transport-neutral inference failures.
///
/// `olp_engine::inference` owns the error's classification, code, message, and retry
/// policy. The gateway only maps that stable contract to HTTP status codes and
/// OpenAI-compatible error envelopes.
#[derive(Debug)]
pub(crate) struct InferenceError(CoreInferenceError);

pub(super) fn valid_json<T>(
    payload: Result<Json<T>, JsonRejection>,
) -> Result<Json<T>, InferenceError> {
    payload.map_err(|error| {
        InferenceError::invalid_request(format!("The JSON request is invalid: {error}"))
    })
}

impl InferenceError {
    pub(crate) fn accounting_outcome(&self) -> olp_engine::inference::accounting::RequestOutcome {
        olp_engine::inference::accounting::RequestOutcome::failure(
            (self.code() != "client_cancelled").then_some(self.status().as_u16()),
            self.code(),
        )
    }

    pub(crate) fn unauthorized() -> Self {
        CoreInferenceError::unauthorized().into()
    }

    pub(crate) fn forbidden(message: impl Into<String>) -> Self {
        CoreInferenceError::forbidden(message).into()
    }

    pub(crate) fn invalid_request(message: impl Into<String>) -> Self {
        CoreInferenceError::invalid_request(message).into()
    }

    pub(super) fn payload_too_large(code: &'static str) -> Self {
        CoreInferenceError::payload_too_large(code).into()
    }

    pub(crate) fn not_found(message: impl Into<String>) -> Self {
        CoreInferenceError::not_found(message).into()
    }

    pub(super) fn resource_not_found(code: &'static str) -> Self {
        CoreInferenceError::resource_not_found(code).into()
    }

    pub(crate) fn conflict(code: &'static str, message: impl Into<String>) -> Self {
        CoreInferenceError::conflict(code, message).into()
    }

    pub(crate) fn rate_limited(dimension: LimitDimension, retry_after: Duration) -> Self {
        CoreInferenceError::rate_limited(dimension, retry_after).into()
    }

    pub(crate) fn unavailable(code: &'static str) -> Self {
        CoreInferenceError::unavailable(code).into()
    }

    pub(crate) fn overloaded() -> Self {
        CoreInferenceError::overloaded().into()
    }

    pub(super) fn multipart_parser_timeout() -> Self {
        CoreInferenceError::multipart_parser_timeout().into()
    }

    pub(crate) fn timeout() -> Self {
        CoreInferenceError::timeout().into()
    }

    pub(crate) fn bad_gateway(code: &'static str, message: impl Into<String>) -> Self {
        CoreInferenceError::bad_gateway(code, message).into()
    }

    pub(crate) fn client_cancelled() -> Self {
        CoreInferenceError::client_cancelled().into()
    }

    pub(crate) fn status(&self) -> StatusCode {
        presentation(self.0.kind()).0
    }

    pub(crate) const fn code(&self) -> &'static str {
        self.0.code()
    }

    pub(crate) fn kind(&self) -> &'static str {
        presentation(self.0.kind()).1
    }

    pub(crate) fn message(&self) -> &str {
        self.0.message()
    }

    pub(crate) const fn retry_after(&self) -> Option<Duration> {
        self.0.retry_after()
    }

    pub(crate) fn into_problem(self) -> Problem {
        self.into()
    }

    pub(crate) fn from_transport(error: TransportError) -> Self {
        CoreInferenceError::from_transport(error).into()
    }

    pub(crate) fn from_canonical(error: &Error) -> Self {
        CoreInferenceError::from_canonical(error).into()
    }
}

impl From<CoreInferenceError> for InferenceError {
    fn from(error: CoreInferenceError) -> Self {
        Self(error)
    }
}

/// The client disconnected. Nothing is delivered, but the status is recorded,
/// so it must not read as a gateway failure the operator should page on.
pub(super) const CLIENT_CLOSED_REQUEST: u16 = 499;

fn presentation(kind: InferenceErrorKind) -> (StatusCode, &'static str) {
    match kind {
        InferenceErrorKind::Authentication => (StatusCode::UNAUTHORIZED, "authentication_error"),
        InferenceErrorKind::Permission => (StatusCode::FORBIDDEN, "permission_error"),
        InferenceErrorKind::InvalidRequest => (StatusCode::BAD_REQUEST, "invalid_request_error"),
        InferenceErrorKind::PayloadTooLarge => {
            (StatusCode::PAYLOAD_TOO_LARGE, "invalid_request_error")
        }
        InferenceErrorKind::NotFound => (StatusCode::NOT_FOUND, "invalid_request_error"),
        InferenceErrorKind::Conflict => (StatusCode::CONFLICT, "invalid_request_error"),
        InferenceErrorKind::RateLimit => (StatusCode::TOO_MANY_REQUESTS, "rate_limit_error"),
        InferenceErrorKind::Unavailable => (StatusCode::SERVICE_UNAVAILABLE, "server_error"),
        InferenceErrorKind::RequestTimeout => {
            (StatusCode::REQUEST_TIMEOUT, "invalid_request_error")
        }
        InferenceErrorKind::GatewayTimeout => (StatusCode::GATEWAY_TIMEOUT, "server_error"),
        InferenceErrorKind::Upstream => (StatusCode::BAD_GATEWAY, "server_error"),
        InferenceErrorKind::UpstreamRejected(status) => (
            StatusCode::from_u16(status).unwrap_or(StatusCode::BAD_GATEWAY),
            "invalid_request_error",
        ),
        InferenceErrorKind::Cancelled => (
            StatusCode::from_u16(CLIENT_CLOSED_REQUEST).unwrap_or(StatusCode::BAD_GATEWAY),
            "invalid_request_error",
        ),
        InferenceErrorKind::Canonical(class) => match class {
            // Upstream credential failures are our problem, not the caller's, so
            // they are reported as a gateway-side error rather than a 401/403.
            ErrorClass::Authentication | ErrorClass::Authorization => {
                (StatusCode::BAD_GATEWAY, "server_error")
            }
            other => {
                let status = match other {
                    ErrorClass::InvalidRequest => StatusCode::BAD_REQUEST,
                    ErrorClass::RateLimit => StatusCode::TOO_MANY_REQUESTS,
                    ErrorClass::Timeout => StatusCode::GATEWAY_TIMEOUT,
                    ErrorClass::Authentication
                    | ErrorClass::Authorization
                    | ErrorClass::Transport
                    | ErrorClass::Upstream
                    | ErrorClass::Internal => StatusCode::BAD_GATEWAY,
                };
                (status, olp_engine::protocols::openai::error_type(other))
            }
        },
    }
}

pub(super) fn openai_error_response(
    status: StatusCode,
    code: &str,
    kind: &str,
    message: &str,
    retry_after: Option<Duration>,
    authenticate: bool,
) -> Response {
    let mut response = (
        status,
        Json(OpenAiErrorEnvelope {
            error: OpenAiErrorBody {
                message,
                kind,
                param: None,
                code,
            },
        }),
    )
        .into_response();
    if authenticate {
        response
            .headers_mut()
            .insert(header::WWW_AUTHENTICATE, HeaderValue::from_static("Bearer"));
    }
    insert_retry_after_header(&mut response, retry_after);
    response
}

pub(super) fn insert_retry_after_header(response: &mut Response, retry_after: Option<Duration>) {
    if let Some(retry_after) = retry_after {
        // Retry-After only accepts whole delta-seconds. Rounding down could
        // tell a client to retry before the limiter's actual reset time.
        let seconds = retry_after
            .as_secs()
            .saturating_add(u64::from(retry_after.subsec_nanos() != 0))
            .max(1)
            .to_string();
        if let Ok(value) = HeaderValue::from_str(&seconds) {
            response.headers_mut().insert(header::RETRY_AFTER, value);
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
        openai_error_response(
            self.status(),
            self.code(),
            self.kind(),
            self.message(),
            self.retry_after(),
            false,
        )
    }
}

impl From<InferenceError> for Problem {
    fn from(error: InferenceError) -> Self {
        Problem::new(
            error.status(),
            error.code(),
            error.kind(),
            error.message().to_owned(),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn presentation_covers_the_transport_neutral_error_contract() {
        let cases = [
            (
                CoreInferenceError::new(
                    InferenceErrorKind::Authentication,
                    "code",
                    "message",
                    None,
                ),
                StatusCode::UNAUTHORIZED,
                "authentication_error",
            ),
            (
                CoreInferenceError::new(InferenceErrorKind::Permission, "code", "message", None),
                StatusCode::FORBIDDEN,
                "permission_error",
            ),
            (
                CoreInferenceError::new(
                    InferenceErrorKind::InvalidRequest,
                    "code",
                    "message",
                    None,
                ),
                StatusCode::BAD_REQUEST,
                "invalid_request_error",
            ),
            (
                CoreInferenceError::new(
                    InferenceErrorKind::PayloadTooLarge,
                    "code",
                    "message",
                    None,
                ),
                StatusCode::PAYLOAD_TOO_LARGE,
                "invalid_request_error",
            ),
            (
                CoreInferenceError::new(InferenceErrorKind::NotFound, "code", "message", None),
                StatusCode::NOT_FOUND,
                "invalid_request_error",
            ),
            (
                CoreInferenceError::new(InferenceErrorKind::Conflict, "code", "message", None),
                StatusCode::CONFLICT,
                "invalid_request_error",
            ),
            (
                CoreInferenceError::new(InferenceErrorKind::RateLimit, "code", "message", None),
                StatusCode::TOO_MANY_REQUESTS,
                "rate_limit_error",
            ),
            (
                CoreInferenceError::new(InferenceErrorKind::Unavailable, "code", "message", None),
                StatusCode::SERVICE_UNAVAILABLE,
                "server_error",
            ),
            (
                CoreInferenceError::new(
                    InferenceErrorKind::RequestTimeout,
                    "code",
                    "message",
                    None,
                ),
                StatusCode::REQUEST_TIMEOUT,
                "invalid_request_error",
            ),
            (
                CoreInferenceError::new(
                    InferenceErrorKind::GatewayTimeout,
                    "code",
                    "message",
                    None,
                ),
                StatusCode::GATEWAY_TIMEOUT,
                "server_error",
            ),
            (
                CoreInferenceError::new(InferenceErrorKind::Upstream, "code", "message", None),
                StatusCode::BAD_GATEWAY,
                "server_error",
            ),
            (
                CoreInferenceError::new(
                    InferenceErrorKind::UpstreamRejected(422),
                    "upstream_rejected",
                    "message",
                    None,
                ),
                StatusCode::UNPROCESSABLE_ENTITY,
                "invalid_request_error",
            ),
            (
                CoreInferenceError::new(
                    InferenceErrorKind::UpstreamRejected(409),
                    "upstream_rejected",
                    "message",
                    None,
                ),
                StatusCode::CONFLICT,
                "invalid_request_error",
            ),
            (
                CoreInferenceError::new(InferenceErrorKind::Cancelled, "code", "message", None),
                StatusCode::from_u16(CLIENT_CLOSED_REQUEST).unwrap(),
                "invalid_request_error",
            ),
            (
                CoreInferenceError::new(
                    InferenceErrorKind::Canonical(ErrorClass::RateLimit),
                    "code",
                    "message",
                    None,
                ),
                StatusCode::TOO_MANY_REQUESTS,
                "rate_limit_error",
            ),
            (
                CoreInferenceError::new(
                    InferenceErrorKind::Canonical(ErrorClass::Internal),
                    "code",
                    "message",
                    None,
                ),
                StatusCode::BAD_GATEWAY,
                "server_error",
            ),
            (
                CoreInferenceError::new(
                    InferenceErrorKind::Canonical(ErrorClass::InvalidRequest),
                    "code",
                    "message",
                    None,
                ),
                StatusCode::BAD_REQUEST,
                "invalid_request_error",
            ),
            (
                CoreInferenceError::new(
                    InferenceErrorKind::Canonical(ErrorClass::Authentication),
                    "code",
                    "message",
                    None,
                ),
                StatusCode::BAD_GATEWAY,
                "server_error",
            ),
        ];

        for (core, status, kind) in cases {
            let error = InferenceError::from(core);
            assert_eq!(error.status(), status);
            assert_eq!(error.kind(), kind);
        }
    }

    #[test]
    fn retry_after_delta_seconds_round_up() {
        for (retry_after, expected) in [
            (Duration::ZERO, "1"),
            (Duration::from_nanos(1), "1"),
            (Duration::from_secs(1), "1"),
            (Duration::from_millis(1_001), "2"),
        ] {
            let response = openai_error_response(
                StatusCode::TOO_MANY_REQUESTS,
                "rate_limit_exceeded",
                "rate_limit_error",
                "retry later",
                Some(retry_after),
                false,
            );
            assert_eq!(
                response.headers().get(header::RETRY_AFTER).unwrap(),
                expected
            );
        }
    }
}

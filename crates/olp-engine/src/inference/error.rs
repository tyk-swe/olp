use std::{fmt, time::Duration};

use crate::domain::{AttemptFailureClass, CanonicalError, ErrorClass, TransportError};
use crate::inference::limits::LimitDimension;

/// Transport-neutral classification used by delivery adapters to select a
/// public status and vendor error envelope.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum InferenceErrorKind {
    Authentication,
    Permission,
    InvalidRequest,
    PayloadTooLarge,
    NotFound,
    Conflict,
    RateLimit,
    Unavailable,
    RequestTimeout,
    GatewayTimeout,
    Upstream,
    Cancelled,
    Canonical(ErrorClass),
}

pub struct InferenceError {
    kind: InferenceErrorKind,
    code: &'static str,
    message: String,
    retry_after: Option<Duration>,
}

impl fmt::Debug for InferenceError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("InferenceError")
            .field("kind", &self.kind)
            .field("code", &self.code)
            .field("message", &"[REDACTED]")
            .field("retry_after", &self.retry_after)
            .finish()
    }
}

impl InferenceError {
    #[must_use]
    pub fn new(
        kind: InferenceErrorKind,
        code: &'static str,
        message: impl Into<String>,
        retry_after: Option<Duration>,
    ) -> Self {
        Self {
            kind,
            code,
            message: message.into(),
            retry_after,
        }
    }

    #[must_use]
    pub fn unauthorized() -> Self {
        Self::new(
            InferenceErrorKind::Authentication,
            "invalid_api_key",
            "The API key is invalid or unavailable.",
            None,
        )
    }

    #[must_use]
    pub fn forbidden(message: impl Into<String>) -> Self {
        Self::new(
            InferenceErrorKind::Permission,
            "permission_denied",
            message,
            None,
        )
    }

    #[must_use]
    pub fn invalid_request(message: impl Into<String>) -> Self {
        Self::new(
            InferenceErrorKind::InvalidRequest,
            "invalid_request",
            message,
            None,
        )
    }

    #[must_use]
    pub fn payload_too_large(code: &'static str) -> Self {
        Self::new(
            InferenceErrorKind::PayloadTooLarge,
            code,
            "The uploaded media exceeds the configured limit.",
            None,
        )
    }

    #[must_use]
    pub fn not_found(message: impl Into<String>) -> Self {
        Self::new(
            InferenceErrorKind::NotFound,
            "route_not_found",
            message,
            None,
        )
    }

    #[must_use]
    pub fn resource_not_found(code: &'static str) -> Self {
        Self::new(
            InferenceErrorKind::NotFound,
            code,
            "The requested resource was not found.",
            None,
        )
    }

    #[must_use]
    pub fn conflict(code: &'static str, message: impl Into<String>) -> Self {
        Self::new(InferenceErrorKind::Conflict, code, message, None)
    }

    #[must_use]
    pub fn rate_limited(dimension: LimitDimension, retry_after: Duration) -> Self {
        let name = match dimension {
            LimitDimension::Requests => "requests per minute",
            LimitDimension::Tokens => "tokens per minute",
            LimitDimension::Concurrency => "concurrency",
            LimitDimension::Unknown => "configured",
        };
        Self::new(
            InferenceErrorKind::RateLimit,
            "rate_limit_exceeded",
            format!("The API key {name} limit was exceeded."),
            Some(retry_after),
        )
    }

    #[must_use]
    pub fn unavailable(code: &'static str) -> Self {
        Self::new(
            InferenceErrorKind::Unavailable,
            code,
            "The gateway is temporarily unavailable.",
            None,
        )
    }

    #[must_use]
    pub fn overloaded() -> Self {
        Self::new(
            InferenceErrorKind::Unavailable,
            "request_admission_overloaded",
            "The gateway is temporarily overloaded.",
            Some(Duration::from_secs(1)),
        )
    }

    #[must_use]
    pub fn multipart_parser_timeout() -> Self {
        Self::new(
            InferenceErrorKind::RequestTimeout,
            "multipart_parser_timeout",
            "The multipart upload exceeded its parser deadline.",
            None,
        )
    }

    #[must_use]
    pub fn timeout() -> Self {
        Self::new(
            InferenceErrorKind::GatewayTimeout,
            "gateway_timeout",
            "The route deadline elapsed.",
            None,
        )
    }

    #[must_use]
    pub fn bad_gateway(code: &'static str, message: impl Into<String>) -> Self {
        Self::new(InferenceErrorKind::Upstream, code, message, None)
    }

    #[must_use]
    pub fn client_cancelled() -> Self {
        Self::new(
            InferenceErrorKind::Cancelled,
            "client_cancelled",
            "The client disconnected.",
            None,
        )
    }

    #[must_use]
    pub const fn kind(&self) -> InferenceErrorKind {
        self.kind
    }

    #[must_use]
    pub const fn code(&self) -> &'static str {
        self.code
    }

    #[must_use]
    pub fn message(&self) -> &str {
        &self.message
    }

    #[must_use]
    pub const fn retry_after(&self) -> Option<Duration> {
        self.retry_after
    }

    #[must_use]
    pub fn from_transport(error: TransportError) -> Self {
        match error.class {
            AttemptFailureClass::RateLimit => Self::new(
                InferenceErrorKind::RateLimit,
                "upstream_rate_limit",
                error.message,
                None,
            ),
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

    #[must_use]
    pub fn from_canonical(error: &CanonicalError) -> Self {
        Self::new(
            InferenceErrorKind::Canonical(error.class),
            "upstream_error",
            error.message.clone(),
            None,
        )
    }
}

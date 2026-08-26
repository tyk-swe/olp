use std::{fmt, time::Duration};

use crate::domain::{
    canonical::events::{Error as CanonicalError, ErrorClass},
    ports::{AttemptFailureClass, TransportError},
};
use crate::inference::limits::LimitDimension;

/// Transport-neutral classification used by delivery adapters to select a
/// public status and vendor error envelope.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Kind {
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
    /// An upstream 4xx the gateway forwards with the provider's own status. A
    /// permanently invalid request must not look like a retryable 502.
    UpstreamRejected(u16),
    Cancelled,
    Canonical(ErrorClass),
}

pub struct Error {
    kind: Kind,
    code: &'static str,
    message: String,
    retry_after: Option<Duration>,
}

impl fmt::Debug for Error {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Error")
            .field("kind", &self.kind)
            .field("code", &self.code)
            .field("message", &"[REDACTED]")
            .field("retry_after", &self.retry_after)
            .finish()
    }
}

impl Error {
    #[must_use]
    pub fn new(
        kind: Kind,
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
            Kind::Authentication,
            "invalid_api_key",
            "The API key is invalid or unavailable.",
            None,
        )
    }

    #[must_use]
    pub fn forbidden(message: impl Into<String>) -> Self {
        Self::new(Kind::Permission, "permission_denied", message, None)
    }

    #[must_use]
    pub fn invalid_request(message: impl Into<String>) -> Self {
        Self::new(Kind::InvalidRequest, "invalid_request", message, None)
    }

    #[must_use]
    pub fn payload_too_large(code: &'static str) -> Self {
        Self::new(
            Kind::PayloadTooLarge,
            code,
            "The uploaded media exceeds the configured limit.",
            None,
        )
    }

    #[must_use]
    pub fn not_found(message: impl Into<String>) -> Self {
        Self::new(Kind::NotFound, "route_not_found", message, None)
    }

    #[must_use]
    pub fn resource_not_found(code: &'static str) -> Self {
        Self::new(
            Kind::NotFound,
            code,
            "The requested resource was not found.",
            None,
        )
    }

    #[must_use]
    pub fn conflict(code: &'static str, message: impl Into<String>) -> Self {
        Self::new(Kind::Conflict, code, message, None)
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
            Kind::RateLimit,
            "rate_limit_exceeded",
            format!("The API key {name} limit was exceeded."),
            Some(retry_after),
        )
    }

    /// A request whose own estimate cannot fit the key's per-minute token
    /// budget can never succeed, however long the caller waits. Reporting it as
    /// a 429 with a `Retry-After` sends conforming clients into an endless
    /// retry loop, so it is a client error with no retry hint.
    #[must_use]
    pub fn request_exceeds_token_limit(estimate: i64, tokens_per_minute: i64) -> Self {
        Self::new(
            Kind::InvalidRequest,
            "request_exceeds_token_limit",
            format!(
                "This request needs about {estimate} tokens, which is more than the API key's \
                 limit of {tokens_per_minute} tokens per minute. Retrying cannot succeed; \
                 shorten the request, lower max_output_tokens, or raise the key's limit."
            ),
            None,
        )
    }

    #[must_use]
    pub fn unavailable(code: &'static str) -> Self {
        Self::new(
            Kind::Unavailable,
            code,
            "The gateway is temporarily unavailable.",
            None,
        )
    }

    #[must_use]
    pub fn overloaded() -> Self {
        Self::new(
            Kind::Unavailable,
            "request_admission_overloaded",
            "The gateway is temporarily overloaded.",
            Some(Duration::from_secs(1)),
        )
    }

    #[must_use]
    pub fn multipart_parser_timeout() -> Self {
        Self::new(
            Kind::RequestTimeout,
            "multipart_parser_timeout",
            "The multipart upload exceeded its parser deadline.",
            None,
        )
    }

    #[must_use]
    pub fn timeout() -> Self {
        Self::new(
            Kind::GatewayTimeout,
            "gateway_timeout",
            "The route deadline elapsed.",
            None,
        )
    }

    #[must_use]
    pub fn bad_gateway(code: &'static str, message: impl Into<String>) -> Self {
        Self::new(Kind::Upstream, code, message, None)
    }

    #[must_use]
    pub fn client_cancelled() -> Self {
        Self::new(
            Kind::Cancelled,
            "client_cancelled",
            "The client disconnected.",
            None,
        )
    }

    #[must_use]
    pub const fn kind(&self) -> Kind {
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
                Kind::RateLimit,
                "upstream_rate_limit",
                error.message,
                error.upstream.retry_after,
            ),
            AttemptFailureClass::Timeout => Self::timeout(),
            AttemptFailureClass::UpstreamClient => match error.upstream.status {
                Some(401) => Self::bad_gateway("upstream_authentication_failed", error.message),
                Some(403) => Self::bad_gateway("upstream_permission_denied", error.message),
                Some(status) if forwardable_upstream_status(status) => Self::new(
                    Kind::UpstreamRejected(status),
                    "upstream_rejected",
                    error.message,
                    None,
                ),
                _ => Self::bad_gateway("upstream_rejected", error.message),
            },
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
            Kind::Canonical(error.class),
            "upstream_error",
            error.message.clone(),
            None,
        )
    }
}

/// Upstream client-fault statuses the gateway repeats verbatim. Anything else
/// a provider might answer with stays a 502: the caller cannot act on it, and
/// inventing a status the provider never sent would be worse than a bad gateway.
/// 401 and 403 are deliberately absent: a rejected provider credential is the
/// operator's fault, and repeating it would tell the caller their own gateway
/// key is invalid.
const fn forwardable_upstream_status(status: u16) -> bool {
    matches!(status, 400 | 404 | 405 | 409 | 413 | 415 | 422)
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use crate::domain::{
        canonical::events::{Error as CanonicalError, ErrorClass},
        ports::{AttemptFailureClass, TransportError, TransportPhase, UpstreamSignal},
    };
    use crate::inference::limits::LimitDimension;

    use super::{Error, Kind};

    #[test]
    fn rate_limit_messages_identify_every_dimension() {
        let retry_after = Duration::from_millis(750);
        for (dimension, name) in [
            (LimitDimension::Requests, "requests per minute"),
            (LimitDimension::Tokens, "tokens per minute"),
            (LimitDimension::Concurrency, "concurrency"),
            (LimitDimension::Unknown, "configured"),
        ] {
            let error = Error::rate_limited(dimension, retry_after);
            assert_eq!(error.kind(), Kind::RateLimit);
            assert_eq!(error.code(), "rate_limit_exceeded");
            assert_eq!(
                error.message(),
                format!("The API key {name} limit was exceeded.")
            );
            assert_eq!(error.retry_after(), Some(retry_after));
        }
    }

    #[test]
    fn every_transport_failure_class_has_an_explicit_public_mapping() {
        use AttemptFailureClass as AFC;
        use Kind as IEK;

        let cases = [
            (
                AFC::RateLimit,
                IEK::RateLimit,
                "upstream_rate_limit",
                "provider detail",
            ),
            (
                AFC::Timeout,
                IEK::GatewayTimeout,
                "gateway_timeout",
                "The route deadline elapsed.",
            ),
            (
                AFC::UpstreamClient,
                IEK::Upstream,
                "upstream_rejected",
                "provider detail",
            ),
            (
                AFC::Connect,
                IEK::Upstream,
                "upstream_unavailable",
                "provider detail",
            ),
            (
                AFC::UpstreamServer,
                IEK::Upstream,
                "upstream_unavailable",
                "provider detail",
            ),
            (
                AFC::Protocol,
                IEK::Upstream,
                "provider_protocol_error",
                "provider detail",
            ),
            (
                AFC::Cancelled,
                IEK::Upstream,
                "provider_cancelled",
                "provider detail",
            ),
            (
                AFC::Ambiguous,
                IEK::Upstream,
                "ambiguous_upstream_result",
                "provider detail",
            ),
        ];

        for (class, kind, code, message) in cases {
            let error = Error::from_transport(TransportError {
                upstream: Default::default(),
                phase: TransportPhase::FirstByte,
                class,
                response_committed: false,
                message: "provider detail".to_owned(),
            });
            assert_eq!(error.kind(), kind, "unexpected kind for {class:?}");
            assert_eq!(error.code(), code, "unexpected code for {class:?}");
            assert_eq!(error.message(), message, "unexpected message for {class:?}");
        }
    }

    #[test]
    fn an_upstream_client_fault_keeps_the_provider_status_and_a_rate_limit_keeps_retry_after() {
        for (status, expected) in [
            (400, Kind::UpstreamRejected(400)),
            (404, Kind::UpstreamRejected(404)),
            (413, Kind::UpstreamRejected(413)),
            (422, Kind::UpstreamRejected(422)),
            // Not a status a caller can act on: stays a bad gateway.
            (418, Kind::Upstream),
            (451, Kind::Upstream),
        ] {
            let error = Error::from_transport(TransportError {
                phase: TransportPhase::FirstByte,
                class: AttemptFailureClass::UpstreamClient,
                response_committed: false,
                message: "context length exceeded".to_owned(),
                upstream: UpstreamSignal::from_status(status),
            });
            assert_eq!(error.kind(), expected, "status {status}");
            assert_eq!(error.code(), "upstream_rejected");
            assert_eq!(error.message(), "context length exceeded");
        }

        let unknown = Error::from_transport(TransportError {
            phase: TransportPhase::FirstByte,
            class: AttemptFailureClass::UpstreamClient,
            response_committed: false,
            message: "no status observed".to_owned(),
            upstream: UpstreamSignal::default(),
        });
        assert_eq!(unknown.kind(), Kind::Upstream);
    }

    #[test]
    fn a_rejected_provider_credential_is_a_bad_gateway_not_the_callers_auth_failure() {
        for (status, code) in [
            (401, "upstream_authentication_failed"),
            (403, "upstream_permission_denied"),
        ] {
            let error = Error::from_transport(TransportError {
                phase: TransportPhase::FirstByte,
                class: AttemptFailureClass::UpstreamClient,
                response_committed: false,
                message: "invalid api key".to_owned(),
                upstream: UpstreamSignal::from_status(status),
            });
            assert_eq!(error.kind(), Kind::Upstream, "status {status}");
            assert_eq!(error.code(), code, "status {status}");
            assert_eq!(error.message(), "invalid api key");
        }
    }

    #[test]
    fn an_upstream_rate_limit_forwards_its_retry_after() {
        let error = Error::from_transport(TransportError {
            phase: TransportPhase::FirstByte,
            class: AttemptFailureClass::RateLimit,
            response_committed: false,
            message: "slow down".to_owned(),
            upstream: UpstreamSignal::from_status(429)
                .with_retry_after(Some(Duration::from_secs(60))),
        });
        assert_eq!(error.kind(), Kind::RateLimit);
        assert_eq!(error.retry_after(), Some(Duration::from_secs(60)));
    }

    #[test]
    fn an_absurd_upstream_retry_after_is_clamped() {
        let signal = UpstreamSignal::from_status(429)
            .with_retry_after(Some(Duration::from_secs(24 * 60 * 60)));
        assert_eq!(
            signal.retry_after,
            Some(crate::domain::ports::MAX_UPSTREAM_RETRY_AFTER)
        );
    }

    #[test]
    fn canonical_mapping_clones_the_safe_message_and_debug_redacts_it() {
        let canonical = CanonicalError {
            class: ErrorClass::Authorization,
            message: "private provider detail".to_owned(),
            provider_code: Some("private-code".to_owned()),
            retryable: false,
        };
        let error = Error::from_canonical(&canonical);

        assert_eq!(error.kind(), Kind::Canonical(ErrorClass::Authorization));
        assert_eq!(error.code(), "upstream_error");
        assert_eq!(error.message(), canonical.message);
        let debug = format!("{error:?}");
        assert!(debug.contains("[REDACTED]"));
        assert!(!debug.contains("private provider detail"));
        assert!(!debug.contains("private-code"));
    }
}

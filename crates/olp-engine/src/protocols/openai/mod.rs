pub mod audio;
pub mod chat;
pub mod client;
pub mod embeddings;
use crate::domain::canonical::events::ErrorClass;
use crate::protocols::extensions;
pub mod images;
pub mod media;
pub mod moderation;
pub mod response;
pub mod responses;
pub mod video;

/// One taxonomy for "this OpenAI error is an upstream rate limit" across the
/// unary Responses decoder, the Responses stream decoder, and the Chat
/// Completions decoder, so retryability cannot diverge per surface.
pub(in crate::protocols) fn error_signals_rate_limit(
    code: Option<&str>,
    kind: Option<&str>,
) -> bool {
    code.is_some_and(|code| code.contains("rate_limit"))
        || kind.is_some_and(|kind| kind.contains("rate_limit"))
}

/// OpenAI publishes a closed set of `error.type` values. Inventing new ones
/// (`upstream_error`, `internal_error`, `timeout_error`, …) means every client
/// that branches on the type falls through its own error handling, so a
/// server-side fault is reported as OpenAI reports one and the specific cause
/// stays in `error.code`.
#[must_use]
pub fn error_type(class: ErrorClass) -> &'static str {
    match class {
        ErrorClass::Authentication => "authentication_error",
        ErrorClass::Authorization => "permission_error",
        ErrorClass::InvalidRequest => "invalid_request_error",
        ErrorClass::RateLimit => "rate_limit_error",
        ErrorClass::Timeout
        | ErrorClass::Transport
        | ErrorClass::Upstream
        | ErrorClass::Internal => "server_error",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn canonical_error_classes_have_stable_openai_types() {
        for (class, expected) in [
            (ErrorClass::Authentication, "authentication_error"),
            (ErrorClass::Authorization, "permission_error"),
            (ErrorClass::InvalidRequest, "invalid_request_error"),
            (ErrorClass::RateLimit, "rate_limit_error"),
            (ErrorClass::Timeout, "server_error"),
            (ErrorClass::Transport, "server_error"),
            (ErrorClass::Upstream, "server_error"),
            (ErrorClass::Internal, "server_error"),
        ] {
            assert_eq!(error_type(class), expected);
        }
    }
}

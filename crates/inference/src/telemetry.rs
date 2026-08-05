use std::time::Duration;

use crate::{InferenceError, InferenceErrorKind};

pub(crate) const fn metadata_status_code(error: &InferenceError) -> u16 {
    match error.kind() {
        InferenceErrorKind::Authentication => 401,
        InferenceErrorKind::Permission => 403,
        InferenceErrorKind::InvalidRequest => 400,
        InferenceErrorKind::PayloadTooLarge => 413,
        InferenceErrorKind::NotFound => 404,
        InferenceErrorKind::Conflict => 409,
        InferenceErrorKind::RateLimit => 429,
        InferenceErrorKind::Unavailable => 503,
        InferenceErrorKind::RequestTimeout => 408,
        InferenceErrorKind::GatewayTimeout => 504,
        InferenceErrorKind::Upstream | InferenceErrorKind::Cancelled => 502,
        InferenceErrorKind::Canonical(class) => match class {
            olp_domain::ErrorClass::RateLimit => 429,
            olp_domain::ErrorClass::Timeout => 504,
            olp_domain::ErrorClass::Authentication
            | olp_domain::ErrorClass::Authorization
            | olp_domain::ErrorClass::InvalidRequest
            | olp_domain::ErrorClass::Transport
            | olp_domain::ErrorClass::Upstream
            | olp_domain::ErrorClass::Internal => 502,
        },
    }
}

pub(crate) fn elapsed_ms(duration: Duration) -> u64 {
    u64::try_from(duration.as_millis()).unwrap_or(u64::MAX)
}

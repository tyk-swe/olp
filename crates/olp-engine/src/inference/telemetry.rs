use std::time::Duration;

use crate::inference::{InferenceError, InferenceErrorKind};

pub(in crate::inference) const fn metadata_status_code(error: &InferenceError) -> u16 {
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
            crate::domain::ErrorClass::RateLimit => 429,
            crate::domain::ErrorClass::Timeout => 504,
            crate::domain::ErrorClass::Authentication
            | crate::domain::ErrorClass::Authorization
            | crate::domain::ErrorClass::InvalidRequest
            | crate::domain::ErrorClass::Transport
            | crate::domain::ErrorClass::Upstream
            | crate::domain::ErrorClass::Internal => 502,
        },
    }
}

pub(in crate::inference) fn elapsed_ms(duration: Duration) -> u64 {
    u64::try_from(duration.as_millis()).unwrap_or(u64::MAX)
}

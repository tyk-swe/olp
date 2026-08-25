use std::time::Duration;

use crate::inference::error::{Error as InferenceError, Kind as InferenceErrorKind};

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
        InferenceErrorKind::Upstream => 502,
        InferenceErrorKind::UpstreamRejected(status) => status,
        InferenceErrorKind::Cancelled => 499,
        InferenceErrorKind::Canonical(class) => canonical_status_code(class),
    }
}

/// A canonical provider error keeps the fault's own class: an upstream
/// authentication or validation failure is not a gateway failure.
pub(in crate::inference) const fn canonical_status_code(
    class: crate::domain::canonical::events::ErrorClass,
) -> u16 {
    use crate::domain::canonical::events::ErrorClass;
    match class {
        ErrorClass::Authentication => 401,
        ErrorClass::Authorization => 403,
        ErrorClass::InvalidRequest => 400,
        ErrorClass::RateLimit => 429,
        ErrorClass::Timeout => 504,
        ErrorClass::Transport | ErrorClass::Upstream | ErrorClass::Internal => 502,
    }
}

pub(in crate::inference) fn elapsed_ms(duration: Duration) -> u64 {
    u64::try_from(duration.as_millis()).unwrap_or(u64::MAX)
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use crate::domain::canonical::events::ErrorClass;
    use crate::inference::error::{Error as InferenceError, Kind as InferenceErrorKind};

    use super::{elapsed_ms, metadata_status_code};

    #[test]
    fn metadata_statuses_cover_the_complete_error_taxonomy() {
        let direct_cases = [
            (InferenceErrorKind::Authentication, 401),
            (InferenceErrorKind::Permission, 403),
            (InferenceErrorKind::InvalidRequest, 400),
            (InferenceErrorKind::PayloadTooLarge, 413),
            (InferenceErrorKind::NotFound, 404),
            (InferenceErrorKind::Conflict, 409),
            (InferenceErrorKind::RateLimit, 429),
            (InferenceErrorKind::Unavailable, 503),
            (InferenceErrorKind::RequestTimeout, 408),
            (InferenceErrorKind::GatewayTimeout, 504),
            (InferenceErrorKind::Upstream, 502),
            (InferenceErrorKind::UpstreamRejected(422), 422),
            (InferenceErrorKind::Cancelled, 499),
        ];
        for (kind, expected) in direct_cases {
            let error = InferenceError::new(kind, "test", "test", None);
            assert_eq!(
                metadata_status_code(&error),
                expected,
                "status for {kind:?}"
            );
        }

        for (class, expected) in [
            (ErrorClass::RateLimit, 429),
            (ErrorClass::Timeout, 504),
            (ErrorClass::Authentication, 401),
            (ErrorClass::Authorization, 403),
            (ErrorClass::InvalidRequest, 400),
            (ErrorClass::Transport, 502),
            (ErrorClass::Upstream, 502),
            (ErrorClass::Internal, 502),
        ] {
            let error =
                InferenceError::new(InferenceErrorKind::Canonical(class), "test", "test", None);
            assert_eq!(
                metadata_status_code(&error),
                expected,
                "status for canonical {class:?}"
            );
        }
    }

    #[test]
    fn elapsed_milliseconds_are_exact_and_saturating() {
        assert_eq!(elapsed_ms(Duration::from_micros(1_999)), 1);
        assert_eq!(elapsed_ms(Duration::MAX), u64::MAX);
    }
}

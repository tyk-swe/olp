use std::time::{SystemTime, UNIX_EPOCH};

use axum::body::Bytes;
use olp_engine::domain::canonical::events::ErrorClass;
use olp_engine::protocols::sse::{Frame, encode_frame};
use serde_json::{Value, json};

use super::error::InferenceError;

pub(super) fn error_sse(error: &InferenceError) -> Bytes {
    sse_json(&json!({ "error": {
        "message": error.message(),
        "type": error.kind(),
        "param": null,
        "code": error.code()
    }}))
}

pub(super) fn error_type(class: ErrorClass) -> &'static str {
    match class {
        ErrorClass::Authentication => "authentication_error",
        ErrorClass::Authorization => "permission_error",
        ErrorClass::InvalidRequest => "invalid_request_error",
        ErrorClass::RateLimit => "rate_limit_error",
        ErrorClass::Timeout => "timeout_error",
        ErrorClass::Transport | ErrorClass::Upstream => "upstream_error",
        ErrorClass::Internal => "internal_error",
    }
}

pub(super) fn sse_json(value: &Value) -> Bytes {
    Bytes::from(
        encode_frame(&Frame {
            event: None,
            data: value.to_string(),
            id: None,
            retry_ms: None,
        })
        .expect("data-only SSE frame is valid"),
    )
}

pub(super) fn unix_seconds() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .ok()
        .and_then(|duration| i64::try_from(duration.as_secs()).ok())
        .unwrap_or_default()
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
            (ErrorClass::Timeout, "timeout_error"),
            (ErrorClass::Transport, "upstream_error"),
            (ErrorClass::Upstream, "upstream_error"),
            (ErrorClass::Internal, "internal_error"),
        ] {
            assert_eq!(error_type(class), expected);
        }
    }

    #[test]
    fn json_and_errors_are_encoded_as_complete_data_only_sse_frames() {
        assert_eq!(
            sse_json(&json!({"line": "one\ntwo"})),
            Bytes::from_static(b"data: {\"line\":\"one\\ntwo\"}\n\n")
        );

        let frame = error_sse(&InferenceError::conflict(
            "video_changed",
            "The video job changed.",
        ));
        let text = std::str::from_utf8(&frame).unwrap();
        assert!(text.starts_with("data: "));
        let payload = text.strip_prefix("data: ").unwrap().trim();
        let payload: Value = serde_json::from_str(payload).unwrap();
        assert_eq!(payload["error"]["message"], "The video job changed.");
        assert_eq!(payload["error"]["type"], "conflict_error");
        assert_eq!(payload["error"]["code"], "video_changed");
        assert!(payload["error"]["param"].is_null());
    }
}

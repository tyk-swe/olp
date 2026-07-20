use std::time::{SystemTime, UNIX_EPOCH};

use axum::body::Bytes;
use olp_domain::ErrorClass;
use olp_protocols::sse::{SseFrame, encode_frame};
use serde_json::{Value, json};

use super::InferenceError;

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
        encode_frame(&SseFrame {
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

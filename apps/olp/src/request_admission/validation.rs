use std::time::Duration;

use axum::body::{Body, to_bytes};

use crate::Problem;

const MAX_JSON_DEPTH: usize = 64;

pub(super) fn is_json_content_type(value: &str) -> bool {
    let media_type = value.split(';').next().unwrap_or_default().trim();
    media_type.eq_ignore_ascii_case("application/json")
        || media_type
            .to_ascii_lowercase()
            .strip_prefix("application/")
            .is_some_and(|subtype| subtype.ends_with("+json"))
}

pub(super) fn is_media_request(path: &str, content_type: &str) -> bool {
    let media_path = path.starts_with("/openai/v1/images/")
        || path.starts_with("/openai/v1/audio/")
        || path == "/openai/v1/videos";
    media_path
        && (content_type
            .split(';')
            .next()
            .is_some_and(|value| value.trim().eq_ignore_ascii_case("multipart/form-data"))
            || content_type.eq_ignore_ascii_case("application/octet-stream"))
}

pub(crate) fn validate_json_depth(bytes: &[u8]) -> Result<(), Problem> {
    let mut depth = 0_usize;
    let mut in_string = false;
    let mut escaped = false;
    for byte in bytes {
        if in_string {
            if escaped {
                escaped = false;
            } else if *byte == b'\\' {
                escaped = true;
            } else if *byte == b'"' {
                in_string = false;
            }
            continue;
        }
        match byte {
            b'"' => in_string = true,
            b'{' | b'[' => {
                depth = depth.saturating_add(1);
                if depth > MAX_JSON_DEPTH {
                    return Err(Problem::bad_request(
                        "json_too_deep",
                        "The JSON document exceeds the maximum nesting depth of 64.",
                    ));
                }
            }
            b'}' | b']' => depth = depth.saturating_sub(1),
            _ => {}
        }
    }
    Ok(())
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum JsonBodyReadError {
    Rejected,
    Timeout,
}

pub(crate) async fn read_json_body(
    body: Body,
    maximum: usize,
    deadline: Duration,
) -> Result<bytes::Bytes, JsonBodyReadError> {
    tokio::time::timeout(deadline, to_bytes(body, maximum))
        .await
        .map_err(|_| JsonBodyReadError::Timeout)?
        .map_err(|_| JsonBodyReadError::Rejected)
}

pub(super) fn request_body_timeout() -> Problem {
    Problem::new(
        axum::http::StatusCode::REQUEST_TIMEOUT,
        "request_timeout",
        "Request timeout",
        "The request body was not received before the deadline.",
    )
}

pub(super) fn payload_too_large(maximum: usize) -> Problem {
    Problem::new(
        axum::http::StatusCode::PAYLOAD_TOO_LARGE,
        "body_too_large",
        "Request body too large",
        format!("The request body exceeds the {maximum}-byte limit."),
    )
}

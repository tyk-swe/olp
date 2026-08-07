use std::time::Duration;

use axum::{
    body::{Body, to_bytes},
    http::Request,
};

use crate::{MAX_HTTP_HEADER_BYTES, MAX_HTTP_HEADER_COUNT, MAX_JSON_BODY_BYTES, Problem, gateway};

const MAX_HEADER_VALUE_BYTES: usize = 8 * 1024;
const MAX_JSON_DEPTH: usize = 64;
const MAX_URI_BYTES: usize = 8 * 1024;

pub(super) fn validate_target_and_headers(request: &Request<Body>) -> Result<(), Problem> {
    if request.uri().to_string().len() > MAX_URI_BYTES {
        return Err(Problem::new(
            axum::http::StatusCode::URI_TOO_LONG,
            "uri_too_long",
            "Request URI too long",
            "The request URI exceeds the gateway limit.",
        ));
    }
    let header_bytes = request
        .headers()
        .iter()
        .fold(0_usize, |size, (name, value)| {
            size.saturating_add(name.as_str().len())
                .saturating_add(value.as_bytes().len())
                .saturating_add(4)
        });
    if request.headers().len() > MAX_HTTP_HEADER_COUNT
        || header_bytes > MAX_HTTP_HEADER_BYTES
        || request
            .headers()
            .values()
            .any(|value| value.as_bytes().len() > MAX_HEADER_VALUE_BYTES)
    {
        return Err(Problem::new(
            axum::http::StatusCode::REQUEST_HEADER_FIELDS_TOO_LARGE,
            "headers_too_large",
            "Request headers too large",
            "Request headers exceed the gateway limit.",
        ));
    }
    Ok(())
}

pub(super) fn validate_body_framing_and_encoding(request: &Request<Body>) -> Result<(), Problem> {
    let content_length_count = request
        .headers()
        .get_all(axum::http::header::CONTENT_LENGTH)
        .iter()
        .count();
    let transfer_encoding = request
        .headers()
        .get_all(axum::http::header::TRANSFER_ENCODING)
        .iter()
        .collect::<Vec<_>>();
    if content_length_count > 1
        || !transfer_encoding.is_empty()
            && request
                .headers()
                .contains_key(axum::http::header::CONTENT_LENGTH)
        || transfer_encoding.len() > 1
        || transfer_encoding.first().is_some_and(|value| {
            !value
                .to_str()
                .is_ok_and(|value| value.trim().eq_ignore_ascii_case("chunked"))
        })
    {
        return Err(Problem::bad_request(
            "ambiguous_body_length",
            "The request has ambiguous framing headers.",
        ));
    }
    if request
        .headers()
        .get(axum::http::header::CONTENT_ENCODING)
        .is_some_and(|value| {
            !value
                .to_str()
                .is_ok_and(|value| value.trim().eq_ignore_ascii_case("identity"))
        })
    {
        return Err(Problem::new(
            axum::http::StatusCode::UNSUPPORTED_MEDIA_TYPE,
            "content_encoding_unsupported",
            "Content encoding unsupported",
            "Compressed request bodies are not accepted.",
        ));
    }
    Ok(())
}

pub(super) struct BodyAdmission {
    maximum: usize,
    pub(super) multipart_content_type: Option<String>,
    pub(super) is_json: bool,
}

impl BodyAdmission {
    pub(super) fn classify(
        request: &Request<Body>,
        endpoint: Option<gateway::InferenceEndpoint>,
    ) -> Self {
        let content_type = request
            .headers()
            .get(axum::http::header::CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default();
        Self {
            maximum: endpoint
                .map(|endpoint| endpoint.body_limit(content_type))
                .unwrap_or(MAX_JSON_BODY_BYTES),
            multipart_content_type: content_type
                .split(';')
                .next()
                .is_some_and(|value| value.trim().eq_ignore_ascii_case("multipart/form-data"))
                .then(|| content_type.to_owned()),
            is_json: is_json_content_type(content_type),
        }
    }

    pub(super) fn enforce_declared_size(&self, request: &Request<Body>) -> Result<(), Problem> {
        if request
            .headers()
            .get(axum::http::header::CONTENT_LENGTH)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.parse::<u64>().ok())
            .is_some_and(|value| value > self.maximum as u64)
        {
            return Err(payload_too_large(self.maximum));
        }
        Ok(())
    }
}

fn is_json_content_type(value: &str) -> bool {
    let media_type = value.split(';').next().unwrap_or_default().trim();
    media_type.eq_ignore_ascii_case("application/json")
        || media_type
            .to_ascii_lowercase()
            .strip_prefix("application/")
            .is_some_and(|subtype| subtype.ends_with("+json"))
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

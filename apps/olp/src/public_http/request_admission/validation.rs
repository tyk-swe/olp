use std::time::Duration;

use axum::{
    body::{Body, to_bytes},
    http::Request,
};

use crate::{
    bootstrap::state::MAX_HTTP_HEADER_BYTES, bootstrap::state::MAX_HTTP_HEADER_COUNT,
    bootstrap::state::MAX_JSON_BODY_BYTES,
    gateway::endpoint_policy::classification::InferenceEndpoint, public_http::problem::Problem,
};

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
    pub(super) fn classify(request: &Request<Body>, endpoint: Option<InferenceEndpoint>) -> Self {
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

#[cfg(test)]
mod tests {
    use axum::http::{HeaderName, HeaderValue, Method, StatusCode, header};

    use super::*;

    fn empty_request(uri: &str) -> Request<Body> {
        Request::builder().uri(uri).body(Body::empty()).unwrap()
    }

    fn request_with_headers(headers: &[(HeaderName, &'static str)]) -> Request<Body> {
        let mut request = empty_request("/");
        for (name, value) in headers {
            request
                .headers_mut()
                .append(name, HeaderValue::from_static(value));
        }
        request
    }

    fn assert_problem(result: Result<(), Problem>, status: StatusCode, code: &str) {
        let problem = result.unwrap_err();
        assert_eq!(problem.status, status.as_u16());
        assert_eq!(
            problem.problem_type.as_ref(),
            format!("https://openllmproxy.dev/problems/{code}")
        );
    }

    #[test]
    fn target_and_header_limits_cover_each_independent_dimension() {
        validate_target_and_headers(&empty_request("/within-limits")).unwrap();

        let uri = format!("/{}", "x".repeat(MAX_URI_BYTES));
        assert_problem(
            validate_target_and_headers(&empty_request(&uri)),
            StatusCode::URI_TOO_LONG,
            "uri_too_long",
        );

        let mut too_many = empty_request("/");
        for index in 0..=MAX_HTTP_HEADER_COUNT {
            let name = HeaderName::from_bytes(format!("x-count-{index}").as_bytes()).unwrap();
            too_many
                .headers_mut()
                .insert(name, HeaderValue::from_static("x"));
        }

        let aggregate_header_count = MAX_HTTP_HEADER_BYTES.div_ceil(MAX_HEADER_VALUE_BYTES);
        let aggregate_value_bytes = MAX_HTTP_HEADER_BYTES.div_ceil(aggregate_header_count);
        assert!(aggregate_header_count <= MAX_HTTP_HEADER_COUNT);
        assert!(aggregate_value_bytes <= MAX_HEADER_VALUE_BYTES);
        let mut too_many_bytes = empty_request("/");
        let large_value = HeaderValue::from_str(&"x".repeat(aggregate_value_bytes)).unwrap();
        for index in 0..aggregate_header_count {
            let name = HeaderName::from_bytes(format!("x-bytes-{index}").as_bytes()).unwrap();
            too_many_bytes
                .headers_mut()
                .insert(name, large_value.clone());
        }

        let mut oversized_value = empty_request("/");
        oversized_value.headers_mut().insert(
            HeaderName::from_static("x-oversized"),
            HeaderValue::from_str(&"x".repeat(MAX_HEADER_VALUE_BYTES + 1)).unwrap(),
        );

        for (dimension, request) in [
            ("header count", too_many),
            ("aggregate header bytes", too_many_bytes),
            ("individual header value", oversized_value),
        ] {
            let problem = validate_target_and_headers(&request).expect_err(dimension);
            assert_eq!(
                problem.status,
                StatusCode::REQUEST_HEADER_FIELDS_TOO_LARGE.as_u16(),
                "unexpected status for {dimension}"
            );
            assert_eq!(
                problem.problem_type.as_ref(),
                "https://openllmproxy.dev/problems/headers_too_large",
                "unexpected problem type for {dimension}"
            );
        }
    }

    #[test]
    fn body_framing_accepts_unambiguous_or_identity_encoded_requests() {
        for headers in [
            &[(header::CONTENT_LENGTH, "12")][..],
            &[(header::TRANSFER_ENCODING, "chunked")][..],
            &[(header::CONTENT_ENCODING, "IDENTITY")][..],
        ] {
            validate_body_framing_and_encoding(&request_with_headers(headers)).unwrap();
        }
    }

    #[test]
    fn body_framing_rejects_ambiguous_or_encoded_requests() {
        for headers in [
            &[(header::CONTENT_LENGTH, "1"), (header::CONTENT_LENGTH, "1")][..],
            &[
                (header::CONTENT_LENGTH, "1"),
                (header::TRANSFER_ENCODING, "chunked"),
            ][..],
            &[
                (header::TRANSFER_ENCODING, "chunked"),
                (header::TRANSFER_ENCODING, "chunked"),
            ][..],
            &[(header::TRANSFER_ENCODING, "gzip")][..],
        ] {
            assert_problem(
                validate_body_framing_and_encoding(&request_with_headers(headers)),
                StatusCode::BAD_REQUEST,
                "ambiguous_body_length",
            );
        }

        assert_problem(
            validate_body_framing_and_encoding(&request_with_headers(&[(
                header::CONTENT_ENCODING,
                "gzip",
            )])),
            StatusCode::UNSUPPORTED_MEDIA_TYPE,
            "content_encoding_unsupported",
        );
    }

    #[test]
    fn body_admission_classifies_media_types_and_declared_size() {
        let mut vendor_json = empty_request("/");
        vendor_json.headers_mut().insert(
            header::CONTENT_TYPE,
            HeaderValue::from_static("Application/Problem+JSON; charset=utf-8"),
        );
        let json = BodyAdmission::classify(&vendor_json, None);
        assert!(json.is_json);
        assert!(json.multipart_content_type.is_none());
        assert_eq!(json.maximum, MAX_JSON_BODY_BYTES);

        let endpoint =
            InferenceEndpoint::classify(&Method::POST, "/openai/v1/audio/transcriptions").unwrap();
        let endpoint_maximum = endpoint.body_limit("multipart/form-data; boundary=olp");
        let mut multipart = empty_request("/");
        multipart.headers_mut().insert(
            header::CONTENT_TYPE,
            HeaderValue::from_static("multipart/form-data; boundary=olp"),
        );
        let admission = BodyAdmission::classify(&multipart, Some(endpoint));
        assert!(!admission.is_json);
        assert_eq!(
            admission.multipart_content_type.as_deref(),
            Some("multipart/form-data; boundary=olp")
        );
        assert_eq!(admission.maximum, endpoint_maximum);

        multipart.headers_mut().insert(
            header::CONTENT_LENGTH,
            HeaderValue::from_str(&admission.maximum.to_string()).unwrap(),
        );
        admission.enforce_declared_size(&multipart).unwrap();
        multipart.headers_mut().insert(
            header::CONTENT_LENGTH,
            HeaderValue::from_str(&(admission.maximum + 1).to_string()).unwrap(),
        );
        assert_problem(
            admission.enforce_declared_size(&multipart),
            StatusCode::PAYLOAD_TOO_LARGE,
            "body_too_large",
        );
    }

    #[tokio::test]
    async fn json_body_reader_distinguishes_bounds_from_success() {
        let bytes = read_json_body(Body::from("four"), 4, Duration::from_secs(1))
            .await
            .unwrap();
        assert_eq!(bytes, "four");

        let error = read_json_body(Body::from("five!"), 4, Duration::from_secs(1))
            .await
            .unwrap_err();
        assert_eq!(error, JsonBodyReadError::Rejected);
    }
}

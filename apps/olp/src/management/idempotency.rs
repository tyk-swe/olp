use axum::{
    body::Body,
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::Response,
};
use olp_storage::idempotency::IdempotencyOutcome;

use crate::Problem;

use super::response_policy::prevent_sensitive_response_caching;

pub(crate) fn require_idempotency_key(headers: &HeaderMap) -> Result<&str, Problem> {
    let value = headers
        .get("idempotency-key")
        .and_then(|value| value.to_str().ok())
        .ok_or_else(|| {
            Problem::bad_request(
                "idempotency_key_required",
                "An Idempotency-Key header is required.",
            )
        })?;
    if !(8..=128).contains(&value.len())
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
    {
        return Err(Problem::bad_request(
            "invalid_idempotency_key",
            "Idempotency-Key must be 8-128 URL-safe ASCII characters.",
        ));
    }
    Ok(value)
}

pub(crate) fn idempotency_http_response<T>(
    outcome: IdempotencyOutcome<T>,
) -> Result<Response, Problem> {
    let replay = match outcome {
        IdempotencyOutcome::Executed { response, .. } | IdempotencyOutcome::Replayed(response) => {
            response
        }
    };
    let (status, content_type, etag, body) = replay.into_parts();
    let mut response = Response::new(Body::from(body));
    *response.status_mut() = StatusCode::from_u16(status).map_err(|_| Problem::internal())?;
    if let Some(content_type) = content_type {
        response.headers_mut().insert(
            header::CONTENT_TYPE,
            HeaderValue::from_str(&content_type).map_err(|_| Problem::internal())?,
        );
    }
    if let Some(etag) = etag {
        response.headers_mut().insert(
            header::ETAG,
            HeaderValue::from_str(&etag).map_err(|_| Problem::internal())?,
        );
    }
    prevent_sensitive_response_caching(&mut response);
    Ok(response)
}

use axum::{
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use uuid::Uuid;

use crate::public_http::problem::Problem;

pub(crate) fn if_match(headers: &HeaderMap) -> Result<Uuid, Problem> {
    optional_if_match(headers)?.ok_or_else(|| {
        Problem::new(
            StatusCode::PRECONDITION_REQUIRED,
            "if_match_required",
            "Precondition required",
            "Supply the current ETag in If-Match.",
        )
    })
}

pub(crate) fn optional_if_match(headers: &HeaderMap) -> Result<Option<Uuid>, Problem> {
    headers
        .get(header::IF_MATCH)
        .map(|value| {
            value
                .to_str()
                .ok()
                .and_then(|value| value.strip_prefix('"')?.strip_suffix('"'))
                .and_then(|value| Uuid::parse_str(value).ok())
                .ok_or_else(|| {
                    Problem::bad_request(
                        "invalid_if_match",
                        "If-Match must contain one strong UUID ETag.",
                    )
                })
        })
        .transpose()
}

pub(crate) fn with_etag(response: impl IntoResponse, etag: Uuid) -> Result<Response, Problem> {
    let mut response = response.into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

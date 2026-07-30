use http::HeaderMap;

const CLIENT_AUTH_HEADERS: &[&str] = &["api-key", "x-api-key", "x-goog-api-key", "authorization"];
#[must_use]
pub(crate) fn sanitize_forward_headers(source: &HeaderMap) -> HeaderMap {
    crate::transport_common::sanitize_forward_headers(source, CLIENT_AUTH_HEADERS)
}

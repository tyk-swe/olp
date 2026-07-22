use http::HeaderMap;

const PROVIDER_OWNED_HEADERS: &[&str] = &["api-key", "x-api-key", "x-goog-api-key"];

#[must_use]
pub(crate) fn sanitize_forward_headers(source: &HeaderMap) -> HeaderMap {
    crate::forward_headers::sanitize_forward_headers(source, PROVIDER_OWNED_HEADERS)
}

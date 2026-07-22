use http::HeaderMap;

const PROVIDER_OWNED_HEADERS: &[&str] = &[
    "api-key",
    "openai-organization",
    "openai-project",
    "x-api-key",
    "x-goog-api-key",
];

/// Returns client headers that are safe to forward to the OpenAI transport.
#[must_use]
pub(crate) fn sanitize_forward_headers(source: &HeaderMap) -> HeaderMap {
    crate::forward_headers::sanitize_forward_headers(source, PROVIDER_OWNED_HEADERS)
}

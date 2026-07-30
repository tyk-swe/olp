use http::HeaderMap;

const CLIENT_AUTH_HEADERS: &[&str] = &[
    "api-key",
    "openai-organization",
    "openai-project",
    "x-api-key",
    "x-goog-api-key",
];

/// Returns client headers that are safe to forward to the OpenAI transport.
///
/// The HTTP API currently does not carry client headers through the core port.
/// This function defines the boundary for future adapters: authentication,
/// host/framing headers, fixed hop-by-hop headers, and headers named by the
/// `Connection` field are always removed before provider credentials are added.
#[must_use]
pub(crate) fn sanitize_forward_headers(source: &HeaderMap) -> HeaderMap {
    crate::transport_common::sanitize_forward_headers(source, CLIENT_AUTH_HEADERS)
}

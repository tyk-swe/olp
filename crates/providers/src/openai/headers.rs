use std::collections::HashSet;

use http::{HeaderMap, HeaderName, header};

const CLIENT_AUTH_HEADERS: &[&str] = &[
    "api-key",
    "openai-organization",
    "openai-project",
    "x-api-key",
    "x-goog-api-key",
];

const FIXED_HOP_BY_HOP_HEADERS: &[&str] = &[
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
];

/// Returns client headers that are safe to forward to the OpenAI transport.
///
/// The HTTP API currently does not carry client headers through the core port.
/// This function defines the boundary for future adapters: authentication,
/// host/framing headers, fixed hop-by-hop headers, and headers named by the
/// `Connection` field are always removed before provider credentials are added.
#[must_use]
pub(crate) fn sanitize_forward_headers(source: &HeaderMap) -> HeaderMap {
    let connection_headers = connection_header_names(source);
    let mut sanitized = HeaderMap::with_capacity(source.len());
    for (name, value) in source {
        if is_forbidden(name, &connection_headers) {
            continue;
        }
        sanitized.append(name, value.clone());
    }
    sanitized
}

fn connection_header_names(headers: &HeaderMap) -> HashSet<HeaderName> {
    headers
        .get_all(header::CONNECTION)
        .iter()
        .filter_map(|value| value.to_str().ok())
        .flat_map(|value| value.split(','))
        .filter_map(|name| HeaderName::from_bytes(name.trim().as_bytes()).ok())
        .collect()
}

fn is_forbidden(name: &HeaderName, connection_headers: &HashSet<HeaderName>) -> bool {
    *name == header::AUTHORIZATION
        || *name == header::COOKIE
        || *name == header::HOST
        || *name == header::CONTENT_LENGTH
        || *name == header::CONTENT_TYPE
        || FIXED_HOP_BY_HOP_HEADERS
            .iter()
            .any(|blocked| name.as_str().eq_ignore_ascii_case(blocked))
        || connection_headers.contains(name)
        || CLIENT_AUTH_HEADERS
            .iter()
            .any(|blocked| name.as_str().eq_ignore_ascii_case(blocked))
}

#[cfg(test)]
mod tests {
    use http::{HeaderValue, header};

    use super::*;

    #[test]
    fn strips_auth_framing_and_hop_by_hop_headers() {
        let mut source = HeaderMap::new();
        source.insert(
            header::AUTHORIZATION,
            HeaderValue::from_static("Bearer client-secret"),
        );
        source.insert(
            header::CONNECTION,
            HeaderValue::from_static("keep-alive, x-private-hop"),
        );
        source.insert("keep-alive", HeaderValue::from_static("timeout=5"));
        source.insert(header::CONTENT_LENGTH, HeaderValue::from_static("999"));
        source.insert("x-api-key", HeaderValue::from_static("client-key"));
        source.insert("x-private-hop", HeaderValue::from_static("remove-me"));
        source.insert("x-request-id", HeaderValue::from_static("request-1"));

        let sanitized = sanitize_forward_headers(&source);

        assert_eq!(sanitized.len(), 1);
        assert_eq!(sanitized["x-request-id"], "request-1");
    }

    #[test]
    fn keeps_repeated_end_to_end_headers() {
        let mut source = HeaderMap::new();
        source.append("x-feature", HeaderValue::from_static("a"));
        source.append("x-feature", HeaderValue::from_static("b"));

        let sanitized = sanitize_forward_headers(&source);
        assert_eq!(sanitized.get_all("x-feature").iter().count(), 2);
    }
}

//! Shared request-header forwarding boundary for provider transports.

use std::collections::HashSet;

use http::{HeaderMap, HeaderName, header};

const FIXED_HOP_BY_HOP_HEADERS: &[&str] = &[
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "proxy-connection",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
];

const COMMON_SENSITIVE_HEADERS: &[&str] = &[
    "authorization",
    "cookie",
    "host",
    "content-length",
    "content-type",
];

/// Copies only end-to-end client headers that cannot override transport-owned
/// authority, framing, authentication, or provider metadata.
#[must_use]
pub(crate) fn sanitize_forward_headers(
    source: &HeaderMap,
    provider_owned_headers: &[&str],
) -> HeaderMap {
    let connection_headers = connection_header_names(source);
    let mut sanitized = HeaderMap::with_capacity(source.len());
    for (name, value) in source {
        if is_forbidden(name, &connection_headers, provider_owned_headers) {
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
        .flat_map(|value| value.as_bytes().split(|byte| *byte == b','))
        .map(trim_ascii_whitespace)
        .filter_map(|name| HeaderName::from_bytes(name).ok())
        .collect()
}

fn trim_ascii_whitespace(mut value: &[u8]) -> &[u8] {
    while value.first().is_some_and(u8::is_ascii_whitespace) {
        value = &value[1..];
    }
    while value.last().is_some_and(u8::is_ascii_whitespace) {
        value = &value[..value.len() - 1];
    }
    value
}

fn is_forbidden(
    name: &HeaderName,
    connection_headers: &HashSet<HeaderName>,
    provider_owned_headers: &[&str],
) -> bool {
    connection_headers.contains(name)
        || FIXED_HOP_BY_HOP_HEADERS
            .iter()
            .chain(COMMON_SENSITIVE_HEADERS)
            .chain(provider_owned_headers)
            .any(|blocked| name.as_str().eq_ignore_ascii_case(blocked))
}

#[cfg(test)]
mod tests {
    use http::HeaderValue;

    use super::*;

    #[test]
    fn removes_transport_owned_and_connection_named_headers() {
        let mut source = HeaderMap::new();
        source.insert(
            header::AUTHORIZATION,
            HeaderValue::from_static("Bearer client-secret"),
        );
        source.append(
            header::CONNECTION,
            HeaderValue::from_static("keep-alive, x-private-hop"),
        );
        source.append(
            header::CONNECTION,
            HeaderValue::from_bytes(b"x-second-hop, \x80invalid").unwrap(),
        );
        source.insert("keep-alive", HeaderValue::from_static("timeout=5"));
        source.insert("proxy-connection", HeaderValue::from_static("keep-alive"));
        source.insert("x-private-hop", HeaderValue::from_static("remove-me"));
        source.insert("x-second-hop", HeaderValue::from_static("remove-me-too"));
        source.insert("x-provider-secret", HeaderValue::from_static("remove-me"));
        source.append("x-feature", HeaderValue::from_static("a"));
        source.append("x-feature", HeaderValue::from_static("b"));

        let sanitized = sanitize_forward_headers(&source, &["x-provider-secret"]);

        assert_eq!(sanitized.get_all("x-feature").iter().count(), 2);
        assert_eq!(sanitized.len(), 2);
    }

    #[test]
    fn header_names_are_matched_exactly() {
        let mut source = HeaderMap::new();
        source.insert("x-api-key", HeaderValue::from_static("secret"));
        source.insert("x-api-key-id", HeaderValue::from_static("public-id"));

        let sanitized = sanitize_forward_headers(&source, &["x-api-key"]);

        assert!(sanitized.get("x-api-key").is_none());
        assert_eq!(sanitized["x-api-key-id"], "public-id");
    }
}

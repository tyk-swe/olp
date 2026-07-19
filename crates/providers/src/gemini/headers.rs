use std::collections::HashSet;

use http::{HeaderMap, HeaderName, header};

const CLIENT_AUTH_HEADERS: &[&str] = &["api-key", "x-api-key", "x-goog-api-key", "authorization"];
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

#[must_use]
pub(crate) fn sanitize_forward_headers(source: &HeaderMap) -> HeaderMap {
    let connection_headers = source
        .get_all(header::CONNECTION)
        .iter()
        .filter_map(|value| value.to_str().ok())
        .flat_map(|value| value.split(','))
        .filter_map(|name| HeaderName::from_bytes(name.trim().as_bytes()).ok())
        .collect::<HashSet<_>>();
    let mut sanitized = HeaderMap::with_capacity(source.len());
    for (name, value) in source {
        let forbidden = *name == header::COOKIE
            || *name == header::HOST
            || *name == header::CONTENT_LENGTH
            || *name == header::CONTENT_TYPE
            || connection_headers.contains(name)
            || FIXED_HOP_BY_HOP_HEADERS
                .iter()
                .chain(CLIENT_AUTH_HEADERS)
                .any(|blocked| name.as_str().eq_ignore_ascii_case(blocked));
        if !forbidden {
            sanitized.append(name, value.clone());
        }
    }
    sanitized
}

#[cfg(test)]
mod tests {
    use http::HeaderValue;

    use super::*;

    #[test]
    fn strips_credentials_framing_and_connection_named_headers() {
        let mut source = HeaderMap::new();
        source.insert("x-goog-api-key", HeaderValue::from_static("client-secret"));
        source.insert(
            header::AUTHORIZATION,
            HeaderValue::from_static("Bearer client"),
        );
        source.insert(header::CONNECTION, HeaderValue::from_static("x-hop"));
        source.insert("x-hop", HeaderValue::from_static("private"));
        source.insert("x-request-id", HeaderValue::from_static("request-1"));
        let sanitized = sanitize_forward_headers(&source);
        assert_eq!(sanitized.len(), 1);
        assert_eq!(sanitized["x-request-id"], "request-1");
    }
}

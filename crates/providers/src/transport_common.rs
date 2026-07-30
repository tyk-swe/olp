//! Shared request metadata and error construction for native HTTP transports.

use std::{
    collections::{BTreeMap, HashSet},
    fmt,
};

use http::{HeaderMap, HeaderName, HeaderValue, StatusCode, header};
use olp_domain::{AttemptFailureClass, SourceExtensions, Surface, TransportError, TransportPhase};
use zeroize::Zeroizing;

use crate::transport_io::ProviderResponseIo;

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

pub(crate) fn sanitize_forward_headers(
    source: &HeaderMap,
    client_auth_headers: &[&str],
) -> HeaderMap {
    let connection_headers = source
        .get_all(header::CONNECTION)
        .iter()
        .filter_map(|value| value.to_str().ok())
        .flat_map(|value| value.split(','))
        .filter_map(|name| HeaderName::from_bytes(name.trim().as_bytes()).ok())
        .collect::<HashSet<_>>();
    let mut sanitized = HeaderMap::with_capacity(source.len());
    for (name, value) in source {
        let forbidden = matches!(
            *name,
            header::AUTHORIZATION
                | header::COOKIE
                | header::HOST
                | header::CONTENT_LENGTH
                | header::CONTENT_TYPE
        ) || connection_headers.contains(name)
            || FIXED_HOP_BY_HOP_HEADERS
                .iter()
                .chain(client_auth_headers)
                .any(|blocked| name.as_str().eq_ignore_ascii_case(blocked));
        if !forbidden {
            sanitized.append(name, value.clone());
        }
    }
    sanitized
}

pub(crate) fn secret_header(
    secret: &str,
    provider: &'static str,
) -> Result<HeaderValue, TransportError> {
    let value = Zeroizing::new(secret.as_bytes().to_vec());
    HeaderValue::from_bytes(value.as_slice()).map_err(|_| {
        protocol_error(format!(
            "{provider} API key cannot be represented as a header"
        ))
    })
}

pub(crate) fn safe_upstream_error_message(
    provider: &'static str,
    status: StatusCode,
    body: &[u8],
    secret: &str,
) -> String {
    let message = serde_json::from_slice::<serde_json::Value>(body)
        .ok()
        .and_then(|value| value.get("error").cloned())
        .and_then(|error| {
            error
                .get("message")
                .and_then(serde_json::Value::as_str)
                .map(str::to_owned)
        })
        .map(|message| message.replace(secret, "[REDACTED]"))
        .map(|message| message.chars().take(512).collect::<String>());
    match message {
        Some(message) if !message.is_empty() => {
            format!("{provider} returned HTTP {status}: {message}")
        }
        _ => format!("{provider} returned HTTP {status}"),
    }
}

pub(crate) fn source_extensions(
    surface: Surface,
    values: BTreeMap<String, serde_json::Value>,
) -> SourceExtensions {
    let values = values
        .into_iter()
        .map(|(key, value)| {
            let escaped = key.replace('~', "~0").replace('/', "~1");
            (format!("/{escaped}"), value)
        })
        .collect();
    SourceExtensions::new(surface, values)
}

pub(crate) fn map_endpoint_error(error: impl fmt::Display, dns_timeout: bool) -> TransportError {
    let class = if dns_timeout {
        AttemptFailureClass::Timeout
    } else {
        AttemptFailureClass::Connect
    };
    transport_error(TransportPhase::Connect, class, false, error.to_string())
}

pub(crate) fn map_send_error(
    provider: &'static str,
    response_io: ProviderResponseIo,
    error: reqwest::Error,
) -> TransportError {
    if error.is_connect() {
        transport_error(
            TransportPhase::Connect,
            if error.is_timeout() {
                AttemptFailureClass::Timeout
            } else {
                AttemptFailureClass::Connect
            },
            false,
            format!("{provider} connection failed"),
        )
    } else if error.is_timeout() {
        response_io.first_byte_timeout()
    } else {
        transport_error(
            TransportPhase::FirstByte,
            AttemptFailureClass::Connect,
            false,
            format!("{provider} request failed before response headers"),
        )
    }
}

pub(crate) fn protocol_error(message: impl Into<String>) -> TransportError {
    transport_error(
        TransportPhase::Connect,
        AttemptFailureClass::Protocol,
        false,
        message,
    )
}

pub(crate) fn protocol_body_error(message: impl Into<String>) -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Protocol,
        false,
        message,
    )
}

pub(crate) fn transport_error(
    phase: TransportPhase,
    class: AttemptFailureClass,
    response_committed: bool,
    message: impl Into<String>,
) -> TransportError {
    TransportError {
        phase,
        class,
        response_committed,
        message: message.into(),
    }
}

pub(crate) const MAX_INLINE_MEDIA_BYTES: usize = 1024 * 1024;

/// Reads a bounded inline-media handle from the spool and returns its bytes
/// base64-encoded, shared by every connector that hydrates inline media.
pub(crate) async fn read_inline_media(
    marker: &str,
    spool: Option<&std::sync::Arc<dyn olp_domain::MediaSpool>>,
) -> Result<String, TransportError> {
    use base64::{Engine as _, engine::general_purpose::STANDARD};
    use futures::StreamExt as _;

    let handle = olp_domain::media_handle_from_inline_marker(marker)
        .ok_or_else(|| protocol_error("invalid bounded inline-media handle"))?;
    let spool = spool.ok_or_else(|| protocol_error("bounded inline-media spool is unavailable"))?;
    let opened = spool.open(&handle).await.map_err(|error| {
        protocol_error(format!(
            "bounded inline-media handle cannot be opened: {error}"
        ))
    })?;
    if opened
        .artifact
        .content_length
        .is_none_or(|length| length > MAX_INLINE_MEDIA_BYTES as u64)
    {
        return Err(protocol_error("bounded inline media exceeded its limit"));
    }
    let mut stream = opened.bytes;
    let mut bytes = Vec::new();
    while let Some(chunk) = stream.next().await {
        let chunk = chunk.map_err(|error| {
            protocol_error(format!("bounded inline-media read failed: {error}"))
        })?;
        if bytes.len().saturating_add(chunk.len()) > MAX_INLINE_MEDIA_BYTES {
            return Err(protocol_error("bounded inline media exceeded its limit"));
        }
        bytes.extend_from_slice(&chunk);
    }
    Ok(STANDARD.encode(bytes))
}

#[cfg(test)]
mod tests {
    use http::HeaderValue;

    use super::*;

    #[test]
    fn sanitizes_forwarded_headers_without_collapsing_safe_repeated_values() {
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
        source.insert("x-api-key", HeaderValue::from_static("client-key"));
        source.insert("x-private-hop", HeaderValue::from_static("remove-me"));
        source.append("x-feature", HeaderValue::from_static("a"));
        source.append("x-feature", HeaderValue::from_static("b"));

        let sanitized = sanitize_forward_headers(&source, &["x-api-key"]);

        assert_eq!(sanitized.len(), 2);
        assert_eq!(sanitized.get_all("x-feature").iter().count(), 2);
    }
}

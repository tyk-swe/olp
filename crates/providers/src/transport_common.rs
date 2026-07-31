//! Shared request metadata and error construction for native HTTP transports.

use std::{
    collections::BTreeMap,
    fmt,
    time::{Duration, SystemTime},
};

use http::{HeaderMap, HeaderValue, StatusCode, header};
use olp_domain::{AttemptFailureClass, SourceExtensions, Surface, TransportError, TransportPhase};
use zeroize::Zeroizing;

use crate::transport_io::ProviderResponseIo;

pub(crate) fn single_content_type(headers: &HeaderMap) -> Option<&str> {
    let mut values = headers.get_all(header::CONTENT_TYPE).iter();
    let value = values.next()?.to_str().ok()?;
    values.next().is_none().then_some(value)
}

pub(crate) fn has_content_type(headers: &HeaderMap, expected: &str) -> bool {
    single_content_type(headers)
        .and_then(|value| value.split(';').next())
        .is_some_and(|value| value.trim().eq_ignore_ascii_case(expected))
}

const MAX_RETRY_AFTER: Duration = Duration::from_secs(24 * 60 * 60);

pub(crate) fn rate_limit_retry_after(status: StatusCode, headers: &HeaderMap) -> Option<Duration> {
    (status == StatusCode::TOO_MANY_REQUESTS)
        .then(|| parse_retry_after_at(headers, SystemTime::now()))
        .flatten()
}

fn parse_retry_after_at(headers: &HeaderMap, now: SystemTime) -> Option<Duration> {
    let mut values = headers.get_all(header::RETRY_AFTER).iter();
    let value = values.next()?.to_str().ok()?.trim();
    if values.next().is_some() {
        return None;
    }
    let duration = if !value.is_empty() && value.bytes().all(|byte| byte.is_ascii_digit()) {
        Duration::from_secs(value.parse().ok()?)
    } else {
        httpdate::parse_http_date(value)
            .ok()?
            .duration_since(now)
            .unwrap_or_default()
    };
    Some(duration.min(MAX_RETRY_AFTER))
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
        retry_after: None,
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
    use super::*;

    #[test]
    fn retry_after_accepts_bounded_delta_or_date_and_rejects_ambiguity() {
        let now = SystemTime::UNIX_EPOCH + Duration::from_secs(1_800_000_000);
        let mut headers = HeaderMap::new();
        headers.insert(header::RETRY_AFTER, HeaderValue::from_static("30"));
        assert_eq!(
            parse_retry_after_at(&headers, now),
            Some(Duration::from_secs(30))
        );
        assert_eq!(
            rate_limit_retry_after(StatusCode::BAD_REQUEST, &headers),
            None
        );

        headers.insert(
            header::RETRY_AFTER,
            HeaderValue::from_str(&httpdate::fmt_http_date(now + Duration::from_secs(60))).unwrap(),
        );
        assert_eq!(
            parse_retry_after_at(&headers, now),
            Some(Duration::from_secs(60))
        );

        headers.insert(header::RETRY_AFTER, HeaderValue::from_static("999999999"));
        assert_eq!(parse_retry_after_at(&headers, now), Some(MAX_RETRY_AFTER));
        headers.append(header::RETRY_AFTER, HeaderValue::from_static("1"));
        assert_eq!(parse_retry_after_at(&headers, now), None);
    }
}

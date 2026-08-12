//! Shared request metadata and error construction for native HTTP transports.

use std::{collections::BTreeMap, fmt, time::Duration};

use crate::domain::{
    AttemptFailureClass, MAX_UPSTREAM_RETRY_AFTER, SourceExtensions, Surface, TransportError,
    TransportPhase,
};
use chrono::{DateTime, NaiveDateTime, Utc};
use http::{HeaderMap, HeaderValue, StatusCode, header};
use zeroize::Zeroizing;

use crate::providers::transport_io::ProviderResponseIo;

pub(in crate::providers) fn secret_header(
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

pub(in crate::providers) fn safe_upstream_error_message(
    provider: &'static str,
    status: StatusCode,
    body: &[u8],
    secret: &str,
) -> String {
    let message = serde_json::from_slice::<serde_json::Value>(body)
        .ok()
        .and_then(|value| {
            let candidates = [
                value
                    .pointer("/error/message")
                    .and_then(serde_json::Value::as_str),
                value.get("error").and_then(serde_json::Value::as_str),
                value.get("message").and_then(serde_json::Value::as_str),
                value.get("detail").and_then(serde_json::Value::as_str),
            ];
            candidates
                .into_iter()
                .flatten()
                .find_map(|candidate| sanitize_upstream_message(candidate, secret))
        });
    match message {
        Some(message) if !message.is_empty() => {
            format!("{provider} returned HTTP {status}: {message}")
        }
        _ => format!("{provider} returned HTTP {status}"),
    }
}

fn sanitize_upstream_message(message: &str, secret: &str) -> Option<String> {
    let redacted = if secret.is_empty() {
        message.to_owned()
    } else {
        message.replace(secret, "[REDACTED]")
    };
    let mut sanitized = String::new();
    let mut characters = 0;
    let mut previous_was_space = false;
    for character in redacted.chars() {
        if characters == 512 {
            break;
        }
        if character.is_control()
            || character.is_whitespace()
            || is_unsafe_format_control(character)
        {
            if !previous_was_space && !sanitized.is_empty() {
                sanitized.push(' ');
                characters += 1;
            }
            previous_was_space = true;
        } else {
            sanitized.push(character);
            characters += 1;
            previous_was_space = false;
        }
    }
    let sanitized = sanitized.trim().to_owned();
    if sanitized.is_empty()
        || looks_like_html(&sanitized)
        || looks_like_structured_content(&sanitized)
    {
        None
    } else {
        Some(sanitized)
    }
}

fn is_unsafe_format_control(character: char) -> bool {
    matches!(
        character,
        '\u{00ad}'
            | '\u{061c}'
            | '\u{200b}'..='\u{200f}'
            | '\u{202a}'..='\u{202e}'
            | '\u{2060}'..='\u{206f}'
            | '\u{feff}'
    )
}

fn looks_like_structured_content(message: &str) -> bool {
    if serde_json::from_str::<serde_json::Value>(message)
        .is_ok_and(|value| value.is_object() || value.is_array())
    {
        return true;
    }
    let without_redaction = message.replace("[REDACTED]", "");
    (without_redaction.contains('{') && without_redaction.contains('}'))
        || (without_redaction.contains('[') && without_redaction.contains(']'))
}

fn looks_like_html(message: &str) -> bool {
    let bytes = message.as_bytes();
    bytes.iter().enumerate().any(|(index, byte)| {
        if *byte != b'<' {
            return false;
        }
        let Some(next) = bytes.get(index + 1) else {
            return false;
        };
        (next.is_ascii_alphabetic() || matches!(next, b'!' | b'/' | b'?'))
            && bytes[index + 2..].contains(&b'>')
    })
}

pub(in crate::providers) fn retry_after(headers: &HeaderMap) -> Option<Duration> {
    retry_after_at(headers, Utc::now())
}

/// Parses either Retry-After wire form against an injected clock, keeping
/// HTTP-date tests deterministic.
pub(in crate::providers) fn retry_after_at(
    headers: &HeaderMap,
    now: DateTime<Utc>,
) -> Option<Duration> {
    let value = headers.get(header::RETRY_AFTER)?.to_str().ok()?.trim();
    let delay = if value.bytes().all(|byte| byte.is_ascii_digit()) && !value.is_empty() {
        Duration::from_secs(value.parse::<u64>().ok()?)
    } else {
        let retry_at = parse_http_date(value)?;
        retry_at.signed_duration_since(now).to_std().ok()?
    };
    Some(delay.min(MAX_UPSTREAM_RETRY_AFTER))
}

fn parse_http_date(value: &str) -> Option<DateTime<Utc>> {
    [
        "%a, %d %b %Y %H:%M:%S GMT",
        "%A, %d-%b-%y %H:%M:%S GMT",
        "%a %b %e %H:%M:%S %Y",
    ]
    .into_iter()
    .find_map(|format| {
        NaiveDateTime::parse_from_str(value, format)
            .ok()
            .map(|value| value.and_utc())
    })
}

pub(in crate::providers) fn source_extensions(
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

pub(in crate::providers) fn map_endpoint_error(
    error: impl fmt::Display,
    dns_timeout: bool,
) -> TransportError {
    let class = if dns_timeout {
        AttemptFailureClass::Timeout
    } else {
        AttemptFailureClass::Connect
    };
    transport_error(TransportPhase::Connect, class, false, error.to_string())
}

pub(in crate::providers) fn map_send_error(
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

pub(in crate::providers) fn protocol_error(message: impl Into<String>) -> TransportError {
    transport_error(
        TransportPhase::Connect,
        AttemptFailureClass::Protocol,
        false,
        message,
    )
}

pub(in crate::providers) fn protocol_body_error(message: impl Into<String>) -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Protocol,
        false,
        message,
    )
}

pub(in crate::providers) fn transport_error(
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

pub(in crate::providers) const MAX_INLINE_MEDIA_BYTES: usize = 1024 * 1024;

/// Reads a bounded inline-media handle from the spool and returns its bytes
/// base64-encoded, shared by every connector that hydrates inline media.
pub(in crate::providers) async fn read_inline_media(
    marker: &str,
    spool: Option<&std::sync::Arc<dyn crate::domain::MediaSpool>>,
) -> Result<String, TransportError> {
    use base64::{Engine as _, engine::general_purpose::STANDARD};
    use futures::StreamExt as _;

    let handle = crate::domain::media_handle_from_inline_marker(marker)
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
    use std::{collections::BTreeMap, sync::Arc, time::Duration};

    use bytes::Bytes;
    use futures::stream;
    use http::StatusCode;
    use serde_json::json;

    use super::*;
    use crate::domain::{
        BoxFuture, MediaArtifact, MediaByteStream, MediaHandle, MediaSpool, MediaSpoolError,
        MediaUpload, OpenedMedia, inline_media_marker,
    };

    #[derive(Clone)]
    struct ReadSpool {
        content_length: Option<u64>,
        chunks: Vec<Result<Bytes, MediaSpoolError>>,
        open_error: Option<MediaSpoolError>,
    }

    impl ReadSpool {
        fn bytes(bytes: impl Into<Bytes>) -> Arc<dyn MediaSpool> {
            let bytes = bytes.into();
            Arc::new(Self {
                content_length: Some(bytes.len() as u64),
                chunks: vec![Ok(bytes)],
                open_error: None,
            })
        }
    }

    impl MediaSpool for ReadSpool {
        fn put(&self, _: MediaUpload) -> BoxFuture<'_, Result<MediaArtifact, MediaSpoolError>> {
            Box::pin(async { Err(MediaSpoolError::Unavailable) })
        }

        fn open<'a>(
            &'a self,
            handle: &'a MediaHandle,
        ) -> BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
            Box::pin(async move {
                if let Some(error) = &self.open_error {
                    return Err(error.clone());
                }
                let bytes: MediaByteStream = Box::pin(stream::iter(self.chunks.clone()));
                Ok(OpenedMedia {
                    artifact: MediaArtifact {
                        handle: handle.clone(),
                        content_type: Some("image/png".to_owned()),
                        content_length: self.content_length,
                    },
                    filename: "bounded.png".to_owned(),
                    bytes,
                })
            })
        }

        fn remove<'a>(&'a self, _: &'a MediaHandle) -> BoxFuture<'a, Result<(), MediaSpoolError>> {
            Box::pin(async { Err(MediaSpoolError::Unavailable) })
        }
    }

    #[test]
    fn shared_error_helpers_classify_failures_without_leaking_secrets() {
        let secret = "credential-value";
        let long_detail = format!("{secret}{}", "x".repeat(600));
        let body = serde_json::to_vec(&json!({"error": {"message": long_detail}})).unwrap();
        let message =
            safe_upstream_error_message("provider", StatusCode::BAD_GATEWAY, &body, secret);
        assert!(message.contains("[REDACTED]"));
        assert!(!message.contains(secret));
        assert!(message.len() < 600, "upstream messages must remain bounded");

        for body in [b"not-json".as_slice(), br#"{"error":{"message":""}}"#] {
            assert_eq!(
                safe_upstream_error_message("provider", StatusCode::BAD_REQUEST, body, secret),
                "provider returned HTTP 400 Bad Request"
            );
        }

        let invalid_header = secret_header("line one\nline two", "provider").unwrap_err();
        assert_eq!(invalid_header.class, AttemptFailureClass::Protocol);
        assert_eq!(invalid_header.phase, TransportPhase::Connect);

        for (dns_timeout, expected) in [
            (false, AttemptFailureClass::Connect),
            (true, AttemptFailureClass::Timeout),
        ] {
            let error = map_endpoint_error("private endpoint detail", dns_timeout);
            assert_eq!(error.class, expected);
            assert_eq!(error.phase, TransportPhase::Connect);
            assert!(!error.response_committed);
        }

        let body_error = protocol_body_error("invalid response body");
        assert_eq!(body_error.phase, TransportPhase::Body);
        assert_eq!(body_error.class, AttemptFailureClass::Protocol);
    }

    #[test]
    fn upstream_error_messages_accept_only_bounded_safe_scalar_shapes() {
        for (body, expected) in [
            (json!({"error": {"message": "nested"}}), "nested"),
            (json!({"error": "string error"}), "string error"),
            (json!({"message": "top-level message"}), "top-level message"),
            (json!({"detail": "top-level detail"}), "top-level detail"),
        ] {
            let body = serde_json::to_vec(&body).unwrap();
            let message =
                safe_upstream_error_message("provider", StatusCode::BAD_REQUEST, &body, "secret");
            assert!(message.ends_with(expected), "{message}");
        }

        let body = serde_json::to_vec(&json!({
            "message": "before\n\u{0} secret\tafter"
        }))
        .unwrap();
        let message =
            safe_upstream_error_message("provider", StatusCode::BAD_REQUEST, &body, "secret");
        assert_eq!(
            message,
            "provider returned HTTP 400 Bad Request: before [REDACTED] after"
        );
        assert!(!message.chars().any(char::is_control));

        let body = serde_json::to_vec(&json!({
            "message": "before\u{202e}after"
        }))
        .unwrap();
        let message =
            safe_upstream_error_message("provider", StatusCode::BAD_REQUEST, &body, "secret");
        assert_eq!(
            message,
            "provider returned HTTP 400 Bad Request: before after"
        );

        for body in [
            b"not JSON".as_slice(),
            b"\xff\xfe".as_slice(),
            br#"[{"message":"array body"}]"#,
            br#"{"error":{"message":{"nested":"object"}}}"#,
            br#"{"detail":["array"]}"#,
            br#"{"message":"<!doctype html><html>error</html>"}"#,
            br#"{"message":"{\"messages\":[{\"role\":\"user\",\"content\":\"private\"}]}"}"#,
            br#"{"message":"request failed: {\"messages\":[{\"role\":\"user\",\"content\":\"private\"}]}"}"#,
        ] {
            assert_eq!(
                safe_upstream_error_message("provider", StatusCode::BAD_REQUEST, body, "secret"),
                "provider returned HTTP 400 Bad Request"
            );
        }
    }

    #[test]
    fn retry_after_supports_delta_seconds_and_http_dates_with_a_fixed_clock() {
        let now = DateTime::parse_from_rfc2822("Wed, 21 Oct 2015 07:28:00 GMT")
            .unwrap()
            .with_timezone(&Utc);
        let mut headers = HeaderMap::new();

        headers.insert(header::RETRY_AFTER, HeaderValue::from_static("17"));
        assert_eq!(retry_after_at(&headers, now), Some(Duration::from_secs(17)));

        headers.insert(
            header::RETRY_AFTER,
            HeaderValue::from_static("Wed, 21 Oct 2015 07:28:30 GMT"),
        );
        assert_eq!(retry_after_at(&headers, now), Some(Duration::from_secs(30)));

        for legacy_http_date in [
            "Wednesday, 21-Oct-15 07:28:30 GMT",
            "Wed Oct 21 07:28:30 2015",
        ] {
            headers.insert(
                header::RETRY_AFTER,
                HeaderValue::from_str(legacy_http_date).unwrap(),
            );
            assert_eq!(
                retry_after_at(&headers, now),
                Some(Duration::from_secs(30)),
                "{legacy_http_date}"
            );
        }

        headers.insert(header::RETRY_AFTER, HeaderValue::from_static("7200"));
        assert_eq!(
            retry_after_at(&headers, now),
            Some(MAX_UPSTREAM_RETRY_AFTER)
        );

        for invalid in [
            "Wed, 21 Oct 2015 07:27:59 GMT",
            "17.5",
            "-1",
            "18446744073709551616",
            "tomorrow",
        ] {
            headers.insert(header::RETRY_AFTER, HeaderValue::from_str(invalid).unwrap());
            assert_eq!(retry_after_at(&headers, now), None, "{invalid}");
        }
    }

    #[test]
    fn source_extension_keys_are_json_pointer_escaped() {
        let extensions = source_extensions(
            Surface::Gemini,
            BTreeMap::from([
                ("plain".to_owned(), json!(1)),
                ("a~/b".to_owned(), json!(2)),
            ]),
        );

        assert_eq!(extensions.source, Some(Surface::Gemini));
        assert_eq!(
            extensions.values,
            BTreeMap::from([
                ("/a~0~1b".to_owned(), json!(2)),
                ("/plain".to_owned(), json!(1))
            ])
        );
    }

    #[tokio::test]
    async fn inline_media_reading_validates_handle_spool_metadata_and_stream_bounds() {
        let marker = inline_media_marker(&MediaHandle::new("media"));
        let unavailable = ReadSpool {
            content_length: Some(1),
            chunks: vec![],
            open_error: Some(MediaSpoolError::NotFound),
        };
        let unknown_length = ReadSpool {
            content_length: None,
            chunks: vec![],
            open_error: None,
        };
        let advertised_oversize = ReadSpool {
            content_length: Some(MAX_INLINE_MEDIA_BYTES as u64 + 1),
            chunks: vec![],
            open_error: None,
        };
        let failed_stream = ReadSpool {
            content_length: Some(1),
            chunks: vec![Err(MediaSpoolError::Unavailable)],
            open_error: None,
        };
        let streamed_oversize = ReadSpool {
            content_length: Some(1),
            chunks: vec![Ok(Bytes::from(vec![0; MAX_INLINE_MEDIA_BYTES + 1]))],
            open_error: None,
        };

        let failures: Vec<(&str, Option<Arc<dyn MediaSpool>>)> = vec![
            ("not-a-marker", None),
            (&marker, None),
            (&marker, Some(Arc::new(unavailable))),
            (&marker, Some(Arc::new(unknown_length))),
            (&marker, Some(Arc::new(advertised_oversize))),
            (&marker, Some(Arc::new(failed_stream))),
            (&marker, Some(Arc::new(streamed_oversize))),
        ];
        for (value, spool) in failures {
            let error = read_inline_media(value, spool.as_ref()).await.unwrap_err();
            assert_eq!(error.class, AttemptFailureClass::Protocol);
        }

        let spool = ReadSpool::bytes(Bytes::from_static(b"abc"));
        assert_eq!(
            read_inline_media(&marker, Some(&spool)).await.unwrap(),
            "YWJj"
        );
    }
}

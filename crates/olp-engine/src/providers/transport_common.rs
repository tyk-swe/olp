//! Shared request metadata and error construction for native HTTP transports.

use std::{collections::BTreeMap, fmt, time::Duration};

use crate::domain::{
    canonical::{identity::Surface, requests::SourceExtensions},
    ports::{AttemptFailureClass, TransportError, TransportPhase, UpstreamSignal},
};
use http::{HeaderMap, HeaderValue, StatusCode};
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

pub(in crate::providers) fn bearer_header(
    secret: &str,
    provider: &'static str,
) -> Result<HeaderValue, TransportError> {
    let mut value = Zeroizing::new(Vec::with_capacity("Bearer ".len() + secret.len()));
    value.extend_from_slice(b"Bearer ");
    value.extend_from_slice(secret.as_bytes());
    HeaderValue::from_bytes(value.as_slice()).map_err(|_| {
        protocol_error(format!(
            "{provider} bearer credential cannot be represented as a header"
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
        upstream: Default::default(),
        phase,
        class,
        response_committed,
        message: message.into(),
    }
}

/// Classifies an upstream HTTP error response and keeps its status and
/// `Retry-After` so the public status code and retry hint match the provider
/// instead of collapsing to a blanket 502 with no backoff signal.
pub(in crate::providers) fn upstream_response_error(
    phase: TransportPhase,
    status: StatusCode,
    headers: &HeaderMap,
    message: impl Into<String>,
) -> TransportError {
    let mut error = transport_error(phase, upstream_failure_class(status), false, message);
    error.upstream =
        UpstreamSignal::from_status(status.as_u16()).with_retry_after(parse_retry_after(headers));
    error
}

pub(in crate::providers) fn upstream_failure_class(status: StatusCode) -> AttemptFailureClass {
    if status == StatusCode::REQUEST_TIMEOUT {
        AttemptFailureClass::Timeout
    } else if status == StatusCode::TOO_MANY_REQUESTS {
        AttemptFailureClass::RateLimit
    } else if status.is_server_error() {
        AttemptFailureClass::UpstreamServer
    } else {
        AttemptFailureClass::UpstreamClient
    }
}

/// Parses RFC 9110 `Retry-After` in both of its forms. A date in the past is
/// zero, not an error, and anything unparsable is simply absent.
pub(in crate::providers) fn parse_retry_after(headers: &HeaderMap) -> Option<Duration> {
    parse_retry_after_value(headers.get(http::header::RETRY_AFTER)?.to_str().ok()?)
}

/// Parses a `Retry-After` header value: delta-seconds or an HTTP-date. Shared
/// with connectors whose SDK exposes headers as strings rather than an
/// `http::HeaderMap`.
pub(in crate::providers) fn parse_retry_after_value(value: &str) -> Option<Duration> {
    let value = value.trim();
    if value.is_empty() {
        return None;
    }
    if let Ok(seconds) = value.parse::<u64>() {
        return Some(Duration::from_secs(seconds));
    }
    let deadline = httpdate_seconds(value)?;
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .ok()?
        .as_secs();
    Some(Duration::from_secs(deadline.saturating_sub(now)))
}

/// Minimal IMF-fixdate reader. Providers emit exactly this form; the obsolete
/// RFC 850 and asctime spellings are treated as absent rather than guessed at.
fn httpdate_seconds(value: &str) -> Option<u64> {
    const MONTHS: [&str; 12] = [
        "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
    ];
    let value = value.strip_suffix(" GMT")?;
    let (_, rest) = value.split_once(", ")?;
    let mut parts = rest.split(' ');
    let day: u32 = parts.next()?.parse().ok()?;
    let month_name = parts.next()?;
    let month = MONTHS.iter().position(|name| *name == month_name)? + 1;
    let year: i32 = parts.next()?.parse().ok()?;
    let mut clock = parts.next()?.split(':');
    let hour: u32 = clock.next()?.parse().ok()?;
    let minute: u32 = clock.next()?.parse().ok()?;
    let second: u32 = clock.next()?.parse().ok()?;
    let date = chrono::NaiveDate::from_ymd_opt(year, u32::try_from(month).ok()?, day)?
        .and_hms_opt(hour, minute, second)?;
    u64::try_from(date.and_utc().timestamp()).ok()
}

pub(in crate::providers) const MAX_INLINE_MEDIA_BYTES: usize = 1024 * 1024;

/// Reads a bounded inline-media handle from the spool and returns its bytes
/// base64-encoded, shared by every connector that hydrates inline media.
pub(in crate::providers) async fn read_inline_media(
    marker: &str,
    spool: Option<&std::sync::Arc<dyn crate::domain::ports::MediaSpool>>,
) -> Result<String, TransportError> {
    use base64::{Engine as _, engine::general_purpose::STANDARD};
    use futures::StreamExt as _;

    let handle = crate::domain::canonical::requests::media_handle_from_inline_marker(marker)
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
    use std::{collections::BTreeMap, sync::Arc};

    use bytes::Bytes;
    use futures::stream;
    use http::StatusCode;
    use serde_json::json;

    use super::*;
    use crate::domain::{
        canonical::{
            requests::{MediaHandle, inline_media_marker},
            results::MediaArtifact,
        },
        ports::{
            BoxFuture, MediaByteStream, MediaSpool, MediaSpoolError, MediaUpload, OpenedMedia,
        },
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

    fn headers_with_retry_after(value: &str) -> HeaderMap {
        let mut headers = HeaderMap::new();
        headers.insert(
            http::header::RETRY_AFTER,
            http::HeaderValue::from_str(value).unwrap(),
        );
        headers
    }

    #[test]
    fn retry_after_reads_delta_seconds_and_http_dates() {
        assert_eq!(
            super::parse_retry_after(&headers_with_retry_after("60")),
            Some(Duration::from_secs(60))
        );
        assert_eq!(
            super::parse_retry_after(&headers_with_retry_after(" 0 ")),
            Some(Duration::ZERO)
        );
        // A date already in the past means "retry now", not "unparsable".
        assert_eq!(
            super::parse_retry_after(&headers_with_retry_after("Wed, 21 Oct 2015 07:28:00 GMT")),
            Some(Duration::ZERO)
        );
        let future =
            super::parse_retry_after(&headers_with_retry_after("Fri, 01 Jan 2100 00:00:00 GMT"))
                .expect("a future IMF-fixdate parses");
        assert!(future > Duration::from_secs(60));

        for unparsable in ["", "  ", "soon", "-5", "Fri, 32 Xxx 2100 00:00:00 GMT"] {
            assert_eq!(
                super::parse_retry_after(&headers_with_retry_after(unparsable)),
                None,
                "{unparsable:?}"
            );
        }
        assert_eq!(super::parse_retry_after(&HeaderMap::new()), None);
    }

    #[test]
    fn an_upstream_response_error_keeps_its_status_and_class() {
        for (status, class) in [
            (StatusCode::BAD_REQUEST, AttemptFailureClass::UpstreamClient),
            (
                StatusCode::UNAUTHORIZED,
                AttemptFailureClass::UpstreamClient,
            ),
            (StatusCode::REQUEST_TIMEOUT, AttemptFailureClass::Timeout),
            (
                StatusCode::TOO_MANY_REQUESTS,
                AttemptFailureClass::RateLimit,
            ),
            (
                StatusCode::SERVICE_UNAVAILABLE,
                AttemptFailureClass::UpstreamServer,
            ),
        ] {
            let error = super::upstream_response_error(
                TransportPhase::FirstByte,
                status,
                &headers_with_retry_after("30"),
                "provider said no",
            );
            assert_eq!(error.class, class, "{status}");
            assert_eq!(error.upstream.status, Some(status.as_u16()));
            assert_eq!(error.upstream.retry_after, Some(Duration::from_secs(30)));
            assert!(!error.response_committed);
        }
    }
}

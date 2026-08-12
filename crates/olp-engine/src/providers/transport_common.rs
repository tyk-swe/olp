//! Shared request metadata and error construction for native HTTP transports.

use std::{collections::BTreeMap, fmt};

use crate::domain::{
    AttemptFailureClass, SourceExtensions, Surface, TransportError, TransportPhase,
};
use http::{HeaderValue, StatusCode};
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
        phase,
        class,
        response_committed,
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
    use std::{collections::BTreeMap, sync::Arc};

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

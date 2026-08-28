use std::{fmt, future::Future, pin::Pin, sync::Arc, time::Duration};

use bytes::Bytes;
use futures::Stream;
use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::domain::{
    canonical::{
        events::Event,
        identity::RequestMetadata,
        requests::{MediaHandle, Operation},
        results::{CanonicalResult, MediaArtifact},
    },
    routing::selection::AttemptPlan,
};

pub type BoxFuture<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;
pub type ProviderEventStream =
    Pin<Box<dyn Stream<Item = Result<Event, TransportError>> + Send + 'static>>;
pub type MediaByteStream =
    Pin<Box<dyn Stream<Item = Result<Bytes, MediaSpoolError>> + Send + 'static>>;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DiscoveredProviderModel {
    pub id: String,
    pub display_name: String,
}

/// A bounded media upload presented to the infrastructure spool. The caller
/// supplies a hard maximum and the spool independently counts streamed bytes,
/// so a false or absent `Content-Length` cannot bypass admission limits.
pub struct MediaUpload {
    pub filename: String,
    pub content_type: Option<String>,
    pub maximum_length: u64,
    pub bytes: MediaByteStream,
}

impl fmt::Debug for MediaUpload {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("MediaUpload")
            .field("filename", &self.filename)
            .field("content_type", &self.content_type)
            .field("maximum_length", &self.maximum_length)
            .field("bytes", &"[STREAM]")
            .finish()
    }
}

/// A media object opened from the spool. Bytes remain streamed and bounded;
/// adapters never receive a path that could escape the spool directory.
pub struct OpenedMedia {
    pub artifact: MediaArtifact,
    pub filename: String,
    pub bytes: MediaByteStream,
}

impl fmt::Debug for OpenedMedia {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("OpenedMedia")
            .field("artifact", &self.artifact)
            .field("filename", &self.filename)
            .field("bytes", &"[STREAM]")
            .finish()
    }
}

pub trait MediaSpool: Send + Sync {
    /// Returns the total capacity of a locally bounded spool when the
    /// implementation can expose it. Admission controllers use this only for
    /// conservative request reservations; callers must still rely on `put`
    /// for the authoritative streamed-byte limit.
    fn capacity_bytes(&self) -> Option<u64> {
        None
    }

    /// Returns the bytes currently reserved against `capacity_bytes` when the
    /// implementation tracks them. Observability reports this; admission must
    /// not read it, because the value is stale the moment it is observed.
    fn used_bytes(&self) -> Option<u64> {
        None
    }

    fn put(&self, upload: MediaUpload) -> BoxFuture<'_, Result<MediaArtifact, MediaSpoolError>>;

    fn open<'a>(
        &'a self,
        handle: &'a MediaHandle,
    ) -> BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>>;

    fn remove<'a>(&'a self, handle: &'a MediaHandle) -> BoxFuture<'a, Result<(), MediaSpoolError>>;
}

#[derive(Clone, Debug, Error, Eq, PartialEq)]
pub enum MediaSpoolError {
    #[error("media upload limit must be greater than zero")]
    ZeroLimit,
    #[error("media filename is invalid")]
    InvalidFilename,
    #[error("media handle is invalid")]
    InvalidHandle,
    #[error("media object was not found")]
    NotFound,
    #[error("media object exceeded its {maximum}-byte limit")]
    TooLarge { maximum: u64 },
    #[error("media spool is unavailable")]
    Unavailable,
}

pub enum ProviderOutput {
    Events(ProviderEventStream),
    Result(Box<CanonicalResult>),
}

impl fmt::Debug for ProviderOutput {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Events(_) => formatter.write_str("ProviderOutput::Events([STREAM])"),
            Self::Result(_) => formatter.write_str("ProviderOutput::Result([REDACTED])"),
        }
    }
}

#[derive(Clone)]
pub struct ProviderRequest {
    pub metadata: RequestMetadata,
    pub attempt: AttemptPlan,
    /// Shared across failover attempts; connectors only read it.
    pub operation: Arc<Operation>,
    pub media: Option<Arc<dyn MediaSpool>>,
}

impl fmt::Debug for ProviderRequest {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ProviderRequest")
            .field("metadata", &self.metadata)
            .field("attempt", &self.attempt)
            .field("operation", &self.operation.kind())
            .field("route", &self.operation.route())
            .field("media", &self.media.as_ref().map(|_| "[MEDIA SPOOL]"))
            .finish_non_exhaustive()
    }
}

pub trait ProviderTransport: Send + Sync {
    fn execute(
        &self,
        request: ProviderRequest,
    ) -> BoxFuture<'_, Result<ProviderOutput, TransportError>>;
}

#[derive(Clone, Error, Eq, PartialEq)]
#[error("provider transport failed during {phase:?} ({class:?})")]
pub struct TransportError {
    pub phase: TransportPhase,
    pub class: AttemptFailureClass,
    pub response_committed: bool,
    pub message: String,
    /// What the upstream response itself said. Preserved so the public status
    /// and the retry hint reflect the provider instead of a blanket 502.
    pub upstream: UpstreamSignal,
}

/// Signals lifted from an upstream HTTP error response.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct UpstreamSignal {
    /// The upstream HTTP status, when the failure came from a response.
    pub status: Option<u16>,
    /// The upstream `Retry-After`, parsed and clamped.
    pub retry_after: Option<Duration>,
}

/// `Retry-After` values above this are treated as "come back much later"
/// rather than propagated verbatim; no caller benefits from a multi-hour hint.
pub const MAX_UPSTREAM_RETRY_AFTER: Duration = Duration::from_secs(300);

impl UpstreamSignal {
    #[must_use]
    pub fn from_status(status: u16) -> Self {
        Self {
            status: Some(status),
            retry_after: None,
        }
    }

    #[must_use]
    pub fn with_retry_after(mut self, retry_after: Option<Duration>) -> Self {
        self.retry_after = retry_after.map(|value| value.min(MAX_UPSTREAM_RETRY_AFTER));
        self
    }
}

impl fmt::Debug for TransportError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TransportError")
            .field("phase", &self.phase)
            .field("class", &self.class)
            .field("response_committed", &self.response_committed)
            .field("message", &"[REDACTED]")
            .field("upstream", &self.upstream)
            .finish()
    }
}

impl TransportError {
    #[must_use]
    pub const fn allows_failover(&self) -> bool {
        !self.response_committed
            && matches!(
                self.class,
                AttemptFailureClass::Connect
                    | AttemptFailureClass::Timeout
                    | AttemptFailureClass::RateLimit
                    | AttemptFailureClass::UpstreamServer
            )
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum TransportPhase {
    Connect,
    FirstByte,
    Body,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum AttemptFailureClass {
    Connect,
    Timeout,
    RateLimit,
    UpstreamServer,
    UpstreamClient,
    Protocol,
    Cancelled,
    Ambiguous,
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::canonical::{
        events::Kind,
        requests::SourceExtensions,
        results::{CanonicalResult, TokenCountResult},
    };
    use futures::stream;

    const PRIVATE_BYTES: &[u8] = b"private-media-payload";

    struct DefaultCapacitySpool;

    impl MediaSpool for DefaultCapacitySpool {
        fn put(&self, _: MediaUpload) -> BoxFuture<'_, Result<MediaArtifact, MediaSpoolError>> {
            Box::pin(async { Err(MediaSpoolError::Unavailable) })
        }

        fn open<'a>(
            &'a self,
            _: &'a MediaHandle,
        ) -> BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
            Box::pin(async { Err(MediaSpoolError::Unavailable) })
        }

        fn remove<'a>(&'a self, _: &'a MediaHandle) -> BoxFuture<'a, Result<(), MediaSpoolError>> {
            Box::pin(async { Err(MediaSpoolError::Unavailable) })
        }
    }

    fn byte_stream() -> MediaByteStream {
        Box::pin(stream::iter([Ok(Bytes::from_static(PRIVATE_BYTES))]))
    }

    #[test]
    fn media_debug_output_exposes_metadata_but_never_stream_contents() {
        let upload = MediaUpload {
            filename: "sample.wav".to_owned(),
            content_type: Some("audio/wav".to_owned()),
            maximum_length: 1024,
            bytes: byte_stream(),
        };
        let opened = OpenedMedia {
            artifact: MediaArtifact {
                handle: MediaHandle::new("bounded-handle"),
                content_type: Some("audio/wav".to_owned()),
                content_length: Some(PRIVATE_BYTES.len() as u64),
            },
            filename: "sample.wav".to_owned(),
            bytes: byte_stream(),
        };

        for debug in [format!("{upload:?}"), format!("{opened:?}")] {
            assert!(debug.contains("sample.wav"));
            assert!(debug.contains("[STREAM]"));
            assert!(!debug.contains(str::from_utf8(PRIVATE_BYTES).unwrap()));
        }
    }

    #[test]
    fn provider_output_debug_is_variant_only() {
        let events = ProviderOutput::Events(Box::pin(stream::iter([Ok(Event::new(
            0,
            Kind::TextDelta {
                output_index: 0,
                text: "private model output".to_owned(),
            },
        ))])));
        let result =
            ProviderOutput::Result(Box::new(CanonicalResult::TokenCount(TokenCountResult {
                input_tokens: 42,
                extensions: SourceExtensions::default(),
            })));

        assert_eq!(format!("{events:?}"), "ProviderOutput::Events([STREAM])");
        assert_eq!(format!("{result:?}"), "ProviderOutput::Result([REDACTED])");
    }

    #[test]
    fn media_spools_do_not_advertise_capacity_unless_they_own_a_bound() {
        assert_eq!(DefaultCapacitySpool.capacity_bytes(), None);
    }
}

use std::{fmt, future::Future, pin::Pin, sync::Arc};

use bytes::Bytes;
use futures::Stream;
use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::{
    AttemptPlan, CanonicalEvent, CanonicalResult, MediaArtifact, MediaHandle, Operation,
    RequestMetadata,
};

pub type BoxFuture<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;
pub type ProviderEventStream =
    Pin<Box<dyn Stream<Item = Result<CanonicalEvent, TransportError>> + Send + 'static>>;
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

    fn put<'a>(
        &'a self,
        upload: MediaUpload,
    ) -> BoxFuture<'a, Result<MediaArtifact, MediaSpoolError>>;

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
    pub operation: Operation,
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
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>>;
}

#[derive(Clone, Error, Eq, PartialEq)]
#[error("provider transport failed during {phase:?} ({class:?})")]
pub struct TransportError {
    pub phase: TransportPhase,
    pub class: AttemptFailureClass,
    pub response_committed: bool,
    pub message: String,
}

impl fmt::Debug for TransportError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TransportError")
            .field("phase", &self.phase)
            .field("class", &self.class)
            .field("response_committed", &self.response_committed)
            .field("message", &"[REDACTED]")
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

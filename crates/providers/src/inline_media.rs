//! Bounded loading of request media that canonical translation represented by
//! an inline-media spool marker.

use std::sync::Arc;

use base64::{Engine as _, engine::general_purpose::STANDARD};
use futures::StreamExt;
use olp_domain::{MediaSpool, MediaSpoolError, media_handle_from_inline_marker};

pub(crate) const MAX_INLINE_MEDIA_BYTES: usize = 1024 * 1024;

#[derive(Debug)]
pub(crate) enum InlineMediaError {
    InvalidHandle,
    MissingSpool,
    Open(MediaSpoolError),
    UnboundedOrTooLarge,
    Read(MediaSpoolError),
}

/// Opens, length-checks, streams, and base64-encodes one admitted media item.
///
/// The declared length is treated only as an admission check. The streaming
/// byte counter independently enforces the same maximum to remain safe if a
/// spool implementation violates its metadata contract.
pub(crate) async fn read_base64(
    marker: &str,
    spool: Option<&Arc<dyn MediaSpool>>,
    maximum: usize,
) -> Result<String, InlineMediaError> {
    let handle = media_handle_from_inline_marker(marker).ok_or(InlineMediaError::InvalidHandle)?;
    let spool = spool.ok_or(InlineMediaError::MissingSpool)?;
    let opened = spool.open(&handle).await.map_err(InlineMediaError::Open)?;
    let maximum_u64 = u64::try_from(maximum).unwrap_or(u64::MAX);
    let length = opened
        .artifact
        .content_length
        .filter(|length| *length <= maximum_u64)
        .ok_or(InlineMediaError::UnboundedOrTooLarge)?;
    let capacity = usize::try_from(length).unwrap_or(maximum).min(maximum);
    let mut bytes = Vec::with_capacity(capacity);
    let mut stream = opened.bytes;
    while let Some(chunk) = stream.next().await {
        let chunk = chunk.map_err(InlineMediaError::Read)?;
        bytes
            .len()
            .checked_add(chunk.len())
            .filter(|length| *length <= maximum)
            .ok_or(InlineMediaError::UnboundedOrTooLarge)?;
        bytes.extend_from_slice(&chunk);
    }
    Ok(STANDARD.encode(bytes))
}

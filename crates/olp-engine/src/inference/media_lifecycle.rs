use std::sync::Arc;

use crate::domain::{MediaByteStream, MediaHandle, MediaSpool, Operation};
use bytes::Bytes;
use tracing::warn;

pub(in crate::inference) fn operation_media_handles(operation: &Operation) -> Vec<MediaHandle> {
    let mut handles = Vec::new();
    match operation {
        Operation::Generation(request) => {
            for message in &request.messages {
                capture_content_handles(&message.content, &mut handles);
            }
        }
        Operation::TokenCount(request) => capture_content_handles(&request.input, &mut handles),
        Operation::Images(crate::domain::ImageOperation::Edit(request)) => {
            handles.extend(request.images.iter().cloned());
            handles.extend(request.mask.iter().cloned());
        }
        Operation::Images(crate::domain::ImageOperation::Variation(request)) => {
            handles.push(request.image.clone());
        }
        Operation::Transcription(request) => handles.push(request.audio.clone()),
        Operation::Video(crate::domain::VideoOperation::Create(request)) => {
            handles.extend(request.input.iter().cloned());
        }
        Operation::Moderation(request) => capture_content_handles(&request.input, &mut handles),
        Operation::Embeddings(_)
        | Operation::Images(crate::domain::ImageOperation::Generation(_))
        | Operation::Speech(_)
        | Operation::Video(_)
        | Operation::Models(_) => {}
    }
    handles.sort_by(|left, right| left.as_str().cmp(right.as_str()));
    handles.dedup_by(|left, right| left.as_str() == right.as_str());
    handles
}

fn capture_content_handles(parts: &[crate::domain::ContentPart], handles: &mut Vec<MediaHandle>) {
    for part in parts {
        match part {
            crate::domain::ContentPart::Image {
                source: crate::domain::MediaSource::Handle(handle),
                ..
            }
            | crate::domain::ContentPart::InputAudio { media: handle, .. }
            | crate::domain::ContentPart::InputFile { media: handle, .. } => {
                handles.push(handle.clone());
            }
            _ => {}
        }
    }
}

async fn cleanup_request_media(spool: &Arc<dyn MediaSpool>, handles: Vec<MediaHandle>) {
    for handle in handles {
        match spool.remove(&handle).await {
            Ok(()) | Err(crate::domain::MediaSpoolError::NotFound) => {}
            Err(error) => warn!(%error, "failed to remove request media from the bounded spool"),
        }
    }
}

pub(in crate::inference) struct RequestMediaGuard {
    spool: Arc<dyn MediaSpool>,
    handles: Vec<MediaHandle>,
}

impl RequestMediaGuard {
    pub(in crate::inference) fn new(spool: Arc<dyn MediaSpool>, handles: Vec<MediaHandle>) -> Self {
        Self { spool, handles }
    }

    pub(in crate::inference) async fn cleanup(mut self) {
        if self.handles.is_empty() {
            return;
        }
        let spool = self.spool.clone();
        let handles = std::mem::take(&mut self.handles);
        let cleanup = tokio::spawn(async move {
            cleanup_request_media(&spool, handles).await;
        });
        let _ = cleanup.await;
    }
}

impl Drop for RequestMediaGuard {
    fn drop(&mut self) {
        if self.handles.is_empty() {
            return;
        }
        let spool = self.spool.clone();
        let handles = std::mem::take(&mut self.handles);
        if let Ok(runtime) = tokio::runtime::Handle::try_current() {
            runtime.spawn(async move {
                cleanup_request_media(&spool, handles).await;
            });
        }
    }
}

pub struct CleanupMediaStream {
    inner: MediaByteStream,
    spool: Arc<dyn MediaSpool>,
    handle: Option<MediaHandle>,
}

impl CleanupMediaStream {
    pub fn new(inner: MediaByteStream, spool: Arc<dyn MediaSpool>, handle: MediaHandle) -> Self {
        Self {
            inner,
            spool,
            handle: Some(handle),
        }
    }

    fn schedule_cleanup(&mut self) {
        let Some(handle) = self.handle.take() else {
            return;
        };
        let spool = self.spool.clone();
        if let Ok(runtime) = tokio::runtime::Handle::try_current() {
            runtime.spawn(async move {
                let _ = spool.remove(&handle).await;
            });
        }
    }
}

impl futures::Stream for CleanupMediaStream {
    type Item = Result<Bytes, crate::domain::MediaSpoolError>;

    fn poll_next(
        mut self: std::pin::Pin<&mut Self>,
        context: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        let next = self.inner.as_mut().poll_next(context);
        if matches!(next, std::task::Poll::Ready(None)) {
            self.schedule_cleanup();
        }
        next
    }
}

impl Drop for CleanupMediaStream {
    fn drop(&mut self) {
        self.schedule_cleanup();
    }
}

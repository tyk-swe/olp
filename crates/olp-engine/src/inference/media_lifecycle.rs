use std::sync::Arc;

use crate::domain::{
    canonical::requests::{MediaHandle, Operation},
    ports::{MediaByteStream, MediaSpool},
};
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
        Operation::Images(crate::domain::canonical::requests::ImageOperation::Edit(request)) => {
            handles.extend(request.images.iter().cloned());
            handles.extend(request.mask.iter().cloned());
        }
        Operation::Images(crate::domain::canonical::requests::ImageOperation::Variation(
            request,
        )) => {
            handles.push(request.image.clone());
        }
        Operation::Transcription(request) => handles.push(request.audio.clone()),
        Operation::Video(crate::domain::canonical::requests::VideoOperation::Create(request)) => {
            handles.extend(request.input.iter().cloned());
        }
        Operation::Moderation(request) => capture_content_handles(&request.input, &mut handles),
        Operation::Embeddings(_)
        | Operation::Images(crate::domain::canonical::requests::ImageOperation::Generation(_))
        | Operation::Speech(_)
        | Operation::Video(_)
        | Operation::Models(_) => {}
    }
    handles.sort_by(|left, right| left.as_str().cmp(right.as_str()));
    handles.dedup_by(|left, right| left.as_str() == right.as_str());
    handles
}

fn capture_content_handles(
    parts: &[crate::domain::canonical::requests::ContentPart],
    handles: &mut Vec<MediaHandle>,
) {
    for part in parts {
        match part {
            crate::domain::canonical::requests::ContentPart::Image {
                source: crate::domain::canonical::requests::MediaSource::Handle(handle),
                ..
            }
            | crate::domain::canonical::requests::ContentPart::InputAudio {
                media: handle, ..
            }
            | crate::domain::canonical::requests::ContentPart::InputFile {
                media: handle, ..
            } => {
                handles.push(handle.clone());
            }
            _ => {}
        }
    }
}

async fn cleanup_request_media(spool: &Arc<dyn MediaSpool>, handles: Vec<MediaHandle>) {
    for handle in handles {
        match spool.remove(&handle).await {
            Ok(()) | Err(crate::domain::ports::MediaSpoolError::NotFound) => {}
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
    type Item = Result<Bytes, crate::domain::ports::MediaSpoolError>;

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

#[cfg(test)]
mod tests {
    use std::{sync::Arc, time::Duration};

    use bytes::Bytes;
    use futures::StreamExt;
    use tokio::sync::mpsc;

    use crate::domain::{
        canonical::{
            requests::{
                ContentPart, MediaHandle, MediaSource, Operation, SourceExtensions,
                TokenCountRequest,
            },
            results::MediaArtifact,
        },
        ids::RouteSlug,
        ports::{BoxFuture, MediaSpool, MediaSpoolError, MediaUpload, OpenedMedia},
    };

    use super::{CleanupMediaStream, RequestMediaGuard, operation_media_handles};

    fn handle(value: &str) -> MediaHandle {
        MediaHandle::new(value)
    }

    fn route() -> RouteSlug {
        RouteSlug::parse("test-route").expect("valid route")
    }

    fn handle_names(operation: &Operation) -> Vec<String> {
        operation_media_handles(operation)
            .iter()
            .map(|handle| handle.as_str().to_owned())
            .collect()
    }

    #[test]
    fn content_media_handles_are_filtered_sorted_and_deduplicated() {
        let operation = Operation::TokenCount(TokenCountRequest {
            route: route(),
            input: vec![
                ContentPart::Text {
                    text: "prompt".to_owned(),
                },
                ContentPart::Image {
                    source: MediaSource::Handle(handle("z-image")),
                    detail: None,
                },
                ContentPart::Image {
                    source: MediaSource::Uri("https://example.test/image.png".to_owned()),
                    detail: None,
                },
                ContentPart::InputAudio {
                    media: handle("a-audio"),
                    format: "wav".to_owned(),
                },
                ContentPart::InputFile {
                    media: handle("z-image"),
                    mime_type: "text/plain".to_owned(),
                    filename: "input.txt".to_owned(),
                },
                ContentPart::Refusal {
                    text: "no".to_owned(),
                },
            ],
            extensions: SourceExtensions::default(),
        });

        assert_eq!(handle_names(&operation), ["a-audio", "z-image"]);
    }

    struct RecordingSpool {
        removed: mpsc::UnboundedSender<String>,
    }

    impl MediaSpool for RecordingSpool {
        fn put(
            &self,
            _upload: MediaUpload,
        ) -> BoxFuture<'_, Result<MediaArtifact, MediaSpoolError>> {
            Box::pin(async { unreachable!("test only exercises removal") })
        }

        fn open<'a>(
            &'a self,
            _handle: &'a MediaHandle,
        ) -> BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
            Box::pin(async { unreachable!("test only exercises removal") })
        }

        fn remove<'a>(
            &'a self,
            handle: &'a MediaHandle,
        ) -> BoxFuture<'a, Result<(), MediaSpoolError>> {
            let removed = self.removed.clone();
            let value = handle.as_str().to_owned();
            Box::pin(async move {
                let _ = removed.send(value.clone());
                match value.as_str() {
                    "missing" => Err(MediaSpoolError::NotFound),
                    "unavailable" => Err(MediaSpoolError::Unavailable),
                    _ => Ok(()),
                }
            })
        }
    }

    fn recording_spool() -> (Arc<dyn MediaSpool>, mpsc::UnboundedReceiver<String>) {
        let (removed, receiver) = mpsc::unbounded_channel();
        (Arc::new(RecordingSpool { removed }), receiver)
    }

    async fn receive_removals(
        receiver: &mut mpsc::UnboundedReceiver<String>,
        count: usize,
    ) -> Vec<String> {
        tokio::time::timeout(Duration::from_secs(1), async {
            let mut values = Vec::with_capacity(count);
            for _ in 0..count {
                values.push(receiver.recv().await.expect("removal sender stays alive"));
            }
            values
        })
        .await
        .expect("media removal completes promptly")
    }

    #[tokio::test]
    async fn explicit_cleanup_attempts_every_handle_and_consumes_the_guard() {
        let (spool, mut receiver) = recording_spool();
        RequestMediaGuard::new(
            spool.clone(),
            vec![handle("ok"), handle("missing"), handle("unavailable")],
        )
        .cleanup()
        .await;
        assert_eq!(
            receive_removals(&mut receiver, 3).await,
            ["ok", "missing", "unavailable"]
        );

        RequestMediaGuard::new(spool, Vec::new()).cleanup().await;
        assert!(receiver.try_recv().is_err());
    }

    #[tokio::test]
    async fn dropped_guards_and_streams_cleanup_without_duplicate_removals() {
        let (spool, mut receiver) = recording_spool();
        drop(RequestMediaGuard::new(
            spool,
            vec![handle("dropped-request")],
        ));
        assert_eq!(
            receive_removals(&mut receiver, 1).await,
            ["dropped-request"]
        );

        let (spool, mut receiver) = recording_spool();
        let bytes = Box::pin(futures::stream::iter([Ok(Bytes::from_static(b"chunk"))]));
        let mut stream = CleanupMediaStream::new(bytes, spool, handle("completed-stream"));
        assert_eq!(
            stream.next().await.unwrap().unwrap(),
            Bytes::from_static(b"chunk")
        );
        assert!(stream.next().await.is_none());
        assert_eq!(
            receive_removals(&mut receiver, 1).await,
            ["completed-stream"]
        );
        drop(stream);
        assert!(receiver.try_recv().is_err());

        let (spool, mut receiver) = recording_spool();
        let bytes = Box::pin(futures::stream::pending::<Result<Bytes, MediaSpoolError>>());
        drop(CleanupMediaStream::new(
            bytes,
            spool,
            handle("dropped-stream"),
        ));
        assert_eq!(receive_removals(&mut receiver, 1).await, ["dropped-stream"]);
    }
}

use std::{sync::Arc, time::Duration};

use axum::body::Bytes;
use olp_domain::{ApiKey, MediaByteStream, MediaHandle, MediaSpool, Operation};
use olp_storage::{LimitError, LimitLease, LimitRequest};
use tracing::{error, warn};

use crate::GatewayState;

use super::error::InferenceError;

pub(super) async fn reserve_limits(
    state: &GatewayState,
    key: &ApiKey,
    operation: &Operation,
    lookup_id: &str,
    lease_ttl: Duration,
) -> Result<Option<LimitLease>, InferenceError> {
    if let Some(reserved_tokens) = crate::http_inference_reserved_tokens() {
        let Some(tokens_per_minute) = key.limits.tokens_per_minute else {
            return Ok(None);
        };
        let delta = estimate_tokens(operation).saturating_sub(reserved_tokens);
        if delta <= 0 {
            return Ok(None);
        }
        let limiter = state
            .limiter
            .current()
            .ok_or_else(|| InferenceError::unavailable("distributed_limits_unavailable"))?;
        let tokens_per_minute = i64::try_from(tokens_per_minute.get())
            .map_err(|_| InferenceError::unavailable("limit_configuration_invalid"))?;
        let result = tokio::time::timeout(
            Duration::from_secs(1),
            limiter.reserve(LimitRequest {
                lookup_id,
                requests_per_minute: None,
                tokens_per_minute: Some(tokens_per_minute),
                max_concurrency: None,
                requested_tokens: delta,
                lease_ttl,
            }),
        )
        .await
        .map_err(|_| InferenceError::unavailable("distributed_limits_unavailable"))?;
        return match result {
            Ok(lease) => Ok(Some(lease)),
            Err(LimitError::Exceeded {
                dimension,
                retry_after,
            }) => Err(InferenceError::rate_limited(dimension, retry_after)),
            Err(error) => {
                error!(%error, "HTTP TPM reconciliation failed closed");
                Err(InferenceError::unavailable(
                    "distributed_limits_unavailable",
                ))
            }
        };
    }
    if !key.limits.has_hard_limits() {
        return Ok(None);
    }
    let limiter = state
        .limiter
        .current()
        .ok_or_else(|| InferenceError::unavailable("distributed_limits_unavailable"))?;
    let tokens_per_minute = key
        .limits
        .tokens_per_minute
        .map(|value| i64::try_from(value.get()))
        .transpose()
        .map_err(|_| InferenceError::unavailable("limit_configuration_invalid"))?;
    let result = tokio::time::timeout(
        Duration::from_secs(1),
        limiter.reserve(LimitRequest {
            lookup_id,
            requests_per_minute: key
                .limits
                .requests_per_minute
                .map(|value| i64::from(value.get())),
            tokens_per_minute,
            max_concurrency: key.limits.concurrency.map(|value| i64::from(value.get())),
            requested_tokens: estimate_tokens(operation),
            lease_ttl,
        }),
    )
    .await
    .map_err(|_| InferenceError::unavailable("distributed_limits_unavailable"))?;
    match result {
        Ok(lease) => Ok(Some(lease)),
        Err(LimitError::Exceeded {
            dimension,
            retry_after,
        }) => Err(InferenceError::rate_limited(dimension, retry_after)),
        Err(error) => {
            error!(%error, "hard distributed limit reservation failed closed");
            Err(InferenceError::unavailable(
                "distributed_limits_unavailable",
            ))
        }
    }
}

fn estimate_tokens(operation: &Operation) -> i64 {
    let estimate = match operation {
        Operation::Generation(request) => {
            let messages = request
                .messages
                .iter()
                .map(|message| {
                    estimated_content_tokens(&message.content)
                        .saturating_add(message.name.as_deref().map_or(0, estimated_text_tokens))
                        .saturating_add(
                            message
                                .tool_call_id
                                .as_deref()
                                .map_or(0, estimated_text_tokens),
                        )
                        .saturating_add(
                            message
                                .tool_calls
                                .iter()
                                .map(|call| {
                                    estimated_text_tokens(&call.name)
                                        .saturating_add(estimated_text_tokens(&call.arguments))
                                })
                                .sum::<usize>(),
                        )
                })
                .sum::<usize>();
            let tools = request
                .tools
                .iter()
                .map(|tool| {
                    estimated_text_tokens(&tool.name)
                        .saturating_add(
                            tool.description.as_deref().map_or(0, estimated_text_tokens),
                        )
                        .saturating_add(estimated_text_tokens(&tool.input_schema.to_string()))
                })
                .sum::<usize>();
            // Omitting the output cap must not make TPM effectively input-only.
            // 4k is a conservative portable default across launch connectors.
            let output = usize::try_from(request.parameters.max_output_tokens.unwrap_or(4_096))
                .unwrap_or(usize::MAX)
                .saturating_mul(usize::from(request.parameters.candidate_count.unwrap_or(1)));
            messages.saturating_add(tools).saturating_add(output)
        }
        Operation::Embeddings(request) => request
            .input
            .iter()
            .map(|input| match input {
                olp_domain::EmbeddingInput::Text(text) => estimated_text_tokens(text),
                olp_domain::EmbeddingInput::Tokens(tokens) => tokens.len(),
            })
            .sum(),
        Operation::TokenCount(request) => estimated_content_tokens(&request.input),
        Operation::Images(olp_domain::ImageOperation::Generation(request)) => {
            estimated_text_tokens(&request.prompt)
        }
        Operation::Images(olp_domain::ImageOperation::Edit(request)) => {
            estimated_text_tokens(&request.prompt)
                .saturating_add(request.images.len().saturating_mul(1_000))
                .saturating_add(usize::from(request.mask.is_some()) * 1_000)
        }
        Operation::Images(olp_domain::ImageOperation::Variation(_)) => 1_000,
        Operation::Speech(request) => estimated_text_tokens(&request.input),
        Operation::Transcription(request) => request.prompt.as_deref().map_or(1_500, |prompt| {
            1_500_usize.saturating_add(estimated_text_tokens(prompt))
        }),
        Operation::Video(olp_domain::VideoOperation::Create(request)) => {
            estimated_text_tokens(&request.prompt)
                .saturating_add(usize::from(request.input.is_some()) * 2_000)
        }
        Operation::Moderation(request) => estimated_content_tokens(&request.input),
        Operation::Video(_) | Operation::Models(_) => 1,
    };
    i64::try_from(estimate.max(1)).unwrap_or(i64::MAX)
}

fn estimated_text_tokens(text: &str) -> usize {
    text.chars().count().saturating_add(3) / 4
}

fn estimated_content_tokens(parts: &[olp_domain::ContentPart]) -> usize {
    parts
        .iter()
        .map(|part| match part {
            olp_domain::ContentPart::Text { text } | olp_domain::ContentPart::Refusal { text } => {
                estimated_text_tokens(text)
            }
            olp_domain::ContentPart::Image { .. } => 1_000,
            olp_domain::ContentPart::InputAudio { .. } => 2_000,
            olp_domain::ContentPart::InputFile { .. } => 2_000,
        })
        .sum()
}

pub(crate) async fn release_limits(state: &GatewayState, lease: Option<&LimitLease>) {
    if let (Some(limiter), Some(lease)) = (state.limiter.current(), lease) {
        match tokio::time::timeout(Duration::from_millis(250), limiter.release(lease)).await {
            Ok(Ok(())) => {}
            Ok(Err(error)) => warn!(%error, "failed to release concurrency lease"),
            Err(_) => warn!("timed out releasing concurrency lease"),
        }
    }
}

pub(super) fn operation_media_handles(operation: &Operation) -> Vec<MediaHandle> {
    let mut handles = Vec::new();
    match operation {
        Operation::Generation(request) => {
            for message in &request.messages {
                capture_content_handles(&message.content, &mut handles);
            }
        }
        Operation::TokenCount(request) => capture_content_handles(&request.input, &mut handles),
        Operation::Images(olp_domain::ImageOperation::Edit(request)) => {
            handles.extend(request.images.iter().cloned());
            handles.extend(request.mask.iter().cloned());
        }
        Operation::Images(olp_domain::ImageOperation::Variation(request)) => {
            handles.push(request.image.clone());
        }
        Operation::Transcription(request) => handles.push(request.audio.clone()),
        Operation::Video(olp_domain::VideoOperation::Create(request)) => {
            handles.extend(request.input.iter().cloned());
        }
        Operation::Moderation(request) => capture_content_handles(&request.input, &mut handles),
        Operation::Embeddings(_)
        | Operation::Images(olp_domain::ImageOperation::Generation(_))
        | Operation::Speech(_)
        | Operation::Video(_)
        | Operation::Models(_) => {}
    }
    handles.sort_by(|left, right| left.as_str().cmp(right.as_str()));
    handles.dedup_by(|left, right| left.as_str() == right.as_str());
    handles
}

fn capture_content_handles(parts: &[olp_domain::ContentPart], handles: &mut Vec<MediaHandle>) {
    for part in parts {
        match part {
            olp_domain::ContentPart::Image {
                source: olp_domain::MediaSource::Handle(handle),
                ..
            }
            | olp_domain::ContentPart::InputAudio { media: handle, .. }
            | olp_domain::ContentPart::InputFile { media: handle, .. } => {
                handles.push(handle.clone());
            }
            _ => {}
        }
    }
}

async fn cleanup_request_media(spool: &Arc<dyn MediaSpool>, handles: Vec<MediaHandle>) {
    for handle in handles {
        match spool.remove(&handle).await {
            Ok(()) | Err(olp_domain::MediaSpoolError::NotFound) => {}
            Err(error) => warn!(%error, "failed to remove request media from the bounded spool"),
        }
    }
}

pub(super) struct RequestMediaGuard {
    spool: Arc<dyn MediaSpool>,
    handles: Vec<MediaHandle>,
}

impl RequestMediaGuard {
    pub(super) fn new(spool: Arc<dyn MediaSpool>, handles: Vec<MediaHandle>) -> Self {
        Self { spool, handles }
    }

    pub(super) async fn cleanup(mut self) {
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

pub(super) struct CleanupMediaStream {
    inner: MediaByteStream,
    spool: Arc<dyn MediaSpool>,
    handle: Option<MediaHandle>,
}

impl CleanupMediaStream {
    pub(super) fn new(
        inner: MediaByteStream,
        spool: Arc<dyn MediaSpool>,
        handle: MediaHandle,
    ) -> Self {
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
    type Item = Result<Bytes, olp_domain::MediaSpoolError>;

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

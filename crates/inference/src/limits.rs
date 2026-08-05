use std::{
    future::Future,
    pin::Pin,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

use arc_swap::ArcSwapOption;
use bytes::Bytes;
use olp_domain::{
    ApiKey, ApiKeyLookupId, MediaByteStream, MediaHandle, MediaSpool, Operation, Surface,
};
use olp_storage::{
    limits::DistributedLimiter, limits::LimitError, limits::LimitLease, limits::LimitRequest,
};
use tracing::{error, warn};

use crate::{InferenceError, runtime::RuntimeBundle};

type ReleaseFuture = Pin<Box<dyn Future<Output = ()> + Send + 'static>>;

struct InferenceReservationInner {
    release: Mutex<Option<ReleaseFuture>>,
    reconcile: Mutex<Option<DistributedReconciliation>>,
}

struct DistributedReconciliation {
    limiter: Arc<DistributedLimiter>,
    lease: LimitLease,
}

/// Cancellation-safe ownership of the request-boundary distributed limit
/// reservation. All clones share one idempotent reconciliation/release path.
#[derive(Clone)]
pub struct InferenceReservation {
    inner: Arc<InferenceReservationInner>,
}

impl InferenceReservation {
    #[must_use]
    pub fn distributed(limiter: Arc<DistributedLimiter>, lease: LimitLease) -> Self {
        let reconcile = DistributedReconciliation {
            limiter: Arc::clone(&limiter),
            lease: lease.clone(),
        };
        Self {
            inner: Arc::new(InferenceReservationInner {
                release: Mutex::new(Some(Box::pin(async move {
                    match tokio::time::timeout(Duration::from_millis(250), limiter.release(&lease))
                        .await
                    {
                        Ok(Ok(())) => {}
                        Ok(Err(error)) => {
                            tracing::warn!(%error, "failed to release inference concurrency lease");
                        }
                        Err(_) => tracing::warn!("timed out releasing inference concurrency lease"),
                    }
                }))),
                reconcile: Mutex::new(Some(reconcile)),
            }),
        }
    }

    #[doc(hidden)]
    #[must_use]
    pub fn for_test(release: impl Future<Output = ()> + Send + 'static) -> Self {
        Self {
            inner: Arc::new(InferenceReservationInner {
                release: Mutex::new(Some(Box::pin(release))),
                reconcile: Mutex::new(None),
            }),
        }
    }

    pub async fn reconcile(&self, actual_tokens: i64) {
        let reconcile = self
            .inner
            .reconcile
            .lock()
            .expect("inference reservation reconciliation mutex is not poisoned")
            .take();
        let Some(reconcile) = reconcile else {
            return;
        };
        match tokio::time::timeout(
            Duration::from_millis(250),
            reconcile.limiter.reconcile(&reconcile.lease, actual_tokens),
        )
        .await
        {
            Ok(Ok(())) => {}
            Ok(Err(error)) => {
                tracing::warn!(%error, "failed to reconcile inference token reservation");
            }
            Err(_) => tracing::warn!("timed out reconciling inference token reservation"),
        }
    }

    pub async fn release(self) {
        if let Some(release) = self.start_release() {
            let _ = release.await;
        }
    }

    pub fn spawn_release(&self) {
        let _ = self.start_release();
    }

    fn start_release(&self) -> Option<tokio::task::JoinHandle<()>> {
        let release = self
            .inner
            .release
            .lock()
            .expect("inference reservation release mutex is not poisoned")
            .take()?;
        spawn_release_future(release)
    }
}

fn spawn_release_future(release: ReleaseFuture) -> Option<tokio::task::JoinHandle<()>> {
    let runtime = tokio::runtime::Handle::try_current().ok()?;
    Some(runtime.spawn(release))
}

impl Drop for InferenceReservationInner {
    fn drop(&mut self) {
        let Some(release) = self
            .release
            .get_mut()
            .expect("inference reservation release mutex is not poisoned")
            .take()
        else {
            return;
        };
        if spawn_release_future(release).is_none() {
            tracing::warn!("could not release inference concurrency lease outside a Tokio runtime");
        }
    }
}

/// Authenticated API-key identity pinned to the runtime generation that
/// performed credential verification.
#[derive(Clone)]
pub struct InferencePrincipal {
    runtime: Arc<RuntimeBundle>,
    lookup_id: ApiKeyLookupId,
    surface: Surface,
}

impl InferencePrincipal {
    #[must_use]
    pub fn new(runtime: Arc<RuntimeBundle>, lookup_id: ApiKeyLookupId, surface: Surface) -> Self {
        Self {
            runtime,
            lookup_id,
            surface,
        }
    }

    #[must_use]
    pub fn runtime(&self) -> &Arc<RuntimeBundle> {
        &self.runtime
    }

    #[must_use]
    pub fn key(&self) -> &ApiKey {
        self.runtime
            .api_keys
            .get(&self.lookup_id)
            .expect("authenticated API key must remain in its pinned runtime")
    }

    #[must_use]
    pub const fn lookup_id(&self) -> &ApiKeyLookupId {
        &self.lookup_id
    }

    #[must_use]
    pub const fn surface(&self) -> Surface {
        self.surface
    }
}

/// Hot-swappable Valkey limiter connection used by inference services.
#[derive(Clone, Default)]
pub struct ReloadableLimiter {
    inner: Arc<ArcSwapOption<DistributedLimiter>>,
    configured: Arc<AtomicBool>,
}

impl ReloadableLimiter {
    pub fn mark_configured(&self) {
        self.configured.store(true, Ordering::Release);
    }

    #[must_use]
    pub fn is_configured(&self) -> bool {
        self.configured.load(Ordering::Acquire)
    }

    pub fn install(&self, limiter: DistributedLimiter) {
        self.inner.store(Some(Arc::new(limiter)));
    }

    pub fn clear(&self) {
        self.inner.store(None);
    }

    #[must_use]
    pub fn current(&self) -> Option<Arc<DistributedLimiter>> {
        self.inner.load_full()
    }
}

pub async fn reserve_limits(
    limiter: &ReloadableLimiter,
    key: &ApiKey,
    operation: &Operation,
    lookup_id: &str,
    lease_ttl: Duration,
    http_reserved_tokens: Option<i64>,
) -> Result<Option<LimitLease>, InferenceError> {
    if let Some(reserved_tokens) = http_reserved_tokens {
        let Some(tokens_per_minute) = key.limits.tokens_per_minute else {
            return Ok(None);
        };
        let delta = estimate_tokens(operation).saturating_sub(reserved_tokens);
        if delta <= 0 {
            return Ok(None);
        }
        let limiter = limiter
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
    let limiter = limiter
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

pub async fn release_limits(
    limiter: &ReloadableLimiter,
    lease: Option<&LimitLease>,
    actual_tokens: Option<i64>,
) {
    if let (Some(limiter), Some(lease)) = (limiter.current(), lease) {
        if let Some(actual_tokens) = actual_tokens {
            match tokio::time::timeout(
                Duration::from_millis(250),
                limiter.reconcile(lease, actual_tokens),
            )
            .await
            {
                Ok(Ok(())) => {}
                Ok(Err(error)) => warn!(%error, "failed to reconcile token reservation"),
                Err(_) => warn!("timed out reconciling token reservation"),
            }
        }
        match tokio::time::timeout(Duration::from_millis(250), limiter.release(lease)).await {
            Ok(Ok(())) => {}
            Ok(Err(error)) => warn!(%error, "failed to release concurrency lease"),
            Err(_) => warn!("timed out releasing concurrency lease"),
        }
    }
}

pub fn operation_media_handles(operation: &Operation) -> Vec<MediaHandle> {
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

pub struct RequestMediaGuard {
    spool: Arc<dyn MediaSpool>,
    handles: Vec<MediaHandle>,
}

impl RequestMediaGuard {
    pub fn new(spool: Arc<dyn MediaSpool>, handles: Vec<MediaHandle>) -> Self {
        Self { spool, handles }
    }

    pub async fn cleanup(mut self) {
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

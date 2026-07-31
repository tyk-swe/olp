use std::{
    fmt,
    future::Future,
    sync::{
        Arc, LazyLock, Mutex,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    time::Duration,
};

use axum::body::Bytes;
use olp_domain::{ApiKey, MediaByteStream, MediaHandle, MediaSpool, Operation};
use olp_storage::{DistributedLimiter, LimitError, LimitLease, LimitRequest, PgStore};
use tracing::{error, warn};

use crate::GatewayState;

use super::error::InferenceError;

const LIMIT_CLEANUP_RETRY_HORIZON: Duration = Duration::from_secs(30);
static LIMIT_CLEANUP_UNCERTAINTIES: AtomicU64 = AtomicU64::new(0);
static LIMIT_CLEANUP_SHUTTING_DOWN: AtomicBool = AtomicBool::new(false);
static LIMIT_CLEANUP_TASKS: LazyLock<Mutex<tokio::task::JoinSet<()>>> =
    LazyLock::new(|| Mutex::new(tokio::task::JoinSet::new()));

pub(crate) fn limit_cleanup_uncertainties() -> u64 {
    LIMIT_CLEANUP_UNCERTAINTIES.load(Ordering::Relaxed)
}

pub(crate) struct LimitReservation {
    limiter: Arc<DistributedLimiter>,
    lease: LimitLease,
    api_key_id: uuid::Uuid,
}

impl fmt::Debug for LimitReservation {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("LimitReservation")
            .field("lease", &self.lease)
            .finish_non_exhaustive()
    }
}

pub(super) async fn reserve_limits(
    state: &GatewayState,
    key: &ApiKey,
    operation: &Operation,
    lookup_id: &str,
    lease_ttl: Duration,
) -> Result<Option<LimitReservation>, InferenceError> {
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
            Ok(lease) => Ok(Some(LimitReservation {
                limiter,
                lease,
                api_key_id: key.id.as_uuid(),
            })),
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
        Ok(lease) => Ok(Some(LimitReservation {
            limiter,
            lease,
            api_key_id: key.id.as_uuid(),
        })),
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

pub(crate) fn release_limits(
    state: &GatewayState,
    reservation: Option<&LimitReservation>,
    actual_tokens: Option<i64>,
) {
    if let Some(reservation) = reservation {
        if let Some(actual_tokens) = actual_tokens {
            reconcile_tokens_in_background(
                Arc::clone(&reservation.limiter),
                reservation.lease.clone(),
                actual_tokens,
                Some((state.store().clone(), reservation.api_key_id)),
            );
        }
        release_concurrency_in_background(
            Arc::clone(&reservation.limiter),
            reservation.lease.clone(),
            Some((state.store().clone(), reservation.api_key_id)),
        );
    }
}

pub(crate) fn reconcile_tokens_in_background(
    limiter: Arc<olp_storage::DistributedLimiter>,
    lease: LimitLease,
    actual_tokens: i64,
    uncertainty: Option<(PgStore, uuid::Uuid)>,
) {
    spawn_bounded_limit_cleanup(
        "token reservation reconciliation",
        move || {
            let limiter = Arc::clone(&limiter);
            let lease = lease.clone();
            async move { limiter.reconcile(&lease, actual_tokens).await }
        },
        uncertainty.map(|(store, api_key_id)| (store, api_key_id, "limit.token_reconciliation")),
    );
}

pub(crate) fn release_concurrency_in_background(
    limiter: Arc<DistributedLimiter>,
    lease: LimitLease,
    uncertainty: Option<(PgStore, uuid::Uuid)>,
) {
    spawn_bounded_limit_cleanup(
        "concurrency lease release",
        move || {
            let limiter = Arc::clone(&limiter);
            let lease = lease.clone();
            async move { limiter.release(&lease).await }
        },
        uncertainty.map(|(store, api_key_id)| (store, api_key_id, "limit.concurrency_release")),
    );
}

fn spawn_bounded_limit_cleanup<F, Fut>(
    operation: &'static str,
    cleanup: F,
    uncertainty: Option<(PgStore, uuid::Uuid, &'static str)>,
) where
    F: FnMut() -> Fut + Send + 'static,
    Fut: Future<Output = Result<(), LimitError>> + Send + 'static,
{
    let Ok(runtime) = tokio::runtime::Handle::try_current() else {
        warn!(operation, "limit cleanup skipped outside a Tokio runtime");
        return;
    };
    let task = async move {
        if retry_limit_cleanup_until(operation, LIMIT_CLEANUP_RETRY_HORIZON, cleanup).await {
            return;
        }
        LIMIT_CLEANUP_UNCERTAINTIES.fetch_add(1, Ordering::Relaxed);
        if let Some((store, api_key_id, action)) = uncertainty {
            match tokio::time::timeout(
                Duration::from_secs(2),
                store.record_limit_cleanup_uncertainty(api_key_id, action),
            )
            .await
            {
                Ok(Ok(())) => {}
                Ok(Err(error)) => {
                    warn!(%error, operation, "failed to persist limit cleanup uncertainty");
                }
                Err(_) => warn!(operation, "timed out persisting limit cleanup uncertainty"),
            }
        }
    };
    let mut tasks = LIMIT_CLEANUP_TASKS
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    while let Some(result) = tasks.try_join_next() {
        if let Err(error) = result {
            warn!(%error, "limit cleanup task failed unexpectedly");
        }
    }
    tasks.spawn_on(task, &runtime);
}

async fn retry_limit_cleanup_until<F, Fut>(
    operation: &str,
    horizon: Duration,
    mut cleanup: F,
) -> bool
where
    F: FnMut() -> Fut,
    Fut: Future<Output = Result<(), LimitError>>,
{
    let started = tokio::time::Instant::now();
    let mut backoff = Duration::from_millis(25);
    let mut attempted = false;
    loop {
        // Shutdown stops the *retries*, never the first attempt: a lease that
        // is released once during the drain costs nothing, while skipping it
        // holds the slot in Valkey until the lease TTL expires.
        if attempted && LIMIT_CLEANUP_SHUTTING_DOWN.load(Ordering::Acquire) {
            warn!(
                operation,
                "limit cleanup stopped retrying for graceful shutdown"
            );
            return false;
        }
        attempted = true;
        match tokio::time::timeout(Duration::from_millis(250), cleanup()).await {
            Ok(Ok(())) => return true,
            Ok(Err(error)) if started.elapsed() >= horizon => {
                warn!(%error, operation, "limit cleanup failed after bounded retries");
                return false;
            }
            Err(_) if started.elapsed() >= horizon => {
                warn!(operation, "limit cleanup timed out after bounded retries");
                return false;
            }
            Ok(Err(_)) | Err(_) => {
                let remaining = horizon.saturating_sub(started.elapsed());
                tokio::time::sleep(backoff.min(remaining)).await;
                backoff = (backoff * 2).min(Duration::from_millis(250));
            }
        }
    }
}

pub(crate) async fn drain_limit_cleanup_tasks(timeout: Duration) {
    LIMIT_CLEANUP_SHUTTING_DOWN.store(true, Ordering::Release);
    let deadline = tokio::time::Instant::now() + timeout;
    loop {
        let mut tasks = {
            let mut tracked = LIMIT_CLEANUP_TASKS
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            std::mem::take(&mut *tracked)
        };
        if tasks.is_empty() {
            break;
        }
        while !tasks.is_empty() {
            match tokio::time::timeout_at(deadline, tasks.join_next()).await {
                Ok(Some(Ok(()))) => {}
                Ok(Some(Err(error))) => {
                    warn!(%error, "limit cleanup task failed during shutdown");
                }
                Ok(None) => break,
                Err(_) => {
                    warn!(
                        remaining = tasks.len(),
                        "limit cleanup tasks did not stop before deadline; aborting them"
                    );
                    tasks.abort_all();
                    while tasks.join_next().await.is_some() {}
                    LIMIT_CLEANUP_SHUTTING_DOWN.store(false, Ordering::Release);
                    return;
                }
            }
        }
    }
    LIMIT_CLEANUP_SHUTTING_DOWN.store(false, Ordering::Release);
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

#[cfg(test)]
mod tests {
    use std::sync::{
        Mutex,
        atomic::{AtomicUsize, Ordering},
    };

    use super::*;

    static CLEANUP_TEST_LOCK: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());

    #[tokio::test]
    async fn token_cleanup_retries_in_the_background_past_the_short_retry_window() {
        let _guard = CLEANUP_TEST_LOCK.lock().await;
        let attempts = Arc::new(AtomicUsize::new(0));
        let (completed_sender, completed) = tokio::sync::oneshot::channel();
        let completed_sender = Arc::new(Mutex::new(Some(completed_sender)));
        spawn_bounded_limit_cleanup(
            "test token cleanup",
            {
                let attempts = Arc::clone(&attempts);
                move || {
                    let attempt = attempts.fetch_add(1, Ordering::Relaxed);
                    let completed_sender = Arc::clone(&completed_sender);
                    async move {
                        if attempt < 4 {
                            Err(LimitError::UnexpectedResponse)
                        } else {
                            completed_sender
                                .lock()
                                .expect("completion sender mutex is not poisoned")
                                .take()
                                .expect("completion is sent once")
                                .send(())
                                .expect("completion receiver remains available");
                            Ok(())
                        }
                    }
                }
            },
            None,
        );

        assert_eq!(attempts.load(Ordering::Relaxed), 0);
        tokio::time::timeout(Duration::from_secs(2), completed)
            .await
            .expect("cleanup should outlive the old three-attempt window")
            .expect("cleanup completion sender should remain available");
        assert_eq!(attempts.load(Ordering::Relaxed), 5);
    }

    #[tokio::test]
    async fn graceful_shutdown_drains_cleanup_into_uncertainty_accounting() {
        let _guard = CLEANUP_TEST_LOCK.lock().await;
        let before = limit_cleanup_uncertainties();
        let (started_sender, started) = tokio::sync::oneshot::channel();
        let started_sender = Arc::new(Mutex::new(Some(started_sender)));
        spawn_bounded_limit_cleanup(
            "test shutdown cleanup",
            move || {
                let started_sender = Arc::clone(&started_sender);
                async move {
                    if let Some(sender) = started_sender
                        .lock()
                        .expect("start sender mutex is not poisoned")
                        .take()
                    {
                        let _ = sender.send(());
                    }
                    Err(LimitError::UnexpectedResponse)
                }
            },
            None,
        );
        tokio::time::timeout(Duration::from_secs(1), started)
            .await
            .expect("cleanup should start")
            .expect("cleanup start sender should remain available");

        drain_limit_cleanup_tasks(Duration::from_secs(1)).await;

        assert!(limit_cleanup_uncertainties() > before);
    }
}

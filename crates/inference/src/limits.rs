use std::{
    fmt,
    future::Future,
    pin::Pin,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

use arc_swap::ArcSwapOption;
use olp_domain::{ApiKey, Operation};
use olp_storage::{
    limits::DistributedLimiter, limits::LimitError, limits::LimitLease, limits::LimitRequest,
};
use tracing::{error, warn};

use crate::InferenceError;

type ReleaseFuture = Pin<Box<dyn Future<Output = ()> + Send + 'static>>;

const LIMIT_CLEANUP_ATTEMPTS: usize = 3;
const LIMIT_CLEANUP_TIMEOUT: Duration = Duration::from_millis(250);

async fn reconcile_distributed_limit(
    limiter: &DistributedLimiter,
    lease: &LimitLease,
    actual_tokens: i64,
) {
    for attempt in 0..LIMIT_CLEANUP_ATTEMPTS {
        match tokio::time::timeout(
            LIMIT_CLEANUP_TIMEOUT,
            limiter.reconcile(lease, actual_tokens),
        )
        .await
        {
            Ok(Ok(())) => return,
            Ok(Err(error)) if attempt + 1 == LIMIT_CLEANUP_ATTEMPTS => {
                warn!(%error, "failed to reconcile inference token reservation");
            }
            Err(_) if attempt + 1 == LIMIT_CLEANUP_ATTEMPTS => {
                warn!("timed out reconciling inference token reservation");
            }
            Ok(Err(_)) | Err(_) => {
                tokio::time::sleep(Duration::from_millis(25_u64 << attempt)).await;
            }
        }
    }
}

async fn release_distributed_limit(limiter: &DistributedLimiter, lease: &LimitLease) {
    for attempt in 0..LIMIT_CLEANUP_ATTEMPTS {
        match tokio::time::timeout(LIMIT_CLEANUP_TIMEOUT, limiter.release(lease)).await {
            Ok(Ok(())) => return,
            Ok(Err(error)) if attempt + 1 == LIMIT_CLEANUP_ATTEMPTS => {
                warn!(%error, "failed to release inference concurrency lease");
            }
            Err(_) if attempt + 1 == LIMIT_CLEANUP_ATTEMPTS => {
                warn!("timed out releasing inference concurrency lease");
            }
            Ok(Err(_)) | Err(_) => {
                tokio::time::sleep(Duration::from_millis(25_u64 << attempt)).await;
            }
        }
    }
}

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
                    release_distributed_limit(&limiter, &lease).await;
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
        reconcile_distributed_limit(&reconcile.limiter, &reconcile.lease, actual_tokens).await;
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

/// A distributed limit lease paired with the exact limiter connection that
/// created it. Cleanup never consults the hot-swappable limiter slot.
pub struct DistributedLimitReservation {
    limiter: Arc<DistributedLimiter>,
    lease: Option<LimitLease>,
}

impl fmt::Debug for DistributedLimitReservation {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("DistributedLimitReservation")
            .field("active", &self.lease.is_some())
            .finish_non_exhaustive()
    }
}

impl DistributedLimitReservation {
    fn new(limiter: Arc<DistributedLimiter>, lease: LimitLease) -> Self {
        Self {
            limiter,
            lease: Some(lease),
        }
    }

    async fn cleanup(mut self, actual_tokens: Option<i64>) {
        let Some(lease) = self.lease.as_ref() else {
            return;
        };
        if let Some(actual_tokens) = actual_tokens {
            reconcile_distributed_limit(&self.limiter, lease, actual_tokens).await;
        }
        release_distributed_limit(&self.limiter, lease).await;
        self.lease.take();
    }
}

impl Drop for DistributedLimitReservation {
    fn drop(&mut self) {
        let Some(lease) = self.lease.take() else {
            return;
        };
        let limiter = Arc::clone(&self.limiter);
        if let Ok(runtime) = tokio::runtime::Handle::try_current() {
            runtime.spawn(async move {
                release_distributed_limit(&limiter, &lease).await;
            });
        } else {
            warn!("could not release distributed limit lease outside a Tokio runtime");
        }
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
) -> Result<Option<DistributedLimitReservation>, InferenceError> {
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
            Ok(lease) => Ok(Some(DistributedLimitReservation::new(limiter, lease))),
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
        Ok(lease) => Ok(Some(DistributedLimitReservation::new(limiter, lease))),
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
    reservation: Option<DistributedLimitReservation>,
    actual_tokens: Option<i64>,
) {
    if let Some(reservation) = reservation {
        reservation.cleanup(actual_tokens).await;
    }
}

use std::{
    error::Error as StdError,
    fmt,
    future::Future,
    pin::Pin,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

use crate::domain::{auth::ApiKey, canonical::requests::Operation, ports::BoxFuture};
use arc_swap::ArcSwapOption;
use thiserror::Error;
use tracing::{error, warn};

use crate::inference::error::Error as InferenceError;

type ReleaseFuture = Pin<Box<dyn Future<Output = ()> + Send + 'static>>;

const LIMIT_CLEANUP_ATTEMPTS: usize = 3;
const LIMIT_CLEANUP_TIMEOUT: Duration = Duration::from_millis(250);

/// A transport-neutral distributed limit request.
#[derive(Debug, Clone)]
pub struct LimitRequest<'a> {
    pub lookup_id: &'a str,
    pub requests_per_minute: Option<i64>,
    pub tokens_per_minute: Option<i64>,
    pub max_concurrency: Option<i64>,
    pub requested_tokens: i64,
    pub lease_ttl: Duration,
}

impl LimitRequest<'_> {
    #[must_use]
    pub fn has_hard_limits(&self) -> bool {
        self.requests_per_minute.is_some()
            || self.tokens_per_minute.is_some()
            || self.max_concurrency.is_some()
    }

    pub fn validate(&self) -> Result<(), LimitError> {
        if !(8..=40).contains(&self.lookup_id.len())
            || !self
                .lookup_id
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_')
        {
            return Err(LimitError::InvalidRequest(
                "API key lookup ID must be 8-40 ASCII letters, digits, or underscores",
            ));
        }

        if self.requests_per_minute.is_some_and(|value| value <= 0) {
            return Err(LimitError::InvalidRequest(
                "requests_per_minute must be positive",
            ));
        }
        if self.tokens_per_minute.is_some_and(|value| value <= 0) {
            return Err(LimitError::InvalidRequest(
                "tokens_per_minute must be positive",
            ));
        }
        if self.max_concurrency.is_some_and(|value| value <= 0) {
            return Err(LimitError::InvalidRequest(
                "max_concurrency must be positive",
            ));
        }

        if self.requested_tokens < 0 {
            return Err(LimitError::InvalidRequest(
                "requested_tokens must be non-negative",
            ));
        }
        if self.tokens_per_minute.is_some() && self.requested_tokens == 0 {
            return Err(LimitError::InvalidRequest(
                "requested_tokens must be positive when a token limit is configured",
            ));
        }
        if self.lease_ttl.is_zero() {
            return Err(LimitError::InvalidRequest(
                "concurrency lease TTL must be greater than zero",
            ));
        }
        Ok(())
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LimitDimension {
    Requests,
    Tokens,
    Concurrency,
    Unknown,
}

/// Failure returned by a distributed limit backend without exposing its
/// storage client implementation to the engine.
#[derive(Debug, Error)]
pub enum LimitError {
    #[error("distributed limit service failed: {source}")]
    Service {
        #[source]
        source: Box<dyn StdError + Send + Sync>,
    },
    #[error("distributed limit state is malformed")]
    MalformedState,
    #[error("distributed limit backend returned an unexpected response")]
    UnexpectedResponse,
    #[error("invalid distributed limit request: {0}")]
    InvalidRequest(&'static str),
    #[error("{dimension:?} limit exceeded; retry after {retry_after:?}")]
    Exceeded {
        dimension: LimitDimension,
        retry_after: Duration,
    },
}

impl LimitError {
    pub fn service(source: impl StdError + Send + Sync + 'static) -> Self {
        Self::Service {
            source: Box::new(source),
        }
    }
}

/// An issued lease bound to the backend connection that created it.
/// Implementations must make reconciliation and release retry-safe.
pub trait LimitLease: Send + Sync {
    fn reconcile(&self, actual_tokens: i64) -> BoxFuture<'_, Result<(), LimitError>>;

    fn release(&self) -> BoxFuture<'_, Result<(), LimitError>>;
}

/// Storage-independent distributed limiter used by the inference engine.
pub trait LimitBackend: Send + Sync {
    fn reserve<'a>(
        &'a self,
        request: LimitRequest<'a>,
    ) -> BoxFuture<'a, Result<Arc<dyn LimitLease>, LimitError>>;

    fn ping(&self) -> BoxFuture<'_, Result<(), LimitError>>;
}

async fn reconcile_distributed_limit(lease: &dyn LimitLease, actual_tokens: i64) {
    for attempt in 0..LIMIT_CLEANUP_ATTEMPTS {
        match tokio::time::timeout(LIMIT_CLEANUP_TIMEOUT, lease.reconcile(actual_tokens)).await {
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

async fn release_distributed_limit(lease: &dyn LimitLease) {
    for attempt in 0..LIMIT_CLEANUP_ATTEMPTS {
        match tokio::time::timeout(LIMIT_CLEANUP_TIMEOUT, lease.release()).await {
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
    lease: Arc<dyn LimitLease>,
}

/// Cancellation-safe ownership of the request-boundary distributed limit
/// reservation. All clones share one idempotent reconciliation/release path.
#[derive(Clone)]
pub struct Reservation {
    inner: Arc<InferenceReservationInner>,
}

impl Reservation {
    #[must_use]
    pub fn distributed(lease: Arc<dyn LimitLease>) -> Self {
        let reconcile = DistributedReconciliation {
            lease: Arc::clone(&lease),
        };
        Self {
            inner: Arc::new(InferenceReservationInner {
                release: Mutex::new(Some(Box::pin(async move {
                    release_distributed_limit(lease.as_ref()).await;
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
        reconcile_distributed_limit(reconcile.lease.as_ref(), actual_tokens).await;
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
    lease: Option<Arc<dyn LimitLease>>,
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
    fn new(lease: Arc<dyn LimitLease>) -> Self {
        Self { lease: Some(lease) }
    }

    #[cfg(test)]
    pub(in crate::inference) fn for_test(lease: Arc<dyn LimitLease>) -> Self {
        Self::new(lease)
    }

    async fn cleanup(mut self, actual_tokens: Option<i64>) {
        let Some(lease) = self.lease.as_ref() else {
            return;
        };
        if let Some(actual_tokens) = actual_tokens {
            reconcile_distributed_limit(lease.as_ref(), actual_tokens).await;
        }
        release_distributed_limit(lease.as_ref()).await;
        self.lease.take();
    }
}

impl Drop for DistributedLimitReservation {
    fn drop(&mut self) {
        let Some(lease) = self.lease.take() else {
            return;
        };
        if let Ok(runtime) = tokio::runtime::Handle::try_current() {
            runtime.spawn(async move {
                release_distributed_limit(lease.as_ref()).await;
            });
        } else {
            warn!("could not release distributed limit lease outside a Tokio runtime");
        }
    }
}

/// Hot-swappable Valkey limiter connection used by inference services.
#[derive(Clone, Default)]
pub struct ReloadableLimiter {
    inner: Arc<ArcSwapOption<InstalledLimitBackend>>,
    configured: Arc<AtomicBool>,
}

struct InstalledLimitBackend {
    backend: Arc<dyn LimitBackend>,
}

impl ReloadableLimiter {
    pub fn mark_configured(&self) {
        self.configured.store(true, Ordering::Release);
    }

    #[must_use]
    pub fn is_configured(&self) -> bool {
        self.configured.load(Ordering::Acquire)
    }

    pub fn install(&self, backend: impl LimitBackend + 'static) {
        self.inner.store(Some(Arc::new(InstalledLimitBackend {
            backend: Arc::new(backend),
        })));
    }

    pub fn clear(&self) {
        self.inner.store(None);
    }

    #[must_use]
    pub fn current(&self) -> Option<Arc<dyn LimitBackend>> {
        self.inner
            .load_full()
            .map(|installed| Arc::clone(&installed.backend))
    }
}

pub async fn reserve(
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
            Ok(lease) => Ok(Some(DistributedLimitReservation::new(lease))),
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
        Ok(lease) => Ok(Some(DistributedLimitReservation::new(lease))),
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
                crate::domain::canonical::requests::EmbeddingInput::Text(text) => {
                    estimated_text_tokens(text)
                }
                crate::domain::canonical::requests::EmbeddingInput::Tokens(tokens) => tokens.len(),
            })
            .sum(),
        Operation::TokenCount(request) => estimated_content_tokens(&request.input),
        Operation::Images(crate::domain::canonical::requests::ImageOperation::Generation(
            request,
        )) => estimated_text_tokens(&request.prompt),
        Operation::Images(crate::domain::canonical::requests::ImageOperation::Edit(request)) => {
            estimated_text_tokens(&request.prompt)
                .saturating_add(request.images.len().saturating_mul(1_000))
                .saturating_add(usize::from(request.mask.is_some()) * 1_000)
        }
        Operation::Images(crate::domain::canonical::requests::ImageOperation::Variation(_)) => {
            1_000
        }
        Operation::Speech(request) => estimated_text_tokens(&request.input),
        Operation::Transcription(request) => request.prompt.as_deref().map_or(1_500, |prompt| {
            1_500_usize.saturating_add(estimated_text_tokens(prompt))
        }),
        Operation::Video(crate::domain::canonical::requests::VideoOperation::Create(request)) => {
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

fn estimated_content_tokens(parts: &[crate::domain::canonical::requests::ContentPart]) -> usize {
    parts
        .iter()
        .map(|part| match part {
            crate::domain::canonical::requests::ContentPart::Text { text }
            | crate::domain::canonical::requests::ContentPart::Refusal { text } => {
                estimated_text_tokens(text)
            }
            crate::domain::canonical::requests::ContentPart::Image { .. } => 1_000,
            crate::domain::canonical::requests::ContentPart::InputAudio { .. }
            | crate::domain::canonical::requests::ContentPart::InputFile { .. } => 2_000,
        })
        .sum()
}

pub async fn release(reservation: Option<DistributedLimitReservation>, actual_tokens: Option<i64>) {
    if let Some(reservation) = reservation {
        reservation.cleanup(actual_tokens).await;
    }
}

#[cfg(test)]
mod tests;

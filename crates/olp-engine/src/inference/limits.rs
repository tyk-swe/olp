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

use crate::domain::{ApiKey, BoxFuture, Operation};
use arc_swap::ArcSwapOption;
use thiserror::Error;
use tracing::{error, warn};

use crate::inference::InferenceError;

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
pub struct InferenceReservation {
    inner: Arc<InferenceReservationInner>,
}

impl InferenceReservation {
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
                crate::domain::EmbeddingInput::Text(text) => estimated_text_tokens(text),
                crate::domain::EmbeddingInput::Tokens(tokens) => tokens.len(),
            })
            .sum(),
        Operation::TokenCount(request) => estimated_content_tokens(&request.input),
        Operation::Images(crate::domain::ImageOperation::Generation(request)) => {
            estimated_text_tokens(&request.prompt)
        }
        Operation::Images(crate::domain::ImageOperation::Edit(request)) => {
            estimated_text_tokens(&request.prompt)
                .saturating_add(request.images.len().saturating_mul(1_000))
                .saturating_add(usize::from(request.mask.is_some()) * 1_000)
        }
        Operation::Images(crate::domain::ImageOperation::Variation(_)) => 1_000,
        Operation::Speech(request) => estimated_text_tokens(&request.input),
        Operation::Transcription(request) => request.prompt.as_deref().map_or(1_500, |prompt| {
            1_500_usize.saturating_add(estimated_text_tokens(prompt))
        }),
        Operation::Video(crate::domain::VideoOperation::Create(request)) => {
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

fn estimated_content_tokens(parts: &[crate::domain::ContentPart]) -> usize {
    parts
        .iter()
        .map(|part| match part {
            crate::domain::ContentPart::Text { text }
            | crate::domain::ContentPart::Refusal { text } => estimated_text_tokens(text),
            crate::domain::ContentPart::Image { .. } => 1_000,
            crate::domain::ContentPart::InputAudio { .. }
            | crate::domain::ContentPart::InputFile { .. } => 2_000,
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

#[cfg(test)]
mod tests {
    use std::{
        collections::BTreeSet,
        num::{NonZeroU32, NonZeroU64},
        sync::atomic::{AtomicBool, AtomicI64, AtomicUsize},
    };

    use crate::domain::{ApiKeyDigest, ApiKeyId, ApiKeyLimits, ApiKeyLookupId, ApiKeyStatus};
    use serde_json::json;

    use super::*;

    #[derive(Debug, Eq, PartialEq)]
    struct CapturedRequest {
        lookup_id: String,
        requests_per_minute: Option<i64>,
        tokens_per_minute: Option<i64>,
        max_concurrency: Option<i64>,
        requested_tokens: i64,
        lease_ttl: Duration,
    }

    #[derive(Default)]
    struct BackendCalls {
        reserves: AtomicUsize,
        reconciles: AtomicUsize,
        releases: AtomicUsize,
        actual_tokens: AtomicI64,
        fail_reconcile_once: AtomicBool,
        fail_release_once: AtomicBool,
        requests: Mutex<Vec<CapturedRequest>>,
    }

    #[derive(Clone, Copy)]
    enum BackendBehavior {
        Success,
        Exceeded(LimitDimension),
        Failure,
    }

    #[derive(Clone)]
    struct FakeBackend {
        calls: Arc<BackendCalls>,
        behavior: BackendBehavior,
    }

    struct FakeLease {
        calls: Arc<BackendCalls>,
    }

    impl LimitLease for FakeLease {
        fn reconcile(&self, actual_tokens: i64) -> BoxFuture<'_, Result<(), LimitError>> {
            self.calls.reconciles.fetch_add(1, Ordering::Relaxed);
            self.calls
                .actual_tokens
                .store(actual_tokens, Ordering::Relaxed);
            let fail = self
                .calls
                .fail_reconcile_once
                .swap(false, Ordering::Relaxed);
            Box::pin(async move {
                if fail {
                    Err(LimitError::UnexpectedResponse)
                } else {
                    Ok(())
                }
            })
        }

        fn release(&self) -> BoxFuture<'_, Result<(), LimitError>> {
            self.calls.releases.fetch_add(1, Ordering::Relaxed);
            let fail = self.calls.fail_release_once.swap(false, Ordering::Relaxed);
            Box::pin(async move {
                if fail {
                    Err(LimitError::UnexpectedResponse)
                } else {
                    Ok(())
                }
            })
        }
    }

    impl LimitBackend for FakeBackend {
        fn reserve<'a>(
            &'a self,
            request: LimitRequest<'a>,
        ) -> BoxFuture<'a, Result<Arc<dyn LimitLease>, LimitError>> {
            let calls = Arc::clone(&self.calls);
            let behavior = self.behavior;
            Box::pin(async move {
                request.validate()?;
                calls.reserves.fetch_add(1, Ordering::Relaxed);
                calls.requests.lock().unwrap().push(CapturedRequest {
                    lookup_id: request.lookup_id.to_owned(),
                    requests_per_minute: request.requests_per_minute,
                    tokens_per_minute: request.tokens_per_minute,
                    max_concurrency: request.max_concurrency,
                    requested_tokens: request.requested_tokens,
                    lease_ttl: request.lease_ttl,
                });
                match behavior {
                    BackendBehavior::Success => {
                        Ok(Arc::new(FakeLease { calls }) as Arc<dyn LimitLease>)
                    }
                    BackendBehavior::Exceeded(dimension) => Err(LimitError::Exceeded {
                        dimension,
                        retry_after: Duration::from_secs(2),
                    }),
                    BackendBehavior::Failure => Err(LimitError::MalformedState),
                }
            })
        }

        fn ping(&self) -> BoxFuture<'_, Result<(), LimitError>> {
            Box::pin(async { Ok(()) })
        }
    }

    fn backend(calls: &Arc<BackendCalls>, behavior: BackendBehavior) -> FakeBackend {
        FakeBackend {
            calls: Arc::clone(calls),
            behavior,
        }
    }

    fn api_key(limits: ApiKeyLimits) -> ApiKey {
        ApiKey {
            id: ApiKeyId::new(),
            lookup_id: ApiKeyLookupId::parse("lookup_01").unwrap(),
            digest: ApiKeyDigest::new([7; 32]),
            status: ApiKeyStatus::Active,
            expires_at: None,
            scopes: BTreeSet::new(),
            allowed_routes: BTreeSet::new(),
            limits,
        }
    }

    fn text_count(text: &str) -> Operation {
        operation(
            "token_count",
            json!({"route": "default", "input": [{"type": "text", "text": text}]}),
        )
    }

    fn operation(kind: &str, request: serde_json::Value) -> Operation {
        serde_json::from_value(json!({"operation": kind, "request": request})).unwrap()
    }

    async fn reserve(
        limiter: &ReloadableLimiter,
        key: &ApiKey,
        operation: &Operation,
        pre_reserved: Option<i64>,
    ) -> Result<Option<DistributedLimitReservation>, InferenceError> {
        reserve_limits(
            limiter,
            key,
            operation,
            key.lookup_id.as_str(),
            Duration::from_secs(30),
            pre_reserved,
        )
        .await
    }

    #[test]
    fn limit_requests_validate_every_backend_invariant() {
        let valid = LimitRequest {
            lookup_id: "lookup_01",
            requests_per_minute: Some(10),
            tokens_per_minute: Some(100),
            max_concurrency: Some(2),
            requested_tokens: 20,
            lease_ttl: Duration::from_secs(30),
        };
        assert!(valid.has_hard_limits());
        assert!(valid.validate().is_ok());

        type Mutator = for<'a> fn(&mut LimitRequest<'a>);
        let cases: [(&str, Mutator); 7] = [
            ("API key lookup ID", |r| r.lookup_id = "short"),
            ("requests_per_minute", |r| r.requests_per_minute = Some(0)),
            ("tokens_per_minute", |r| r.tokens_per_minute = Some(-1)),
            ("max_concurrency", |r| r.max_concurrency = Some(0)),
            ("non-negative", |r| r.requested_tokens = -1),
            ("positive when", |r| r.requested_tokens = 0),
            ("lease TTL", |r| r.lease_ttl = Duration::ZERO),
        ];
        for (expected, mutate) in cases {
            let mut request = valid.clone();
            mutate(&mut request);
            assert!(matches!(
                request.validate(),
                Err(LimitError::InvalidRequest(message)) if message.contains(expected)
            ));
        }

        let unlimited = LimitRequest {
            lookup_id: "lookup_01",
            requests_per_minute: None,
            tokens_per_minute: None,
            max_concurrency: None,
            requested_tokens: 0,
            lease_ttl: Duration::from_secs(1),
        };
        assert!(!unlimited.has_hard_limits());
        assert!(unlimited.validate().is_ok());
    }

    #[tokio::test]
    async fn full_limit_reservation_forwards_all_dimensions_and_reconciles_usage() {
        let calls = Arc::new(BackendCalls::default());
        let limiter = ReloadableLimiter::default();
        limiter.install(backend(&calls, BackendBehavior::Success));
        let key = api_key(ApiKeyLimits {
            requests_per_minute: NonZeroU32::new(10),
            tokens_per_minute: NonZeroU64::new(100),
            concurrency: NonZeroU32::new(2),
        });

        let reservation = reserve_limits(
            &limiter,
            &key,
            &text_count("12345678"),
            key.lookup_id.as_str(),
            Duration::from_secs(45),
            None,
        )
        .await
        .unwrap();
        assert_eq!(
            calls.requests.lock().unwrap().as_slice(),
            [CapturedRequest {
                lookup_id: "lookup_01".into(),
                requests_per_minute: Some(10),
                tokens_per_minute: Some(100),
                max_concurrency: Some(2),
                requested_tokens: 2,
                lease_ttl: Duration::from_secs(45),
            }]
        );

        release_limits(reservation, Some(1)).await;
        assert_eq!(calls.reconciles.load(Ordering::Relaxed), 1);
        assert_eq!(calls.actual_tokens.load(Ordering::Relaxed), 1);
        assert_eq!(calls.releases.load(Ordering::Relaxed), 1);
    }

    #[tokio::test]
    async fn http_token_reservation_charges_only_a_positive_estimate_delta() {
        let limiter = ReloadableLimiter::default();
        let no_token_limit = api_key(ApiKeyLimits::default());
        assert!(
            reserve(&limiter, &no_token_limit, &text_count("12345678"), Some(1))
                .await
                .unwrap()
                .is_none()
        );

        let token_limited = api_key(ApiKeyLimits {
            tokens_per_minute: NonZeroU64::new(100),
            ..ApiKeyLimits::default()
        });
        assert!(
            reserve(&limiter, &token_limited, &text_count("12345678"), Some(2))
                .await
                .unwrap()
                .is_none()
        );

        let calls = Arc::new(BackendCalls::default());
        limiter.install(backend(&calls, BackendBehavior::Success));
        let reservation = reserve(
            &limiter,
            &token_limited,
            &text_count("123456789012"),
            Some(1),
        )
        .await
        .unwrap();
        {
            let requests = calls.requests.lock().unwrap();
            assert_eq!(requests[0].requests_per_minute, None);
            assert_eq!(requests[0].tokens_per_minute, Some(100));
            assert_eq!(requests[0].max_concurrency, None);
            assert_eq!(requests[0].requested_tokens, 2);
        }
        release_limits(reservation, None).await;
    }

    #[tokio::test]
    async fn both_reservation_paths_map_exceeded_and_backend_failures_fail_closed() {
        let key = api_key(ApiKeyLimits {
            requests_per_minute: NonZeroU32::new(10),
            tokens_per_minute: NonZeroU64::new(100),
            concurrency: None,
        });
        for (behavior, expected_code) in [
            (
                BackendBehavior::Exceeded(LimitDimension::Tokens),
                "rate_limit_exceeded",
            ),
            (BackendBehavior::Failure, "distributed_limits_unavailable"),
        ] {
            for pre_reserved in [None, Some(1)] {
                let limiter = ReloadableLimiter::default();
                let calls = Arc::new(BackendCalls::default());
                limiter.install(backend(&calls, behavior));
                let error = reserve(&limiter, &key, &text_count("123456789012"), pre_reserved)
                    .await
                    .err()
                    .unwrap();
                assert_eq!(error.code(), expected_code);
                if matches!(behavior, BackendBehavior::Exceeded(_)) {
                    assert_eq!(error.retry_after(), Some(Duration::from_secs(2)));
                }
            }
        }

        let unavailable = ReloadableLimiter::default();
        assert_eq!(
            reserve(&unavailable, &key, &text_count("1234"), None)
                .await
                .err()
                .unwrap()
                .code(),
            "distributed_limits_unavailable"
        );
    }

    #[tokio::test]
    async fn shared_reservation_retries_cleanup_and_runs_each_action_once() {
        let calls = Arc::new(BackendCalls::default());
        calls.fail_reconcile_once.store(true, Ordering::Relaxed);
        calls.fail_release_once.store(true, Ordering::Relaxed);
        let lease: Arc<dyn LimitLease> = Arc::new(FakeLease {
            calls: Arc::clone(&calls),
        });
        let reservation = InferenceReservation::distributed(lease);
        let clone = reservation.clone();

        reservation.reconcile(17).await;
        clone.reconcile(99).await;
        clone.release().await;
        reservation.release().await;

        assert_eq!(calls.reconciles.load(Ordering::Relaxed), 2);
        assert_eq!(calls.actual_tokens.load(Ordering::Relaxed), 17);
        assert_eq!(calls.releases.load(Ordering::Relaxed), 2);
    }

    #[tokio::test]
    async fn dropping_a_test_reservation_schedules_its_release() {
        let (released, observed) = tokio::sync::oneshot::channel();
        let reservation = InferenceReservation::for_test(async move {
            let _ = released.send(());
        });
        drop(reservation);
        tokio::time::timeout(Duration::from_secs(1), observed)
            .await
            .expect("drop release should be scheduled")
            .expect("drop release should run");
    }

    #[test]
    fn token_estimation_covers_generation_metadata_and_every_media_weight() {
        let generation = operation(
            "generation",
            json!({
                "route": "default",
                "messages": [{
                    "role": "assistant",
                    "content": [{"type": "text", "text": "1234"}],
                    "name": "name",
                    "tool_call_id": "call",
                    "tool_calls": [{"id": "ignored", "name": "lookup", "arguments": "{}"}]
                }],
                "parameters": {"max_output_tokens": 10, "candidate_count": 2, "stream": false},
                "tools": [{"name": "tool", "description": "desc", "input_schema": null}],
                "tool_choice": null,
                "response_format": null
            }),
        );
        assert_eq!(estimate_tokens(&generation), 29);

        let Operation::TokenCount(media) = operation(
            "token_count",
            json!({
                "route": "default",
                "input": [
                    {"type": "text", "text": "12345678"},
                    {"type": "refusal", "text": "x"},
                    {"type": "image", "source": {"kind": "uri", "value": "https://example.test/image"}, "detail": null},
                    {"type": "input_audio", "media": "audio", "format": "wav"},
                    {"type": "input_file", "media": "file", "mime_type": "text/plain", "filename": "input.txt"}
                ]
            }),
        ) else {
            unreachable!()
        };
        assert_eq!(estimated_content_tokens(&media.input), 5_003);
    }

    #[test]
    fn token_estimation_is_defined_for_each_non_generation_operation_family() {
        let cases = [
            (
                "embeddings",
                r#"{"operation":"embeddings","request":{"route":"default","input":["12345",[1,2,3]],"dimensions":null}}"#,
                5,
            ),
            (
                "image generation",
                r#"{"operation":"images","request":{"kind":"generation","request":{"route":"default","prompt":"12345","count":null,"size":null,"stream":false}}}"#,
                2,
            ),
            (
                "image edit",
                r#"{"operation":"images","request":{"kind":"edit","request":{"route":"default","images":["a","b"],"mask":"c","prompt":"1234","stream":false}}}"#,
                3_001,
            ),
            (
                "image variation",
                r#"{"operation":"images","request":{"kind":"variation","request":{"route":"default","image":"media","count":null,"size":null}}}"#,
                1_000,
            ),
            (
                "speech",
                r#"{"operation":"speech","request":{"route":"default","input":"12345","voice":"voice","format":null,"stream":false}}"#,
                2,
            ),
            (
                "transcription",
                r#"{"operation":"transcription","request":{"route":"default","audio":"media","language":null,"prompt":"12345","stream":false}}"#,
                1_502,
            ),
            (
                "video create",
                r#"{"operation":"video","request":{"kind":"create","request":{"route":"default","prompt":"1234","input":"media"}}}"#,
                2_001,
            ),
            (
                "moderation",
                r#"{"operation":"moderation","request":{"route":"default","input":[{"type":"text","text":"12345678"}]}}"#,
                2,
            ),
            (
                "video metadata",
                r#"{"operation":"video","request":{"kind":"list","request":{"route":null,"cursor":null,"limit":null}}}"#,
                1,
            ),
            (
                "model metadata",
                r#"{"operation":"models","request":{"kind":"list"}}"#,
                1,
            ),
        ];
        for (name, value, expected) in cases {
            let operation: Operation = serde_json::from_str(value).unwrap();
            assert_eq!(estimate_tokens(&operation), expected, "{name}");
        }

        let no_prompt = operation(
            "transcription",
            json!({"route": "default", "audio": "media", "language": null, "prompt": null, "stream": false}),
        );
        assert_eq!(estimate_tokens(&no_prompt), 1_500);
    }

    #[tokio::test]
    async fn issued_lease_survives_backend_replacement_and_clear() {
        let first_calls = Arc::new(BackendCalls::default());
        let second_calls = Arc::new(BackendCalls::default());
        let limiter = ReloadableLimiter::default();
        limiter.install(backend(&first_calls, BackendBehavior::Success));

        let lease = limiter
            .current()
            .unwrap()
            .reserve(LimitRequest {
                lookup_id: "lookup_01",
                requests_per_minute: Some(10),
                tokens_per_minute: Some(100),
                max_concurrency: Some(2),
                requested_tokens: 20,
                lease_ttl: Duration::from_secs(30),
            })
            .await
            .unwrap();

        limiter.install(backend(&second_calls, BackendBehavior::Success));
        limiter.clear();
        DistributedLimitReservation::new(lease)
            .cleanup(Some(13))
            .await;

        assert_eq!(first_calls.reserves.load(Ordering::Relaxed), 1);
        assert_eq!(first_calls.reconciles.load(Ordering::Relaxed), 1);
        assert_eq!(first_calls.releases.load(Ordering::Relaxed), 1);
        assert_eq!(first_calls.actual_tokens.load(Ordering::Relaxed), 13);
        assert_eq!(second_calls.reserves.load(Ordering::Relaxed), 0);
        assert_eq!(second_calls.reconciles.load(Ordering::Relaxed), 0);
        assert_eq!(second_calls.releases.load(Ordering::Relaxed), 0);
        assert!(limiter.current().is_none());
    }
}

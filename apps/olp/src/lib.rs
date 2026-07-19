//! Axum delivery adapter for management, inference, the static operator
//! console, and the separately bound private observability surface.

mod catalog;
mod circuit;
mod cli;
mod connectors;
mod image_response;
mod json_media;
mod listener;
mod management;
mod media_spool;
mod oidc;
mod openai_models;
mod openai_response;
mod operations;
mod playground;
mod problem;
mod runtime;
mod semantic_validation;
mod services;
mod streaming_response;
mod vendor_gateway;

use std::{
    collections::{BTreeMap, BTreeSet},
    error::Error as StdError,
    fmt::Write as _,
    future::Future,
    net::{IpAddr, SocketAddr},
    path::PathBuf,
    pin::Pin,
    str::FromStr,
    sync::{
        Arc, Mutex, RwLock,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    task::{Context, Poll},
    time::{Duration, Instant},
};

use arc_swap::ArcSwapOption;
use axum::{
    BoxError, Router,
    body::{Body, HttpBody, to_bytes},
    error_handling::HandleErrorLayer,
    extract::{DefaultBodyLimit, State},
    http::{HeaderMap, HeaderName, Request, Uri},
    middleware,
    response::{IntoResponse, Response},
    routing::{any, get},
};
use olp_domain::{
    ApiKey, ApiKeyLookupId, ApiKeyStatus, MediaSpool, OperationKind, ProviderId, ProviderKind,
    ProviderTransport, RouteSlug, Surface, authorize_api_key,
};
use olp_storage::{
    DistributedLimiter, KeyHasher, LimitError, LimitLease, LimitRequest, MasterKey, PgStore,
    UsageConsumerStatus, UsageEmitter, UsageEpochHealth, UsageEvent,
};
use serde::Serialize;
use tokio::sync::RwLock as AsyncRwLock;
use tower::ServiceBuilder;
use tower_http::{
    catch_panic::CatchPanicLayer,
    request_id::{MakeRequestUuid, PropagateRequestIdLayer, SetRequestIdLayer},
    sensitive_headers::{SetSensitiveRequestHeadersLayer, SetSensitiveResponseHeadersLayer},
    set_header::SetResponseHeaderLayer,
    timeout::RequestBodyTimeoutLayer,
    trace::TraceLayer,
};
use utoipa::ToSchema;
use zeroize::Zeroizing;

pub use cli::run_cli;
pub use gateway::reconcile_media_jobs_once;
pub use management::management_openapi;
#[cfg(any(test, feature = "test-util"))]
pub use media_spool::create_bounded_media_spool_for_test;
pub use media_spool::create_media_spool;
pub use olp_providers::{
    CredentialKind, OpenAiConnector, ProviderConfig, ProviderCredential, ProviderError,
    ProviderFactory,
};
pub use problem::{FieldErrors, Problem};
pub use runtime::{RuntimeBundle, RuntimeInstallError, RuntimeManager};
pub use services::{
    ApiStartupError, CatalogService, IdentityService, InferenceService, ModeServices,
    OperationsService, WorkerService,
};

pub(crate) const MAX_JSON_BODY_BYTES: usize = 2 * 1024 * 1024;
pub(crate) const MAX_MEDIA_BODY_BYTES: usize = 64 * 1024 * 1024;
pub const MAX_HTTP_HEADER_COUNT: usize = 100;
pub const MAX_HTTP_HEADER_BYTES: usize = 32 * 1024;
const MAX_HEADER_VALUE_BYTES: usize = 8 * 1024;
const MAX_URI_BYTES: usize = 8 * 1024;
const MAX_JSON_DEPTH: usize = 64;
const REQUEST_BODY_TIMEOUT: Duration = Duration::from_secs(30);
const IMAGE_VARIATION_BODY_BYTES: usize = 55 * 1024 * 1024;
const TRANSCRIPTION_BODY_BYTES: usize = 30 * 1024 * 1024;
const VIDEO_CREATE_BODY_BYTES: usize = 25 * 1024 * 1024;
const OBSERVABILITY_CONCURRENCY_LIMIT: usize = 8;
const OBSERVABILITY_REQUEST_TIMEOUT: Duration = Duration::from_secs(2);
const OBSERVABILITY_REFRESH_TIMEOUT: Duration = Duration::from_secs(4);
const OBSERVABILITY_READINESS_REFRESH_INTERVAL: Duration = Duration::from_secs(5);
const OBSERVABILITY_METRICS_REFRESH_INTERVAL: Duration = Duration::from_secs(15);
// Metrics are refreshed every fifteen seconds.  Give a successful snapshot
// enough headroom for normal scheduler jitter and a single refresh timeout;
// otherwise a healthy metrics endpoint would mark itself stale for the last
// third of every refresh interval.
const OBSERVABILITY_SNAPSHOT_STALE_AFTER: Duration = Duration::from_secs(30);

tokio::task_local! {
    /// The immutable generation selected by the inference HTTP boundary. Every
    /// downstream authentication, route, capability, and transport decision
    /// must use this same bundle for the lifetime of the request.
    static HTTP_INFERENCE_RUNTIME: Arc<RuntimeBundle>;

    /// Set by the canonical pipeline once it owns metadata completion for an
    /// authenticated request. The HTTP boundary emits a content-free fallback
    /// only when decoding or authorization fails before that handoff.
    static HTTP_INFERENCE_METADATA_CLAIMED: Arc<AtomicBool>;

    /// Set while an authenticated inference request is executing beneath the
    /// HTTP boundary. Canonical executors use this marker to avoid charging a
    /// second RPM/TPM reservation for the same request.
    static HTTP_INFERENCE_LIMITS_RESERVED: i64;

    /// Present only for multipart reservations whose fixed pre-parse token
    /// baseline must be reconciled against the decoded canonical request.
    static HTTP_MULTIPART_TOKEN_RECONCILIATION: ();
}

pub(crate) fn pin_inference_runtime(state: &ApiState) -> Arc<RuntimeBundle> {
    HTTP_INFERENCE_RUNTIME
        .try_with(Arc::clone)
        .unwrap_or_else(|_| state.runtime.pin())
}

pub(crate) fn http_inference_reserved_tokens() -> Option<i64> {
    HTTP_INFERENCE_LIMITS_RESERVED
        .try_with(|tokens| *tokens)
        .ok()
}

pub(crate) fn http_multipart_token_reconciliation_required() -> bool {
    HTTP_MULTIPART_TOKEN_RECONCILIATION
        .try_with(|()| ())
        .is_ok()
}

pub(crate) fn claim_http_inference_metadata() {
    let _ = HTTP_INFERENCE_METADATA_CLAIMED.try_with(|claimed| {
        claimed.store(true, Ordering::Release);
    });
}

type ReleaseFuture = Pin<Box<dyn Future<Output = ()> + Send + 'static>>;

struct InferenceReservation {
    release: Option<ReleaseFuture>,
}

impl InferenceReservation {
    fn distributed(limiter: Arc<DistributedLimiter>, lease: LimitLease) -> Self {
        Self {
            release: Some(Box::pin(async move {
                match tokio::time::timeout(Duration::from_millis(250), limiter.release(&lease))
                    .await
                {
                    Ok(Ok(())) => {}
                    Ok(Err(error)) => {
                        tracing::warn!(%error, "failed to release HTTP concurrency lease");
                    }
                    Err(_) => tracing::warn!("timed out releasing HTTP concurrency lease"),
                }
            })),
        }
    }

    #[cfg(test)]
    fn for_test(release: impl Future<Output = ()> + Send + 'static) -> Self {
        Self {
            release: Some(Box::pin(release)),
        }
    }

    async fn release(mut self) {
        if let Some(release) = self.release.take() {
            release.await;
        }
    }

    fn spawn_release(&mut self) {
        if let Some(release) = self.release.take() {
            tokio::spawn(release);
        }
    }
}

struct ReleaseReservationBody {
    inner: Body,
    reservation: InferenceReservation,
}

impl HttpBody for ReleaseReservationBody {
    type Data = bytes::Bytes;
    type Error = axum::Error;

    fn poll_frame(
        self: Pin<&mut Self>,
        context: &mut Context<'_>,
    ) -> Poll<Option<Result<http_body::Frame<Self::Data>, Self::Error>>> {
        let this = self.get_mut();
        let poll = Pin::new(&mut this.inner).poll_frame(context);
        if matches!(poll, Poll::Ready(None)) {
            this.reservation.spawn_release();
        }
        poll
    }

    fn is_end_stream(&self) -> bool {
        self.inner.is_end_stream()
    }

    fn size_hint(&self) -> http_body::SizeHint {
        self.inner.size_hint()
    }
}

impl Drop for ReleaseReservationBody {
    fn drop(&mut self) {
        self.reservation.spawn_release();
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ApiMode {
    All,
    Gateway,
    Control,
}

impl ApiMode {
    pub const fn serves_gateway(self) -> bool {
        matches!(self, Self::All | Self::Gateway)
    }

    pub const fn serves_control(self) -> bool {
        matches!(self, Self::All | Self::Control)
    }
}

/// A CIDR range whose peer addresses are allowed to provide a forwarding
/// chain for public-auth source attribution. Direct clients never control the
/// resolved source through `X-Forwarded-For`.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TrustedProxyCidr {
    network: ipnet::IpNet,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TrustedProxyCidrParseError {
    detail: &'static str,
}

impl std::fmt::Display for TrustedProxyCidrParseError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(self.detail)
    }
}

impl StdError for TrustedProxyCidrParseError {}

impl FromStr for TrustedProxyCidr {
    type Err = TrustedProxyCidrParseError;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        let value = value.trim();
        if !value.contains('/') {
            return Err(TrustedProxyCidrParseError {
                detail: "trusted proxy CIDRs must use address/prefix notation",
            });
        }
        let network = value.parse().map_err(|_| TrustedProxyCidrParseError {
            detail: "trusted proxy CIDR is invalid",
        })?;
        Ok(Self { network })
    }
}

impl TrustedProxyCidr {
    #[must_use]
    pub fn contains(&self, address: IpAddr) -> bool {
        self.network.contains(&address)
    }
}

/// The route information authenticated before a multipart body is read. A
/// route-restricted key must either supply the header it was pre-authorized
/// for, or place `model` before every file part so the parser can authorize it
/// before creating a spool file.
#[derive(Clone, Debug)]
pub(crate) enum MultipartRouteAdmission {
    Unrestricted,
    RequireModelBeforeFile(BTreeSet<RouteSlug>),
    Expected(RouteSlug),
}

impl MultipartRouteAdmission {
    pub(crate) const fn requires_model_before_file(&self) -> bool {
        matches!(self, Self::RequireModelBeforeFile(_))
    }
}

#[derive(Clone)]
pub(crate) struct MultipartRequestAdmission {
    pub(crate) route: MultipartRouteAdmission,
    lease: Option<MultipartParserLease>,
}

#[derive(Clone, Copy)]
pub(crate) struct FirstOwnerSetupAuthorized;

impl MultipartRequestAdmission {
    #[cfg(test)]
    pub(crate) const fn unrestricted() -> Self {
        Self {
            route: MultipartRouteAdmission::Unrestricted,
            lease: None,
        }
    }

    pub(crate) fn release(&self) {
        if let Some(lease) = &self.lease {
            lease.release();
        }
    }
}

#[derive(Clone)]
struct MultipartAdmissionState {
    inner: Arc<MultipartAdmissionInner>,
}

struct MultipartAdmissionInner {
    /// Only half the total spool is ever promised to untrusted parsers. The
    /// spool itself continues to enforce byte-accurate accounting for all
    /// request and response media.
    budget_bytes: u64,
    reserved_bytes: AtomicU64,
    active_keys: Mutex<BTreeSet<uuid::Uuid>>,
}

#[derive(Clone)]
struct MultipartParserLease {
    inner: Arc<MultipartParserLeaseInner>,
}

struct MultipartParserLeaseInner {
    admission: MultipartAdmissionState,
    api_key_id: uuid::Uuid,
    reservation_bytes: u64,
    released: AtomicBool,
}

impl MultipartAdmissionState {
    fn new(capacity_bytes: u64) -> Self {
        Self {
            inner: Arc::new(MultipartAdmissionInner {
                budget_bytes: capacity_bytes / 2,
                reserved_bytes: AtomicU64::new(0),
                active_keys: Mutex::new(BTreeSet::new()),
            }),
        }
    }

    fn try_admit(
        &self,
        api_key_id: uuid::Uuid,
        reservation_bytes: u64,
    ) -> Option<MultipartParserLease> {
        if reservation_bytes == 0 || reservation_bytes > self.inner.budget_bytes {
            return None;
        }
        let mut active_keys = self.inner.active_keys.lock().ok()?;
        if active_keys.contains(&api_key_id) {
            return None;
        }
        let reserved = self
            .inner
            .reserved_bytes
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |current| {
                current
                    .checked_add(reservation_bytes)
                    .filter(|next| *next <= self.inner.budget_bytes)
            })
            .is_ok();
        if !reserved {
            return None;
        }
        active_keys.insert(api_key_id);
        Some(MultipartParserLease {
            inner: Arc::new(MultipartParserLeaseInner {
                admission: self.clone(),
                api_key_id,
                reservation_bytes,
                released: AtomicBool::new(false),
            }),
        })
    }
}

impl MultipartParserLease {
    fn release(&self) {
        self.inner.release();
    }
}

impl MultipartParserLeaseInner {
    fn release(&self) {
        if self.released.swap(true, Ordering::AcqRel) {
            return;
        }
        let previous = self
            .admission
            .inner
            .reserved_bytes
            .fetch_sub(self.reservation_bytes, Ordering::AcqRel);
        debug_assert!(previous >= self.reservation_bytes);
        if let Ok(mut active_keys) = self.admission.inner.active_keys.lock() {
            active_keys.remove(&self.api_key_id);
        }
    }
}

impl Drop for MultipartParserLeaseInner {
    fn drop(&mut self) {
        // A cancelled request can drop before the parser explicitly returns.
        // The final Arc owner performs the same cleanup exactly once.
        self.release();
    }
}

#[derive(Clone)]
pub struct ApiState {
    pub mode: ApiMode,
    pub store: Option<PgStore>,
    pub runtime: Arc<RuntimeManager>,
    pub limiter: LimiterManager,
    pub key_hasher: Option<Arc<KeyHasher>>,
    trusted_proxy_cidrs: Arc<[TrustedProxyCidr]>,
    bootstrap_token_digest: Arc<AsyncRwLock<Option<Zeroizing<[u8; 32]>>>>,
    pub master_key: Option<Arc<MasterKey>>,
    pub usage: Option<UsageEmitter>,
    pub(crate) circuits: circuit::CircuitBreaker,
    media_reconciliation_gaps: Arc<AtomicU64>,
    pub media_spool: Arc<dyn MediaSpool>,
    multipart_admission: MultipartAdmissionState,
    pub transports: TransportRegistry,
    catalog_openai_connectors: olp_providers::OpenAiConnectorOverrideRegistry,
    pub public_origin: Arc<str>,
    pub console_dir: Arc<PathBuf>,
    pub session_ttl: chrono::Duration,
    observability: ObservabilityCache,
    /// Enables loopback HTTP only for local mock-IdP tests. Runtime wiring
    /// keeps this production-safe default disabled.
    pub oidc_allow_insecure_test_endpoints: bool,
}

impl ApiState {
    pub fn new(
        mode: ApiMode,
        store: Option<PgStore>,
        runtime: Arc<RuntimeManager>,
        public_origin: impl Into<String>,
        console_dir: impl Into<PathBuf>,
    ) -> Self {
        let media_spool = media_spool::FileMediaSpool::create()
            .expect("the private bounded media spool must be creatable");
        Self::new_with_media_spool(
            mode,
            store,
            runtime,
            public_origin,
            console_dir,
            media_spool,
        )
    }

    pub fn new_with_media_spool(
        mode: ApiMode,
        store: Option<PgStore>,
        runtime: Arc<RuntimeManager>,
        public_origin: impl Into<String>,
        console_dir: impl Into<PathBuf>,
        media_spool: Arc<dyn MediaSpool>,
    ) -> Self {
        let multipart_admission = MultipartAdmissionState::new(
            media_spool
                .capacity_bytes()
                .unwrap_or(media_spool::DEFAULT_CAPACITY_BYTES),
        );
        Self {
            mode,
            store,
            runtime,
            limiter: LimiterManager::default(),
            key_hasher: None,
            trusted_proxy_cidrs: Arc::from([]),
            bootstrap_token_digest: Arc::new(AsyncRwLock::new(None)),
            master_key: None,
            usage: None,
            circuits: circuit::CircuitBreaker::default(),
            media_reconciliation_gaps: Arc::new(AtomicU64::new(0)),
            media_spool,
            multipart_admission,
            transports: TransportRegistry::default(),
            catalog_openai_connectors: Default::default(),
            public_origin: Arc::from(public_origin.into().trim_end_matches('/')),
            console_dir: Arc::new(console_dir.into()),
            session_ttl: chrono::Duration::hours(12),
            observability: ObservabilityCache::default(),
            oidc_allow_insecure_test_endpoints: false,
        }
    }

    /// Installs the explicit proxy trust boundary parsed at process startup.
    /// An empty set means that all `X-Forwarded-For` headers are ignored.
    pub fn set_trusted_proxy_cidrs(&mut self, cidrs: Vec<TrustedProxyCidr>) {
        self.trusted_proxy_cidrs = Arc::from(cidrs);
    }

    /// Stores only a keyed digest of the first-run setup token. The raw token
    /// is never retained by the HTTP state.
    pub fn set_bootstrap_token_digest(&mut self, digest: [u8; 32]) {
        self.bootstrap_token_digest = Arc::new(AsyncRwLock::new(Some(Zeroizing::new(digest))));
    }

    pub(crate) async fn verify_bootstrap_token(&self, supplied: Option<&str>) -> Option<bool> {
        let digest = self.bootstrap_token_digest.read().await;
        let expected = digest.as_ref()?;
        let Some(hasher) = self.key_hasher.as_ref() else {
            return Some(false);
        };
        Some(
            supplied
                .is_some_and(|supplied| hasher.verify_bootstrap_token_digest(supplied, expected)),
        )
    }

    pub(crate) async fn clear_bootstrap_token(&self) {
        let mut digest = self.bootstrap_token_digest.write().await;
        // `Zeroizing` clears the digest as it is dropped.
        *digest = None;
    }

    #[must_use]
    fn peer_is_trusted_proxy(&self, peer: IpAddr) -> bool {
        self.trusted_proxy_cidrs
            .iter()
            .any(|cidr| cidr.contains(peer))
    }

    pub(crate) fn record_media_reconciliation_gap(&self) {
        let _ = self.media_reconciliation_gaps.fetch_update(
            Ordering::Relaxed,
            Ordering::Relaxed,
            |value| Some(value.saturating_add(1)),
        );
    }

    /// Installs a prebuilt OpenAI connector for catalog connectivity checks.
    ///
    /// This is an internal dependency-injection seam used by integration tests
    /// to exercise real connector I/O against a local mock without weakening
    /// the production custom-endpoint SSRF policy. Production wiring never
    /// installs an override.
    #[doc(hidden)]
    pub fn register_catalog_openai_connector_for_test(
        &self,
        provider_id: uuid::Uuid,
        connector: OpenAiConnector,
    ) {
        self.catalog_openai_connectors
            .register(provider_id, connector);
    }

    pub(crate) fn catalog_openai_connector(
        &self,
        provider_id: uuid::Uuid,
        kind: ProviderKind,
    ) -> Option<olp_providers::ProviderFacade> {
        self.catalog_openai_connectors.get(provider_id, kind)
    }

    fn media_reconciliation_gap_count(&self) -> u64 {
        self.media_reconciliation_gaps.load(Ordering::Relaxed)
    }

    /// Returns the cached readiness evaluation without performing dependency
    /// I/O. Management callers use this authenticated view instead of reaching
    /// the private observability listener from a browser.
    pub(crate) fn cached_readiness(&self) -> Result<HealthResponse, Problem> {
        let snapshot = self.observability.readiness();
        cached_readiness_from_snapshot(&snapshot, Instant::now())
    }
}

fn forwarded_for_invalid() -> Problem {
    Problem::bad_request(
        "forwarded_for_invalid",
        "The trusted proxy supplied a malformed forwarding chain.",
    )
}

/// Resolves the source identity used exclusively for unauthenticated public
/// authentication admission. Production listeners attach the TCP peer via
/// `ConnectInfo`. Embeddings that omit it fail closed rather than silently
/// sharing a single global admission bucket.
pub fn public_auth_source(
    state: &ApiState,
    headers: &HeaderMap,
    peer: Option<SocketAddr>,
) -> Result<String, Problem> {
    let peer = peer
        .map(|address| address.ip())
        .ok_or_else(|| Problem::service_unavailable("client_address_unavailable"))?;
    if !state.peer_is_trusted_proxy(peer) {
        // A direct client cannot influence admission by spoofing a forwarding
        // header; only its connected peer address is authoritative.
        return Ok(peer.to_string());
    }

    let forwarded_for = HeaderName::from_static("x-forwarded-for");
    let mut chain = Vec::new();
    for value in headers.get_all(forwarded_for).iter() {
        let value = value.to_str().map_err(|_| forwarded_for_invalid())?;
        for candidate in value.split(',') {
            let candidate = candidate.trim();
            if candidate.is_empty() {
                return Err(forwarded_for_invalid());
            }
            let address = candidate
                .parse::<IpAddr>()
                .map_err(|_| forwarded_for_invalid())?;
            chain.push(address);
        }
    }
    if chain.is_empty() {
        return Err(Problem::bad_request(
            "forwarded_for_required",
            "A trusted proxy must provide a forwarding chain for public authentication.",
        ));
    }
    chain
        .into_iter()
        .rev()
        .find(|address| !state.peer_is_trusted_proxy(*address))
        .map(|address| address.to_string())
        .ok_or_else(|| {
            Problem::bad_request(
                "forwarded_for_invalid",
                "The trusted proxy supplied a forwarding chain without a client address.",
            )
        })
}

fn resolve_auth_source<'a>(
    state: &'a ApiState,
    headers: &HeaderMap,
    peer: Option<SocketAddr>,
) -> Result<(String, &'a Arc<KeyHasher>), Problem> {
    let source = public_auth_source(state, headers, peer)?;
    let hasher = state
        .key_hasher
        .as_ref()
        .ok_or_else(|| Problem::service_unavailable("key_hash_key_not_configured"))?;
    Ok((source, hasher))
}

pub(crate) fn public_auth_source_digest(
    state: &ApiState,
    headers: &HeaderMap,
    peer: Option<SocketAddr>,
) -> Result<[u8; 32], Problem> {
    let (source, hasher) = resolve_auth_source(state, headers, peer)?;
    Ok(hasher.public_auth_source_digest(&source))
}

pub(crate) fn public_auth_source_target_digests(
    state: &ApiState,
    headers: &HeaderMap,
    peer: Option<SocketAddr>,
    target: &str,
) -> Result<([u8; 32], [u8; 32]), Problem> {
    let (source, hasher) = resolve_auth_source(state, headers, peer)?;
    Ok((
        hasher.public_auth_source_digest(&source),
        hasher.public_auth_source_target_digest(&source, target),
    ))
}

/// Hot-swappable Valkey limiter connection. Gateways start even if Valkey is
/// temporarily unavailable; hard-limited keys still fail closed, while a
/// background supervisor can install a healthy connection without restarting
/// the process.
#[derive(Clone, Default)]
pub struct LimiterManager {
    inner: Arc<ArcSwapOption<DistributedLimiter>>,
    configured: Arc<AtomicBool>,
}

impl LimiterManager {
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
    pub fn get(&self) -> Option<Arc<DistributedLimiter>> {
        self.inner.load_full()
    }
}

#[derive(Clone, Default)]
pub struct TransportRegistry {
    inner: Arc<RwLock<BTreeMap<ProviderId, Arc<dyn ProviderTransport>>>>,
}

impl TransportRegistry {
    pub fn register(&self, provider_id: ProviderId, transport: Arc<dyn ProviderTransport>) {
        self.inner
            .write()
            .expect("transport registry lock poisoned")
            .insert(provider_id, transport);
    }

    #[must_use]
    pub fn snapshot(&self) -> BTreeMap<ProviderId, Arc<dyn ProviderTransport>> {
        self.inner
            .read()
            .expect("transport registry lock poisoned")
            .clone()
    }
}

/// Builds the public application router. Observability is intentionally served
/// by [`observability_router`] on a separate listener. Public-auth callers
/// must attach [`axum::extract::ConnectInfo`] with the socket peer; the
/// hardened application listener does so automatically.
///
pub fn public_router(state: ApiState) -> Router {
    let request_id = HeaderName::from_static("x-request-id");
    let request_limit_state = state.clone();
    let content_security_policy = static_console::content_security_policy(&state.console_dir);
    // Keep these exact paths ahead of the console fallback. A static console
    // file must never accidentally republish a probe or metrics endpoint.
    let mut router = Router::new()
        .route("/health", any(public_observability_not_found))
        .route("/health/{*path}", any(public_observability_not_found))
        .route("/metrics", any(public_observability_not_found))
        .route("/metrics/{*path}", any(public_observability_not_found));

    if state.mode.serves_control() {
        let control = Router::new()
            .route("/openapi.json", any(api_not_found))
            .merge(management::router())
            .merge(oidc::router())
            .merge(catalog::router())
            .merge(operations::router())
            .merge(playground::router())
            .route("/api/{*path}", any(api_not_found))
            .layer(middleware::from_fn(normalize_management_rejection));
        router = router
            .merge(control)
            .fallback_service(static_console::service(&state.console_dir));
    }

    if state.mode.serves_gateway() {
        // Protocol routes are merged here by the gateway module once transports
        // have been wired. Keeping mode composition explicit prevents a control
        // deployment from accidentally becoming an inference data plane.
        router = router
            .merge(gateway::router())
            .merge(vendor_gateway::router())
            .route("/openai/{*path}", any(protocol_not_found))
            .route("/anthropic/{*path}", any(protocol_not_found))
            .route("/gemini/{*path}", any(protocol_not_found));
    }

    router
        .layer(
            ServiceBuilder::new()
                .layer(SetSensitiveRequestHeadersLayer::new(
                    sensitive_request_headers(),
                ))
                .layer(SetRequestIdLayer::new(request_id.clone(), MakeRequestUuid))
                .layer(PropagateRequestIdLayer::new(request_id))
                .layer(TraceLayer::new_for_http().make_span_with(http_request_span))
                .layer(SetSensitiveResponseHeadersLayer::new(
                    sensitive_response_headers(),
                ))
                .layer(CatchPanicLayer::custom(problem_panic_response))
                .layer(RequestBodyTimeoutLayer::new(REQUEST_BODY_TIMEOUT))
                .layer(SetResponseHeaderLayer::if_not_present(
                    HeaderName::from_static("x-content-type-options"),
                    axum::http::HeaderValue::from_static("nosniff"),
                ))
                .layer(SetResponseHeaderLayer::if_not_present(
                    HeaderName::from_static("x-frame-options"),
                    axum::http::HeaderValue::from_static("DENY"),
                ))
                .layer(SetResponseHeaderLayer::if_not_present(
                    HeaderName::from_static("referrer-policy"),
                    axum::http::HeaderValue::from_static("no-referrer"),
                ))
                .layer(SetResponseHeaderLayer::if_not_present(
                    HeaderName::from_static("permissions-policy"),
                    axum::http::HeaderValue::from_static(
                        "camera=(), microphone=(), geolocation=(), payment=()",
                    ),
                ))
                .layer(SetResponseHeaderLayer::if_not_present(
                    HeaderName::from_static("content-security-policy"),
                    content_security_policy,
                )),
        )
        .layer(DefaultBodyLimit::max(MAX_JSON_BODY_BYTES))
        .layer(middleware::from_fn_with_state(
            request_limit_state,
            enforce_request_limits,
        ))
        .layer(middleware::from_fn(normalize_management_rejection))
        .with_state(state)
}

/// Builds the public router only after proving the selected mode's dependency
/// contract. Process composition uses this entrypoint so missing dependencies
/// fail before either listener is advertised.
pub fn try_public_router(state: ApiState) -> Result<Router, ApiStartupError> {
    let _services = state.mode_services()?;
    Ok(public_router(state))
}

/// Builds the private observability router. It exposes no console,
/// management, or inference routes.
pub fn observability_router(state: ApiState) -> Router {
    Router::new()
        .route("/health/live", get(live))
        .route("/health/ready", get(ready))
        .route("/metrics", get(metrics))
        .with_state(state)
        .layer(
            ServiceBuilder::new()
                .layer(HandleErrorLayer::new(observability_service_error))
                .layer(tower::load_shed::LoadShedLayer::new())
                .layer(tower::limit::ConcurrencyLimitLayer::new(
                    OBSERVABILITY_CONCURRENCY_LIMIT,
                ))
                .layer(tower::timeout::TimeoutLayer::new(
                    OBSERVABILITY_REQUEST_TIMEOUT,
                )),
        )
}

async fn public_observability_not_found() -> axum::http::StatusCode {
    axum::http::StatusCode::NOT_FOUND
}

async fn observability_service_error(error: BoxError) -> Problem {
    if error.is::<tower::timeout::error::Elapsed>() {
        Problem::new(
            axum::http::StatusCode::SERVICE_UNAVAILABLE,
            "observability_timeout",
            "Observability unavailable",
            "The observability request exceeded its deadline.",
        )
    } else {
        Problem::new(
            axum::http::StatusCode::SERVICE_UNAVAILABLE,
            "observability_overloaded",
            "Observability unavailable",
            "The observability listener is temporarily overloaded.",
        )
    }
}

/// Axum extractor rejections otherwise bypass the RFC 9457 management error
/// contract and return `text/plain`. Normalize malformed path/query values at
/// the management boundary without reflecting their potentially sensitive raw
/// values.
async fn normalize_management_rejection(
    request: Request<Body>,
    next: middleware::Next,
) -> Response {
    let uri = request.uri().clone();
    let response = next.run(request).await;
    if !uri.path().starts_with("/api/")
        || !response.status().is_client_error() && !response.status().is_server_error()
    {
        return response;
    }
    let is_problem = response
        .headers()
        .get(axum::http::header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .is_some_and(|value| value.starts_with("application/problem+json"));
    if is_problem {
        return response;
    }
    let status = response.status();
    let allow = response.headers().get(axum::http::header::ALLOW).cloned();
    let (code, title, detail) = match status {
        axum::http::StatusCode::BAD_REQUEST => (
            "invalid_parameters",
            "Invalid request",
            "One or more path, query, or body parameters are malformed.",
        ),
        axum::http::StatusCode::NOT_FOUND => (
            "management_endpoint_not_found",
            "Endpoint not found",
            "The requested management endpoint does not exist.",
        ),
        axum::http::StatusCode::METHOD_NOT_ALLOWED => (
            "method_not_allowed",
            "Method not allowed",
            "The management endpoint does not support this HTTP method.",
        ),
        axum::http::StatusCode::PAYLOAD_TOO_LARGE => (
            "payload_too_large",
            "Payload too large",
            "The request body exceeds the configured limit.",
        ),
        axum::http::StatusCode::REQUEST_TIMEOUT => (
            "request_timeout",
            "Request timeout",
            "The request body was not received before the deadline.",
        ),
        _ if status.is_server_error() => (
            "internal_error",
            "Internal error",
            "The request could not be completed.",
        ),
        _ => (
            "request_rejected",
            "Request rejected",
            "The management request was rejected.",
        ),
    };
    let mut problem = Problem::new(status, code, title, detail);
    if status == axum::http::StatusCode::BAD_REQUEST {
        problem.errors.insert(
            "request".to_owned(),
            vec!["One or more request parameters are malformed.".to_owned()],
        );
    }
    let mut normalized = problem.with_instance(&uri).into_response();
    if let Some(allow) = allow {
        normalized
            .headers_mut()
            .insert(axum::http::header::ALLOW, allow);
    }
    normalized
}

fn problem_panic_response(_panic: Box<dyn std::any::Any + Send + 'static>) -> Response<Body> {
    // The panic payload can contain request or upstream data. The active HTTP
    // span retains method, path, and request ID without exposing that payload.
    tracing::error!("HTTP request handler panicked");
    Problem::internal().into_response()
}

fn sensitive_request_headers() -> [HeaderName; 6] {
    [
        axum::http::header::AUTHORIZATION,
        axum::http::header::COOKIE,
        HeaderName::from_static(management::CSRF_HEADER),
        HeaderName::from_static(management::SETUP_TOKEN_HEADER),
        HeaderName::from_static("x-api-key"),
        HeaderName::from_static("x-goog-api-key"),
    ]
}

fn sensitive_response_headers() -> [HeaderName; 1] {
    [axum::http::header::SET_COOKIE]
}

fn request_trace_path(uri: &Uri) -> &str {
    uri.path()
}

fn http_request_span(request: &Request<Body>) -> tracing::Span {
    let request_id = request
        .headers()
        .get("x-request-id")
        .and_then(|value| value.to_str().ok())
        .unwrap_or("unavailable");
    tracing::info_span!(
        "http_request",
        method = %request.method(),
        path = %request_trace_path(request.uri()),
        request_id = %request_id,
    )
}

async fn api_not_found(uri: Uri) -> Problem {
    Problem::new(
        axum::http::StatusCode::NOT_FOUND,
        "management_endpoint_not_found",
        "Endpoint not found",
        "The requested management endpoint does not exist.",
    )
    .with_instance(&uri)
}

async fn protocol_not_found(uri: Uri) -> Problem {
    Problem::new(
        axum::http::StatusCode::NOT_FOUND,
        "protocol_endpoint_not_found",
        "Endpoint not found",
        "The requested inference endpoint is not enabled in this release.",
    )
    .with_instance(&uri)
}

async fn enforce_request_limits(
    State(state): State<ApiState>,
    request: Request<axum::body::Body>,
    next: middleware::Next,
) -> Response {
    let surface = inference_surface(request.uri().path());
    match enforce_request_limits_inner(&state, request, next, surface).await {
        Ok(response) => response,
        Err(RequestLimitRejection::Problem(problem)) => match surface {
            Some(surface) => vendor_gateway::problem_response(surface, problem),
            None => problem.into_response(),
        },
        Err(RequestLimitRejection::Inference(error)) => match surface {
            Some(surface) => vendor_gateway::inference_error_response(surface, error),
            None => Problem::from(error).into_response(),
        },
    }
}

enum RequestLimitRejection {
    Problem(Problem),
    Inference(gateway::InferenceError),
}

impl From<Problem> for RequestLimitRejection {
    fn from(problem: Problem) -> Self {
        Self::Problem(problem)
    }
}

impl From<gateway::InferenceError> for RequestLimitRejection {
    fn from(error: gateway::InferenceError) -> Self {
        Self::Inference(error)
    }
}

#[derive(Clone)]
struct LocalRequestMetadata {
    usage: Option<UsageEmitter>,
    request_started_at: chrono::DateTime<chrono::Utc>,
    runtime_generation_id: uuid::Uuid,
    api_key_id: uuid::Uuid,
    route_slug: String,
    operation: &'static str,
    surface: Surface,
    always_emit: bool,
}

impl LocalRequestMetadata {
    fn emit(self, status: axum::http::StatusCode) {
        let Some(usage) = self.usage else {
            return;
        };
        let completed_at = chrono::Utc::now();
        let latency_ms = completed_at
            .signed_duration_since(self.request_started_at)
            .num_milliseconds()
            .max(0)
            .try_into()
            .unwrap_or(u64::MAX);
        let operation = self.operation;
        let event = UsageEvent {
            event_id: uuid::Uuid::now_v7(),
            request_id: uuid::Uuid::now_v7(),
            runtime_generation_id: self.runtime_generation_id,
            api_key_id: self.api_key_id,
            provider_id: None,
            route_slug: self.route_slug,
            upstream_model: None,
            operation: operation
                .parse()
                .expect("local metadata uses a canonical operation"),
            surface: self.surface,
            request_started_at: self.request_started_at,
            request_completed_at: completed_at,
            observed_at: completed_at,
            status_code: Some(status.as_u16()),
            error_class: status
                .is_client_error()
                .then(|| "client_error".to_owned())
                .or_else(|| status.is_server_error().then(|| "server_error".to_owned())),
            committed: false,
            latency_ms,
            first_byte_ms: None,
            input_tokens: None,
            output_tokens: None,
            cached_input_tokens: None,
            media_units: None,
            usage_complete: false,
            unpriced: true,
            attempts: Vec::new(),
        };
        if let Err(error) = usage.emit(event) {
            tracing::warn!(%error, operation, "local request metadata was not queued");
        }
    }
}

fn inference_metadata_operation(
    method: &axum::http::Method,
    path: &str,
) -> Option<(&'static str, &'static str)> {
    if *method == axum::http::Method::GET
        && (path == "/openai/v1/models"
            || path == "/anthropic/v1/models"
            || path == "/gemini/v1/models"
            || path == "/gemini/v1beta/models")
    {
        Some(("model_list", "models"))
    } else if *method == axum::http::Method::GET
        && (path.starts_with("/openai/v1/models/")
            || path.starts_with("/anthropic/v1/models/")
            || path.starts_with("/gemini/v1/models/")
            || path.starts_with("/gemini/v1beta/models/"))
    {
        Some(("model_get", "models"))
    } else if path == "/openai/v1/videos" && *method == axum::http::Method::GET {
        Some(("video_list", "videos"))
    } else if path == "/openai/v1/videos" && *method == axum::http::Method::POST {
        Some(("video_create", "invalid-request"))
    } else if path.starts_with("/openai/v1/videos/") && path.ends_with("/content") {
        Some(("video_content", "invalid-request"))
    } else if path.starts_with("/openai/v1/videos/") && *method == axum::http::Method::DELETE {
        Some(("video_delete", "invalid-request"))
    } else if path.starts_with("/openai/v1/videos/") && *method == axum::http::Method::GET {
        Some(("video_get", "invalid-request"))
    } else if path == "/openai/v1/responses/input_tokens"
        || path == "/anthropic/v1/messages/count_tokens"
        || path.ends_with(":countTokens")
    {
        Some(("token_count", "invalid-request"))
    } else if path == "/openai/v1/embeddings" {
        Some(("embeddings", "invalid-request"))
    } else if path == "/openai/v1/images/generations" {
        Some(("image_generation", "invalid-request"))
    } else if path == "/openai/v1/images/edits" {
        Some(("image_edit", "invalid-request"))
    } else if path == "/openai/v1/images/variations" {
        Some(("image_variation", "invalid-request"))
    } else if path == "/openai/v1/audio/speech" {
        Some(("speech", "invalid-request"))
    } else if path == "/openai/v1/audio/transcriptions" {
        Some(("transcription", "invalid-request"))
    } else if path == "/openai/v1/moderations" {
        Some(("moderation", "invalid-request"))
    } else if path == "/openai/v1/chat/completions"
        || path == "/openai/v1/responses"
        || path == "/anthropic/v1/messages"
        || path.ends_with(":generateContent")
        || path.ends_with(":streamGenerateContent")
    {
        Some(("generation", "invalid-request"))
    } else {
        None
    }
}

fn inference_route_from_json(path: &str, body: &[u8]) -> Option<String> {
    if let Ok(value) = serde_json::from_slice::<serde_json::Value>(body)
        && let Some(model) = value.get("model").and_then(serde_json::Value::as_str)
        && olp_domain::RouteSlug::parse(model).is_ok()
    {
        return Some(model.to_owned());
    }
    let resource = path.split("/models/").nth(1)?;
    let model = resource.split(':').next()?;
    olp_domain::RouteSlug::parse(model)
        .is_ok()
        .then(|| model.to_owned())
}

async fn enforce_request_limits_inner(
    state: &ApiState,
    request: Request<axum::body::Body>,
    next: middleware::Next,
    surface: Option<Surface>,
) -> Result<Response, RequestLimitRejection> {
    let request_started_at = chrono::Utc::now();
    let metadata_operation = inference_metadata_operation(request.method(), request.uri().path());
    if request.uri().to_string().len() > MAX_URI_BYTES {
        return Err(Problem::new(
            axum::http::StatusCode::URI_TOO_LONG,
            "uri_too_long",
            "Request URI too long",
            "The request URI exceeds the gateway limit.",
        )
        .into());
    }
    let header_bytes = request
        .headers()
        .iter()
        .fold(0_usize, |size, (name, value)| {
            size.saturating_add(name.as_str().len())
                .saturating_add(value.as_bytes().len())
                .saturating_add(4)
        });
    if request.headers().len() > MAX_HTTP_HEADER_COUNT
        || header_bytes > MAX_HTTP_HEADER_BYTES
        || request
            .headers()
            .values()
            .any(|value| value.as_bytes().len() > MAX_HEADER_VALUE_BYTES)
    {
        return Err(Problem::new(
            axum::http::StatusCode::REQUEST_HEADER_FIELDS_TOO_LARGE,
            "headers_too_large",
            "Request headers too large",
            "Request headers exceed the gateway limit.",
        )
        .into());
    }
    // Public authentication endpoints must reject a malformed forwarding
    // chain before their extractors consume a JSON body.  This keeps the
    // trusted-proxy boundary uniform even for syntactically invalid login or
    // invitation payloads, which otherwise return before source admission.
    if public_auth_source_required(&request) {
        public_auth_source(
            state,
            request.headers(),
            request
                .extensions()
                .get::<axum::extract::ConnectInfo<SocketAddr>>()
                .map(|connect_info| connect_info.0),
        )?;
    }
    let mut request = request;
    if is_first_owner_setup(&request) {
        let authorization = preauthorize_first_owner_setup(state, request.headers()).await?;
        request.extensions_mut().insert(authorization);
    }
    let count = request
        .headers()
        .get_all(axum::http::header::CONTENT_LENGTH)
        .iter()
        .count();
    let transfer_encoding = request
        .headers()
        .get_all(axum::http::header::TRANSFER_ENCODING)
        .iter()
        .collect::<Vec<_>>();
    if count > 1
        || !transfer_encoding.is_empty()
            && request
                .headers()
                .contains_key(axum::http::header::CONTENT_LENGTH)
        || transfer_encoding.len() > 1
        || transfer_encoding.first().is_some_and(|value| {
            !value
                .to_str()
                .is_ok_and(|value| value.trim().eq_ignore_ascii_case("chunked"))
        })
    {
        return Err(Problem::bad_request(
            "ambiguous_body_length",
            "The request has ambiguous framing headers.",
        )
        .into());
    }
    if request
        .headers()
        .get(axum::http::header::CONTENT_ENCODING)
        .is_some_and(|value| {
            !value
                .to_str()
                .is_ok_and(|value| value.trim().eq_ignore_ascii_case("identity"))
        })
    {
        return Err(Problem::new(
            axum::http::StatusCode::UNSUPPORTED_MEDIA_TYPE,
            "content_encoding_unsupported",
            "Content encoding unsupported",
            "Compressed request bodies are not accepted.",
        )
        .into());
    }

    let content_type = request
        .headers()
        .get(axum::http::header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default();
    let media_request = is_media_request(request.uri().path(), content_type);
    let maximum = if media_request {
        MAX_MEDIA_BODY_BYTES
    } else {
        MAX_JSON_BODY_BYTES
    };
    if request
        .headers()
        .get(axum::http::header::CONTENT_LENGTH)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.parse::<u64>().ok())
        .is_some_and(|value| value > maximum as u64)
    {
        return Err(payload_too_large(maximum).into());
    }

    let authenticated = surface
        .map(|surface| authenticate_inference_headers(state, request.headers(), surface))
        .transpose()?;
    let local_metadata = authenticated.as_ref().and_then(|authenticated| {
        metadata_operation.map(|(operation, route_slug)| LocalRequestMetadata {
            usage: state.usage.clone(),
            request_started_at,
            runtime_generation_id: authenticated.runtime_generation_id,
            api_key_id: authenticated.key.id.as_uuid(),
            route_slug: route_slug.to_owned(),
            operation,
            surface: surface.expect("local inference metadata has a protocol surface"),
            always_emit: matches!(operation, "model_list" | "model_get" | "video_list"),
        })
    });
    let is_multipart = content_type
        .split(';')
        .next()
        .is_some_and(|value| value.trim().eq_ignore_ascii_case("multipart/form-data"));
    let multipart_policy = multipart_endpoint(request.method(), request.uri().path());
    if multipart_policy.is_some() && !is_multipart {
        if let Some(metadata) = local_metadata {
            metadata.emit(axum::http::StatusCode::BAD_REQUEST);
        }
        return Err(gateway::InferenceError::invalid_request(
            "Content-Type must be multipart/form-data.",
        )
        .into());
    }

    if is_json_content_type(content_type) {
        let (parts, body) = request.into_parts();
        let bytes = match read_json_body(body, MAX_JSON_BODY_BYTES, REQUEST_BODY_TIMEOUT).await {
            Ok(bytes) => bytes,
            Err(JsonBodyReadError::Rejected) => {
                if let Some(metadata) = local_metadata.clone() {
                    metadata.emit(axum::http::StatusCode::PAYLOAD_TOO_LARGE);
                }
                return Err(payload_too_large(MAX_JSON_BODY_BYTES).into());
            }
            Err(JsonBodyReadError::Timeout) => {
                if let Some(metadata) = local_metadata.clone() {
                    metadata.emit(axum::http::StatusCode::REQUEST_TIMEOUT);
                }
                return Err(request_body_timeout().into());
            }
        };
        let local_metadata = local_metadata.map(|mut metadata| {
            if let Some(route) = inference_route_from_json(parts.uri.path(), &bytes) {
                metadata.route_slug = route;
            }
            metadata
        });
        let requested_tokens = estimate_http_json_request_tokens(parts.uri.path(), &bytes);
        let reservation = if let Some(authenticated) = &authenticated {
            match reserve_http_inference_limits(state, authenticated, requested_tokens).await {
                Ok(reservation) => reservation,
                Err(error) => {
                    if let Some(metadata) = local_metadata.clone() {
                        metadata.emit(error.status());
                    }
                    return Err(error.into());
                }
            }
        } else {
            None
        };
        if let Err(problem) = validate_json_depth(&bytes) {
            release_reservation(reservation).await;
            if let Some(metadata) = local_metadata {
                metadata.emit(axum::http::StatusCode::BAD_REQUEST);
            }
            return Err(problem.into());
        }
        let request = Request::from_parts(parts, Body::from(bytes));
        let runtime = authenticated
            .as_ref()
            .map(|authenticated| Arc::clone(&authenticated.runtime));
        let reserved_tokens = reservation.as_ref().map(|_| requested_tokens);
        return Ok(run_request_with_reservation(
            request,
            next,
            reservation,
            local_metadata,
            runtime,
            reserved_tokens,
            false,
        )
        .await);
    }

    let requested_tokens = estimate_http_non_json_request_tokens(request.uri().path());
    let reservation = if let Some(authenticated) = &authenticated {
        match reserve_http_inference_limits(state, authenticated, requested_tokens).await {
            Ok(reservation) => reservation,
            Err(error) => {
                if let Some(metadata) = local_metadata.clone() {
                    metadata.emit(error.status());
                }
                return Err(error.into());
            }
        }
    } else {
        None
    };
    let multipart_preauthorization = if is_multipart {
        if let Err(problem) = validate_multipart_boundary(content_type) {
            release_reservation(reservation).await;
            if let Some(metadata) = local_metadata.clone() {
                metadata.emit(axum::http::StatusCode::BAD_REQUEST);
            }
            return Err(problem.into());
        }
        match (multipart_policy, authenticated.as_ref()) {
            (Some(_), Some(authenticated)) => {
                match preauthorize_multipart(
                    request.headers(),
                    &authenticated.key,
                    request.method(),
                    request.uri().path(),
                ) {
                    Ok(admission) => Some(admission),
                    Err(error) => {
                        release_reservation(reservation).await;
                        if let Some(metadata) = local_metadata.clone() {
                            metadata.emit(error.status());
                        }
                        return Err(error.into());
                    }
                }
            }
            // Only gateway endpoints use multipart today. Keep unrelated
            // control-plane multipart content out of this admission path.
            _ => None,
        }
    } else {
        None
    };
    let multipart_admission = if let Some((route, reservation_bytes)) = multipart_preauthorization {
        let Some(authenticated) = authenticated.as_ref() else {
            release_reservation(reservation).await;
            return Err(gateway::InferenceError::unauthorized().into());
        };
        let Some(lease) = state
            .multipart_admission
            .try_admit(authenticated.key.id.as_uuid(), reservation_bytes)
        else {
            release_reservation(reservation).await;
            if let Some(metadata) = local_metadata.clone() {
                metadata.emit(axum::http::StatusCode::SERVICE_UNAVAILABLE);
            }
            return Err(
                gateway::InferenceError::unavailable("multipart_admission_exhausted").into(),
            );
        };
        Some(MultipartRequestAdmission {
            route,
            lease: Some(lease),
        })
    } else {
        None
    };
    if let Some(admission) = multipart_admission {
        request.extensions_mut().insert(admission);
    }
    let runtime = authenticated
        .as_ref()
        .map(|authenticated| Arc::clone(&authenticated.runtime));
    let reserved_tokens = reservation.as_ref().map(|_| requested_tokens);
    Ok(run_request_with_reservation(
        request,
        next,
        reservation,
        local_metadata,
        runtime,
        reserved_tokens,
        is_multipart,
    )
    .await)
}

fn public_auth_source_required(request: &Request<Body>) -> bool {
    matches!(
        (request.method(), request.uri().path()),
        (&axum::http::Method::POST, "/api/v1/setup")
            | (&axum::http::Method::POST, "/api/v1/sessions")
            | (&axum::http::Method::POST, "/api/v1/invitations/accept")
            | (&axum::http::Method::GET, "/api/v1/oidc/login")
    )
}

fn is_first_owner_setup(request: &Request<Body>) -> bool {
    request.method() == axum::http::Method::POST && request.uri().path() == "/api/v1/setup"
}

async fn preauthorize_first_owner_setup(
    state: &ApiState,
    headers: &HeaderMap,
) -> Result<FirstOwnerSetupAuthorized, RequestLimitRejection> {
    let store = state
        .store
        .as_ref()
        .ok_or_else(|| Problem::service_unavailable("database_not_configured"))?;
    if !store
        .setup_required()
        .await
        .map_err(management::map_persistence)?
    {
        return Err(Problem::conflict(
            "setup_already_completed",
            "This installation already has an owner.",
        )
        .into());
    }
    let supplied_token = headers
        .get(management::SETUP_TOKEN_HEADER)
        .and_then(|value| value.to_str().ok());
    match state.verify_bootstrap_token(supplied_token).await {
        Some(true) => {}
        Some(false) => {
            return Err(Problem::unauthorized(
                "A valid setup token is required to create the first owner.",
            )
            .into());
        }
        None => {
            return Err(Problem::service_unavailable("bootstrap_token_not_configured").into());
        }
    }
    management::enforce_origin(state, headers)?;
    Ok(FirstOwnerSetupAuthorized)
}

async fn run_request_with_reservation(
    request: Request<Body>,
    next: middleware::Next,
    reservation: Option<InferenceReservation>,
    local_metadata: Option<LocalRequestMetadata>,
    runtime: Option<Arc<RuntimeBundle>>,
    reserved_tokens: Option<i64>,
    reconcile_multipart_tokens: bool,
) -> Response {
    let metadata_claimed = runtime.as_ref().map(|_| Arc::new(AtomicBool::new(false)));
    let run = async move {
        // Only suppress the canonical fallback when this exact HTTP request
        // actually acquired a hard-limit reservation. Unlimited keys retain
        // the same pinned generation and therefore remain unlimited throughout
        // this request even if a newer release activates concurrently.
        if let Some(reserved_tokens) = reserved_tokens {
            HTTP_INFERENCE_LIMITS_RESERVED
                .scope(reserved_tokens, async move {
                    if reconcile_multipart_tokens {
                        HTTP_MULTIPART_TOKEN_RECONCILIATION
                            .scope((), next.run(request))
                            .await
                    } else {
                        next.run(request).await
                    }
                })
                .await
        } else {
            next.run(request).await
        }
    };
    let response = match (runtime, metadata_claimed.as_ref()) {
        (Some(runtime), Some(claimed)) => {
            HTTP_INFERENCE_METADATA_CLAIMED
                .scope(
                    Arc::clone(claimed),
                    HTTP_INFERENCE_RUNTIME.scope(runtime, run),
                )
                .await
        }
        _ => run.await,
    };
    if let Some(metadata) = local_metadata {
        let claimed = metadata_claimed
            .as_ref()
            .is_some_and(|claimed| claimed.load(Ordering::Acquire));
        if metadata.always_emit || !claimed {
            metadata.emit(response.status());
        }
    }
    if let Some(reservation) = reservation {
        let (parts, body) = response.into_parts();
        Response::from_parts(
            parts,
            Body::new(ReleaseReservationBody {
                inner: body,
                reservation,
            }),
        )
    } else {
        response
    }
}

async fn release_reservation(reservation: Option<InferenceReservation>) {
    if let Some(reservation) = reservation {
        reservation.release().await;
    }
}

fn inference_surface(path: &str) -> Option<Surface> {
    if path.starts_with("/openai/") {
        Some(Surface::OpenAi)
    } else if path.starts_with("/anthropic/") {
        Some(Surface::Anthropic)
    } else if path.starts_with("/gemini/") {
        Some(Surface::Gemini)
    } else {
        None
    }
}

struct AuthenticatedInferenceKey {
    key: ApiKey,
    lookup_id: String,
    lease_ttl: Duration,
    runtime_generation_id: uuid::Uuid,
    runtime: Arc<RuntimeBundle>,
}

fn authenticate_inference_headers(
    state: &ApiState,
    headers: &axum::http::HeaderMap,
    surface: Surface,
) -> Result<AuthenticatedInferenceKey, Problem> {
    let token = match surface {
        Surface::OpenAi => headers
            .get(axum::http::header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.split_once(' '))
            .filter(|(scheme, token)| {
                scheme.eq_ignore_ascii_case("bearer")
                    && !token.is_empty()
                    && !token.contains(char::is_whitespace)
            })
            .map(|(_, token)| token),
        Surface::Anthropic => inference_header_token(headers, "x-api-key"),
        Surface::Gemini => inference_header_token(headers, "x-goog-api-key"),
    }
    .ok_or_else(|| Problem::unauthorized("The API key is invalid or unavailable."))?;
    let hasher = state
        .key_hasher
        .as_ref()
        .ok_or_else(|| Problem::service_unavailable("api_key_authentication_unavailable"))?;
    let lookup = hasher
        .lookup_id(token)
        .map_err(|_| Problem::unauthorized("The API key is invalid or unavailable."))?
        .to_owned();
    let lookup_id = ApiKeyLookupId::parse(&lookup)
        .map_err(|_| Problem::unauthorized("The API key is invalid or unavailable."))?;
    let snapshot = state.runtime.pin();
    let key = snapshot
        .api_keys
        .get(&lookup_id)
        .ok_or_else(|| Problem::unauthorized("The API key is invalid or unavailable."))?;
    hasher
        .parse_and_verify(token, key.digest.as_bytes())
        .map_err(|_| Problem::unauthorized("The API key is invalid or unavailable."))?;
    if key.status != ApiKeyStatus::Active
        || key
            .expires_at
            .is_some_and(|expires_at| expires_at <= chrono::Utc::now())
    {
        return Err(Problem::unauthorized(
            "The API key is invalid or unavailable.",
        ));
    }
    let route_timeout = snapshot
        .routes
        .iter()
        .filter(|(slug, _)| key.allowed_routes.is_empty() || key.allowed_routes.contains(*slug))
        .map(|(_, route)| route.overall_timeout.as_duration())
        .max()
        .unwrap_or(Duration::from_secs(30));
    Ok(AuthenticatedInferenceKey {
        key: key.clone(),
        lookup_id: lookup,
        // Account for the bounded body-read phase in addition to the route's
        // own deadline. Expiry remains a crash-recovery backstop; normal
        // completion releases the lease immediately.
        lease_ttl: route_timeout.saturating_add(Duration::from_secs(60)),
        runtime_generation_id: snapshot.generation.id.as_uuid(),
        runtime: snapshot,
    })
}

async fn reserve_http_inference_limits(
    state: &ApiState,
    authenticated: &AuthenticatedInferenceKey,
    requested_tokens: i64,
) -> Result<Option<InferenceReservation>, gateway::InferenceError> {
    if !authenticated.key.limits.has_hard_limits() {
        return Ok(None);
    }
    let limiter = state
        .limiter
        .get()
        .ok_or_else(|| gateway::InferenceError::unavailable("distributed_limits_unavailable"))?;
    let tokens_per_minute = authenticated
        .key
        .limits
        .tokens_per_minute
        .map(|value| i64::try_from(value.get()))
        .transpose()
        .map_err(|_| gateway::InferenceError::unavailable("limit_configuration_invalid"))?;
    let result = tokio::time::timeout(
        Duration::from_secs(1),
        limiter.reserve(LimitRequest {
            lookup_id: &authenticated.lookup_id,
            requests_per_minute: authenticated
                .key
                .limits
                .requests_per_minute
                .map(|value| i64::from(value.get())),
            tokens_per_minute,
            max_concurrency: authenticated
                .key
                .limits
                .concurrency
                .map(|value| i64::from(value.get())),
            requested_tokens,
            lease_ttl: authenticated.lease_ttl,
        }),
    )
    .await
    .map_err(|_| gateway::InferenceError::unavailable("distributed_limits_unavailable"))?;
    match result {
        Ok(lease) => Ok(Some(InferenceReservation::distributed(limiter, lease))),
        Err(LimitError::Exceeded {
            dimension,
            retry_after,
        }) => Err(gateway::InferenceError::rate_limited(
            dimension,
            retry_after,
        )),
        Err(error) => {
            tracing::error!(%error, "hard HTTP limit reservation failed closed");
            Err(gateway::InferenceError::unavailable(
                "distributed_limits_unavailable",
            ))
        }
    }
}

fn estimate_http_json_request_tokens(path: &str, body: &[u8]) -> i64 {
    let encoded_body = body.len().saturating_add(3) / 4;
    let baseline = if is_generation_path(path) {
        let value = serde_json::from_slice::<serde_json::Value>(body).ok();
        let output = value
            .as_ref()
            .and_then(|value| {
                [
                    "/max_completion_tokens",
                    "/max_tokens",
                    "/max_output_tokens",
                    "/generationConfig/maxOutputTokens",
                ]
                .into_iter()
                .find_map(|pointer| value.pointer(pointer).and_then(serde_json::Value::as_u64))
            })
            .and_then(|value| usize::try_from(value).ok())
            .unwrap_or(4_096)
            .max(1);
        let candidates = value
            .as_ref()
            .and_then(|value| {
                value
                    .pointer("/n")
                    .or_else(|| value.pointer("/generationConfig/candidateCount"))
                    .and_then(serde_json::Value::as_u64)
            })
            .and_then(|value| usize::try_from(value).ok())
            .unwrap_or(1)
            .max(1);
        output.saturating_mul(candidates)
    } else {
        1
    };
    i64::try_from(encoded_body.saturating_add(baseline).max(1)).unwrap_or(i64::MAX)
}

fn estimate_http_non_json_request_tokens(path: &str) -> i64 {
    let baseline: i64 = if is_generation_path(path) {
        4_096
    } else if path.ends_with("/audio/transcriptions") {
        1_500
    } else if path.ends_with("/images/edits")
        || path.ends_with("/images/variations")
        || path.ends_with("/videos")
    {
        2_000
    } else {
        1
    };
    baseline
}

fn is_generation_path(path: &str) -> bool {
    path.ends_with("/chat/completions")
        || path.ends_with("/responses")
        || path.ends_with("/messages")
        || path.ends_with(":generateContent")
        || path.ends_with(":streamGenerateContent")
}

fn inference_header_token<'a>(
    headers: &'a axum::http::HeaderMap,
    name: &'static str,
) -> Option<&'a str> {
    headers
        .get(name)
        .and_then(|value| value.to_str().ok())
        .filter(|token| !token.is_empty() && !token.contains(char::is_whitespace))
}

fn is_json_content_type(value: &str) -> bool {
    let media_type = value.split(';').next().unwrap_or_default().trim();
    media_type.eq_ignore_ascii_case("application/json")
        || media_type
            .to_ascii_lowercase()
            .strip_prefix("application/")
            .is_some_and(|subtype| subtype.ends_with("+json"))
}

fn is_media_request(path: &str, content_type: &str) -> bool {
    let media_path = path.starts_with("/openai/v1/images/")
        || path.starts_with("/openai/v1/audio/")
        || path == "/openai/v1/videos";
    media_path
        && (content_type
            .split(';')
            .next()
            .is_some_and(|value| value.trim().eq_ignore_ascii_case("multipart/form-data"))
            || content_type.eq_ignore_ascii_case("application/octet-stream"))
}

fn validate_multipart_boundary(content_type: &str) -> Result<(), Problem> {
    let boundary = content_type.split(';').skip(1).find_map(|parameter| {
        let (name, value) = parameter.trim().split_once('=')?;
        name.trim()
            .eq_ignore_ascii_case("boundary")
            .then(|| value.trim().trim_matches('"'))
    });
    if boundary.is_none_or(|value| {
        value.is_empty()
            || value.len() > 200
            || !value
                .bytes()
                .all(|byte| byte.is_ascii_graphic() && byte != b'"' && byte != b'\\')
    }) {
        return Err(Problem::bad_request(
            "invalid_multipart_boundary",
            "A multipart/form-data request requires a valid boundary no longer than 200 bytes.",
        ));
    }
    Ok(())
}

fn multipart_endpoint(method: &axum::http::Method, path: &str) -> Option<(OperationKind, u64)> {
    if *method != axum::http::Method::POST {
        return None;
    }
    match path {
        // Reserve against the route's fixed body ceiling, never against an
        // attacker-controlled Content-Length. Individual file limits are
        // still enforced by the spool while streaming.
        "/openai/v1/images/edits" => Some((OperationKind::ImageEdit, MAX_MEDIA_BODY_BYTES as u64)),
        "/openai/v1/images/variations" => Some((
            OperationKind::ImageVariation,
            IMAGE_VARIATION_BODY_BYTES as u64,
        )),
        "/openai/v1/audio/transcriptions" => Some((
            OperationKind::Transcription,
            TRANSCRIPTION_BODY_BYTES as u64,
        )),
        "/openai/v1/videos" => Some((OperationKind::VideoCreate, VIDEO_CREATE_BODY_BYTES as u64)),
        _ => None,
    }
}

fn preauthorize_multipart(
    headers: &HeaderMap,
    key: &ApiKey,
    method: &axum::http::Method,
    path: &str,
) -> Result<(MultipartRouteAdmission, u64), gateway::InferenceError> {
    let Some((operation, reservation_bytes)) = multipart_endpoint(method, path) else {
        return Ok((MultipartRouteAdmission::Unrestricted, 0));
    };
    authorize_api_key(key, None, operation, chrono::Utc::now())
        .map_err(|error| gateway::InferenceError::forbidden(error.to_string()))?;

    let route_header = HeaderName::from_static("x-olp-route");
    let values = headers.get_all(&route_header);
    if values.iter().count() > 1 {
        return Err(gateway::InferenceError::invalid_request(
            "X-OLP-Route must appear at most once.",
        ));
    }
    let supplied = values
        .iter()
        .next()
        .map(|value| {
            value
                .to_str()
                .map_err(|_| gateway::InferenceError::invalid_request("X-OLP-Route is invalid."))
        })
        .transpose()?;
    if key.allowed_routes.is_empty() {
        return Ok((MultipartRouteAdmission::Unrestricted, reservation_bytes));
    }
    if let Some(supplied) = supplied {
        let route = RouteSlug::parse(supplied)
            .map_err(|_| gateway::InferenceError::invalid_request("X-OLP-Route is invalid."))?;
        authorize_api_key(key, Some(&route), operation, chrono::Utc::now())
            .map_err(|error| gateway::InferenceError::forbidden(error.to_string()))?;
        Ok((MultipartRouteAdmission::Expected(route), reservation_bytes))
    } else {
        Ok((
            MultipartRouteAdmission::RequireModelBeforeFile(key.allowed_routes.clone()),
            reservation_bytes,
        ))
    }
}

fn validate_json_depth(bytes: &[u8]) -> Result<(), Problem> {
    let mut depth = 0_usize;
    let mut in_string = false;
    let mut escaped = false;
    for byte in bytes {
        if in_string {
            if escaped {
                escaped = false;
            } else if *byte == b'\\' {
                escaped = true;
            } else if *byte == b'"' {
                in_string = false;
            }
            continue;
        }
        match byte {
            b'"' => in_string = true,
            b'{' | b'[' => {
                depth = depth.saturating_add(1);
                if depth > MAX_JSON_DEPTH {
                    return Err(Problem::bad_request(
                        "json_too_deep",
                        "The JSON document exceeds the maximum nesting depth of 64.",
                    ));
                }
            }
            b'}' | b']' => depth = depth.saturating_sub(1),
            _ => {}
        }
    }
    Ok(())
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum JsonBodyReadError {
    Rejected,
    Timeout,
}

async fn read_json_body(
    body: Body,
    maximum: usize,
    deadline: Duration,
) -> Result<bytes::Bytes, JsonBodyReadError> {
    tokio::time::timeout(deadline, to_bytes(body, maximum))
        .await
        .map_err(|_| JsonBodyReadError::Timeout)?
        .map_err(|_| JsonBodyReadError::Rejected)
}

fn request_body_timeout() -> Problem {
    Problem::new(
        axum::http::StatusCode::REQUEST_TIMEOUT,
        "request_timeout",
        "Request timeout",
        "The request body was not received before the deadline.",
    )
}

fn payload_too_large(maximum: usize) -> Problem {
    Problem::new(
        axum::http::StatusCode::PAYLOAD_TOO_LARGE,
        "body_too_large",
        "Request body too large",
        format!("The request body exceeds the {maximum}-byte limit."),
    )
}

#[derive(Clone, Serialize, ToSchema)]
pub(crate) struct HealthResponse {
    status: &'static str,
    generation: Option<u64>,
    database: &'static str,
    limits: &'static str,
    usage_complete: bool,
    usage_consumer: &'static str,
    usage_consumer_pending_events: u64,
    usage_consumer_lag_events: u64,
    usage_consumer_oldest_pending_at: Option<chrono::DateTime<chrono::Utc>>,
    usage_consumer_checked_at: Option<chrono::DateTime<chrono::Utc>>,
    usage_consumer_heartbeat_age_seconds: Option<u64>,
    usage_open_epochs: u64,
    usage_unresolved_epochs: u64,
    usage_historical_uncertain_gaps: u64,
    usage_unresolved_event_lower_bound: u64,
    media_reconciliation: &'static str,
    media_reconciliation_pending: u64,
    media_reconciliation_stale: u64,
    media_reconciliation_failed: u64,
    media_reconciliation_unbound: u64,
    media_reconciliation_gaps_total: u64,
}

/// Process-local snapshots used by the private observability listener. The
/// request path only reads this lock; all dependency I/O happens in the
/// background refresh task below.
#[derive(Clone, Default)]
struct ObservabilityCache {
    readiness: Arc<RwLock<CachedReadiness>>,
    metrics: Arc<RwLock<CachedMetrics>>,
}

#[derive(Clone, Default)]
struct CachedReadiness {
    result: Option<HealthResponse>,
    last_attempt_at: Option<Instant>,
    last_success_at: Option<Instant>,
}

#[derive(Clone, Default)]
struct CachedMetrics {
    body: Option<Arc<str>>,
    last_attempt_at: Option<Instant>,
    last_success_at: Option<Instant>,
}

impl ObservabilityCache {
    fn readiness(&self) -> CachedReadiness {
        self.readiness
            .read()
            .expect("observability readiness cache lock poisoned")
            .clone()
    }

    fn metrics(&self) -> CachedMetrics {
        self.metrics
            .read()
            .expect("observability metrics cache lock poisoned")
            .clone()
    }

    fn record_readiness(&self, result: Result<HealthResponse, Problem>) {
        let now = Instant::now();
        let mut readiness = self
            .readiness
            .write()
            .expect("observability readiness cache lock poisoned");
        readiness.last_attempt_at = Some(now);
        if let Ok(result) = result {
            readiness.last_success_at = Some(now);
            readiness.result = Some(result);
        }
    }

    fn record_metrics(&self, body: String) {
        let now = Instant::now();
        let mut metrics = self
            .metrics
            .write()
            .expect("observability metrics cache lock poisoned");
        metrics.last_attempt_at = Some(now);
        metrics.last_success_at = Some(now);
        metrics.body = Some(Arc::from(body));
    }

    fn record_metrics_failure(&self) {
        self.metrics
            .write()
            .expect("observability metrics cache lock poisoned")
            .last_attempt_at = Some(Instant::now());
    }
}

/// Refresh both snapshots immediately. This is public so embeddings and
/// integration tests can prime the cache before opening an observability
/// listener; production servers should use [`spawn_observability_cache`].
pub async fn refresh_observability_cache(state: &ApiState) {
    tokio::join!(refresh_readiness_cache(state), refresh_metrics_cache(state));
}

/// Starts the background cache supervisor used by the private observability
/// listener. Readiness is refreshed every five seconds, while the more
/// expensive metrics rollups are refreshed every fifteen seconds.
pub fn spawn_observability_cache(
    state: ApiState,
    mut shutdown: tokio::sync::watch::Receiver<bool>,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        refresh_observability_cache(&state).await;

        let mut readiness_interval =
            tokio::time::interval(OBSERVABILITY_READINESS_REFRESH_INTERVAL);
        let mut metrics_interval = tokio::time::interval(OBSERVABILITY_METRICS_REFRESH_INTERVAL);
        readiness_interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        metrics_interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        // `interval` ticks immediately. The initial synchronous refresh above
        // already populated both snapshots, so consume those first ticks.
        readiness_interval.tick().await;
        metrics_interval.tick().await;

        let readiness_state = state.clone();
        let mut readiness_shutdown = shutdown.clone();
        let readiness_refresh = async move {
            loop {
                tokio::select! {
                    _ = readiness_interval.tick() => refresh_readiness_cache(&readiness_state).await,
                    changed = readiness_shutdown.changed() => {
                        if changed.is_err() || *readiness_shutdown.borrow() {
                            return;
                        }
                    }
                }
            }
        };
        let metrics_refresh = async move {
            loop {
                tokio::select! {
                    _ = metrics_interval.tick() => refresh_metrics_cache(&state).await,
                    changed = shutdown.changed() => {
                        if changed.is_err() || *shutdown.borrow() {
                            return;
                        }
                    }
                }
            }
        };
        tokio::join!(readiness_refresh, metrics_refresh);
    })
}

async fn refresh_readiness_cache(state: &ApiState) {
    let result =
        match tokio::time::timeout(OBSERVABILITY_REFRESH_TIMEOUT, collect_readiness(state)).await {
            Ok(result) => result,
            Err(_) => {
                tracing::warn!("observability readiness refresh timed out");
                Err(Problem::service_unavailable(
                    "observability_snapshot_timeout",
                ))
            }
        };
    state.observability.record_readiness(result);
}

async fn refresh_metrics_cache(state: &ApiState) {
    match tokio::time::timeout(OBSERVABILITY_REFRESH_TIMEOUT, collect_metrics(state)).await {
        Ok(body) => state.observability.record_metrics(body),
        Err(_) => {
            tracing::warn!("observability metrics refresh timed out");
            state.observability.record_metrics_failure();
        }
    }
}

fn snapshot_age_seconds(at: Option<Instant>, now: Instant) -> Option<u64> {
    at.map(|at| now.saturating_duration_since(at).as_secs())
}

fn snapshot_is_fresh(at: Option<Instant>, now: Instant) -> bool {
    at.is_some_and(|at| now.saturating_duration_since(at) <= OBSERVABILITY_SNAPSHOT_STALE_AFTER)
}

fn snapshot_is_current(
    last_success_at: Option<Instant>,
    last_attempt_at: Option<Instant>,
    now: Instant,
) -> bool {
    snapshot_is_fresh(last_success_at, now) && last_success_at == last_attempt_at
}

fn cached_readiness_is_fresh(snapshot: &CachedReadiness, now: Instant) -> bool {
    snapshot_is_current(snapshot.last_success_at, snapshot.last_attempt_at, now)
}

fn cached_readiness_from_snapshot(
    snapshot: &CachedReadiness,
    now: Instant,
) -> Result<HealthResponse, Problem> {
    if !cached_readiness_is_fresh(snapshot, now) {
        return Err(Problem::service_unavailable("observability_snapshot_stale"));
    }
    snapshot
        .result
        .clone()
        .ok_or_else(|| Problem::service_unavailable("observability_snapshot_unavailable"))
}

fn cached_metrics_is_fresh(snapshot: &CachedMetrics, now: Instant) -> bool {
    snapshot_is_current(snapshot.last_success_at, snapshot.last_attempt_at, now)
}

fn attach_snapshot_freshness(response: &mut Response, age: Option<u64>, fresh: bool) {
    let age = age
        .map(|age| age.to_string())
        .and_then(|age| axum::http::HeaderValue::from_str(&age).ok())
        .unwrap_or_else(|| axum::http::HeaderValue::from_static("unknown"));
    response.headers_mut().insert(
        HeaderName::from_static("x-olp-observability-snapshot-age-seconds"),
        age,
    );
    response.headers_mut().insert(
        HeaderName::from_static("x-olp-observability-snapshot-fresh"),
        axum::http::HeaderValue::from_static(if fresh { "1" } else { "0" }),
    );
}

async fn live() -> axum::Json<HealthResponse> {
    axum::Json(HealthResponse {
        status: "ok",
        generation: None,
        database: "not_checked",
        limits: "not_checked",
        usage_complete: true,
        usage_consumer: "not_checked",
        usage_consumer_pending_events: 0,
        usage_consumer_lag_events: 0,
        usage_consumer_oldest_pending_at: None,
        usage_consumer_checked_at: None,
        usage_consumer_heartbeat_age_seconds: None,
        usage_open_epochs: 0,
        usage_unresolved_epochs: 0,
        usage_historical_uncertain_gaps: 0,
        usage_unresolved_event_lower_bound: 0,
        media_reconciliation: "not_checked",
        media_reconciliation_pending: 0,
        media_reconciliation_stale: 0,
        media_reconciliation_failed: 0,
        media_reconciliation_unbound: 0,
        media_reconciliation_gaps_total: 0,
    })
}

async fn ready(axum::extract::State(state): axum::extract::State<ApiState>) -> Response {
    let now = Instant::now();
    let snapshot = state.observability.readiness();
    let fresh = cached_readiness_is_fresh(&snapshot, now);
    let mut response = match cached_readiness_from_snapshot(&snapshot, now) {
        Ok(health) => axum::Json(health).into_response(),
        Err(problem) => problem.into_response(),
    };
    attach_snapshot_freshness(
        &mut response,
        snapshot_age_seconds(snapshot.last_success_at, now),
        fresh,
    );
    response
}

async fn collect_readiness(state: &ApiState) -> Result<HealthResponse, Problem> {
    let generation = state.runtime.ordinal();
    let now = chrono::Utc::now();
    let unknown_consumer = UsageConsumerStatus::from_health(None, now);
    let (database, media_reconciliation, usage_consumer, usage_epochs) =
        if let Some(store) = &state.store {
            match store.ping().await {
                Ok(()) => {
                    let (media, consumer, epochs) = tokio::join!(
                        store.media_reconciliation_summary(now),
                        store.usage_consumer_status(now),
                        store.usage_gateway_epoch_health(),
                    );
                    let media =
                        media.map_err(|_| Problem::service_unavailable("database_unavailable"))?;
                    let consumer = consumer
                        .map_err(|_| Problem::service_unavailable("database_unavailable"))?;
                    let epochs =
                        epochs.map_err(|_| Problem::service_unavailable("database_unavailable"))?;
                    ("ok", Some(media), consumer, epochs)
                }
                Err(_) if state.mode.serves_gateway() && generation.is_some() => (
                    "unavailable_lkg",
                    None,
                    unknown_consumer,
                    UsageEpochHealth::default(),
                ),
                Err(_) => return Err(Problem::service_unavailable("database_unavailable")),
            }
        } else if state.mode.serves_control() {
            return Err(Problem::service_unavailable("database_not_configured"));
        } else {
            (
                "not_configured",
                None,
                unknown_consumer,
                UsageEpochHealth::default(),
            )
        };

    if state.mode.serves_gateway() {
        let snapshot = state.runtime.pin();
        if generation.is_none() {
            return Err(Problem::service_unavailable(
                "runtime_generation_unavailable",
            ));
        }
        if state.key_hasher.is_none() {
            return Err(Problem::service_unavailable(
                "api_key_authentication_unavailable",
            ));
        }
        if !snapshot.has_all_transports() {
            return Err(Problem::service_unavailable(
                "provider_transport_unavailable",
            ));
        }
    }
    let limiter = state.limiter.get();
    let limits_healthy = if let Some(limiter) = &limiter {
        matches!(
            tokio::time::timeout(Duration::from_millis(500), limiter.ping()).await,
            Ok(Ok(()))
        )
    } else {
        false
    };
    let hard_limits_present = state
        .runtime
        .pin()
        .api_keys
        .values()
        .any(|key| key.limits.has_hard_limits());
    // Valkey loss degrades only requests whose keys declare hard limits. The
    // request path fails those keys closed, while unlimited keys remain safe to
    // serve from the immutable snapshot. Returning 503 here would remove the
    // whole gateway from a Kubernetes Service and incorrectly fail unlimited
    // traffic too.
    let degraded_limits = state.mode.serves_gateway() && hard_limits_present && !limits_healthy;
    let media_reconciliation_gaps = state.media_reconciliation_gap_count();
    let degraded_media = media_reconciliation
        .as_ref()
        .is_some_and(|summary| summary.stale > 0 || summary.failed > 0 || summary.unbound > 0)
        || media_reconciliation_gaps > 0;
    let local_usage_complete = state
        .usage
        .as_ref()
        .map_or(!state.mode.serves_gateway(), |usage| {
            usage.snapshot().complete()
        });
    let usage_complete =
        local_usage_complete && usage_consumer.complete() && usage_epochs.unresolved_epochs == 0;
    Ok(HealthResponse {
        status: if degraded_limits || degraded_media || !usage_complete {
            "degraded"
        } else {
            "ok"
        },
        generation,
        database,
        limits: if limits_healthy {
            "ok"
        } else if state.limiter.is_configured() {
            "unavailable"
        } else {
            "not_configured"
        },
        usage_complete,
        usage_consumer: usage_consumer.state.as_str(),
        usage_consumer_pending_events: usage_consumer.pending_events,
        usage_consumer_lag_events: usage_consumer.lag_events,
        usage_consumer_oldest_pending_at: usage_consumer.oldest_pending_at,
        usage_consumer_checked_at: usage_consumer.checked_at,
        usage_consumer_heartbeat_age_seconds: usage_consumer.heartbeat_age_seconds,
        usage_open_epochs: usage_epochs.open_epochs,
        usage_unresolved_epochs: usage_epochs.unresolved_epochs,
        usage_historical_uncertain_gaps: usage_epochs.historical_uncertain_gap_count,
        usage_unresolved_event_lower_bound: usage_epochs.unresolved_event_lower_bound,
        media_reconciliation: if media_reconciliation.is_some() {
            "ok"
        } else {
            "unknown"
        },
        media_reconciliation_pending: media_reconciliation
            .as_ref()
            .map_or(0, |summary| summary.pending),
        media_reconciliation_stale: media_reconciliation
            .as_ref()
            .map_or(0, |summary| summary.stale),
        media_reconciliation_failed: media_reconciliation
            .as_ref()
            .map_or(0, |summary| summary.failed),
        media_reconciliation_unbound: media_reconciliation
            .as_ref()
            .map_or(0, |summary| summary.unbound),
        media_reconciliation_gaps_total: media_reconciliation_gaps,
    })
}

async fn metrics(axum::extract::State(state): axum::extract::State<ApiState>) -> Response {
    let now = Instant::now();
    let readiness = state.observability.readiness();
    let metrics = state.observability.metrics();
    let readiness_fresh = cached_readiness_is_fresh(&readiness, now);
    let metrics_fresh = cached_metrics_is_fresh(&metrics, now);
    let readiness_available = readiness_fresh && readiness.result.is_some();
    let readiness_age = snapshot_age_seconds(readiness.last_success_at, now);
    let metrics_age = snapshot_age_seconds(metrics.last_success_at, now);
    let mut body = format!(
        concat!(
            "# HELP olp_ready Whether the process currently satisfies the HTTP readiness contract.\n",
            "# TYPE olp_ready gauge\n",
            "olp_ready {}\n",
            "# HELP olp_observability_readiness_snapshot_age_seconds Age of the last successful readiness snapshot.\n",
            "# TYPE olp_observability_readiness_snapshot_age_seconds gauge\n",
            "olp_observability_readiness_snapshot_age_seconds {}\n",
            "# HELP olp_observability_metrics_snapshot_age_seconds Age of the last successful metrics snapshot.\n",
            "# TYPE olp_observability_metrics_snapshot_age_seconds gauge\n",
            "olp_observability_metrics_snapshot_age_seconds {}\n",
            "# HELP olp_observability_readiness_snapshot_fresh Whether the readiness snapshot is fresh.\n",
            "# TYPE olp_observability_readiness_snapshot_fresh gauge\n",
            "olp_observability_readiness_snapshot_fresh {}\n",
            "# HELP olp_observability_metrics_snapshot_fresh Whether the metrics snapshot is fresh.\n",
            "# TYPE olp_observability_metrics_snapshot_fresh gauge\n",
            "olp_observability_metrics_snapshot_fresh {}\n",
        ),
        u8::from(readiness_available),
        readiness_age.unwrap_or(u64::MAX),
        metrics_age.unwrap_or(u64::MAX),
        u8::from(readiness_fresh),
        u8::from(metrics_fresh),
    );
    if let Some(metrics) = metrics.body {
        body.push_str(&metrics);
    }
    let mut response = (
        [(
            axum::http::header::CONTENT_TYPE,
            "text/plain; version=0.0.4",
        )],
        body,
    )
        .into_response();
    attach_snapshot_freshness(&mut response, metrics_age, metrics_fresh);
    response
}

async fn collect_metrics(state: &ApiState) -> String {
    let usage = state.usage.as_ref().map(UsageEmitter::snapshot);
    let limiter_available = state.limiter.get().is_some();
    let now = chrono::Utc::now();
    let mut usage_consumer = UsageConsumerStatus::from_health(None, now);
    let mut usage_epochs = UsageEpochHealth::default();
    let mut operations_summary = None;
    let mut provider_health = Vec::new();
    let media_reconciliation = if let Some(store) = &state.store {
        let (consumer, epochs, operations, providers, media) = tokio::join!(
            store.usage_consumer_status(now),
            store.usage_gateway_epoch_health(),
            store.prometheus_operations_summary(5),
            store.provider_health(15, None, 100),
            store.media_reconciliation_summary(now),
        );
        if let Ok(status) = consumer {
            usage_consumer = status;
        }
        if let Ok(health) = epochs {
            usage_epochs = health;
        }
        operations_summary = operations.ok();
        if let Ok(page) = providers {
            provider_health = page.items;
        }
        media.ok()
    } else {
        None
    };
    let mut body = format!(
        "# HELP olp_runtime_generation Current immutable runtime generation.\n\
         # TYPE olp_runtime_generation gauge\n\
         olp_runtime_generation {}\n\
         # HELP olp_usage_events_dropped_total Metadata events dropped from the bounded buffer.\n\
         # TYPE olp_usage_events_dropped_total counter\n\
         olp_usage_events_dropped_total {}\n\
         # HELP olp_usage_events_abandoned_total Accepted metadata events abandoned during shutdown or worker failure.\n\
         # TYPE olp_usage_events_abandoned_total counter\n\
         olp_usage_events_abandoned_total {}\n\
         # HELP olp_usage_events_pending Accepted metadata events not yet written to the stream.\n\
         # TYPE olp_usage_events_pending gauge\n\
         olp_usage_events_pending {}\n\
         # HELP olp_usage_stream_retrying Whether the local writer is retrying Valkey.\n\
         # TYPE olp_usage_stream_retrying gauge\n\
         olp_usage_stream_retrying {}\n\
         # HELP olp_usage_persistence_available Whether a metadata usage sink is active.\n\
         # TYPE olp_usage_persistence_available gauge\n\
         olp_usage_persistence_available {}\n\
         # HELP olp_usage_consumer_pending_events Delivered usage events awaiting consumer acknowledgement.\n\
         # TYPE olp_usage_consumer_pending_events gauge\n\
         olp_usage_consumer_pending_events {}\n\
         # HELP olp_usage_consumer_lag_events Usage stream events not yet delivered to the persistence consumer.\n\
         # TYPE olp_usage_consumer_lag_events gauge\n\
         olp_usage_consumer_lag_events {}\n\
         # HELP olp_usage_consumer_heartbeat_age_seconds Age of the last durable worker checkpoint.\n\
         # TYPE olp_usage_consumer_heartbeat_age_seconds gauge\n\
         olp_usage_consumer_heartbeat_age_seconds {}\n\
         # HELP olp_usage_consumer_healthy Whether the durable consumer is current and fully drained.\n\
         # TYPE olp_usage_consumer_healthy gauge\n\
         olp_usage_consumer_healthy {}\n\
         # HELP olp_usage_consumer_stale Whether the durable consumer missed its heartbeat threshold.\n\
         # TYPE olp_usage_consumer_stale gauge\n\
         olp_usage_consumer_stale {}\n\
         # HELP olp_usage_gateway_open_epochs Gateway process epochs still emitting checkpoints.\n\
         # TYPE olp_usage_gateway_open_epochs gauge\n\
         olp_usage_gateway_open_epochs {}\n\
         # HELP olp_usage_gateway_unresolved_epochs Unclean gateway epochs awaiting operator acknowledgement.\n\
         # TYPE olp_usage_gateway_unresolved_epochs gauge\n\
         olp_usage_gateway_unresolved_epochs {}\n\
         # HELP olp_usage_historical_uncertain_gaps Retained exactness gaps across raw and hourly evidence.\n\
         # TYPE olp_usage_historical_uncertain_gaps gauge\n\
         olp_usage_historical_uncertain_gaps {}\n\
         # HELP olp_usage_gateway_unresolved_event_lower_bound Last durable in-flight event lower bound across unresolved epochs.\n\
         # TYPE olp_usage_gateway_unresolved_event_lower_bound gauge\n\
         olp_usage_gateway_unresolved_event_lower_bound {}\n\
         # HELP olp_distributed_limiter_available Whether a Valkey limiter connection is installed.\n\
         # TYPE olp_distributed_limiter_available gauge\n\
         olp_distributed_limiter_available {}\n\
         # HELP olp_open_target_circuits Number of target circuits currently open or half-open.\n\
         # TYPE olp_open_target_circuits gauge\n\
         olp_open_target_circuits {}\n\
         # HELP olp_media_reconciliation_pending Metadata-only media jobs awaiting reconciliation.\n\
         # TYPE olp_media_reconciliation_pending gauge\n\
         olp_media_reconciliation_pending {}\n\
         # HELP olp_media_reconciliation_stale Media reconciliation jobs past their grace period.\n\
         # TYPE olp_media_reconciliation_stale gauge\n\
         olp_media_reconciliation_stale {}\n\
         # HELP olp_media_reconciliation_failed Media jobs whose latest autonomous reconciliation attempt failed.\n\
         # TYPE olp_media_reconciliation_failed gauge\n\
         olp_media_reconciliation_failed {}\n\
         # HELP olp_media_reconciliation_unbound Live media jobs without immutable runtime authority.\n\
         # TYPE olp_media_reconciliation_unbound gauge\n\
         olp_media_reconciliation_unbound {}\n\
         # HELP olp_media_reconciliation_gaps_total Upstream media side effects that could not be durably recorded.\n\
         # TYPE olp_media_reconciliation_gaps_total counter\n\
         olp_media_reconciliation_gaps_total {}\n",
        state.runtime.ordinal().unwrap_or(0),
        usage.map_or(0, |snapshot| snapshot.dropped),
        usage.map_or(0, |snapshot| snapshot.abandoned),
        usage.map_or(0, |snapshot| snapshot.pending()),
        usage.map_or(0, |snapshot| u8::from(snapshot.retrying)),
        usage.map_or(0, |snapshot| u8::from(!snapshot.closed)),
        usage_consumer.pending_events,
        usage_consumer.lag_events,
        usage_consumer.heartbeat_age_seconds.unwrap_or(0),
        u8::from(usage_consumer.complete()),
        u8::from(matches!(
            usage_consumer.state,
            olp_storage::UsageConsumerState::Stale
        )),
        usage_epochs.open_epochs,
        usage_epochs.unresolved_epochs,
        usage_epochs.historical_uncertain_gap_count,
        usage_epochs.unresolved_event_lower_bound,
        u8::from(limiter_available),
        state.circuits.open_count(),
        media_reconciliation
            .as_ref()
            .map_or(0, |value| value.pending),
        media_reconciliation.as_ref().map_or(0, |value| value.stale),
        media_reconciliation
            .as_ref()
            .map_or(0, |value| value.failed),
        media_reconciliation
            .as_ref()
            .map_or(0, |value| value.unbound),
        state.media_reconciliation_gap_count(),
    );
    body.push_str(
        "# HELP olp_operational_metrics_available Whether the PostgreSQL operational rollup was available.\n\
         # TYPE olp_operational_metrics_available gauge\n",
    );
    let _ = writeln!(
        body,
        "olp_operational_metrics_available {}",
        u8::from(operations_summary.is_some())
    );
    if let Some(summary) = operations_summary {
        let success_ratio = if summary.request_count == 0 {
            1.0
        } else {
            summary.success_count as f64 / summary.request_count as f64
        };
        body.push_str(
            "# HELP olp_requests_5m Metadata requests observed during the trailing five minutes.\n\
             # TYPE olp_requests_5m gauge\n\
             # HELP olp_request_success_ratio_5m Successful request ratio during the trailing five minutes.\n\
             # TYPE olp_request_success_ratio_5m gauge\n\
             # HELP olp_request_latency_seconds Request latency quantiles during the trailing five minutes.\n\
             # TYPE olp_request_latency_seconds gauge\n\
             # HELP olp_upstream_cancellations_5m Cancelled upstream attempts during the trailing five minutes.\n\
             # TYPE olp_upstream_cancellations_5m gauge\n",
        );
        let _ = writeln!(body, "olp_requests_5m {}", summary.request_count);
        let _ = writeln!(body, "olp_request_success_ratio_5m {success_ratio:.6}");
        let _ = writeln!(
            body,
            "olp_request_latency_seconds{{quantile=\"0.95\"}} {:.6}",
            summary.p95_latency_ms.unwrap_or(0.0) / 1_000.0
        );
        let _ = writeln!(
            body,
            "olp_request_latency_seconds{{quantile=\"0.99\"}} {:.6}",
            summary.p99_latency_ms.unwrap_or(0.0) / 1_000.0
        );
        let _ = writeln!(
            body,
            "olp_upstream_cancellations_5m {}",
            summary.cancelled_attempt_count
        );
    }
    if !provider_health.is_empty() {
        body.push_str(
            "# HELP olp_provider_health Provider health classification over the trailing fifteen minutes.\n\
             # TYPE olp_provider_health gauge\n\
             # HELP olp_provider_success_ratio_15m Provider attempt success ratio over the trailing fifteen minutes.\n\
             # TYPE olp_provider_success_ratio_15m gauge\n\
             # HELP olp_provider_latency_seconds_15m Provider average attempt latency over the trailing fifteen minutes.\n\
             # TYPE olp_provider_latency_seconds_15m gauge\n",
        );
        for provider in provider_health {
            let provider_id = provider.provider_id;
            let name = prometheus_label(&provider.provider_name);
            let kind = prometheus_label(provider.provider_kind.as_str());
            let status = prometheus_label(&provider.status);
            let success_ratio = if provider.attempt_count == 0 {
                1.0
            } else {
                provider.success_count as f64 / provider.attempt_count as f64
            };
            let labels = format!(
                "provider_id=\"{provider_id}\",provider_name=\"{name}\",provider_kind=\"{kind}\",status=\"{status}\""
            );
            let _ = writeln!(body, "olp_provider_health{{{labels}}} 1");
            let _ = writeln!(
                body,
                "olp_provider_success_ratio_15m{{{labels}}} {success_ratio:.6}"
            );
            let _ = writeln!(
                body,
                "olp_provider_latency_seconds_15m{{{labels}}} {:.6}",
                provider.average_latency_ms.unwrap_or(0.0) / 1_000.0
            );
        }
    }
    body
}

fn prometheus_label(value: &str) -> String {
    value
        .replace('\\', "\\\\")
        .replace('\n', "\\n")
        .replace('"', "\\\"")
}

mod event_completion;
mod gateway;
mod static_console;

#[cfg(test)]
mod tests {
    use std::{
        collections::{BTreeMap, BTreeSet},
        convert::Infallible,
        num::NonZeroU32,
        path::PathBuf,
        sync::atomic::{AtomicBool, Ordering},
    };

    use axum::{
        body::Body,
        http::{HeaderValue, Response},
    };
    use base64::Engine as _;
    use http_body_util::BodyExt as _;
    use olp_domain::{
        ApiKeyDigest, ApiKeyId, ApiKeyLimits, ApiKeyScope, RuntimeGeneration, RuntimeGenerationId,
        RuntimeSnapshot,
    };
    use tower::{ServiceBuilder, ServiceExt, service_fn};
    use uuid::Uuid;

    use super::*;

    #[test]
    fn prometheus_labels_escape_control_syntax() {
        assert_eq!(
            prometheus_label("provider\\\"name\nnext"),
            "provider\\\\\\\"name\\nnext"
        );
    }

    #[test]
    fn public_auth_source_uses_forwarding_only_from_trusted_peers() {
        let mut state = ApiState::new(
            ApiMode::Control,
            None,
            Arc::new(RuntimeManager::empty()),
            "https://olp.example.test",
            PathBuf::from("missing-console"),
        );
        state.set_trusted_proxy_cidrs(vec!["10.0.0.0/8".parse().unwrap()]);
        let mut forwarded = HeaderMap::new();
        forwarded.insert(
            "x-forwarded-for",
            HeaderValue::from_static("198.51.100.24, 10.1.2.3"),
        );
        assert_eq!(
            public_auth_source(&state, &forwarded, Some("10.2.3.4:443".parse().unwrap()),).unwrap(),
            "198.51.100.24"
        );

        let mut spoofed = HeaderMap::new();
        spoofed.insert("x-forwarded-for", HeaderValue::from_static("not-an-ip"));
        assert_eq!(
            public_auth_source(&state, &spoofed, Some("203.0.113.30:443".parse().unwrap()),)
                .unwrap(),
            "203.0.113.30"
        );
        assert_eq!(
            public_auth_source(
                &state,
                &HeaderMap::new(),
                Some("10.2.3.4:443".parse().unwrap()),
            )
            .unwrap_err()
            .status,
            400
        );
        assert_eq!(
            public_auth_source(&state, &spoofed, Some("10.2.3.4:443".parse().unwrap()),)
                .unwrap_err()
                .status,
            400
        );
        assert_eq!(
            public_auth_source(&state, &HeaderMap::new(), None)
                .unwrap_err()
                .status,
            503
        );
    }

    #[test]
    fn multipart_admission_is_post_only_and_recovers_after_a_parser_drops() {
        assert!(multipart_endpoint(&axum::http::Method::GET, "/openai/v1/videos").is_none());
        assert!(multipart_endpoint(&axum::http::Method::POST, "/openai/v1/videos").is_some());

        // With a 256-byte spool, untrusted multipart parsers may reserve at
        // most its 128-byte half-budget. A key gets at most one live parser,
        // and releasing/dropping a parser promptly admits the next one.
        let admission = MultipartAdmissionState::new(256);
        let first_key = uuid::Uuid::now_v7();
        let second_key = uuid::Uuid::now_v7();
        let first = admission.try_admit(first_key, 64).unwrap();
        assert!(admission.try_admit(first_key, 64).is_none());
        let second = admission.try_admit(second_key, 64).unwrap();
        assert!(admission.try_admit(uuid::Uuid::now_v7(), 64).is_none());

        first.release();
        assert!(admission.try_admit(first_key, 64).is_some());
        drop(second);
    }

    #[tokio::test]
    async fn malformed_trusted_proxy_chain_is_rejected_before_public_auth_body_handling() {
        let mut state = ApiState::new(
            ApiMode::Control,
            None,
            Arc::new(RuntimeManager::empty()),
            "https://olp.example.test",
            PathBuf::from("missing-console"),
        );
        state.set_trusted_proxy_cidrs(vec!["10.0.0.0/8".parse().unwrap()]);
        let response = public_router(state)
            .oneshot(
                Request::post("/api/v1/sessions")
                    .header(axum::http::header::CONTENT_TYPE, "application/json")
                    .header("x-forwarded-for", "not-an-ip")
                    .extension(axum::extract::ConnectInfo(
                        "10.2.3.4:443".parse::<SocketAddr>().unwrap(),
                    ))
                    .body(Body::from("{"))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), axum::http::StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn bootstrap_token_digest_is_verified_then_cleared() {
        let mut state = ApiState::new(
            ApiMode::Control,
            None,
            Arc::new(RuntimeManager::empty()),
            "https://olp.example.test",
            PathBuf::from("missing-console"),
        );
        let hasher = Arc::new(KeyHasher::new([3; 32]));
        let token = base64::engine::general_purpose::STANDARD.encode([7_u8; 32]);
        let digest = hasher.bootstrap_token_digest_from_base64(&token).unwrap();
        state.key_hasher = Some(hasher);
        state.set_bootstrap_token_digest(digest);
        assert_eq!(state.verify_bootstrap_token(Some(&token)).await, Some(true));
        assert_eq!(
            state.verify_bootstrap_token(Some("not-a-token")).await,
            Some(false)
        );
        state.clear_bootstrap_token().await;
        assert_eq!(state.verify_bootstrap_token(Some(&token)).await, None);
    }

    #[tokio::test]
    async fn public_router_explicitly_hides_observability_paths() {
        let app = public_router(ApiState::new(
            ApiMode::Control,
            None,
            Arc::new(RuntimeManager::empty()),
            "https://olp.example.test",
            PathBuf::from("missing-console"),
        ));

        for path in [
            "/health",
            "/health/live",
            "/health/ready",
            "/metrics",
            "/metrics/",
        ] {
            let response = app
                .clone()
                .oneshot(Request::get(path).body(Body::empty()).unwrap())
                .await
                .unwrap();
            assert_eq!(
                response.status(),
                axum::http::StatusCode::NOT_FOUND,
                "{path}"
            );
        }
    }

    #[tokio::test]
    async fn observability_router_serves_cached_snapshots_and_freshness_telemetry() {
        let (state, _) = inference_state(false);
        refresh_observability_cache(&state).await;
        let app = observability_router(state.clone());

        let live = app
            .clone()
            .oneshot(Request::get("/health/live").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(live.status(), axum::http::StatusCode::OK);

        let ready = app
            .clone()
            .oneshot(Request::get("/health/ready").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(ready.status(), axum::http::StatusCode::OK);
        assert_eq!(ready.headers()["x-olp-observability-snapshot-fresh"], "1");

        let metrics = app
            .clone()
            .oneshot(Request::get("/metrics").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(metrics.status(), axum::http::StatusCode::OK);
        let metrics = metrics.into_body().collect().await.unwrap().to_bytes();
        let metrics = String::from_utf8(metrics.to_vec()).unwrap();
        assert!(metrics.contains("olp_ready 1"));
        assert!(metrics.contains("olp_observability_metrics_snapshot_fresh 1"));

        let private_only = app
            .oneshot(
                Request::get("/api/v1/providers")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(private_only.status(), axum::http::StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn stale_observability_snapshots_force_unready_metrics_and_readiness() {
        let (state, _) = inference_state(false);
        refresh_observability_cache(&state).await;
        let stale_at = Instant::now() - OBSERVABILITY_SNAPSHOT_STALE_AFTER - Duration::from_secs(1);
        {
            let mut readiness = state.observability.readiness.write().unwrap();
            readiness.last_success_at = Some(stale_at);
            readiness.last_attempt_at = Some(stale_at);
        }
        {
            let mut metrics = state.observability.metrics.write().unwrap();
            metrics.last_success_at = Some(stale_at);
            metrics.last_attempt_at = Some(stale_at);
        }
        let app = observability_router(state);

        let ready = app
            .clone()
            .oneshot(Request::get("/health/ready").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(ready.status(), axum::http::StatusCode::SERVICE_UNAVAILABLE);
        assert_eq!(ready.headers()["x-olp-observability-snapshot-fresh"], "0");

        let metrics = app
            .oneshot(Request::get("/metrics").body(Body::empty()).unwrap())
            .await
            .unwrap();
        let metrics = metrics.into_body().collect().await.unwrap().to_bytes();
        let metrics = String::from_utf8(metrics.to_vec()).unwrap();
        assert!(metrics.contains("olp_ready 0"));
        assert!(metrics.contains("olp_observability_metrics_snapshot_fresh 0"));
    }

    #[tokio::test]
    async fn stale_metrics_do_not_change_the_readiness_contract() {
        let (state, _) = inference_state(false);
        refresh_observability_cache(&state).await;
        state.observability.record_metrics_failure();

        let response = observability_router(state)
            .oneshot(Request::get("/metrics").body(Body::empty()).unwrap())
            .await
            .unwrap();
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let body = String::from_utf8(body.to_vec()).unwrap();
        assert!(body.contains("olp_ready 1"));
        assert!(body.contains("olp_observability_metrics_snapshot_fresh 0"));
    }

    fn inference_state(limited: bool) -> (ApiState, String) {
        let key_hasher = Arc::new(KeyHasher::new([19; 32]));
        let material = key_hasher.generate_api_key();
        let plaintext = material.expose_once().to_owned();
        let lookup_id = ApiKeyLookupId::parse(material.lookup_id.clone()).unwrap();
        let runtime = Arc::new(RuntimeManager::empty());
        runtime
            .install(
                RuntimeSnapshot {
                    generation: RuntimeGeneration {
                        id: RuntimeGenerationId::new(),
                        ordinal: 1,
                        activated_at: chrono::Utc::now(),
                    },
                    providers: BTreeMap::new(),
                    routes: BTreeMap::new(),
                    api_keys: BTreeMap::from([(
                        lookup_id.clone(),
                        ApiKey {
                            id: ApiKeyId::new(),
                            lookup_id,
                            digest: ApiKeyDigest::new(material.digest),
                            status: ApiKeyStatus::Active,
                            expires_at: None,
                            scopes: BTreeSet::from([
                                ApiKeyScope::Inference,
                                ApiKeyScope::ModelsRead,
                            ]),
                            allowed_routes: BTreeSet::new(),
                            limits: ApiKeyLimits {
                                requests_per_minute: limited.then(|| NonZeroU32::new(10).unwrap()),
                                tokens_per_minute: None,
                                concurrency: limited.then(|| NonZeroU32::new(2).unwrap()),
                            },
                        },
                    )]),
                },
                BTreeMap::new(),
            )
            .unwrap();
        let mut state = ApiState::new(
            ApiMode::Gateway,
            None,
            runtime,
            "https://olp.example.test",
            PathBuf::from("missing-console"),
        );
        state.key_hasher = Some(key_hasher);
        (state, plaintext)
    }

    #[test]
    fn local_metadata_detection_is_method_and_surface_exact() {
        assert_eq!(
            inference_metadata_operation(&axum::http::Method::GET, "/openai/v1/models"),
            Some(("model_list", "models"))
        );
        assert_eq!(
            inference_metadata_operation(
                &axum::http::Method::GET,
                "/gemini/v1beta/models/team-route"
            ),
            Some(("model_get", "models"))
        );
        assert_eq!(
            inference_metadata_operation(&axum::http::Method::GET, "/openai/v1/videos"),
            Some(("video_list", "videos"))
        );
        assert_eq!(
            inference_metadata_operation(&axum::http::Method::POST, "/openai/v1/videos"),
            Some(("video_create", "invalid-request"))
        );
    }

    #[tokio::test]
    async fn local_metadata_event_is_content_free_and_reconcilable() {
        let (usage, mut receiver) = UsageEmitter::bounded(1);
        let generation_id = uuid::Uuid::now_v7();
        let api_key_id = uuid::Uuid::now_v7();
        LocalRequestMetadata {
            usage: Some(usage),
            request_started_at: chrono::Utc::now(),
            runtime_generation_id: generation_id,
            api_key_id,
            route_slug: "models".to_owned(),
            operation: "model_list",
            surface: Surface::OpenAi,
            always_emit: true,
        }
        .emit(axum::http::StatusCode::OK);
        let event = receiver.recv_next().await.unwrap();
        assert_eq!(event.runtime_generation_id, generation_id);
        assert_eq!(event.api_key_id, api_key_id);
        assert_eq!(event.operation, OperationKind::ModelList);
        assert_eq!(event.route_slug, "models");
        assert!(event.provider_id.is_none());
        assert!(event.upstream_model.is_none());
        assert!(event.attempts.is_empty());
        assert!(!event.usage_complete);
    }

    #[tokio::test]
    async fn trace_boundary_marks_authentication_headers_sensitive() {
        let service = ServiceBuilder::new()
            .layer(SetSensitiveRequestHeadersLayer::new(
                sensitive_request_headers(),
            ))
            .layer(TraceLayer::new_for_http().make_span_with(http_request_span))
            .layer(SetSensitiveResponseHeadersLayer::new(
                sensitive_response_headers(),
            ))
            .service(service_fn(|request: Request<Body>| async move {
                for header in sensitive_request_headers() {
                    assert!(request.headers()[header].is_sensitive());
                }
                let mut response = Response::new(Body::empty());
                response.headers_mut().insert(
                    axum::http::header::SET_COOKIE,
                    HeaderValue::from_static("session=secret"),
                );
                Ok::<_, Infallible>(response)
            }));

        let mut request = Request::new(Body::empty());
        request.headers_mut().insert(
            axum::http::header::AUTHORIZATION,
            HeaderValue::from_static("Bearer secret"),
        );
        request.headers_mut().insert(
            axum::http::header::COOKIE,
            HeaderValue::from_static("session=secret"),
        );
        request.headers_mut().insert(
            HeaderName::from_static(management::CSRF_HEADER),
            HeaderValue::from_static("csrf-secret"),
        );
        request.headers_mut().insert(
            HeaderName::from_static(management::SETUP_TOKEN_HEADER),
            HeaderValue::from_static("bootstrap-secret"),
        );
        request.headers_mut().insert(
            HeaderName::from_static("x-api-key"),
            HeaderValue::from_static("anthropic-secret"),
        );
        request.headers_mut().insert(
            HeaderName::from_static("x-goog-api-key"),
            HeaderValue::from_static("gemini-secret"),
        );
        let response = service.oneshot(request).await.unwrap();
        assert!(
            response.headers()[axum::http::header::SET_COOKIE].is_sensitive(),
            "TraceLayer must observe Set-Cookie only after it is marked sensitive"
        );
    }

    #[test]
    fn request_trace_path_omits_query_parameters() {
        let uri: Uri = "/openai/v1/models?key=must-not-be-logged".parse().unwrap();
        assert_eq!(request_trace_path(&uri), "/openai/v1/models");
    }

    #[test]
    fn json_depth_scanner_ignores_strings_and_rejects_excessive_nesting() {
        validate_json_depth(br#"{"text":"[[[[{{{{","nested":[{"ok":true}]} }"#).unwrap();
        let too_deep = format!("{}0{}", "[".repeat(65), "]".repeat(65));
        assert_eq!(
            validate_json_depth(too_deep.as_bytes()).unwrap_err().status,
            axum::http::StatusCode::BAD_REQUEST.as_u16()
        );
    }

    #[test]
    fn multipart_boundary_is_required_and_bounded() {
        validate_multipart_boundary("multipart/form-data; boundary=olp-boundary").unwrap();
        assert!(validate_multipart_boundary("multipart/form-data").is_err());
        assert!(
            validate_multipart_boundary(&format!(
                "multipart/form-data; boundary={}",
                "x".repeat(201)
            ))
            .is_err()
        );
    }

    #[test]
    fn raw_json_tpm_estimate_includes_requested_output_and_candidates() {
        let body = br#"{"max_completion_tokens":8192,"n":3,"messages":[]}"#;
        let estimate = estimate_http_json_request_tokens("/openai/v1/chat/completions", body);
        assert!(estimate >= 8_192 * 3);
        assert!(
            estimate_http_json_request_tokens("/anthropic/v1/messages", b"{") >= 4_096,
            "malformed generation requests retain a fail-safe output estimate"
        );
        assert!(
            estimate_http_json_request_tokens("/openai/v1/embeddings", body) < 4_096,
            "non-generation operations do not inherit generation output tokens"
        );
    }

    #[tokio::test]
    async fn json_body_read_has_its_own_deadline_outside_route_layers() {
        let body =
            Body::from_stream(futures::stream::pending::<Result<bytes::Bytes, Infallible>>());
        let result = read_json_body(body, MAX_JSON_BODY_BYTES, Duration::from_millis(5)).await;
        assert_eq!(result.unwrap_err(), JsonBodyReadError::Timeout);
    }

    #[tokio::test]
    async fn management_openapi_is_only_served_on_the_versioned_route() {
        let console_dir = std::env::temp_dir().join(format!("olp-console-test-{}", Uuid::now_v7()));
        std::fs::create_dir(&console_dir).unwrap();
        std::fs::write(
            console_dir.join("index.html"),
            "<!doctype html><title>OLP</title>",
        )
        .unwrap();
        let app = public_router(ApiState::new(
            ApiMode::Control,
            None,
            Arc::new(RuntimeManager::empty()),
            "https://olp.example.test",
            &console_dir,
        ));

        let versioned = app
            .clone()
            .oneshot(
                Request::get("/api/v1/openapi.json")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(versioned.status(), axum::http::StatusCode::OK);

        let legacy = app
            .oneshot(Request::get("/openapi.json").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(legacy.status(), axum::http::StatusCode::NOT_FOUND);
        std::fs::remove_dir_all(console_dir).unwrap();
    }

    #[tokio::test]
    async fn request_limit_matrix_rejects_depth_size_encoding_and_bad_multipart() {
        let app = public_router(ApiState::new(
            ApiMode::Gateway,
            None,
            Arc::new(RuntimeManager::empty()),
            "https://olp.example.test",
            PathBuf::from("missing-console"),
        ));

        let too_deep = format!("{}0{}", "[".repeat(65), "]".repeat(65));
        let response = app
            .clone()
            .oneshot(
                Request::post("/api/not-found")
                    .header(axum::http::header::CONTENT_TYPE, "application/json")
                    .body(Body::from(too_deep))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), axum::http::StatusCode::BAD_REQUEST);

        let response = app
            .clone()
            .oneshot(
                Request::post("/openai/not-found")
                    .header(axum::http::header::CONTENT_TYPE, "application/json")
                    .header(
                        axum::http::header::CONTENT_LENGTH,
                        (MAX_JSON_BODY_BYTES + 1).to_string(),
                    )
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), axum::http::StatusCode::PAYLOAD_TOO_LARGE);

        let response = app
            .clone()
            .oneshot(
                Request::post("/openai/not-found")
                    .header(axum::http::header::CONTENT_TYPE, "application/json")
                    .header(axum::http::header::CONTENT_ENCODING, "gzip")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(
            response.status(),
            axum::http::StatusCode::UNSUPPORTED_MEDIA_TYPE
        );

        let response = app
            .oneshot(
                Request::post("/openai/v1/audio/transcriptions")
                    .header(axum::http::header::CONTENT_TYPE, "multipart/form-data")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(
            response.status(),
            axum::http::StatusCode::UNAUTHORIZED,
            "inference authentication precedes multipart decoding"
        );
    }

    #[tokio::test]
    async fn authenticated_multipart_routes_reject_non_multipart_content_types() {
        let (state, key) = inference_state(false);
        let app = public_router(state);
        for content_type in [None, Some("application/json")] {
            let mut request = Request::post("/openai/v1/images/edits")
                .header(axum::http::header::AUTHORIZATION, format!("Bearer {key}"));
            if let Some(content_type) = content_type {
                request = request.header(axum::http::header::CONTENT_TYPE, content_type);
            }
            let response = app
                .clone()
                .oneshot(request.body(Body::empty()).unwrap())
                .await
                .unwrap();
            assert_eq!(response.status(), axum::http::StatusCode::BAD_REQUEST);
        }
    }

    #[tokio::test]
    async fn management_extractor_rejections_are_rfc9457_without_query_reflection() {
        let app = public_router(ApiState::new(
            ApiMode::Control,
            None,
            Arc::new(RuntimeManager::empty()),
            "https://olp.example.test",
            PathBuf::from("missing-console"),
        ));
        for (uri, expected_instance) in [
            (
                "/api/v1/providers?limit=not-a-number&secret=must-not-reflect",
                "/api/v1/providers",
            ),
            (
                "/api/v1/providers/not-a-uuid",
                "/api/v1/providers/not-a-uuid",
            ),
        ] {
            let response = app
                .clone()
                .oneshot(Request::get(uri).body(Body::empty()).unwrap())
                .await
                .unwrap();
            assert_eq!(response.status(), axum::http::StatusCode::BAD_REQUEST);
            assert_eq!(
                response.headers()[axum::http::header::CONTENT_TYPE],
                "application/problem+json"
            );
            let body = response.into_body().collect().await.unwrap().to_bytes();
            let problem: Problem = serde_json::from_slice(&body).unwrap();
            assert_eq!(problem.instance.as_deref(), Some(expected_instance));
            assert!(problem.errors.contains_key("request"));
            assert!(!String::from_utf8_lossy(&body).contains("must-not-reflect"));
        }
    }

    #[tokio::test]
    async fn inference_authentication_precedes_body_decode_with_native_errors() {
        let mut state = ApiState::new(
            ApiMode::Gateway,
            None,
            Arc::new(RuntimeManager::empty()),
            "https://olp.example.test",
            PathBuf::from("missing-console"),
        );
        state.key_hasher = Some(Arc::new(KeyHasher::new([3; 32])));
        let app = public_router(state);
        let too_deep = format!("{}0{}", "[".repeat(65), "]".repeat(65));
        for (path, header_name, expected_pointer) in [
            (
                "/openai/v1/chat/completions",
                axum::http::header::AUTHORIZATION,
                "/error/code",
            ),
            (
                "/anthropic/v1/messages",
                HeaderName::from_static("x-api-key"),
                "/error/type",
            ),
            (
                "/gemini/v1beta/models/test:generateContent",
                HeaderName::from_static("x-goog-api-key"),
                "/error/status",
            ),
        ] {
            let value = if header_name == axum::http::header::AUTHORIZATION {
                "Bearer invalid-key"
            } else {
                "invalid-key"
            };
            let response = app
                .clone()
                .oneshot(
                    Request::post(path)
                        .header(axum::http::header::CONTENT_TYPE, "application/json")
                        .header(header_name, value)
                        .body(Body::from(too_deep.clone()))
                        .unwrap(),
                )
                .await
                .unwrap();
            assert_eq!(response.status(), axum::http::StatusCode::UNAUTHORIZED);
            assert_ne!(
                response.headers()[axum::http::header::CONTENT_TYPE],
                "application/problem+json"
            );
            let body = response.into_body().collect().await.unwrap().to_bytes();
            let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
            assert!(body.pointer(expected_pointer).is_some(), "body was {body}");
        }
    }

    #[tokio::test]
    async fn malformed_inference_requests_with_hard_limits_fail_closed_before_decode() {
        let (state, key) = inference_state(true);
        let app = public_router(state);
        for (path, header_name, header_value, content_type, body, pointer, expected) in [
            (
                "/openai/v1/chat/completions",
                axum::http::header::AUTHORIZATION,
                format!("Bearer {key}"),
                "application/json",
                "{",
                "/error/code",
                "distributed_limits_unavailable",
            ),
            (
                "/anthropic/v1/messages",
                HeaderName::from_static("x-api-key"),
                key.clone(),
                "application/json",
                "{",
                "/error/type",
                "api_error",
            ),
            (
                "/gemini/v1beta/models/default:generateContent",
                HeaderName::from_static("x-goog-api-key"),
                key.clone(),
                "application/json",
                "{",
                "/error/status",
                "UNAVAILABLE",
            ),
            (
                "/openai/v1/audio/transcriptions",
                axum::http::header::AUTHORIZATION,
                format!("Bearer {key}"),
                "multipart/form-data",
                "not-multipart",
                "/error/code",
                "distributed_limits_unavailable",
            ),
        ] {
            let response = app
                .clone()
                .oneshot(
                    Request::post(path)
                        .header(header_name, header_value)
                        .header(axum::http::header::CONTENT_TYPE, content_type)
                        .body(Body::from(body))
                        .unwrap(),
                )
                .await
                .unwrap();
            assert_eq!(
                response.status(),
                axum::http::StatusCode::SERVICE_UNAVAILABLE,
                "{path} bypassed hard-limit fail-closed behavior"
            );
            assert_ne!(
                response.headers()[axum::http::header::CONTENT_TYPE],
                "application/problem+json",
                "{path} did not retain its native protocol error envelope"
            );
            let body = response.into_body().collect().await.unwrap().to_bytes();
            let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
            assert_eq!(
                body.pointer(pointer).and_then(|value| value.as_str()),
                Some(expected)
            );
        }
    }

    #[tokio::test]
    async fn malformed_inference_json_without_hard_limits_reaches_native_decoder() {
        let (mut state, key) = inference_state(false);
        let (usage, mut receiver) = UsageEmitter::bounded(2);
        state.usage = Some(usage);
        let response = public_router(state)
            .oneshot(
                Request::post("/openai/v1/chat/completions")
                    .header(axum::http::header::AUTHORIZATION, format!("Bearer {key}"))
                    .header(axum::http::header::CONTENT_TYPE, "application/json")
                    .body(Body::from("{"))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), axum::http::StatusCode::BAD_REQUEST);
        let event = receiver.recv_next().await.unwrap();
        assert_eq!(event.status_code, Some(400));
        assert_eq!(event.operation, OperationKind::Generation);
        assert_eq!(event.route_slug, "invalid-request");
        assert!(event.attempts.is_empty());
        assert!(!event.committed);
    }

    async fn activate_runtime_inside_handler(State(state): State<ApiState>) -> String {
        let pinned_before_activation = pin_inference_runtime(&state);
        let pinned_generation = pinned_before_activation.generation.id;
        state
            .runtime
            .install(
                RuntimeSnapshot {
                    generation: RuntimeGeneration {
                        id: RuntimeGenerationId::new(),
                        ordinal: pinned_before_activation.generation.ordinal + 1,
                        activated_at: chrono::Utc::now(),
                    },
                    providers: pinned_before_activation.providers.clone(),
                    routes: pinned_before_activation.routes.clone(),
                    api_keys: pinned_before_activation.api_keys.clone(),
                },
                BTreeMap::new(),
            )
            .unwrap();
        assert_ne!(state.runtime.pin().generation.id, pinned_generation);
        assert_eq!(
            pin_inference_runtime(&state).generation.id,
            pinned_generation,
            "a request must not mix authentication and route generations"
        );
        pinned_generation.to_string()
    }

    #[tokio::test]
    async fn inference_http_boundary_pins_one_generation_across_activation() {
        let (state, key) = inference_state(false);
        let original_generation = state.runtime.pin().generation.id;
        let app = Router::new()
            .route(
                "/openai/test-generation-pin",
                get(activate_runtime_inside_handler),
            )
            .layer(middleware::from_fn_with_state(
                state.clone(),
                enforce_request_limits,
            ))
            .with_state(state.clone());

        let response = app
            .oneshot(
                Request::get("/openai/test-generation-pin")
                    .header(axum::http::header::AUTHORIZATION, format!("Bearer {key}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), axum::http::StatusCode::OK);
        let body = response.into_body().collect().await.unwrap().to_bytes();
        assert_eq!(body.as_ref(), original_generation.to_string().as_bytes());
        assert_ne!(state.runtime.pin().generation.id, original_generation);
    }

    #[tokio::test]
    async fn response_completion_and_drop_release_the_http_concurrency_reservation() {
        for consume in [true, false] {
            let released = Arc::new(AtomicBool::new(false));
            let release_signal = released.clone();
            let body = Body::new(ReleaseReservationBody {
                inner: Body::from("response"),
                reservation: InferenceReservation::for_test(async move {
                    release_signal.store(true, Ordering::Release);
                }),
            });
            if consume {
                body.collect().await.unwrap();
            } else {
                drop(body);
            }
            tokio::task::yield_now().await;
            assert!(
                released.load(Ordering::Acquire),
                "reservation was not released when consume={consume}"
            );
        }
    }
}

//! Axum delivery adapter for management, inference, the static operator
//! console, and the separately bound private observability surface.

mod circuit;
mod cli;
mod connectors;
mod event_completion;
mod gateway;
mod image_response;
mod json_media;
mod listener;
mod management_api;
mod media_spool;
mod mode_dependencies;
mod observability;
mod oidc;
mod operations;
mod playground;
mod problem;
mod proxy;
mod request_admission;
mod router;
mod runtime;
mod semantic_validation;
mod static_console;
mod streaming_response;

use std::{
    collections::BTreeMap,
    net::IpAddr,
    path::PathBuf,
    sync::{
        Arc, RwLock,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    time::Instant,
};

use arc_swap::ArcSwapOption;
use olp_domain::{MediaSpool, ProviderId, ProviderKind, ProviderTransport};
use olp_storage::{AuthHmacKey, DistributedLimiter, MasterKey, PgStore, RequestMetadataEmitter};
use tokio::sync::RwLock as AsyncRwLock;
use zeroize::Zeroizing;

use observability::{ObservabilityCache, cached_readiness_from_snapshot};
use request_admission::MultipartAdmissionState;

pub use cli::run_cli;
pub use gateway::reconcile_media_jobs_once;
pub use management_api::management_openapi;
#[cfg(any(test, feature = "test-util"))]
pub use media_spool::create_bounded_media_spool_for_test;
pub use media_spool::create_media_spool;
pub use mode_dependencies::{
    ConfigurationDependencies, IdentityDependencies, InferenceDependencies, ModeDependencies,
    ModeDependencyError, OperationsDependencies, WorkerDependencies,
};
pub use observability::{
    observability_router, refresh_observability_cache, spawn_observability_cache,
};
pub use olp_providers::{
    CredentialKind, OpenAiConnector, ProviderConfig, ProviderCredential, ProviderError,
    ProviderFactory,
};
pub use problem::{FieldErrors, Problem};
pub use proxy::{TrustedProxyCidr, TrustedProxyCidrParseError, public_auth_source};
pub use router::{public_router, try_public_router};
pub use runtime::{RuntimeBundle, RuntimeInstallError, RuntimeManager};

pub(crate) use observability::HealthResponse;
pub(crate) use proxy::{public_auth_source_digest, public_auth_source_target_digests};
#[cfg(test)]
pub(crate) use request_admission::HTTP_INFERENCE_LIMITS_RESERVED;
pub(crate) use request_admission::{
    FirstOwnerSetupAuthorized, IMAGE_VARIATION_BODY_BYTES, MultipartRequestAdmission,
    MultipartRouteAdmission, TRANSCRIPTION_BODY_BYTES, VIDEO_CREATE_BODY_BYTES,
    claim_http_inference_metadata, http_inference_reserved_tokens, pin_inference_runtime,
    spawn_http_inference_task,
};

pub(crate) const MAX_JSON_BODY_BYTES: usize = 2 * 1024 * 1024;
pub(crate) const MAX_MEDIA_BODY_BYTES: usize = 64 * 1024 * 1024;
pub const MAX_HTTP_HEADER_COUNT: usize = 100;
pub const MAX_HTTP_HEADER_BYTES: usize = 32 * 1024;

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

#[derive(Clone)]
pub struct ApiState {
    pub mode: ApiMode,
    pub store: Option<PgStore>,
    pub runtime: Arc<RuntimeManager>,
    pub limiter: ReloadableLimiter,
    pub auth_hmac_key: Option<Arc<AuthHmacKey>>,
    trusted_proxy_cidrs: Arc<[TrustedProxyCidr]>,
    bootstrap_token_digest: Arc<AsyncRwLock<Option<Zeroizing<[u8; 32]>>>>,
    pub master_key: Option<Arc<MasterKey>>,
    pub request_metadata: Option<RequestMetadataEmitter>,
    pub(crate) circuits: circuit::CircuitBreaker,
    media_reconciliation_gaps: Arc<AtomicU64>,
    pub media_spool: Arc<dyn MediaSpool>,
    multipart_admission: MultipartAdmissionState,
    pub transports: TransportRegistry,
    certification_probe_connectors: olp_providers::OpenAiConnectorOverrideRegistry,
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
            limiter: ReloadableLimiter::default(),
            auth_hmac_key: None,
            trusted_proxy_cidrs: Arc::from([]),
            bootstrap_token_digest: Arc::new(AsyncRwLock::new(None)),
            master_key: None,
            request_metadata: None,
            circuits: circuit::CircuitBreaker::default(),
            media_reconciliation_gaps: Arc::new(AtomicU64::new(0)),
            media_spool,
            multipart_admission,
            transports: TransportRegistry::default(),
            certification_probe_connectors: Default::default(),
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
        let Some(auth_hmac_key) = self.auth_hmac_key.as_ref() else {
            return Some(false);
        };
        Some(supplied.is_some_and(|supplied| {
            auth_hmac_key.verify_bootstrap_token_digest(supplied, expected)
        }))
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

    /// Installs a prebuilt OpenAI connector for certification and connectivity probes.
    ///
    /// This is an internal dependency-injection seam used by integration tests
    /// to exercise real connector I/O against a local mock without weakening
    /// the production custom-endpoint SSRF policy. Production wiring never
    /// installs an override.
    #[doc(hidden)]
    pub fn register_certification_probe_connector_for_test(
        &self,
        provider_id: uuid::Uuid,
        connector: OpenAiConnector,
    ) {
        self.certification_probe_connectors
            .register(provider_id, connector);
    }

    pub(crate) fn certification_probe_connector(
        &self,
        provider_id: uuid::Uuid,
        kind: ProviderKind,
    ) -> Option<olp_providers::ProviderFacade> {
        self.certification_probe_connectors.get(provider_id, kind)
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

/// Hot-swappable Valkey limiter connection. Gateways start even if Valkey is
/// temporarily unavailable; hard-limited keys still fail closed, while a
/// background supervisor can install a healthy connection without restarting
/// the process.
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

#[cfg(test)]
mod tests;

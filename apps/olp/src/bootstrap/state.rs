//! Process-wide shared state: the [`ProcessComposition`] handed to every HTTP surface,
//! the hot-swappable Valkey limiter, and the runtime transport registry.

use std::{
    collections::BTreeMap,
    path::PathBuf,
    sync::{Arc, RwLock, atomic::AtomicU64},
};

use olp_db::{security::envelope::MasterKey, security::key_material::AuthHmacKey, store::Store};
use olp_engine::domain::{
    ids::ProviderId,
    ports::{MediaSpool, ProviderTransport},
};
use olp_engine::inference::{
    circuit::Breaker, limits::ReloadableLimiter, request_metadata::Emitter, runtime::Manager,
};
#[cfg(feature = "test-util")]
use olp_engine::providers::openai::transport::Connector;
use olp_engine::providers::{EgressPolicy, connector::ResponseLimits};
use tokio::sync::RwLock as AsyncRwLock;
use zeroize::Zeroizing;

use crate::{
    observability::{cache::ObservabilityCache, metrics::RequestMetadataLossCounters},
    public_http::proxy::TrustedProxyCidr,
    public_http::public_origin::PublicOrigin,
    public_http::request_admission::{self, multipart::MultipartAdmissionState},
};

use crate::media_spool;

pub const MAX_HTTP_HEADER_COUNT: usize = 100;
pub const MAX_HTTP_HEADER_BYTES: usize = 32 * 1024;

/// Request body caps enforced at the public boundary. Header caps stay
/// constant because they are tied to the Hyper parser configuration.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct BodyLimits {
    pub json_body_bytes: usize,
    pub media_body_bytes: usize,
    pub inline_media_items: usize,
    pub inline_media_item_bytes: usize,
    pub inline_media_total_bytes: usize,
}

impl Default for BodyLimits {
    fn default() -> Self {
        Self {
            json_body_bytes: 2 * 1024 * 1024,
            media_body_bytes: 64 * 1024 * 1024,
            inline_media_items: 4,
            inline_media_item_bytes: 1024 * 1024,
            inline_media_total_bytes: 2 * 1024 * 1024,
        }
    }
}

impl BodyLimits {
    /// Multipart admission budgets half the spool, so a media cap above that
    /// would make every multipart request fail with 503.
    pub fn validate(self, spool_capacity_bytes: u64) -> Result<Self, String> {
        if self.inline_media_item_bytes > self.inline_media_total_bytes {
            return Err(
                "OLP_HTTP_MAX_INLINE_MEDIA_ITEM_BYTES must not exceed OLP_HTTP_MAX_INLINE_MEDIA_TOTAL_BYTES"
                    .to_owned(),
            );
        }
        if self.inline_media_total_bytes > self.json_body_bytes {
            return Err(
                "OLP_HTTP_MAX_INLINE_MEDIA_TOTAL_BYTES must not exceed OLP_HTTP_MAX_JSON_BODY_BYTES"
                    .to_owned(),
            );
        }
        if self.media_body_bytes as u64 > spool_capacity_bytes / 2 {
            return Err(format!(
                "OLP_HTTP_MAX_MEDIA_BODY_BYTES must not exceed half of OLP_MEDIA_SPOOL_CAPACITY_BYTES ({})",
                spool_capacity_bytes / 2
            ));
        }
        Ok(self)
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

#[derive(Clone)]
pub struct ProcessComposition {
    pub mode: ApiMode,
    pub store: Store,
    pub runtime: Arc<Manager>,
    pub limiter: ReloadableLimiter,
    pub auth_hmac_key: Arc<AuthHmacKey>,
    pub(crate) trusted_proxy_cidrs: Arc<[TrustedProxyCidr]>,
    pub(crate) gateway_cors_allowed_origins: Arc<[axum::http::HeaderValue]>,
    pub(crate) provider_egress_policy: Arc<EgressPolicy>,
    pub(crate) provider_response_limits: ResponseLimits,
    pub(crate) body_limits: BodyLimits,
    pub(crate) bootstrap_token_digest: Arc<AsyncRwLock<Option<Zeroizing<[u8; 32]>>>>,
    pub master_key: Option<Arc<MasterKey>>,
    pub request_metadata: Option<Emitter>,
    pub(crate) circuits: Breaker,
    pub(crate) media_reconciliation_gaps: Arc<AtomicU64>,
    pub(crate) request_metadata_loss: RequestMetadataLossCounters,
    pub media_spool: Arc<dyn MediaSpool>,
    pub(crate) multipart_admission: MultipartAdmissionState,
    pub(crate) public_admission: request_admission::public::PublicAdmission,
    pub transports: TransportRegistry,
    #[cfg(any(test, feature = "test-util"))]
    pub(crate) certification_probe_connectors: olp_engine::providers::factory::overrides::Registry,
    pub public_origin: PublicOrigin,
    pub console_dir: Arc<PathBuf>,
    pub session_ttl: chrono::Duration,
    pub local_login_enabled: bool,
    pub(crate) observability: ObservabilityCache,
    /// Enables loopback HTTP only for local mock-IdP tests. Runtime wiring
    /// keeps this production-safe default disabled.
    pub oidc_allow_insecure_test_endpoints: bool,
}

impl ProcessComposition {
    /// Test composition with a private spool and a fixed authentication
    /// key; tests that need a specific key assign `auth_hmac_key` afterwards.
    #[cfg(any(test, feature = "test-util"))]
    pub fn new(
        mode: ApiMode,
        store: Store,
        runtime: Arc<Manager>,
        public_origin: impl AsRef<str>,
        console_dir: impl Into<PathBuf>,
    ) -> Self {
        let media_spool = media_spool::FileMediaSpool::create()
            .expect("the private bounded media spool must be creatable");
        Self::new_with_media_spool(
            mode,
            store,
            runtime,
            Arc::new(AuthHmacKey::new([0xA5; 32])),
            public_origin,
            console_dir,
            media_spool,
        )
    }

    pub fn new_with_media_spool(
        mode: ApiMode,
        store: Store,
        runtime: Arc<Manager>,
        auth_hmac_key: Arc<AuthHmacKey>,
        public_origin: impl AsRef<str>,
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
            auth_hmac_key,
            trusted_proxy_cidrs: Arc::from([]),
            gateway_cors_allowed_origins: Arc::from([]),
            provider_egress_policy: Arc::new(EgressPolicy::default()),
            provider_response_limits: ResponseLimits::default(),
            body_limits: BodyLimits::default(),
            bootstrap_token_digest: Arc::new(AsyncRwLock::new(None)),
            master_key: None,
            request_metadata: None,
            circuits: Breaker::default(),
            media_reconciliation_gaps: Arc::new(AtomicU64::new(0)),
            request_metadata_loss: RequestMetadataLossCounters::default(),
            media_spool,
            multipart_admission,
            public_admission: request_admission::public::PublicAdmission::default(),
            transports: TransportRegistry::default(),
            #[cfg(any(test, feature = "test-util"))]
            certification_probe_connectors: Default::default(),
            public_origin: PublicOrigin::parse(public_origin.as_ref())
                .expect("ProcessComposition public origin must be a valid canonical origin"),
            console_dir: Arc::new(console_dir.into()),
            session_ttl: chrono::Duration::hours(12),
            local_login_enabled: true,
            observability: ObservabilityCache::default(),
            oidc_allow_insecure_test_endpoints: false,
        }
    }

    /// Installs the explicit proxy trust boundary parsed at process startup.
    /// An empty set means that all `X-Forwarded-For` headers are ignored.
    pub fn set_trusted_proxy_cidrs(&mut self, cidrs: Vec<TrustedProxyCidr>) {
        self.trusted_proxy_cidrs = Arc::from(cidrs);
    }

    pub fn set_gateway_cors_allowed_origins(&mut self, origins: Vec<axum::http::HeaderValue>) {
        self.gateway_cors_allowed_origins = Arc::from(origins);
    }

    /// Installs the operator egress allowlists applied to every provider
    /// endpoint parse, DNS answer, and connector assembly.
    pub fn set_provider_egress_policy(&mut self, policy: EgressPolicy) {
        self.provider_egress_policy = Arc::new(policy);
    }

    /// Stores only a keyed digest of the first-run setup token. The raw token
    /// is never retained by the HTTP state.
    pub fn set_bootstrap_token_digest(&mut self, digest: [u8; 32]) {
        self.bootstrap_token_digest = Arc::new(AsyncRwLock::new(Some(Zeroizing::new(digest))));
    }

    pub(crate) fn set_public_admission_limits(
        &mut self,
        max_in_flight_inference_requests: usize,
        max_in_flight_management_requests: usize,
    ) {
        self.public_admission = request_admission::public::PublicAdmission::new(
            max_in_flight_inference_requests,
            max_in_flight_management_requests,
        );
    }

    /// Installs a prebuilt OpenAI connector for certification and connectivity probes.
    ///
    /// This is an internal dependency-injection seam used by integration tests
    /// to exercise real connector I/O against a local mock without weakening
    /// the production custom-endpoint SSRF policy. Production wiring never
    /// installs an override.
    #[cfg(feature = "test-util")]
    #[doc(hidden)]
    pub fn register_certification_probe_connector_for_test(
        &self,
        provider_id: uuid::Uuid,
        connector: Connector,
    ) {
        self.certification_probe_connectors
            .register(provider_id, connector);
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

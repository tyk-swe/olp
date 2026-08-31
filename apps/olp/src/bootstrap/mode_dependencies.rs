//! Validated, mode-owned runtime state.
//!
//! [`ProcessComposition`] is deliberately only a process-composition builder.  This
//! module consumes its optional inputs and produces the immutable states that
//! Axum handlers are allowed to extract.  Consequently a routed handler can
//! never observe a missing database or authentication key.

use std::{
    path::PathBuf,
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
    },
    time::Instant,
};

use olp_db::{security::envelope::MasterKey, security::key_material::AuthHmacKey, store::Store};
use olp_engine::domain::ports::MediaSpool;
#[cfg(any(test, feature = "test-util"))]
use olp_engine::domain::routing::provider::ProviderKind;
use olp_engine::inference::{
    limits::ReloadableLimiter, request_metadata::Emitter, runtime::Manager, service::Service,
};
use olp_engine::providers::{EgressPolicy, connector::ResponseLimits};

use crate::{
    bootstrap::state::ApiMode,
    bootstrap::state::BodyLimits,
    bootstrap::state::ProcessComposition,
    bootstrap::state::TransportRegistry,
    observability::{
        cache::ObservabilityCache,
        metrics::RequestMetadataLossCounters,
        readiness::{HealthResponse, cached_readiness_from_snapshot},
    },
    public_http::problem::Problem,
    public_http::proxy::TrustedProxyCidr,
    public_http::public_origin::PublicOrigin,
    public_http::request_admission::{multipart::MultipartAdmissionState, public::PublicAdmission},
};

/// Dependencies used before a request reaches either product surface.
/// Control-only mode owns this boundary without acquiring gateway handlers.
#[derive(Clone)]
pub(crate) struct RequestBoundaryState {
    pub(crate) store: Store,
    pub(crate) inference: Arc<Service>,
    pub(crate) auth_hmac_key: Arc<AuthHmacKey>,
    pub(crate) multipart_admission: MultipartAdmissionState,
    pub(crate) public_admission: PublicAdmission,
    pub(crate) public_origin: PublicOrigin,
    pub(crate) body_limits: BodyLimits,
    trusted_proxy_cidrs: Arc<[TrustedProxyCidr]>,
    bootstrap_token_digest: Arc<tokio::sync::RwLock<Option<zeroize::Zeroizing<[u8; 32]>>>>,
}

impl RequestBoundaryState {
    #[must_use]
    pub(crate) const fn store(&self) -> &Store {
        &self.store
    }

    pub(crate) async fn verify_bootstrap_token(&self, supplied: Option<&str>) -> Option<bool> {
        let digest = self.bootstrap_token_digest.read().await;
        let expected = digest.as_ref()?;
        Some(supplied.is_some_and(|supplied| {
            self.auth_hmac_key
                .verify_bootstrap_token_digest(supplied, expected)
        }))
    }

    pub(crate) async fn clear_bootstrap_token(&self) {
        let mut digest = self.bootstrap_token_digest.write().await;
        *digest = None;
    }

    #[must_use]
    pub(crate) fn peer_is_trusted_proxy(&self, peer: std::net::IpAddr) -> bool {
        self.trusted_proxy_cidrs
            .iter()
            .any(|cidr| cidr.contains(peer))
    }

    #[must_use]
    pub(crate) fn trusted_proxies_configured(&self) -> bool {
        !self.trusted_proxy_cidrs.is_empty()
    }
}

/// Gateway HTTP dependencies plus the shared inference service.
#[derive(Clone)]
pub struct GatewayState {
    pub(crate) request_boundary: RequestBoundaryState,
    pub(crate) cors_allowed_origins: Arc<[axum::http::HeaderValue]>,
    pub(crate) transports: TransportRegistry,
    pub(crate) master_key: Option<Arc<MasterKey>>,
    pub(crate) provider_egress_policy: Arc<EgressPolicy>,
    pub(crate) provider_response_limits: ResponseLimits,
    media_reconciliation_gaps: Arc<AtomicU64>,
}

impl GatewayState {
    #[cfg(test)]
    pub(crate) fn new(
        mode: ApiMode,
        store: Option<Store>,
        runtime: Arc<Manager>,
        public_origin: impl AsRef<str>,
        console_dir: impl Into<PathBuf>,
    ) -> Self {
        let store = store.unwrap_or_else(test_store);
        let mut builder = ProcessComposition::new(mode, store, runtime, public_origin, console_dir);
        builder.mode = ApiMode::All;
        builder.auth_hmac_key = Arc::new(AuthHmacKey::new([0xA5; 32]));
        match builder.mode_dependencies() {
            ModeDependencies::All { gateway, .. } | ModeDependencies::Gateway { gateway, .. } => {
                *gateway
            }
            ModeDependencies::Control { .. } => unreachable!("test builder uses all mode"),
        }
    }

    #[must_use]
    pub fn store(&self) -> &Store {
        &self.request_boundary.store
    }

    #[must_use]
    #[cfg(any(test, feature = "test-util"))]
    pub fn runtime(&self) -> &Manager {
        self.inference().runtime()
    }

    #[must_use]
    pub(crate) fn inference(&self) -> &Service {
        &self.request_boundary.inference
    }

    #[must_use]
    #[cfg(test)]
    pub(crate) fn limiter(&self) -> &ReloadableLimiter {
        self.inference().limiter()
    }

    #[must_use]
    pub(crate) fn circuits(&self) -> &olp_engine::inference::circuit::Breaker {
        self.inference().circuits()
    }

    #[must_use]
    pub(crate) fn media_spool(&self) -> &Arc<dyn MediaSpool> {
        self.inference().media_spool()
    }

    #[must_use]
    pub(crate) const fn body_limits(&self) -> BodyLimits {
        self.request_boundary.body_limits
    }

    #[must_use]
    #[cfg(test)]
    pub(crate) fn request_boundary(&self) -> &RequestBoundaryState {
        &self.request_boundary
    }

    #[must_use]
    #[cfg(test)]
    pub(crate) fn auth_hmac_key(&self) -> &Arc<AuthHmacKey> {
        &self.request_boundary.auth_hmac_key
    }

    #[cfg(test)]
    pub(crate) async fn verify_bootstrap_token(&self, supplied: Option<&str>) -> Option<bool> {
        self.request_boundary.verify_bootstrap_token(supplied).await
    }

    #[cfg(test)]
    pub(crate) async fn clear_bootstrap_token(&self) {
        self.request_boundary.clear_bootstrap_token().await;
    }

    pub(crate) fn record_media_reconciliation_gap(&self) {
        let _ = self.media_reconciliation_gaps.fetch_update(
            Ordering::Relaxed,
            Ordering::Relaxed,
            |value| Some(value.saturating_add(1)),
        );
    }

    #[cfg(test)]
    pub(crate) fn replace_request_metadata_for_test(&mut self, emitter: Emitter) {
        Arc::make_mut(&mut self.request_boundary.inference).replace_request_metadata(Some(emitter));
    }

    #[cfg(test)]
    pub(crate) fn replace_media_spool_for_test(&mut self, media_spool: Arc<dyn MediaSpool>) {
        Arc::make_mut(&mut self.request_boundary.inference).replace_media_spool(media_spool);
    }

    #[cfg(test)]
    pub(crate) fn replace_auth_hmac_key_for_test(&mut self, auth_hmac_key: Arc<AuthHmacKey>) {
        self.request_boundary.auth_hmac_key = auth_hmac_key;
    }
}

/// Control-plane dependencies plus the explicitly shared inference service
/// used by the authenticated playground.
#[derive(Clone)]
pub struct ManagementState {
    request_boundary: RequestBoundaryState,
    pub(crate) transports: TransportRegistry,
    pub(crate) master_key: Option<Arc<MasterKey>>,
    pub(crate) provider_egress_policy: Arc<EgressPolicy>,
    pub(crate) provider_response_limits: ResponseLimits,
    #[cfg(any(test, feature = "test-util"))]
    certification_probe_connectors: olp_engine::providers::factory::overrides::Registry,
    pub(crate) public_origin: PublicOrigin,
    pub(crate) console_dir: Arc<PathBuf>,
    pub(crate) session_ttl: chrono::Duration,
    pub(crate) local_login_enabled: bool,
    pub(crate) oidc_allow_insecure_test_endpoints: bool,
    observability: ObservabilityCache,
}

impl ManagementState {
    #[cfg(test)]
    pub(crate) fn new(
        mode: ApiMode,
        store: Option<Store>,
        runtime: Arc<Manager>,
        public_origin: impl AsRef<str>,
        console_dir: impl Into<PathBuf>,
    ) -> Self {
        let store = store.unwrap_or_else(test_store);
        let mut builder = ProcessComposition::new(mode, store, runtime, public_origin, console_dir);
        builder.mode = ApiMode::All;
        builder.auth_hmac_key = Arc::new(AuthHmacKey::new([0xA5; 32]));
        match builder.mode_dependencies() {
            ModeDependencies::All { management, .. }
            | ModeDependencies::Control { management, .. } => *management,
            ModeDependencies::Gateway { .. } => unreachable!("test builder uses all mode"),
        }
    }

    #[must_use]
    pub fn store(&self) -> &Store {
        &self.request_boundary.store
    }

    #[must_use]
    pub(crate) fn inference(&self) -> &Service {
        &self.request_boundary.inference
    }

    #[must_use]
    pub(crate) fn request_boundary(&self) -> &RequestBoundaryState {
        &self.request_boundary
    }

    #[must_use]
    pub(crate) fn auth_hmac_key(&self) -> &AuthHmacKey {
        &self.request_boundary.auth_hmac_key
    }

    pub(crate) async fn clear_bootstrap_token(&self) {
        self.request_boundary.clear_bootstrap_token().await;
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(crate) fn certification_probe_connector(
        &self,
        provider_id: uuid::Uuid,
        kind: ProviderKind,
    ) -> Option<olp_engine::providers::factory::assembly::Facade> {
        self.certification_probe_connectors.get(provider_id, kind)
    }

    pub(crate) fn cached_readiness(&self) -> Result<HealthResponse, Problem> {
        let snapshot = self.observability.readiness();
        cached_readiness_from_snapshot(&snapshot, Instant::now())
    }
}

/// State installed only on the separately bound private listener.
#[derive(Clone)]
pub struct ObservabilityState {
    store: Store,
    inference: Arc<Service>,
    pub(crate) public_admission: PublicAdmission,
    media_reconciliation_gaps: Arc<AtomicU64>,
    request_metadata_loss: RequestMetadataLossCounters,
    pub(crate) mode: ApiMode,
    pub(crate) observability: ObservabilityCache,
}

impl ObservabilityState {
    #[must_use]
    pub(crate) const fn store(&self) -> &Store {
        &self.store
    }

    #[must_use]
    pub(crate) fn runtime(&self) -> &Manager {
        self.inference.runtime()
    }

    #[must_use]
    pub(crate) fn limiter(&self) -> &ReloadableLimiter {
        self.inference.limiter()
    }

    #[must_use]
    pub(crate) fn circuits(&self) -> &olp_engine::inference::circuit::Breaker {
        self.inference.circuits()
    }

    #[must_use]
    pub(crate) fn request_metadata(&self) -> Option<&Emitter> {
        self.inference.request_metadata()
    }

    #[must_use]
    pub(crate) fn media_reconciliation_gap_count(&self) -> u64 {
        self.media_reconciliation_gaps.load(Ordering::Relaxed)
    }

    #[must_use]
    pub(crate) fn media_spool(&self) -> &Arc<dyn MediaSpool> {
        self.inference.media_spool()
    }

    #[must_use]
    pub(crate) const fn request_metadata_loss_counters(&self) -> &RequestMetadataLossCounters {
        &self.request_metadata_loss
    }
}

/// Fully validated state for one process mode.  Router composition consumes
/// this value so the proof cannot be accidentally discarded.
#[derive(Clone)]
pub enum ModeDependencies {
    All {
        gateway: Box<GatewayState>,
        management: Box<ManagementState>,
        observability: ObservabilityState,
    },
    Gateway {
        gateway: Box<GatewayState>,
        observability: ObservabilityState,
    },
    Control {
        management: Box<ManagementState>,
        observability: ObservabilityState,
    },
}

impl ModeDependencies {
    #[must_use]
    pub fn observability(&self) -> ObservabilityState {
        match self {
            Self::All { observability, .. }
            | Self::Gateway { observability, .. }
            | Self::Control { observability, .. } => observability.clone(),
        }
    }

    #[must_use]
    pub fn gateway(&self) -> Option<GatewayState> {
        match self {
            Self::All { gateway, .. } | Self::Gateway { gateway, .. } => {
                Some(gateway.as_ref().clone())
            }
            Self::Control { .. } => None,
        }
    }

    #[must_use]
    #[cfg(feature = "test-util")]
    pub fn management(&self) -> Option<ManagementState> {
        match self {
            Self::All { management, .. } | Self::Control { management, .. } => {
                Some(management.as_ref().clone())
            }
            Self::Gateway { .. } => None,
        }
    }
}

impl std::fmt::Display for ApiMode {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::All => formatter.write_str("all"),
            Self::Gateway => formatter.write_str("gateway"),
            Self::Control => formatter.write_str("control"),
        }
    }
}

impl ProcessComposition {
    pub fn mode_dependencies(&self) -> ModeDependencies {
        let store = self.store.clone();
        let auth_hmac_key = self.auth_hmac_key.clone();
        let inference = Arc::new(
            Service::new(
                Arc::clone(&self.runtime),
                self.limiter.clone(),
                self.request_metadata.clone(),
                self.circuits.clone(),
                Arc::clone(&self.media_spool),
            )
            .with_max_inline_media_bytes(self.body_limits.inline_media_item_bytes)
            .with_max_collected_event_bytes(self.provider_response_limits.max_response_bytes),
        );
        let request_boundary = RequestBoundaryState {
            store: store.clone(),
            inference: Arc::clone(&inference),
            auth_hmac_key: Arc::clone(&auth_hmac_key),
            multipart_admission: self.multipart_admission.clone(),
            public_admission: self.public_admission.clone(),
            public_origin: self.public_origin.clone(),
            body_limits: self.body_limits,
            trusted_proxy_cidrs: Arc::clone(&self.trusted_proxy_cidrs),
            bootstrap_token_digest: Arc::clone(&self.bootstrap_token_digest),
        };
        let gateway = GatewayState {
            request_boundary: request_boundary.clone(),
            cors_allowed_origins: Arc::clone(&self.gateway_cors_allowed_origins),
            transports: self.transports.clone(),
            master_key: self.master_key.clone(),
            provider_egress_policy: Arc::clone(&self.provider_egress_policy),
            provider_response_limits: self.provider_response_limits,
            media_reconciliation_gaps: Arc::clone(&self.media_reconciliation_gaps),
        };
        let management = ManagementState {
            request_boundary: request_boundary.clone(),
            transports: self.transports.clone(),
            master_key: self.master_key.clone(),
            provider_egress_policy: Arc::clone(&self.provider_egress_policy),
            provider_response_limits: self.provider_response_limits,
            #[cfg(any(test, feature = "test-util"))]
            certification_probe_connectors: self.certification_probe_connectors.clone(),
            public_origin: self.public_origin.clone(),
            console_dir: Arc::clone(&self.console_dir),
            session_ttl: self.session_ttl,
            local_login_enabled: self.local_login_enabled,
            oidc_allow_insecure_test_endpoints: self.oidc_allow_insecure_test_endpoints,
            observability: self.observability.clone(),
        };
        let observability = ObservabilityState {
            store,
            inference,
            public_admission: request_boundary.public_admission.clone(),
            media_reconciliation_gaps: Arc::clone(&self.media_reconciliation_gaps),
            request_metadata_loss: self.request_metadata_loss.clone(),
            mode: self.mode,
            observability: self.observability.clone(),
        };
        match self.mode {
            ApiMode::All => ModeDependencies::All {
                gateway: Box::new(gateway),
                management: Box::new(management),
                observability,
            },
            ApiMode::Gateway => ModeDependencies::Gateway {
                gateway: Box::new(gateway),
                observability,
            },
            ApiMode::Control => ModeDependencies::Control {
                management: Box::new(management),
                observability,
            },
        }
    }

    #[cfg(test)]
    pub(crate) fn gateway_state_for_test(&self) -> GatewayState {
        let mut builder = self.clone();
        builder.mode = ApiMode::All;
        test_dependencies(&builder).gateway_state_for_test()
    }

    #[cfg(test)]
    pub(crate) fn management_state_for_test(&self) -> ManagementState {
        let mut builder = self.clone();
        builder.mode = ApiMode::All;
        match test_dependencies(&builder) {
            ModeDependencies::All { management, .. }
            | ModeDependencies::Control { management, .. } => *management,
            ModeDependencies::Gateway { .. } => unreachable!("test builder uses all mode"),
        }
    }

    #[cfg(test)]
    pub(crate) fn observability_state_for_test(&self) -> ObservabilityState {
        test_dependencies(self).observability()
    }
}

#[cfg(test)]
impl ModeDependencies {
    fn gateway_state_for_test(self) -> GatewayState {
        match self {
            Self::All { gateway, .. } | Self::Gateway { gateway, .. } => *gateway,
            Self::Control { .. } => unreachable!("test builder uses all mode"),
        }
    }
}

#[cfg(test)]
fn test_dependencies(state: &ProcessComposition) -> ModeDependencies {
    state.mode_dependencies()
}

/// A lazily connecting store for compositions under unit test; nothing
/// touches PostgreSQL unless a test actually issues a query.
#[cfg(test)]
pub(crate) fn test_store() -> Store {
    static TEST_RUNTIME: std::sync::OnceLock<tokio::runtime::Runtime> = std::sync::OnceLock::new();
    static TEST_STORE: std::sync::OnceLock<Store> = std::sync::OnceLock::new();

    TEST_STORE
        .get_or_init(|| {
            let runtime = TEST_RUNTIME.get_or_init(|| {
                tokio::runtime::Builder::new_multi_thread()
                    .worker_threads(1)
                    .enable_all()
                    .build()
                    .expect("test runtime must be constructible")
            });
            let _runtime_guard = runtime.enter();
            let pool = sqlx::postgres::PgPoolOptions::new()
                .max_connections(1)
                .acquire_timeout(std::time::Duration::from_millis(10))
                .connect_lazy("postgres://olp:olp@127.0.0.1/olp")
                .expect("test PostgreSQL URL is valid");
            Store::from_pool(pool)
        })
        .clone()
}

#[cfg(test)]
mod tests {
    use std::{path::PathBuf, sync::Arc};

    use super::*;

    fn state(mode: ApiMode) -> ProcessComposition {
        ProcessComposition::new(
            mode,
            test_store(),
            Arc::new(Manager::empty()),
            "https://olp.example.test",
            PathBuf::from("missing-console"),
        )
    }

    #[test]
    fn fully_composed_modes_produce_only_their_owned_surfaces() {
        assert!(matches!(
            state(ApiMode::All).mode_dependencies(),
            ModeDependencies::All { .. }
        ));
        assert!(matches!(
            state(ApiMode::Control).mode_dependencies(),
            ModeDependencies::Control { .. }
        ));
        assert!(matches!(
            state(ApiMode::Gateway).mode_dependencies(),
            ModeDependencies::Gateway { .. }
        ));
    }

    #[test]
    fn inline_media_item_limit_reaches_inference_connectors() {
        let mut state = state(ApiMode::Gateway);
        state.body_limits.inline_media_item_bytes = 2 * 1024 * 1024;

        let gateway = state.mode_dependencies().gateway_state_for_test();

        assert_eq!(
            gateway.inference().max_inline_media_bytes(),
            2 * 1024 * 1024
        );
    }
}

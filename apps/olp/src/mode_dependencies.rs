//! Validated, mode-owned runtime state.
//!
//! [`ApiState`] is deliberately only a process-composition builder.  This
//! module consumes its optional inputs and produces the immutable states that
//! Axum handlers are allowed to extract.  Consequently a routed handler can
//! never observe a missing database or authentication key.

use std::{
    ops::Deref,
    path::PathBuf,
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
    },
    time::Instant,
};

use olp_domain::{MediaSpool, ProviderKind};
use olp_storage::{AuthHmacKey, MasterKey, PgStore, RequestMetadataEmitter};
use thiserror::Error;

use crate::{
    ApiMode, ApiState, HealthResponse, Problem, PublicOrigin, ReloadableLimiter, RuntimeManager,
    TransportRegistry, TrustedProxyCidr,
    circuit::CircuitBreaker,
    observability::{ObservabilityCache, cached_readiness_from_snapshot},
    request_admission::MultipartAdmissionState,
};

/// Inference and request-boundary capabilities shared by gateway endpoints.
/// Every field is required by at least one route on the gateway surface.
#[derive(Clone)]
pub struct GatewayState {
    pub(crate) store: PgStore,
    pub(crate) runtime: Arc<RuntimeManager>,
    pub(crate) limiter: ReloadableLimiter,
    pub(crate) auth_hmac_key: Arc<AuthHmacKey>,
    pub(crate) request_metadata: Option<RequestMetadataEmitter>,
    pub(crate) circuits: CircuitBreaker,
    pub(crate) media_spool: Arc<dyn MediaSpool>,
    pub(crate) multipart_admission: MultipartAdmissionState,
    pub(crate) transports: TransportRegistry,
    pub(crate) public_origin: PublicOrigin,
    trusted_proxy_cidrs: Arc<[TrustedProxyCidr]>,
    bootstrap_token_digest: Arc<tokio::sync::RwLock<Option<zeroize::Zeroizing<[u8; 32]>>>>,
    media_reconciliation_gaps: Arc<AtomicU64>,
}

impl GatewayState {
    #[cfg(test)]
    pub(crate) fn new(
        mode: ApiMode,
        store: Option<PgStore>,
        runtime: Arc<RuntimeManager>,
        public_origin: impl AsRef<str>,
        console_dir: impl Into<PathBuf>,
    ) -> Self {
        let store = store.unwrap_or_else(test_store);
        let mut builder = ApiState::new(mode, Some(store), runtime, public_origin, console_dir);
        builder.auth_hmac_key = Some(Arc::new(AuthHmacKey::new([0xA5; 32])));
        match builder.mode_dependencies() {
            Ok(ModeDependencies::All { gateway, .. })
            | Ok(ModeDependencies::Gateway { gateway, .. }) => *gateway,
            Ok(ModeDependencies::Control { management, .. }) => management.gateway_state(),
            Err(error) => panic!("test state must be valid: {error}"),
        }
    }

    #[must_use]
    pub fn store(&self) -> &PgStore {
        &self.store
    }

    #[must_use]
    pub fn runtime(&self) -> &RuntimeManager {
        &self.runtime
    }

    #[must_use]
    pub fn transports(&self) -> &TransportRegistry {
        &self.transports
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

    pub(crate) fn record_media_reconciliation_gap(&self) {
        let _ = self.media_reconciliation_gaps.fetch_update(
            Ordering::Relaxed,
            Ordering::Relaxed,
            |value| Some(value.saturating_add(1)),
        );
    }

    pub(crate) fn media_reconciliation_gap_count(&self) -> u64 {
        self.media_reconciliation_gaps.load(Ordering::Relaxed)
    }
}

/// Control-plane capabilities.  The embedded gateway capabilities are needed
/// by the authenticated playground; no gateway HTTP routes are implied.
#[derive(Clone)]
pub struct ManagementState {
    gateway: GatewayState,
    pub(crate) master_key: Option<Arc<MasterKey>>,
    certification_probe_connectors: olp_providers::OpenAiConnectorOverrideRegistry,
    pub(crate) public_origin: PublicOrigin,
    pub(crate) console_dir: Arc<PathBuf>,
    pub(crate) session_ttl: chrono::Duration,
    pub(crate) local_login_enabled: bool,
    pub(crate) oidc_allow_insecure_test_endpoints: bool,
    observability: ObservabilityCache,
}

impl Deref for ManagementState {
    type Target = GatewayState;

    fn deref(&self) -> &Self::Target {
        &self.gateway
    }
}

impl ManagementState {
    #[cfg(test)]
    pub(crate) fn new(
        mode: ApiMode,
        store: Option<PgStore>,
        runtime: Arc<RuntimeManager>,
        public_origin: impl AsRef<str>,
        console_dir: impl Into<PathBuf>,
    ) -> Self {
        let store = store.unwrap_or_else(test_store);
        let mut builder = ApiState::new(mode, Some(store), runtime, public_origin, console_dir);
        builder.auth_hmac_key = Some(Arc::new(AuthHmacKey::new([0xA5; 32])));
        match builder.mode_dependencies() {
            Ok(ModeDependencies::All { management, .. })
            | Ok(ModeDependencies::Control { management, .. }) => *management,
            Ok(ModeDependencies::Gateway { gateway, .. }) => Self {
                gateway: *gateway,
                master_key: builder.master_key.clone(),
                certification_probe_connectors: builder.certification_probe_connectors.clone(),
                public_origin: builder.public_origin.clone(),
                console_dir: Arc::clone(&builder.console_dir),
                session_ttl: builder.session_ttl,
                local_login_enabled: builder.local_login_enabled,
                oidc_allow_insecure_test_endpoints: builder.oidc_allow_insecure_test_endpoints,
                observability: builder.observability.clone(),
            },
            Err(error) => panic!("test state must be valid: {error}"),
        }
    }

    #[must_use]
    pub(crate) fn gateway_state(&self) -> GatewayState {
        self.gateway.clone()
    }

    pub(crate) fn certification_probe_connector(
        &self,
        provider_id: uuid::Uuid,
        kind: ProviderKind,
    ) -> Option<olp_providers::ProviderFacade> {
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
    gateway: GatewayState,
    pub(crate) mode: ApiMode,
    pub(crate) observability: ObservabilityCache,
}

impl Deref for ObservabilityState {
    type Target = GatewayState;

    fn deref(&self) -> &Self::Target {
        &self.gateway
    }
}

#[derive(Clone)]
pub struct ConfigurationDependencies {
    pub(crate) store: PgStore,
    pub(crate) master_key: Option<Arc<MasterKey>>,
}

impl ConfigurationDependencies {
    #[must_use]
    pub fn store(&self) -> &PgStore {
        &self.store
    }

    #[must_use]
    pub fn master_key(&self) -> Option<&MasterKey> {
        self.master_key.as_deref()
    }
}

#[derive(Clone)]
pub struct IdentityDependencies {
    pub(crate) store: PgStore,
    pub(crate) auth_hmac_key: Arc<AuthHmacKey>,
}

impl IdentityDependencies {
    #[must_use]
    pub fn store(&self) -> &PgStore {
        &self.store
    }

    #[must_use]
    pub fn auth_hmac_key(&self) -> &AuthHmacKey {
        &self.auth_hmac_key
    }
}

#[derive(Clone)]
pub struct InferenceDependencies {
    pub(crate) store: PgStore,
    pub(crate) runtime: Arc<RuntimeManager>,
    pub(crate) transports: TransportRegistry,
    pub(crate) auth_hmac_key: Arc<AuthHmacKey>,
}

impl InferenceDependencies {
    #[must_use]
    pub fn store(&self) -> &PgStore {
        &self.store
    }

    #[must_use]
    pub fn runtime(&self) -> &RuntimeManager {
        &self.runtime
    }

    #[must_use]
    pub fn transports(&self) -> &TransportRegistry {
        &self.transports
    }

    #[must_use]
    pub fn auth_hmac_key(&self) -> &AuthHmacKey {
        &self.auth_hmac_key
    }
}

#[derive(Clone)]
pub struct OperationsDependencies {
    pub(crate) store: PgStore,
}

impl OperationsDependencies {
    #[must_use]
    pub fn store(&self) -> &PgStore {
        &self.store
    }
}

#[derive(Clone)]
pub struct WorkerDependencies {
    pub(crate) store: PgStore,
}

impl WorkerDependencies {
    #[must_use]
    pub fn new(store: PgStore) -> Self {
        Self { store }
    }

    #[must_use]
    pub fn store(&self) -> &PgStore {
        &self.store
    }
}

/// Fully validated state for one process mode.  Router composition consumes
/// this value so the proof cannot be accidentally discarded.
#[derive(Clone)]
pub enum ModeDependencies {
    All {
        configuration: ConfigurationDependencies,
        identity: IdentityDependencies,
        inference: InferenceDependencies,
        operations: OperationsDependencies,
        gateway: Box<GatewayState>,
        management: Box<ManagementState>,
        observability: ObservabilityState,
    },
    Gateway {
        inference: InferenceDependencies,
        gateway: Box<GatewayState>,
        observability: ObservabilityState,
    },
    Control {
        configuration: ConfigurationDependencies,
        identity: IdentityDependencies,
        operations: OperationsDependencies,
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
    pub fn management(&self) -> Option<ManagementState> {
        match self {
            Self::All { management, .. } | Self::Control { management, .. } => {
                Some(management.as_ref().clone())
            }
            Self::Gateway { .. } => None,
        }
    }
}

#[derive(Clone, Debug, Error, Eq, PartialEq)]
pub enum ModeDependencyError {
    #[error("{0} mode requires PostgreSQL storage")]
    MissingStorage(ApiMode),
    #[error("{0} mode requires the authentication HMAC key")]
    MissingAuthHmacKey(ApiMode),
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

impl ApiState {
    pub fn mode_dependencies(&self) -> Result<ModeDependencies, ModeDependencyError> {
        let store = self
            .store
            .clone()
            .ok_or(ModeDependencyError::MissingStorage(self.mode))?;
        let auth_hmac_key = self
            .auth_hmac_key
            .clone()
            .ok_or(ModeDependencyError::MissingAuthHmacKey(self.mode))?;
        let gateway = GatewayState {
            store: store.clone(),
            runtime: Arc::clone(&self.runtime),
            limiter: self.limiter.clone(),
            auth_hmac_key: Arc::clone(&auth_hmac_key),
            request_metadata: self.request_metadata.clone(),
            circuits: self.circuits.clone(),
            media_spool: Arc::clone(&self.media_spool),
            multipart_admission: self.multipart_admission.clone(),
            transports: self.transports.clone(),
            public_origin: self.public_origin.clone(),
            trusted_proxy_cidrs: Arc::clone(&self.trusted_proxy_cidrs),
            bootstrap_token_digest: Arc::clone(&self.bootstrap_token_digest),
            media_reconciliation_gaps: Arc::clone(&self.media_reconciliation_gaps),
        };
        let management = ManagementState {
            gateway: gateway.clone(),
            master_key: self.master_key.clone(),
            certification_probe_connectors: self.certification_probe_connectors.clone(),
            public_origin: self.public_origin.clone(),
            console_dir: Arc::clone(&self.console_dir),
            session_ttl: self.session_ttl,
            local_login_enabled: self.local_login_enabled,
            oidc_allow_insecure_test_endpoints: self.oidc_allow_insecure_test_endpoints,
            observability: self.observability.clone(),
        };
        let observability = ObservabilityState {
            gateway: gateway.clone(),
            mode: self.mode,
            observability: self.observability.clone(),
        };
        let inference = InferenceDependencies {
            store: store.clone(),
            runtime: Arc::clone(&self.runtime),
            transports: self.transports.clone(),
            auth_hmac_key: Arc::clone(&auth_hmac_key),
        };
        let configuration = ConfigurationDependencies {
            store: store.clone(),
            master_key: self.master_key.clone(),
        };
        let identity = IdentityDependencies {
            store: store.clone(),
            auth_hmac_key,
        };
        let operations = OperationsDependencies { store };
        match self.mode {
            ApiMode::All => Ok(ModeDependencies::All {
                configuration,
                identity,
                inference,
                operations,
                gateway: Box::new(gateway),
                management: Box::new(management),
                observability,
            }),
            ApiMode::Gateway => Ok(ModeDependencies::Gateway {
                inference,
                gateway: Box::new(gateway),
                observability,
            }),
            ApiMode::Control => Ok(ModeDependencies::Control {
                configuration,
                identity,
                operations,
                management: Box::new(management),
                observability,
            }),
        }
    }

    #[cfg(test)]
    pub(crate) fn gateway_state_for_test(&self) -> GatewayState {
        test_dependencies(self).gateway_state_for_test()
    }

    #[cfg(test)]
    pub(crate) fn management_state_for_test(&self) -> ManagementState {
        match test_dependencies(self) {
            ModeDependencies::All { management, .. }
            | ModeDependencies::Control { management, .. } => *management,
            ModeDependencies::Gateway { gateway, .. } => ManagementState {
                gateway: *gateway,
                master_key: self.master_key.clone(),
                certification_probe_connectors: self.certification_probe_connectors.clone(),
                public_origin: self.public_origin.clone(),
                console_dir: Arc::clone(&self.console_dir),
                session_ttl: self.session_ttl,
                local_login_enabled: self.local_login_enabled,
                oidc_allow_insecure_test_endpoints: self.oidc_allow_insecure_test_endpoints,
                observability: self.observability.clone(),
            },
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
            Self::Control { management, .. } => management.gateway_state(),
        }
    }
}

#[cfg(test)]
fn test_dependencies(state: &ApiState) -> ModeDependencies {
    let mut builder = state.clone();
    if builder.store.is_none() {
        builder.store = Some(test_store());
    }
    if builder.auth_hmac_key.is_none() {
        builder.auth_hmac_key = Some(Arc::new(AuthHmacKey::new([0xA5; 32])));
    }
    match builder.mode_dependencies() {
        Ok(dependencies) => dependencies,
        Err(error) => panic!("test state must be valid: {error}"),
    }
}

#[cfg(test)]
fn test_store() -> PgStore {
    static TEST_RUNTIME: std::sync::OnceLock<tokio::runtime::Runtime> = std::sync::OnceLock::new();
    static TEST_STORE: std::sync::OnceLock<PgStore> = std::sync::OnceLock::new();

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
            PgStore::from_pool(pool)
        })
        .clone()
}

#[cfg(test)]
mod tests {
    use std::{path::PathBuf, sync::Arc};

    use super::*;

    fn state(mode: ApiMode, with_store: bool, with_auth_hmac_key: bool) -> ApiState {
        let store = with_store.then(test_store);
        let mut state = ApiState::new(
            mode,
            store,
            Arc::new(RuntimeManager::empty()),
            "https://olp.example.test",
            PathBuf::from("missing-console"),
        );
        if with_auth_hmac_key {
            state.auth_hmac_key = Some(Arc::new(AuthHmacKey::new([7; 32])));
        }
        state
    }

    #[test]
    fn every_http_mode_rejects_missing_storage_at_startup() {
        for mode in [ApiMode::All, ApiMode::Gateway, ApiMode::Control] {
            assert_eq!(
                state(mode, false, false).mode_dependencies().err(),
                Some(ModeDependencyError::MissingStorage(mode))
            );
        }
    }

    #[test]
    fn every_http_mode_rejects_missing_authentication_key() {
        for mode in [ApiMode::All, ApiMode::Gateway, ApiMode::Control] {
            assert_eq!(
                state(mode, true, false).mode_dependencies().err(),
                Some(ModeDependencyError::MissingAuthHmacKey(mode))
            );
        }
    }

    #[test]
    fn fully_composed_modes_produce_only_their_owned_surfaces() {
        assert!(matches!(
            state(ApiMode::All, true, true).mode_dependencies(),
            Ok(ModeDependencies::All { .. })
        ));
        assert!(matches!(
            state(ApiMode::Control, true, true).mode_dependencies(),
            Ok(ModeDependencies::Control { .. })
        ));
        assert!(matches!(
            state(ApiMode::Gateway, true, true).mode_dependencies(),
            Ok(ModeDependencies::Gateway { .. })
        ));
    }
}

//! Validated, mode-owned runtime state.
//!
//! [`ApiState`] is deliberately only a process-composition builder.  This
//! module consumes its optional inputs and produces the immutable states that
//! Axum handlers are allowed to extract.  Consequently a routed handler can
//! never observe a missing database or authentication key.

use std::{
    collections::BTreeMap,
    ops::Deref,
    path::PathBuf,
    sync::{
        Arc, RwLock,
        atomic::{AtomicU64, Ordering},
    },
    time::{Duration, Instant},
};

use jsonwebtoken::jwk::JwkSet;
use olp_domain::{MediaSpool, ProviderKind};
use olp_storage::{AuthHmacKey, MasterKey, PgStore, RequestMetadataEmitter};
use thiserror::Error;

use crate::{
    ApiMode, ApiState, HealthResponse, Problem, PublicOrigin, ReloadableLimiter, RuntimeManager,
    TransportRegistry, TrustedProxyCidr,
    circuit::CircuitBreaker,
    observability::{ObservabilityCache, cached_readiness_from_snapshot},
    request_admission::{MultipartAdmissionState, PublicAdmission},
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
    pub(crate) media_job_journal: Option<Arc<crate::media_job_journal::MediaJobJournal>>,
    pub(crate) media_spool: Arc<dyn MediaSpool>,
    pub(crate) multipart_admission: MultipartAdmissionState,
    pub(crate) public_admission: PublicAdmission,
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

    #[must_use]
    pub(crate) fn trusted_proxies_configured(&self) -> bool {
        !self.trusted_proxy_cidrs.is_empty()
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
    oidc_jwks_cache: OidcJwksCache,
    observability: ObservabilityCache,
}

const OIDC_JWKS_STALE_TTL: Duration = Duration::from_secs(60 * 60);
const OIDC_JWKS_CACHE_ENTRIES: usize = 4;

#[derive(Clone, Default)]
struct OidcJwksCache {
    inner: Arc<RwLock<BTreeMap<(uuid::Uuid, uuid::Uuid), CachedOidcJwks>>>,
}

struct CachedOidcJwks {
    callback_started_at: Instant,
    refresh_started_at: Instant,
    jwks: JwkSet,
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
                oidc_jwks_cache: OidcJwksCache::default(),
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

    pub(crate) fn cache_oidc_jwks(
        &self,
        configuration_id: uuid::Uuid,
        configuration_etag: uuid::Uuid,
        callback_started_at: Instant,
        refresh_started_at: Instant,
        jwks: JwkSet,
    ) {
        if refresh_started_at.elapsed() > OIDC_JWKS_STALE_TTL {
            return;
        }
        let mut cache = self
            .oidc_jwks_cache
            .inner
            .write()
            .expect("OIDC JWKS cache lock poisoned");
        cache.retain(|_, cached| cached.refresh_started_at.elapsed() <= OIDC_JWKS_STALE_TTL);
        let key = (configuration_id, configuration_etag);
        if cache
            .get(&key)
            .is_some_and(|cached| cached.refresh_started_at >= refresh_started_at)
        {
            return;
        }
        if !cache.contains_key(&key)
            && cache.len() >= OIDC_JWKS_CACHE_ENTRIES
            && let Some(oldest) = cache
                .iter()
                .min_by_key(|(_, cached)| cached.callback_started_at)
                .map(|(key, cached)| (*key, cached.callback_started_at))
        {
            if oldest.1 >= callback_started_at {
                return;
            }
            cache.remove(&oldest.0);
        }
        cache.insert(
            key,
            CachedOidcJwks {
                callback_started_at,
                refresh_started_at,
                jwks,
            },
        );
    }

    pub(crate) fn cached_oidc_jwks(
        &self,
        configuration_id: uuid::Uuid,
        configuration_etag: uuid::Uuid,
    ) -> Option<JwkSet> {
        let cache = self
            .oidc_jwks_cache
            .inner
            .read()
            .expect("OIDC JWKS cache lock poisoned");
        cache
            .get(&(configuration_id, configuration_etag))
            .filter(|cached| cached.refresh_started_at.elapsed() <= OIDC_JWKS_STALE_TTL)
            .map(|cached| cached.jwks.clone())
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
            media_job_journal: self.media_job_journal.clone(),
            media_spool: Arc::clone(&self.media_spool),
            multipart_admission: self.multipart_admission.clone(),
            public_admission: self.public_admission.clone(),
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
            oidc_jwks_cache: OidcJwksCache::default(),
            observability: self.observability.clone(),
        };
        let observability = ObservabilityState {
            gateway: gateway.clone(),
            mode: self.mode,
            observability: self.observability.clone(),
        };
        match self.mode {
            ApiMode::All => Ok(ModeDependencies::All {
                gateway: Box::new(gateway),
                management: Box::new(management),
                observability,
            }),
            ApiMode::Gateway => Ok(ModeDependencies::Gateway {
                gateway: Box::new(gateway),
                observability,
            }),
            ApiMode::Control => Ok(ModeDependencies::Control {
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
                oidc_jwks_cache: OidcJwksCache::default(),
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

    #[test]
    fn oidc_jwks_cache_is_shared_bounded_and_monotonic_per_configuration_etag() {
        let dependencies = state(ApiMode::Control, true, true)
            .mode_dependencies()
            .unwrap();
        let ModeDependencies::Control { management, .. } = dependencies else {
            unreachable!()
        };
        let configuration_id = uuid::Uuid::now_v7();
        let configuration_etag = uuid::Uuid::now_v7();
        let now = Instant::now();
        let earlier_callback = now - Duration::from_secs(4);
        let later_callback = now - Duration::from_secs(3);
        let older_refresh = now - Duration::from_secs(2);
        let newer_refresh = now - Duration::from_secs(1);
        management.cache_oidc_jwks(
            configuration_id,
            configuration_etag,
            later_callback,
            older_refresh,
            JwkSet { keys: Vec::new() },
        );
        management.cache_oidc_jwks(
            configuration_id,
            configuration_etag,
            earlier_callback,
            newer_refresh,
            JwkSet { keys: Vec::new() },
        );
        management.cache_oidc_jwks(
            configuration_id,
            configuration_etag,
            Instant::now(),
            older_refresh,
            JwkSet { keys: Vec::new() },
        );

        assert!(
            management
                .clone()
                .cached_oidc_jwks(configuration_id, configuration_etag)
                .is_some()
        );
        assert!(
            management
                .cached_oidc_jwks(configuration_id, uuid::Uuid::now_v7())
                .is_none()
        );
        assert_eq!(
            management
                .oidc_jwks_cache
                .inner
                .read()
                .expect("OIDC JWKS cache lock poisoned")
                .get(&(configuration_id, configuration_etag))
                .expect("current JWKS entry must remain cached")
                .callback_started_at,
            earlier_callback
        );
        assert_eq!(
            management
                .oidc_jwks_cache
                .inner
                .read()
                .expect("OIDC JWKS cache lock poisoned")
                .get(&(configuration_id, configuration_etag))
                .expect("current JWKS entry must remain cached")
                .refresh_started_at,
            newer_refresh
        );

        let other_configuration_id = uuid::Uuid::now_v7();
        let other_configuration_etag = uuid::Uuid::now_v7();
        let replacement_etag = uuid::Uuid::now_v7();
        let other_started_at = Instant::now();
        management.cache_oidc_jwks(
            other_configuration_id,
            other_configuration_etag,
            other_started_at,
            other_started_at,
            JwkSet { keys: Vec::new() },
        );
        let replacement_started_at = Instant::now();
        management.cache_oidc_jwks(
            configuration_id,
            replacement_etag,
            replacement_started_at,
            replacement_started_at,
            JwkSet { keys: Vec::new() },
        );
        assert!(
            management
                .cached_oidc_jwks(configuration_id, configuration_etag)
                .is_some()
        );
        assert!(
            management
                .cached_oidc_jwks(configuration_id, replacement_etag)
                .is_some()
        );
        assert!(
            management
                .cached_oidc_jwks(other_configuration_id, other_configuration_etag)
                .is_some()
        );

        let filler_started_at = Instant::now();
        management.cache_oidc_jwks(
            uuid::Uuid::now_v7(),
            uuid::Uuid::now_v7(),
            filler_started_at,
            filler_started_at,
            JwkSet { keys: Vec::new() },
        );
        let delayed_old_etag = uuid::Uuid::now_v7();
        management.cache_oidc_jwks(
            configuration_id,
            delayed_old_etag,
            earlier_callback - Duration::from_secs(1),
            Instant::now(),
            JwkSet { keys: Vec::new() },
        );
        assert!(
            management
                .cached_oidc_jwks(configuration_id, replacement_etag)
                .is_some()
        );
        assert!(
            management
                .cached_oidc_jwks(configuration_id, delayed_old_etag)
                .is_none()
        );

        for _ in 0..OIDC_JWKS_CACHE_ENTRIES {
            let started_at = Instant::now();
            management.cache_oidc_jwks(
                uuid::Uuid::now_v7(),
                uuid::Uuid::now_v7(),
                started_at,
                started_at,
                JwkSet { keys: Vec::new() },
            );
        }
        assert_eq!(
            management
                .oidc_jwks_cache
                .inner
                .read()
                .expect("OIDC JWKS cache lock poisoned")
                .len(),
            OIDC_JWKS_CACHE_ENTRIES
        );
    }
}

use super::state::ProcessComposition;
use crate::public_http::state::ModeDependencies;
use crate::{
    application::mode::ApiMode, gateway::state::GatewayState, management::state::ManagementState,
    observability::state::ObservabilityState, public_http::state::RequestBoundaryState,
};
#[cfg(test)]
use olp_db::{security::key_material::AuthHmacKey, store::Store};
#[cfg(test)]
use olp_engine::inference::runtime::Manager;
use olp_engine::inference::service::Service;
#[cfg(test)]
use std::path::PathBuf;
use std::sync::Arc;

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
            request_tracing: self.request_tracing,
            trusted_proxy_cidrs: Arc::clone(&self.trusted_proxy_cidrs),
            bootstrap_token_digest: Arc::clone(&self.bootstrap_token_digest),
        };
        let gateway = GatewayState {
            request_boundary: request_boundary.clone(),
            cors_allowed_origins: Arc::clone(&self.gateway_cors_allowed_origins),
            media_jobs: crate::application::media_jobs::MediaJobs {
                store: store.clone(),
                inference: Arc::clone(&inference),
                transports: self.transports.clone(),
                master_key: self.master_key.clone(),
                provider_egress_policy: Arc::clone(&self.provider_egress_policy),
                provider_response_limits: self.provider_response_limits,
                media_reconciliation_gaps: Arc::clone(&self.media_reconciliation_gaps),
            },
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
}
#[cfg(test)]
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

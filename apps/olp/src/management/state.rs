use crate::{
    application::transports::TransportRegistry,
    observability::{
        cache::ObservabilityCache,
        readiness::{HealthResponse, cached_readiness_from_snapshot},
    },
    public_http::{problem::Problem, public_origin::PublicOrigin, state::RequestBoundaryState},
};
use olp_db::{
    security::{envelope::MasterKey, key_material::AuthHmacKey},
    store::Store,
};
#[cfg(any(test, feature = "test-util"))]
use olp_engine::domain::routing::provider::ProviderKind;
use olp_engine::{
    inference::service::Service,
    providers::{connector::ResponseLimits, http_egress::EgressPolicy},
};
use std::{path::PathBuf, sync::Arc, time::Instant};
/// Control-plane dependencies plus the explicitly shared inference service
/// used by the authenticated playground.
#[derive(Clone)]
pub struct ManagementState {
    pub(crate) request_boundary: RequestBoundaryState,
    pub(crate) transports: TransportRegistry,
    pub(crate) master_key: Option<Arc<MasterKey>>,
    pub(crate) provider_egress_policy: Arc<EgressPolicy>,
    pub(crate) provider_response_limits: ResponseLimits,
    #[cfg(any(test, feature = "test-util"))]
    pub(crate) certification_probe_connectors: olp_engine::providers::factory::overrides::Registry,
    pub(crate) public_origin: PublicOrigin,
    pub(crate) console_dir: Arc<PathBuf>,
    pub(crate) session_ttl: chrono::Duration,
    pub(crate) local_login_enabled: bool,
    pub(crate) oidc_allow_insecure_test_endpoints: bool,
    pub(crate) observability: ObservabilityCache,
}

impl ManagementState {
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

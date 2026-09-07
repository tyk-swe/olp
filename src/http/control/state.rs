use crate::crypto::envelope::MasterKey;
use crate::http::problem::Problem;
use crate::http::public_origin::PublicOrigin;
use crate::http::state::RequestBoundaryState;
use crate::net::egress::EgressPolicy;
use crate::observability::cache::ObservabilityCache;
use crate::observability::readiness::HealthResponse;
use crate::observability::readiness::cached_readiness_from_snapshot;
use crate::providers::connector::ResponseLimits;
#[cfg(any(test, feature = "test-util"))]
use crate::providers::runtime_model::ProviderKind;
use crate::runtime::transports::TransportRegistry;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Instant;
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
    pub(crate) certification_probe_connectors: crate::providers::connectors::overrides::Registry,
    pub(crate) public_origin: PublicOrigin,
    pub(crate) console_dir: Arc<PathBuf>,
    pub(crate) session_ttl: chrono::Duration,
    pub(crate) local_login_enabled: bool,
    pub(crate) oidc_allow_insecure_test_endpoints: bool,
    pub(crate) observability: ObservabilityCache,
}

impl ManagementState {
    pub(crate) async fn clear_bootstrap_token(&self) {
        self.request_boundary.clear_bootstrap_token().await;
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(crate) fn certification_probe_connector(
        &self,
        provider_id: uuid::Uuid,
        kind: ProviderKind,
    ) -> Option<crate::providers::connectors::ProviderConnector> {
        self.certification_probe_connectors.get(provider_id, kind)
    }

    pub(crate) fn cached_readiness(&self) -> Result<HealthResponse, Problem> {
        let snapshot = self.observability.readiness();
        cached_readiness_from_snapshot(&snapshot, Instant::now())
    }
}

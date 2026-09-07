use crate::crypto::key_material::AuthHmacKey;
use crate::http::body_limits::BodyLimits;
use crate::http::proxy::TrustedProxyCidr;
use crate::http::public_origin::PublicOrigin;
use crate::http::request_admission::multipart::MultipartAdmissionState;
use crate::http::request_admission::public::PublicAdmission;
use crate::inference::executor::Executor;
use crate::observability::tracing::RequestConfig as RequestTracingConfig;
use sqlx::PgPool;
use std::sync::Arc;
/// Dependencies used before a request reaches either product surface.
/// Control-only mode owns this boundary without acquiring gateway handlers.
#[derive(Clone)]
pub(crate) struct RequestBoundaryState {
    pub(crate) pool: PgPool,
    pub(crate) inference: Arc<Executor>,
    pub(crate) auth_hmac_key: Arc<AuthHmacKey>,
    pub(crate) multipart_admission: MultipartAdmissionState,
    pub(crate) public_admission: PublicAdmission,
    pub(crate) public_origin: PublicOrigin,
    pub(crate) body_limits: BodyLimits,
    pub(crate) request_tracing: Option<RequestTracingConfig>,
    pub(crate) trusted_proxy_cidrs: Arc<[TrustedProxyCidr]>,
    pub(crate) bootstrap_token_digest:
        Arc<tokio::sync::RwLock<Option<zeroize::Zeroizing<[u8; 32]>>>>,
}

impl RequestBoundaryState {
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

use crate::http::control::state::ManagementState;
use crate::inference::http::state::GatewayState;
use crate::observability::state::ObservabilityState;
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

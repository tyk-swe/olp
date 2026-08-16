use std::sync::Arc;

use crate::domain::{
    auth::{ApiKey, GatewayCapability},
    canonical::identity::Surface,
    ids::ApiKeyLookupId,
};

use crate::inference::runtime::Bundle;

/// Authenticated API-key identity pinned to the runtime generation that
/// performed credential verification.
#[derive(Clone)]
pub struct Principal {
    runtime: Arc<Bundle>,
    lookup_id: ApiKeyLookupId,
    surface: Surface,
    gateway_capability: Option<GatewayCapability>,
}

impl Principal {
    #[must_use]
    pub fn new(
        runtime: Arc<Bundle>,
        lookup_id: ApiKeyLookupId,
        surface: Surface,
        gateway_capability: Option<GatewayCapability>,
    ) -> Self {
        Self {
            runtime,
            lookup_id,
            surface,
            gateway_capability,
        }
    }

    #[must_use]
    pub fn runtime(&self) -> &Arc<Bundle> {
        &self.runtime
    }

    #[must_use]
    pub fn key(&self) -> &ApiKey {
        self.runtime
            .api_keys
            .get(&self.lookup_id)
            .expect("authenticated API key must remain in its pinned runtime")
    }

    #[must_use]
    pub const fn lookup_id(&self) -> &ApiKeyLookupId {
        &self.lookup_id
    }

    #[must_use]
    pub const fn surface(&self) -> Surface {
        self.surface
    }

    /// Capability declared by the classified public endpoint, if the action
    /// has a supported authorization policy.
    #[must_use]
    pub const fn gateway_capability(&self) -> Option<GatewayCapability> {
        self.gateway_capability
    }
}

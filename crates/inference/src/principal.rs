use std::sync::Arc;

use olp_domain::{ApiKey, ApiKeyLookupId, Surface};

use crate::runtime::RuntimeBundle;

/// Authenticated API-key identity pinned to the runtime generation that
/// performed credential verification.
#[derive(Clone)]
pub struct InferencePrincipal {
    runtime: Arc<RuntimeBundle>,
    lookup_id: ApiKeyLookupId,
    surface: Surface,
}

impl InferencePrincipal {
    #[must_use]
    pub fn new(runtime: Arc<RuntimeBundle>, lookup_id: ApiKeyLookupId, surface: Surface) -> Self {
        Self {
            runtime,
            lookup_id,
            surface,
        }
    }

    #[must_use]
    pub fn runtime(&self) -> &Arc<RuntimeBundle> {
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
}

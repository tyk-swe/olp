//! Mode-owned orchestration services.
//!
//! These types establish startup-time dependency contracts. HTTP handlers can
//! share the same `ApiState` extraction type while process composition proves
//! that every service exposed by a mode has its required dependencies.

use std::sync::Arc;

use olp_storage::{AuthHmacKey, MasterKey, PgStore};
use thiserror::Error;

use crate::{ApiMode, ApiState, RuntimeManager, TransportRegistry};

#[derive(Clone)]
pub struct ConfigurationService {
    pub(crate) store: PgStore,
    pub(crate) master_key: Option<Arc<MasterKey>>,
}

impl ConfigurationService {
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
pub struct IdentityService {
    pub(crate) store: PgStore,
    pub(crate) auth_hmac_key: Arc<AuthHmacKey>,
}

impl IdentityService {
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
pub struct InferenceService {
    pub(crate) store: PgStore,
    pub(crate) runtime: Arc<RuntimeManager>,
    pub(crate) transports: TransportRegistry,
}

impl InferenceService {
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
}

#[derive(Clone)]
pub struct OperationsService {
    pub(crate) store: PgStore,
}

impl OperationsService {
    #[must_use]
    pub fn store(&self) -> &PgStore {
        &self.store
    }
}

#[derive(Clone)]
pub struct WorkerService {
    pub(crate) store: PgStore,
}

impl WorkerService {
    #[must_use]
    pub fn new(store: PgStore) -> Self {
        Self { store }
    }

    #[must_use]
    pub fn store(&self) -> &PgStore {
        &self.store
    }
}

#[derive(Clone)]
pub enum ModeServices {
    All {
        configuration: ConfigurationService,
        identity: IdentityService,
        inference: InferenceService,
        operations: OperationsService,
    },
    Gateway {
        inference: InferenceService,
    },
    Control {
        configuration: ConfigurationService,
        identity: IdentityService,
        operations: OperationsService,
    },
}

#[derive(Clone, Debug, Error, Eq, PartialEq)]
pub enum ApiStartupError {
    #[error("{0} mode requires PostgreSQL storage")]
    Storage(ApiMode),
    #[error("{0} mode requires the authentication HMAC key")]
    AuthHmacKey(ApiMode),
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
    pub fn mode_services(&self) -> Result<ModeServices, ApiStartupError> {
        let store = self
            .store
            .clone()
            .ok_or(ApiStartupError::Storage(self.mode))?;
        let inference = || InferenceService {
            store: store.clone(),
            runtime: Arc::clone(&self.runtime),
            transports: self.transports.clone(),
        };
        let configuration = || ConfigurationService {
            store: store.clone(),
            master_key: self.master_key.clone(),
        };
        let identity = || {
            self.auth_hmac_key
                .clone()
                .map(|auth_hmac_key| IdentityService {
                    store: store.clone(),
                    auth_hmac_key,
                })
                .ok_or(ApiStartupError::AuthHmacKey(self.mode))
        };
        let operations = || OperationsService {
            store: store.clone(),
        };
        match self.mode {
            ApiMode::All => Ok(ModeServices::All {
                configuration: configuration(),
                identity: identity()?,
                inference: inference(),
                operations: operations(),
            }),
            ApiMode::Gateway => Ok(ModeServices::Gateway {
                inference: inference(),
            }),
            ApiMode::Control => Ok(ModeServices::Control {
                configuration: configuration(),
                identity: identity()?,
                operations: operations(),
            }),
        }
    }
}

#[cfg(test)]
mod tests {
    use std::{path::PathBuf, sync::Arc};

    use sqlx::postgres::PgPoolOptions;

    use super::*;

    fn state(mode: ApiMode, with_store: bool, with_auth_hmac_key: bool) -> ApiState {
        let store = with_store.then(|| {
            let pool = PgPoolOptions::new()
                .connect_lazy("postgres://olp:olp@127.0.0.1/olp")
                .expect("test PostgreSQL URL is valid");
            PgStore::from_pool(pool)
        });
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
                state(mode, false, false).mode_services().err(),
                Some(ApiStartupError::Storage(mode))
            );
        }
    }

    #[tokio::test]
    async fn control_surfaces_require_identity_dependencies_but_gateway_does_not() {
        assert_eq!(
            state(ApiMode::All, true, false).mode_services().err(),
            Some(ApiStartupError::AuthHmacKey(ApiMode::All))
        );
        assert_eq!(
            state(ApiMode::Control, true, false).mode_services().err(),
            Some(ApiStartupError::AuthHmacKey(ApiMode::Control))
        );
        assert!(matches!(
            state(ApiMode::Gateway, true, false).mode_services(),
            Ok(ModeServices::Gateway { .. })
        ));
    }

    #[tokio::test]
    async fn fully_composed_modes_produce_only_their_owned_services() {
        assert!(matches!(
            state(ApiMode::All, true, true).mode_services(),
            Ok(ModeServices::All { .. })
        ));
        assert!(matches!(
            state(ApiMode::Control, true, true).mode_services(),
            Ok(ModeServices::Control { .. })
        ));
        assert!(matches!(
            state(ApiMode::Gateway, true, true).mode_services(),
            Ok(ModeServices::Gateway { .. })
        ));
    }
}

//! Mode-owned dependency bundles.
//!
//! These types establish startup-time dependency contracts. HTTP handlers can
//! share the same `ApiState` extraction type while process composition proves
//! that every surface exposed by a mode has its required dependencies.

use std::sync::Arc;

use olp_storage::{AuthHmacKey, MasterKey, PgStore};
use thiserror::Error;

use crate::{ApiMode, ApiState, RuntimeManager, TransportRegistry};

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

#[derive(Clone)]
pub enum ModeDependencies {
    All {
        configuration: ConfigurationDependencies,
        identity: IdentityDependencies,
        inference: InferenceDependencies,
        operations: OperationsDependencies,
    },
    Gateway {
        inference: InferenceDependencies,
    },
    Control {
        configuration: ConfigurationDependencies,
        identity: IdentityDependencies,
        operations: OperationsDependencies,
    },
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
        let inference = || InferenceDependencies {
            store: store.clone(),
            runtime: Arc::clone(&self.runtime),
            transports: self.transports.clone(),
        };
        let configuration = || ConfigurationDependencies {
            store: store.clone(),
            master_key: self.master_key.clone(),
        };
        let identity = || {
            self.auth_hmac_key
                .clone()
                .map(|auth_hmac_key| IdentityDependencies {
                    store: store.clone(),
                    auth_hmac_key,
                })
                .ok_or(ModeDependencyError::MissingAuthHmacKey(self.mode))
        };
        let operations = || OperationsDependencies {
            store: store.clone(),
        };
        match self.mode {
            ApiMode::All => Ok(ModeDependencies::All {
                configuration: configuration(),
                identity: identity()?,
                inference: inference(),
                operations: operations(),
            }),
            ApiMode::Gateway => Ok(ModeDependencies::Gateway {
                inference: inference(),
            }),
            ApiMode::Control => Ok(ModeDependencies::Control {
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
                state(mode, false, false).mode_dependencies().err(),
                Some(ModeDependencyError::MissingStorage(mode))
            );
        }
    }

    #[tokio::test]
    async fn control_surfaces_require_identity_dependencies_but_gateway_does_not() {
        assert_eq!(
            state(ApiMode::All, true, false).mode_dependencies().err(),
            Some(ModeDependencyError::MissingAuthHmacKey(ApiMode::All))
        );
        assert_eq!(
            state(ApiMode::Control, true, false).mode_dependencies().err(),
            Some(ModeDependencyError::MissingAuthHmacKey(ApiMode::Control))
        );
        assert!(matches!(
            state(ApiMode::Gateway, true, false).mode_dependencies(),
            Ok(ModeDependencies::Gateway { .. })
        ));
    }

    #[tokio::test]
    async fn fully_composed_modes_produce_only_their_owned_dependencies() {
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

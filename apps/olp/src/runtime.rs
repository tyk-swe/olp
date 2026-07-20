use std::{
    collections::BTreeMap,
    ops::Deref,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
};

use arc_swap::ArcSwap;
use chrono::Utc;
use olp_domain::{
    ApiKey, ApiKeyLookupId, ProviderId, ProviderTransport, RuntimeGeneration, RuntimeGenerationId,
    RuntimeSnapshot,
};
use olp_storage::PublishedRelease;
use thiserror::Error;

pub struct RuntimeManager {
    bundle: ArcSwap<RuntimeBundle>,
    loaded: AtomicBool,
    install_lock: Mutex<()>,
}

/// Everything a request may resolve after pinning a generation. In particular,
/// credentials live inside connector objects in this same `Arc`, so a config
/// activation cannot make an old request observe a future credential.
pub struct RuntimeBundle {
    snapshot: RuntimeSnapshot,
    transports: BTreeMap<ProviderId, Arc<dyn ProviderTransport>>,
}

impl RuntimeBundle {
    #[must_use]
    pub fn transport(&self, provider_id: ProviderId) -> Option<Arc<dyn ProviderTransport>> {
        self.transports.get(&provider_id).cloned()
    }

    #[must_use]
    pub fn has_all_transports(&self) -> bool {
        self.snapshot
            .providers
            .keys()
            .all(|provider_id| self.transports.contains_key(provider_id))
    }
}

impl Deref for RuntimeBundle {
    type Target = RuntimeSnapshot;

    fn deref(&self) -> &Self::Target {
        &self.snapshot
    }
}

impl RuntimeManager {
    pub fn empty() -> Self {
        Self {
            bundle: ArcSwap::from_pointee(RuntimeBundle {
                snapshot: RuntimeSnapshot {
                    generation: RuntimeGeneration {
                        id: RuntimeGenerationId::new(),
                        ordinal: 0,
                        activated_at: Utc::now(),
                    },
                    providers: Default::default(),
                    routes: Default::default(),
                    api_keys: Default::default(),
                },
                transports: Default::default(),
            }),
            loaded: AtomicBool::new(false),
            install_lock: Mutex::new(()),
        }
    }

    /// Pins one immutable generation for the lifetime of a request.
    pub fn pin(&self) -> Arc<RuntimeBundle> {
        self.bundle.load_full()
    }

    pub fn ordinal(&self) -> Option<u64> {
        self.loaded
            .load(Ordering::Acquire)
            .then(|| self.bundle.load().generation.ordinal)
    }

    pub fn install(
        &self,
        snapshot: RuntimeSnapshot,
        transports: BTreeMap<ProviderId, Arc<dyn ProviderTransport>>,
    ) -> Result<bool, RuntimeInstallError> {
        snapshot.validate()?;
        if let Some(provider_id) = snapshot
            .providers
            .keys()
            .find(|provider_id| !transports.contains_key(provider_id))
        {
            return Err(RuntimeInstallError::MissingTransport(*provider_id));
        }
        let _install = self
            .install_lock
            .lock()
            .expect("runtime install lock poisoned");
        if self.loaded.load(Ordering::Acquire)
            && snapshot.generation.ordinal <= self.bundle.load().generation.ordinal
        {
            return Ok(false);
        }
        self.bundle.store(Arc::new(RuntimeBundle {
            snapshot,
            transports,
        }));
        self.loaded.store(true, Ordering::Release);
        Ok(true)
    }

    fn decode_release(
        &self,
        release: &PublishedRelease,
    ) -> Result<RuntimeSnapshot, RuntimeInstallError> {
        let mut snapshot = RuntimeSnapshot::from_persisted_slice(&release.payload)?;
        if snapshot.generation.id.as_uuid() != release.generation_id {
            return Err(RuntimeInstallError::GenerationMismatch);
        }
        snapshot.generation.ordinal =
            u64::try_from(release.sequence).map_err(|_| RuntimeInstallError::GenerationMismatch)?;
        snapshot.validate()?;
        Ok(snapshot)
    }

    /// Decodes a release while replacing all historical API-key material with
    /// the complete current authority view. Filtering only by lookup ID is not
    /// sufficient: the same public lookup can have newer scopes, allowlists,
    /// expiry, limits, or digest material than an LKG release contains.
    pub fn decode_release_candidate(
        &self,
        release: &PublishedRelease,
        current_api_keys: BTreeMap<ApiKeyLookupId, ApiKey>,
    ) -> Result<RuntimeSnapshot, RuntimeInstallError> {
        let mut snapshot = self.decode_release(release)?;
        snapshot.api_keys = current_api_keys;
        snapshot.validate()?;
        Ok(snapshot)
    }
}

impl Default for RuntimeManager {
    fn default() -> Self {
        Self::empty()
    }
}

#[derive(Debug, Error)]
pub enum RuntimeInstallError {
    #[error("runtime snapshot is invalid: {0}")]
    InvalidSnapshot(#[from] olp_domain::SnapshotValidationError),
    #[error("runtime release is not valid JSON: {0}")]
    Decode(#[from] serde_json::Error),
    #[error("runtime generation ID does not match its release envelope")]
    GenerationMismatch,
    #[error("runtime provider {0} has no transport in the candidate generation")]
    MissingTransport(ProviderId),
}

#[cfg(test)]
mod tests {
    use std::{collections::BTreeSet, num::NonZeroU32};

    use chrono::Duration;
    use futures::stream;
    use olp_domain::{
        ApiKeyDigest, ApiKeyId, ApiKeyLimits, ApiKeyScope, ApiKeyStatus, BoxFuture, Provider,
        ProviderEventStream, ProviderKind, ProviderOutput, ProviderRequest, RouteSlug,
        TransportError,
    };

    use super::*;

    struct MarkerTransport;

    impl ProviderTransport for MarkerTransport {
        fn execute<'a>(
            &'a self,
            _request: ProviderRequest,
        ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
            Box::pin(async {
                Ok(ProviderOutput::Events(
                    Box::pin(stream::empty::<Result<_, TransportError>>()) as ProviderEventStream,
                ))
            })
        }
    }

    #[test]
    fn swaps_only_forward_and_pins_old_generation() {
        let manager = RuntimeManager::empty();
        let old = manager.pin();
        let mut newer = old.snapshot.clone();
        newer.generation.id = RuntimeGenerationId::new();
        newer.generation.ordinal = 2;
        assert!(manager.install(newer, BTreeMap::new()).unwrap());
        assert_eq!(old.generation.ordinal, 0);
        assert_eq!(manager.pin().generation.ordinal, 2);

        let mut stale = old.snapshot.clone();
        stale.generation.ordinal = 1;
        assert!(!manager.install(stale, BTreeMap::new()).unwrap());
        assert_eq!(manager.pin().generation.ordinal, 2);
    }

    #[test]
    fn pinned_generation_retains_its_own_transport_objects() {
        let manager = RuntimeManager::empty();
        let provider_id = ProviderId::new();
        let snapshot = |ordinal| RuntimeSnapshot {
            generation: RuntimeGeneration {
                id: RuntimeGenerationId::new(),
                ordinal,
                activated_at: Utc::now(),
            },
            providers: BTreeMap::from([(
                provider_id,
                Provider {
                    id: provider_id,
                    name: "provider".into(),
                    kind: ProviderKind::OpenAi,
                    enabled: true,
                    active_credential: None,
                    capabilities: Default::default(),
                },
            )]),
            routes: Default::default(),
            api_keys: Default::default(),
        };
        let first: Arc<dyn ProviderTransport> = Arc::new(MarkerTransport);
        manager
            .install(snapshot(1), BTreeMap::from([(provider_id, first.clone())]))
            .unwrap();
        let pinned = manager.pin();
        let second: Arc<dyn ProviderTransport> = Arc::new(MarkerTransport);
        manager
            .install(snapshot(2), BTreeMap::from([(provider_id, second.clone())]))
            .unwrap();

        assert!(Arc::ptr_eq(&pinned.transport(provider_id).unwrap(), &first));
        assert!(Arc::ptr_eq(
            &manager.pin().transport(provider_id).unwrap(),
            &second
        ));
    }

    #[test]
    fn fallback_replaces_every_historical_api_key_security_field() {
        let manager = RuntimeManager::empty();
        let lookup_id = ApiKeyLookupId::parse("lookup_same_key").unwrap();
        let key_id = ApiKeyId::new();
        let historical_key = ApiKey {
            id: key_id,
            lookup_id: lookup_id.clone(),
            digest: ApiKeyDigest::new([1; 32]),
            status: ApiKeyStatus::Active,
            expires_at: None,
            scopes: BTreeSet::from([ApiKeyScope::Inference]),
            allowed_routes: BTreeSet::new(),
            limits: ApiKeyLimits::default(),
        };
        let generation_id = RuntimeGenerationId::new();
        let historical = RuntimeSnapshot {
            generation: RuntimeGeneration {
                id: generation_id,
                ordinal: 9,
                activated_at: Utc::now() - Duration::hours(1),
            },
            providers: BTreeMap::new(),
            routes: BTreeMap::new(),
            api_keys: BTreeMap::from([(lookup_id.clone(), historical_key)]),
        };
        let release = PublishedRelease {
            generation_id: generation_id.as_uuid(),
            sequence: 9,
            payload: serde_json::to_vec(&historical).unwrap(),
            sha256: [0; 32],
            created_at: historical.generation.activated_at,
        };
        let expires_at = Utc::now() + Duration::minutes(10);
        let route = RouteSlug::parse("restricted").unwrap();
        let current_key = ApiKey {
            id: key_id,
            lookup_id: lookup_id.clone(),
            digest: ApiKeyDigest::new([2; 32]),
            status: ApiKeyStatus::Active,
            expires_at: Some(expires_at),
            scopes: BTreeSet::from([ApiKeyScope::ModelsRead]),
            allowed_routes: BTreeSet::from([route.clone()]),
            limits: ApiKeyLimits {
                requests_per_minute: NonZeroU32::new(7),
                tokens_per_minute: None,
                concurrency: NonZeroU32::new(2),
            },
        };

        let candidate = manager
            .decode_release_candidate(&release, BTreeMap::from([(lookup_id.clone(), current_key)]))
            .unwrap();
        let installed_key = candidate.api_keys.get(&lookup_id).unwrap();
        assert_eq!(installed_key.digest.as_bytes(), &[2; 32]);
        assert_eq!(installed_key.expires_at, Some(expires_at));
        assert_eq!(
            installed_key.scopes,
            BTreeSet::from([ApiKeyScope::ModelsRead])
        );
        assert_eq!(installed_key.allowed_routes, BTreeSet::from([route]));
        assert_eq!(
            installed_key
                .limits
                .requests_per_minute
                .map(NonZeroU32::get),
            Some(7)
        );
        assert_eq!(
            installed_key.limits.concurrency.map(NonZeroU32::get),
            Some(2)
        );
    }
}

//! Immutable runtime publication and generation pinning.

use std::{
    collections::BTreeMap,
    fmt,
    ops::Deref,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
};

use crate::domain::{
    auth::ApiKey,
    ids::{ApiKeyLookupId, ProviderId, RuntimeGenerationId},
    ports::ProviderTransport,
    routing::snapshot::{RuntimeGeneration, Snapshot},
};
use arc_swap::ArcSwap;
use chrono::Utc;
use thiserror::Error as ThisError;

/// Storage-verified payload proposed for activation by the delivery layer.
#[derive(Clone, Copy)]
pub struct ReleaseCandidate<'a> {
    pub generation_id: uuid::Uuid,
    pub sequence: i64,
    pub payload: &'a [u8],
}

impl fmt::Debug for ReleaseCandidate<'_> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ReleaseCandidate")
            .field("generation_id", &self.generation_id)
            .field("sequence", &self.sequence)
            .field("payload", &"[REDACTED]")
            .finish()
    }
}

pub struct Manager {
    bundle: ArcSwap<Bundle>,
    loaded: AtomicBool,
    install_lock: Mutex<()>,
}

/// Everything a request may resolve after pinning a generation. In particular,
/// credentials live inside connector objects in this same `Arc`, so a config
/// activation cannot make an old request observe a future credential.
pub struct Bundle {
    snapshot: Snapshot,
    transports: BTreeMap<ProviderId, Arc<dyn ProviderTransport>>,
}

impl Bundle {
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

impl Deref for Bundle {
    type Target = Snapshot;

    fn deref(&self) -> &Self::Target {
        &self.snapshot
    }
}

impl Manager {
    pub fn empty() -> Self {
        Self {
            bundle: ArcSwap::from_pointee(Bundle {
                snapshot: Snapshot {
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
    pub fn pin(&self) -> Arc<Bundle> {
        self.bundle.load_full()
    }

    pub fn active_generation_ordinal(&self) -> Option<u64> {
        self.loaded
            .load(Ordering::Acquire)
            .then(|| self.bundle.load().generation.ordinal)
    }

    pub fn install(
        &self,
        snapshot: Snapshot,
        transports: BTreeMap<ProviderId, Arc<dyn ProviderTransport>>,
    ) -> Result<bool, Error> {
        snapshot.validate()?;
        if let Some(provider_id) = snapshot
            .providers
            .keys()
            .find(|provider_id| !transports.contains_key(provider_id))
        {
            return Err(Error::MissingTransport(*provider_id));
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
        self.bundle.store(Arc::new(Bundle {
            snapshot,
            transports,
        }));
        self.loaded.store(true, Ordering::Release);
        Ok(true)
    }

    pub fn decode_persisted_release(release: &ReleaseCandidate<'_>) -> Result<Snapshot, Error> {
        let mut snapshot = Snapshot::from_persisted_slice(release.payload)?;
        if snapshot.generation.id.as_uuid() != release.generation_id {
            return Err(Error::GenerationMismatch);
        }
        snapshot.generation.ordinal =
            u64::try_from(release.sequence).map_err(|_| Error::GenerationMismatch)?;
        snapshot.validate()?;
        Ok(snapshot)
    }

    pub fn reconciliation_bundle(
        snapshot: Snapshot,
        provider_id: ProviderId,
        transport: Arc<dyn ProviderTransport>,
    ) -> Result<Arc<Bundle>, Error> {
        snapshot.validate()?;
        if !snapshot.providers.contains_key(&provider_id) {
            return Err(Error::MissingTransport(provider_id));
        }
        Ok(Arc::new(Bundle {
            snapshot,
            transports: BTreeMap::from([(provider_id, transport)]),
        }))
    }

    /// Decodes a release while replacing all historical API-key material with
    /// the complete current authority view. Filtering only by lookup ID is not
    /// sufficient: the same public lookup can have newer scopes, allowlists,
    /// expiry, limits, or digest material than an LKG release contains.
    pub fn decode_release_candidate(
        &self,
        release: ReleaseCandidate<'_>,
        current_api_keys: BTreeMap<ApiKeyLookupId, ApiKey>,
    ) -> Result<Snapshot, Error> {
        let mut snapshot = Self::decode_persisted_release(&release)?;
        snapshot.api_keys = current_api_keys;
        snapshot.validate()?;
        Ok(snapshot)
    }
}

impl Default for Manager {
    fn default() -> Self {
        Self::empty()
    }
}

#[derive(Debug, ThisError)]
pub enum Error {
    #[error("runtime snapshot is invalid: {0}")]
    InvalidSnapshot(#[from] crate::domain::routing::snapshot::Error),
    #[error("runtime release is not valid JSON: {0}")]
    Decode(#[from] serde_json::Error),
    #[error("runtime generation ID does not match its activation candidate")]
    GenerationMismatch,
    #[error("runtime provider {0} has no transport in the candidate generation")]
    MissingTransport(ProviderId),
}

#[cfg(test)]
mod tests {
    use std::{collections::BTreeSet, num::NonZeroU32};

    use crate::domain::{
        auth::{ApiKeyDigest, ApiKeyLimits, ApiKeyScope, ApiKeyStatus},
        ids::{ApiKeyId, RouteSlug},
        ports::{BoxFuture, ProviderEventStream, ProviderOutput, ProviderRequest, TransportError},
        routing::provider::{Provider, ProviderKind},
    };
    use chrono::Duration;
    use futures::stream;

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
        let manager = Manager::empty();
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
        let manager = Manager::empty();
        let provider_id = ProviderId::new();
        let snapshot = |ordinal| Snapshot {
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
    fn reconciliation_bundle_keeps_the_persisted_generation_and_transport() {
        let provider_id = ProviderId::new();
        let generation_id = RuntimeGenerationId::new();
        let transport: Arc<dyn ProviderTransport> = Arc::new(MarkerTransport);
        let snapshot = Snapshot {
            generation: RuntimeGeneration {
                id: generation_id,
                ordinal: 7,
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

        let bundle =
            Manager::reconciliation_bundle(snapshot, provider_id, transport.clone()).unwrap();

        assert_eq!(bundle.generation.id, generation_id);
        assert_eq!(bundle.generation.ordinal, 7);
        assert!(Arc::ptr_eq(
            &bundle.transport(provider_id).unwrap(),
            &transport
        ));
    }

    #[test]
    fn fallback_replaces_every_historical_api_key_security_field() {
        let manager = Manager::empty();
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
        let historical = Snapshot {
            generation: RuntimeGeneration {
                id: generation_id,
                ordinal: 9,
                activated_at: Utc::now() - Duration::hours(1),
            },
            providers: BTreeMap::new(),
            routes: BTreeMap::new(),
            api_keys: BTreeMap::from([(lookup_id.clone(), historical_key)]),
        };
        let release_payload = serde_json::to_vec(&historical).unwrap();
        let release = ReleaseCandidate {
            generation_id: generation_id.as_uuid(),
            sequence: 9,
            payload: &release_payload,
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
            .decode_release_candidate(release, BTreeMap::from([(lookup_id.clone(), current_key)]))
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

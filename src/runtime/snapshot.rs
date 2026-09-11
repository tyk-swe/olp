use std::collections::BTreeMap;

use chrono::DateTime;
use chrono::Utc;
use serde::Deserialize;
use serde::Serialize;
use thiserror::Error;

use crate::access::policy::ApiKey;
use crate::ids::ApiKeyLookupId;
use crate::ids::ProviderId;
use crate::ids::RouteSlug;
use crate::ids::RuntimeGenerationId;
use crate::ids::TargetId;
use crate::protocols::canonical::identity::OperationKind;

use crate::providers::runtime_model::Provider;
use crate::routes::model::Error as RouteError;
use crate::routes::model::Route;

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct RuntimeGeneration {
    pub id: RuntimeGenerationId,
    pub ordinal: u64,
    pub activated_at: DateTime<Utc>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Snapshot {
    #[serde(default)]
    pub routing: crate::routes::policy::RoutingConfiguration,
    pub generation: RuntimeGeneration,
    pub providers: BTreeMap<ProviderId, Provider>,
    pub routes: BTreeMap<RouteSlug, Route>,
    pub api_keys: BTreeMap<ApiKeyLookupId, ApiKey>,
}

/// Persisted release format. Migration 0004 wrapped every historical release,
/// so a bare snapshot is never read back from storage.
#[derive(Serialize, Deserialize)]
#[serde(tag = "format", content = "snapshot")]
enum ReleaseEnvelope<S> {
    #[serde(rename = "olp-routing-v1")]
    RoutingV1(S),
}

impl Snapshot {
    /// Older gateways cannot decode this envelope and silently omit constraints.
    /// The coordinated upgrade migrates historical envelopes before new policies.
    pub fn to_persisted_vec(&self) -> Result<Vec<u8>, serde_json::Error> {
        serde_json::to_vec(&ReleaseEnvelope::RoutingV1(self))
    }

    pub fn from_persisted_slice(payload: &[u8]) -> Result<Self, serde_json::Error> {
        let ReleaseEnvelope::RoutingV1(snapshot) = serde_json::from_slice(payload)?;
        Ok(snapshot)
    }

    pub fn validate(&self) -> Result<(), Error> {
        self.routing
            .installation
            .validate()
            .map_err(Error::InvalidRoutingPolicy)?;
        for policy in self
            .routing
            .routes
            .values()
            .chain(self.api_keys.values().map(|key| &key.routing_policy))
        {
            policy.validate().map_err(Error::InvalidRoutingPolicy)?;
        }
        for (id, options) in &self.routing.providers {
            if let Some(provider) = self.providers.get(id) {
                options
                    .validate(provider.kind)
                    .map_err(Error::InvalidRoutingPolicy)?;
            }
        }
        for slots in self.routing.credentials.values() {
            for slot in slots {
                slot.validate().map_err(Error::InvalidRoutingPolicy)?;
            }
        }

        for (provider_id, provider) in &self.providers {
            if *provider_id != provider.id {
                return Err(Error::ProviderKeyMismatch {
                    map_key: *provider_id,
                    provider_id: provider.id,
                });
            }
        }
        for (lookup_id, api_key) in &self.api_keys {
            if lookup_id != &api_key.lookup_id {
                return Err(Error::ApiKeyLookupMismatch {
                    map_key: lookup_id.clone(),
                    key_lookup_id: api_key.lookup_id.clone(),
                });
            }
        }

        for (slug, route) in &self.routes {
            if slug != &route.slug {
                return Err(Error::RouteKeyMismatch {
                    map_key: slug.clone(),
                    route_slug: route.slug.clone(),
                });
            }
            route.validate().map_err(|source| Error::InvalidRoute {
                slug: slug.clone(),
                source,
            })?;
            for target in &route.targets {
                if !self.providers.contains_key(&target.provider_id) {
                    return Err(Error::UnknownProvider {
                        slug: slug.clone(),
                        target_id: target.id,
                        provider_id: target.provider_id,
                    });
                }
            }
            for operation in &route.operations {
                let has_eligible_target = route.targets.iter().any(|target| {
                    self.providers
                        .get(&target.provider_id)
                        .is_some_and(|provider| {
                            provider.enabled
                                && provider.capabilities.iter().any(|capability| {
                                    capability.model == target.upstream_model
                                        && capability.operation == *operation
                                })
                        })
                });
                if !has_eligible_target {
                    return Err(Error::NoEligibleTarget {
                        slug: slug.clone(),
                        operation: *operation,
                    });
                }
            }
        }

        Ok(())
    }
}

#[derive(Clone, Debug, Error, Eq, PartialEq)]
pub enum Error {
    #[error("invalid routing policy: {0}")]
    InvalidRoutingPolicy(String),
    #[error("provider map key {map_key} does not match provider ID {provider_id}")]
    ProviderKeyMismatch {
        map_key: ProviderId,
        provider_id: ProviderId,
    },
    #[error("API-key map lookup {map_key} does not match key lookup {key_lookup_id}")]
    ApiKeyLookupMismatch {
        map_key: ApiKeyLookupId,
        key_lookup_id: ApiKeyLookupId,
    },
    #[error("route map key {map_key} does not match route slug {route_slug}")]
    RouteKeyMismatch {
        map_key: RouteSlug,
        route_slug: RouteSlug,
    },
    #[error("route {slug} is invalid: {source}")]
    InvalidRoute { slug: RouteSlug, source: RouteError },
    #[error("route {slug} target {target_id} refers to unknown provider {provider_id}")]
    UnknownProvider {
        slug: RouteSlug,
        target_id: TargetId,
        provider_id: ProviderId,
    },
    #[error("route {slug} has no eligible target for operation {operation:?}")]
    NoEligibleTarget {
        slug: RouteSlug,
        operation: OperationKind,
    },
}

#[cfg(test)]
mod envelope_tests {
    use super::*;
    #[test]
    fn release_envelope_is_the_only_persisted_form() {
        let bare = serde_json::json!({"generation":{"id":uuid::Uuid::now_v7(),"ordinal":1,"activated_at":Utc::now()},"providers":{},"routes":{},"api_keys":{}});
        let snapshot: Snapshot = serde_json::from_value(bare.clone()).unwrap();
        let payload = snapshot.to_persisted_vec().unwrap();
        let decoded = Snapshot::from_persisted_slice(&payload).unwrap();
        assert_eq!(decoded.generation.id, snapshot.generation.id);
        // A bare snapshot is neither written nor read back; legacy decoders
        // cannot mistake the envelope for one either.
        assert!(Snapshot::from_persisted_slice(&serde_json::to_vec(&bare).unwrap()).is_err());
        assert!(serde_json::from_slice::<Snapshot>(&payload).is_err());
        let mut future: serde_json::Value = serde_json::from_slice(&payload).unwrap();
        future["format"] = "unknown-format".into();
        assert!(Snapshot::from_persisted_slice(&serde_json::to_vec(&future).unwrap()).is_err());
    }
}

use std::collections::BTreeMap;

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::{
    ApiKey, ApiKeyLookupId, OperationKind, ProviderId, RouteSlug, RuntimeGenerationId, TargetId,
};

use super::{Provider, Route, RouteValidationError};

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct RuntimeGeneration {
    pub id: RuntimeGenerationId,
    pub ordinal: u64,
    pub activated_at: DateTime<Utc>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct RuntimeSnapshot {
    pub generation: RuntimeGeneration,
    #[serde(default)]
    pub providers: BTreeMap<ProviderId, Provider>,
    #[serde(default)]
    pub routes: BTreeMap<RouteSlug, Route>,
    #[serde(default)]
    pub api_keys: BTreeMap<ApiKeyLookupId, ApiKey>,
}

impl RuntimeSnapshot {
    pub fn from_persisted_slice(payload: &[u8]) -> Result<Self, serde_json::Error> {
        serde_json::from_slice(payload)
    }

    pub fn validate(&self) -> Result<(), SnapshotValidationError> {
        for (provider_id, provider) in &self.providers {
            if *provider_id != provider.id {
                return Err(SnapshotValidationError::ProviderKeyMismatch {
                    map_key: *provider_id,
                    provider_id: provider.id,
                });
            }
        }
        for (lookup_id, api_key) in &self.api_keys {
            if lookup_id != &api_key.lookup_id {
                return Err(SnapshotValidationError::ApiKeyLookupMismatch {
                    map_key: lookup_id.clone(),
                    key_lookup_id: api_key.lookup_id.clone(),
                });
            }
        }

        for (slug, route) in &self.routes {
            if slug != &route.slug {
                return Err(SnapshotValidationError::RouteKeyMismatch {
                    map_key: slug.clone(),
                    route_slug: route.slug.clone(),
                });
            }
            route
                .validate()
                .map_err(|source| SnapshotValidationError::InvalidRoute {
                    slug: slug.clone(),
                    source,
                })?;
            for target in &route.targets {
                if !self.providers.contains_key(&target.provider_id) {
                    return Err(SnapshotValidationError::UnknownProvider {
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
                    return Err(SnapshotValidationError::NoEligibleTarget {
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
pub enum SnapshotValidationError {
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
    InvalidRoute {
        slug: RouteSlug,
        source: RouteValidationError,
    },
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

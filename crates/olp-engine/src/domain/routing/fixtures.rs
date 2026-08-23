//! Test-only constructors for runtime snapshots. Every field that tests do
//! not care about gets a sensible default so fixtures state only what they
//! assert on.

use std::{
    collections::{BTreeMap, BTreeSet},
    num::{NonZeroU16, NonZeroU32},
};

use chrono::Utc;

use crate::domain::{
    auth::{ApiKey, ApiKeyDigest, ApiKeyLimits, ApiKeyScope, ApiKeyStatus},
    canonical::identity::{OperationKind, Surface, TransportMode},
    ids::{
        ApiKeyId, ApiKeyLookupId, CredentialVersionId, DurationMs, ProviderId, RouteId, RouteSlug,
        RuntimeGenerationId, TargetId,
    },
    routing::{
        provider::{Capability, Provider, ProviderKind},
        route::{Route, Target},
        snapshot::{RuntimeGeneration, Snapshot},
    },
};

#[must_use]
fn generation(ordinal: u64) -> RuntimeGeneration {
    RuntimeGeneration {
        id: RuntimeGenerationId::new(),
        ordinal,
        activated_at: Utc::now(),
    }
}

#[must_use]
pub fn snapshot(ordinal: u64) -> Snapshot {
    Snapshot {
        generation: generation(ordinal),
        providers: BTreeMap::new(),
        routes: BTreeMap::new(),
        api_keys: BTreeMap::new(),
    }
}

impl Snapshot {
    #[must_use]
    pub fn with_provider(mut self, provider: Provider) -> Self {
        self.providers.insert(provider.id, provider);
        self
    }

    #[must_use]
    pub fn with_route(mut self, route: Route) -> Self {
        self.routes.insert(route.slug.clone(), route);
        self
    }

    #[must_use]
    pub fn with_api_key(mut self, api_key: ApiKey) -> Self {
        self.api_keys.insert(api_key.lookup_id.clone(), api_key);
        self
    }
}

/// An enabled provider with an active credential.
#[must_use]
pub fn provider(
    id: ProviderId,
    kind: ProviderKind,
    capabilities: impl IntoIterator<Item = Capability>,
) -> Provider {
    Provider {
        id,
        name: format!("{kind:?}").to_lowercase(),
        kind,
        enabled: true,
        active_credential: Some(CredentialVersionId::new()),
        capabilities: capabilities.into_iter().collect(),
    }
}

/// Capabilities for one model: the given operations at every transport mode
/// the operation supports on `surface`.
#[must_use]
pub fn capabilities(
    model: &str,
    surface: Surface,
    operations: impl IntoIterator<Item = (OperationKind, TransportMode)>,
) -> BTreeSet<Capability> {
    operations
        .into_iter()
        .map(|(operation, mode)| Capability::new(model, operation, surface, mode))
        .collect()
}

/// Unary and streaming generation plus unary token counting for one model.
#[must_use]
pub fn generation_capabilities(model: &str, surface: Surface) -> BTreeSet<Capability> {
    capabilities(
        model,
        surface,
        [
            (OperationKind::Generation, TransportMode::Unary),
            (OperationKind::Generation, TransportMode::Streaming),
            (OperationKind::TokenCount, TransportMode::Unary),
        ],
    )
}

#[must_use]
pub fn target(provider_id: ProviderId, upstream_model: &str) -> Target {
    Target {
        id: TargetId::new(),
        routing_id: None,
        provider_id,
        upstream_model: upstream_model.to_owned(),
        priority: 0,
        weight: NonZeroU32::new(1).expect("non-zero"),
        timeout: DurationMs::new(4_000),
    }
}

/// A route that may try every target once.
#[must_use]
pub fn route(
    slug: &str,
    operations: impl IntoIterator<Item = OperationKind>,
    targets: Vec<Target>,
) -> Route {
    Route {
        id: RouteId::new(),
        routing_id: None,
        slug: RouteSlug::parse(slug).expect("fixture slug is valid"),
        operations: operations.into_iter().collect(),
        overall_timeout: DurationMs::new(5_000),
        max_attempts: NonZeroU16::new(u16::try_from(targets.len().max(1)).expect("fixture fits"))
            .expect("non-zero"),
        targets,
    }
}

/// An active, unrestricted key.
#[must_use]
pub fn api_key(
    lookup_id: ApiKeyLookupId,
    digest: ApiKeyDigest,
    scopes: impl IntoIterator<Item = ApiKeyScope>,
) -> ApiKey {
    ApiKey {
        id: ApiKeyId::new(),
        lookup_id,
        digest,
        status: ApiKeyStatus::Active,
        expires_at: None,
        scopes: scopes.into_iter().collect(),
        allowed_routes: BTreeSet::new(),
        limits: ApiKeyLimits::default(),
    }
}

/// A copy of `snapshot` published as the next generation.
#[must_use]
pub fn next_generation(snapshot: &Snapshot) -> Snapshot {
    Snapshot {
        generation: generation(snapshot.generation.ordinal + 1),
        providers: snapshot.providers.clone(),
        routes: snapshot.routes.clone(),
        api_keys: snapshot.api_keys.clone(),
    }
}

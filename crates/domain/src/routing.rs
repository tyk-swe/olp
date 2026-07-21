use std::{
    cmp::Ordering,
    collections::{BTreeMap, BTreeSet, HashSet},
    fmt,
    num::{NonZeroU16, NonZeroU32},
    str::FromStr,
};

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{
    ApiKey, CredentialVersionId, DurationMs, OperationKind, ProviderId, RouteId, RouteSlug,
    RuntimeGenerationId, Surface, TargetId, TransportMode,
};

#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ProviderKind {
    #[serde(rename = "openai")]
    OpenAi,
    Anthropic,
    Gemini,
    VertexAi,
    Bedrock,
    #[serde(rename = "azure_openai")]
    AzureOpenAi,
    #[serde(rename = "openai_compatible")]
    OpenAiCompatible,
}

impl ProviderKind {
    pub const ALL: [Self; 7] = [
        Self::OpenAi,
        Self::Anthropic,
        Self::Gemini,
        Self::VertexAi,
        Self::Bedrock,
        Self::AzureOpenAi,
        Self::OpenAiCompatible,
    ];

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::OpenAi => "openai",
            Self::Anthropic => "anthropic",
            Self::Gemini => "gemini",
            Self::VertexAi => "vertex_ai",
            Self::Bedrock => "bedrock",
            Self::AzureOpenAi => "azure_openai",
            Self::OpenAiCompatible => "openai_compatible",
        }
    }

    /// Returns whether this provider family can serve a reviewed capability
    /// tuple. Connector-specific request validation remains at the adapter
    /// boundary; this is the canonical configuration eligibility policy.
    #[must_use]
    pub const fn supports_capability(
        self,
        operation: OperationKind,
        surface: Surface,
        mode: TransportMode,
    ) -> bool {
        let shared_canonical_operation = matches!(
            surface,
            Surface::OpenAi | Surface::Anthropic | Surface::Gemini
        ) && matches!(
            (operation, mode),
            (
                OperationKind::Generation,
                TransportMode::Unary | TransportMode::Streaming
            ) | (OperationKind::TokenCount, TransportMode::Unary)
        );

        match self {
            Self::Anthropic | Self::Gemini | Self::VertexAi | Self::Bedrock => {
                shared_canonical_operation
            }
            Self::OpenAi | Self::OpenAiCompatible | Self::AzureOpenAi => {
                shared_canonical_operation
                    || (matches!(surface, Surface::OpenAi)
                        && matches!(
                            (operation, mode),
                            (
                                OperationKind::Embeddings
                                    | OperationKind::ImageVariation
                                    | OperationKind::Moderation,
                                TransportMode::Unary
                            ) | (
                                OperationKind::ImageGeneration
                                    | OperationKind::ImageEdit
                                    | OperationKind::Speech
                                    | OperationKind::Transcription,
                                TransportMode::Unary | TransportMode::Streaming
                            ) | (OperationKind::VideoCreate, TransportMode::Async)
                                | (
                                    OperationKind::VideoList
                                        | OperationKind::VideoGet
                                        | OperationKind::VideoContent
                                        | OperationKind::VideoDelete,
                                    TransportMode::Unary
                                )
                        ))
            }
        }
    }

    /// Iterates the reviewed capability tuples supported by this provider
    /// family. This is intentionally derived from [`Self::supports_capability`]
    /// so API consumers cannot drift from configuration validation.
    pub fn supported_capabilities(
        self,
    ) -> impl Iterator<Item = (OperationKind, Surface, TransportMode)> {
        OperationKind::ALL.into_iter().flat_map(move |operation| {
            Surface::ALL.into_iter().flat_map(move |surface| {
                TransportMode::ALL
                    .into_iter()
                    .filter(move |mode| self.supports_capability(operation, surface, *mode))
                    .map(move |mode| (operation, surface, mode))
            })
        })
    }
}

impl FromStr for ProviderKind {
    type Err = InvalidProviderKind;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        match value {
            "openai" => Ok(Self::OpenAi),
            "anthropic" => Ok(Self::Anthropic),
            "gemini" => Ok(Self::Gemini),
            "vertex_ai" => Ok(Self::VertexAi),
            "bedrock" => Ok(Self::Bedrock),
            "azure_openai" => Ok(Self::AzureOpenAi),
            "openai_compatible" => Ok(Self::OpenAiCompatible),
            _ => Err(InvalidProviderKind),
        }
    }
}

impl fmt::Display for ProviderKind {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
#[error("invalid canonical provider kind")]
pub struct InvalidProviderKind;

#[derive(Clone, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
pub struct Capability {
    pub model: String,
    pub operation: OperationKind,
    pub surface: Surface,
    pub mode: TransportMode,
}

impl Capability {
    #[must_use]
    pub fn new(
        model: impl Into<String>,
        operation: OperationKind,
        surface: Surface,
        mode: TransportMode,
    ) -> Self {
        Self {
            model: model.into(),
            operation,
            surface,
            mode,
        }
    }
}

#[derive(Clone, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
pub struct CapabilityKey {
    pub provider_id: ProviderId,
    pub model: String,
    pub operation: OperationKind,
    pub surface: Surface,
    pub mode: TransportMode,
}

impl CapabilityKey {
    #[must_use]
    pub fn without_provider(&self) -> Capability {
        Capability {
            model: self.model.clone(),
            operation: self.operation,
            surface: self.surface,
            mode: self.mode,
        }
    }
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Provider {
    pub id: ProviderId,
    pub name: String,
    pub kind: ProviderKind,
    pub enabled: bool,
    pub active_credential: Option<CredentialVersionId>,
    #[serde(default)]
    pub capabilities: BTreeSet<Capability>,
}

impl Provider {
    #[must_use]
    pub fn supports(
        &self,
        model: &str,
        operation: OperationKind,
        surface: Surface,
        mode: TransportMode,
    ) -> bool {
        self.enabled
            && self
                .capabilities
                .contains(&Capability::new(model, operation, surface, mode))
    }
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Target {
    pub id: TargetId,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub routing_id: Option<TargetId>,
    pub provider_id: ProviderId,
    #[serde(rename = "provider_model")]
    pub upstream_model: String,
    pub priority: u16,
    pub weight: NonZeroU32,
    pub timeout: DurationMs,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Route {
    pub id: RouteId,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub routing_id: Option<RouteId>,
    pub slug: RouteSlug,
    #[serde(default)]
    pub operations: BTreeSet<OperationKind>,
    pub overall_timeout: DurationMs,
    pub max_attempts: NonZeroU16,
    pub targets: Vec<Target>,
}

impl Route {
    pub fn validate(&self) -> Result<(), RouteValidationError> {
        if self.overall_timeout.is_zero() {
            return Err(RouteValidationError::ZeroOverallTimeout);
        }
        if self.targets.is_empty() {
            return Err(RouteValidationError::NoTargets);
        }
        if usize::from(self.max_attempts.get()) > self.targets.len() {
            return Err(RouteValidationError::AttemptsExceedTargets);
        }

        let mut target_ids = HashSet::with_capacity(self.targets.len());
        for target in &self.targets {
            if target.timeout.is_zero() {
                return Err(RouteValidationError::ZeroTargetTimeout {
                    target_id: target.id,
                });
            }
            if target.timeout.get() > self.overall_timeout.get() {
                return Err(RouteValidationError::TargetTimeoutExceedsRoute {
                    target_id: target.id,
                });
            }
            if !target_ids.insert(target.id) {
                return Err(RouteValidationError::DuplicateTarget {
                    target_id: target.id,
                });
            }
        }

        Ok(())
    }
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
pub enum RouteValidationError {
    #[error("route must contain at least one target")]
    NoTargets,
    #[error("route maximum attempts cannot exceed its target count")]
    AttemptsExceedTargets,
    #[error("route overall timeout must be greater than zero")]
    ZeroOverallTimeout,
    #[error("target {target_id} timeout must be greater than zero")]
    ZeroTargetTimeout { target_id: TargetId },
    #[error("target {target_id} timeout exceeds the route overall timeout")]
    TargetTimeoutExceedsRoute { target_id: TargetId },
    #[error("target ID {target_id} appears more than once")]
    DuplicateTarget { target_id: TargetId },
}

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
    pub api_keys: BTreeMap<crate::ApiKeyLookupId, ApiKey>,
}

impl RuntimeSnapshot {
    /// Decodes an immutable runtime release written before the external OpenAI
    /// naming change. Compatibility is intentionally limited to enum-bearing
    /// snapshot fields; user-controlled names, models, and route values are not
    /// rewritten.
    pub fn from_persisted_slice(payload: &[u8]) -> Result<Self, serde_json::Error> {
        let mut value: serde_json::Value = serde_json::from_slice(payload)?;
        if let Some(providers) = value
            .get_mut("providers")
            .and_then(serde_json::Value::as_object_mut)
        {
            for provider in providers.values_mut() {
                if let Some(kind) = provider.get_mut("kind") {
                    let replacement = match kind.as_str() {
                        Some("open_ai") => Some("openai"),
                        Some("azure_open_ai") => Some("azure_openai"),
                        Some("open_ai_compatible") => Some("openai_compatible"),
                        _ => None,
                    };
                    if let Some(replacement) = replacement {
                        *kind = serde_json::Value::String(replacement.to_owned());
                    }
                }
                if let Some(capabilities) = provider
                    .get_mut("capabilities")
                    .and_then(serde_json::Value::as_array_mut)
                {
                    for capability in capabilities {
                        if let Some(surface) = capability.get_mut("surface")
                            && surface.as_str() == Some("open_ai")
                        {
                            *surface = serde_json::Value::String("openai".to_owned());
                        }
                    }
                }
            }
        }
        serde_json::from_value(value)
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
        map_key: crate::ApiKeyLookupId,
        key_lookup_id: crate::ApiKeyLookupId,
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

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AttemptPlan {
    pub generation_id: RuntimeGenerationId,
    pub route_id: RouteId,
    pub target_id: TargetId,
    pub provider_id: ProviderId,
    pub provider_kind: ProviderKind,
    pub upstream_model: String,
    pub timeout: DurationMs,
    pub priority: u16,
}

pub fn select_attempts(
    snapshot: &RuntimeSnapshot,
    route_slug: &RouteSlug,
    operation: OperationKind,
    surface: Surface,
    mode: TransportMode,
    affinity_key: &[u8],
) -> Result<Vec<AttemptPlan>, RoutingError> {
    select_attempts_filtered(
        snapshot,
        route_slug,
        operation,
        surface,
        mode,
        affinity_key,
        |_, _| true,
    )
}

/// Selects deterministic attempts after applying a concrete request-level
/// eligibility predicate. The predicate runs before priority/weight ordering
/// and `max_attempts`, so an unrepresentable high-ranked target cannot hide a
/// representable lower-ranked target.
pub fn select_attempts_filtered(
    snapshot: &RuntimeSnapshot,
    route_slug: &RouteSlug,
    operation: OperationKind,
    surface: Surface,
    mode: TransportMode,
    affinity_key: &[u8],
    mut eligible: impl FnMut(&Provider, &Target) -> bool,
) -> Result<Vec<AttemptPlan>, RoutingError> {
    let route = snapshot
        .routes
        .get(route_slug)
        .ok_or_else(|| RoutingError::RouteNotFound(route_slug.clone()))?;

    if !route.operations.contains(&operation) {
        return Err(RoutingError::OperationNotSupported {
            route: route_slug.clone(),
            operation,
        });
    }

    let mut groups: BTreeMap<u16, Vec<RankedTarget<'_>>> = BTreeMap::new();
    for target in &route.targets {
        let Some(provider) = snapshot.providers.get(&target.provider_id) else {
            continue;
        };
        if !provider.supports(&target.upstream_model, operation, surface, mode) {
            continue;
        }
        if !eligible(provider, target) {
            continue;
        }

        groups
            .entry(target.priority)
            .or_default()
            .push(RankedTarget {
                target,
                provider,
                score: weighted_rendezvous_score(
                    route.routing_id.unwrap_or(route.id),
                    target.routing_id.unwrap_or(target.id),
                    target.weight,
                    operation,
                    surface,
                    mode,
                    affinity_key,
                ),
            });
    }

    let maximum = usize::from(route.max_attempts.get());
    let mut attempts = Vec::with_capacity(maximum);
    for (priority, mut group) in groups {
        group.sort_by(|left, right| {
            right
                .score
                .partial_cmp(&left.score)
                .unwrap_or(Ordering::Equal)
                .then_with(|| {
                    left.target
                        .routing_id
                        .unwrap_or(left.target.id)
                        .cmp(&right.target.routing_id.unwrap_or(right.target.id))
                })
        });

        for ranked in group {
            attempts.push(AttemptPlan {
                generation_id: snapshot.generation.id,
                route_id: route.id,
                target_id: ranked.target.id,
                provider_id: ranked.provider.id,
                provider_kind: ranked.provider.kind,
                upstream_model: ranked.target.upstream_model.clone(),
                timeout: ranked.target.timeout,
                priority,
            });
            if attempts.len() == maximum {
                return Ok(attempts);
            }
        }
    }

    if attempts.is_empty() {
        return Err(RoutingError::NoEligibleTargets {
            route: route_slug.clone(),
            operation,
            surface,
            mode,
        });
    }

    Ok(attempts)
}

struct RankedTarget<'a> {
    target: &'a Target,
    provider: &'a Provider,
    score: f64,
}

/// Returns the deterministic weighted-rendezvous score used for route target
/// ordering. Configuration simulations call this same primitive as live routing.
#[must_use]
pub fn weighted_rendezvous_score(
    route_routing_id: RouteId,
    target_routing_id: TargetId,
    weight: NonZeroU32,
    operation: OperationKind,
    surface: Surface,
    mode: TransportMode,
    affinity_key: &[u8],
) -> f64 {
    let mut hasher = Sha256::new();
    hasher.update(b"olp-v2-weighted-rendezvous\0");
    hasher.update(route_routing_id.as_uuid().as_bytes());
    hasher.update(target_routing_id.as_uuid().as_bytes());
    hasher.update([operation_hash_tag(operation)]);
    hasher.update([surface_hash_tag(surface)]);
    hasher.update([mode_hash_tag(mode)]);
    hasher.update(
        u64::try_from(affinity_key.len())
            .unwrap_or(u64::MAX)
            .to_be_bytes(),
    );
    hasher.update(affinity_key);
    let digest = hasher.finalize();
    let raw = u64::from_be_bytes(digest[..8].try_into().expect("SHA-256 prefix has 8 bytes"));

    // Use the high 53 bits, which an f64 can represent exactly, and keep the
    // sample strictly between zero and one.
    let sample = ((raw >> 11) as f64 + 1.0) / ((1_u64 << 53) as f64 + 1.0);
    f64::from(weight.get()) / -sample.ln()
}

const fn operation_hash_tag(operation: OperationKind) -> u8 {
    match operation {
        OperationKind::Generation => 0,
        OperationKind::Embeddings => 1,
        OperationKind::TokenCount => 2,
        OperationKind::ImageGeneration => 3,
        OperationKind::ImageEdit => 4,
        OperationKind::ImageVariation => 5,
        OperationKind::Speech => 6,
        OperationKind::Transcription => 7,
        OperationKind::VideoCreate => 8,
        OperationKind::VideoList => 9,
        OperationKind::VideoGet => 10,
        OperationKind::VideoContent => 11,
        OperationKind::VideoDelete => 12,
        OperationKind::Moderation => 13,
        OperationKind::ModelList => 14,
        OperationKind::ModelGet => 15,
    }
}

const fn surface_hash_tag(surface: Surface) -> u8 {
    match surface {
        Surface::OpenAi => 0,
        Surface::Anthropic => 1,
        Surface::Gemini => 2,
    }
}

const fn mode_hash_tag(mode: TransportMode) -> u8 {
    match mode {
        TransportMode::Unary => 0,
        TransportMode::Streaming => 1,
        TransportMode::Async => 2,
    }
}

#[derive(Clone, Debug, Error, Eq, PartialEq)]
pub enum RoutingError {
    #[error("route {0} was not found in the pinned runtime generation")]
    RouteNotFound(RouteSlug),
    #[error("route {route} does not support operation {operation:?}")]
    OperationNotSupported {
        route: RouteSlug,
        operation: OperationKind,
    },
    #[error("route {route} has no target for {operation:?} on {surface:?} in {mode:?} mode")]
    NoEligibleTargets {
        route: RouteSlug,
        operation: OperationKind,
        surface: Surface,
        mode: TransportMode,
    },
}

#[cfg(test)]
mod tests {
    use std::num::NonZeroU32;

    use uuid::Uuid;

    use super::*;

    #[test]
    fn weighted_rendezvous_score_is_deterministic_and_weighted() {
        let route = RouteId::from_uuid(Uuid::nil());
        let target = TargetId::from_uuid(Uuid::from_u128(1));
        let first = weighted_rendezvous_score(
            route,
            target,
            NonZeroU32::new(3).unwrap(),
            OperationKind::Generation,
            Surface::OpenAi,
            TransportMode::Unary,
            b"request-1",
        );
        let second = weighted_rendezvous_score(
            route,
            target,
            NonZeroU32::new(3).unwrap(),
            OperationKind::Generation,
            Surface::OpenAi,
            TransportMode::Unary,
            b"request-1",
        );
        let heavier = weighted_rendezvous_score(
            route,
            target,
            NonZeroU32::new(6).unwrap(),
            OperationKind::Generation,
            Surface::OpenAi,
            TransportMode::Unary,
            b"request-1",
        );

        assert_eq!(first, second);
        assert_eq!(heavier, first * 2.0);
    }

    #[test]
    fn provider_capability_policy_covers_shared_and_provider_specific_tuples() {
        assert!(ProviderKind::Bedrock.supports_capability(
            OperationKind::Generation,
            Surface::Anthropic,
            TransportMode::Streaming,
        ));
        assert!(ProviderKind::OpenAiCompatible.supports_capability(
            OperationKind::Embeddings,
            Surface::OpenAi,
            TransportMode::Unary,
        ));
        assert!(!ProviderKind::Anthropic.supports_capability(
            OperationKind::Embeddings,
            Surface::Anthropic,
            TransportMode::Unary,
        ));
        assert!(!ProviderKind::OpenAiCompatible.supports_capability(
            OperationKind::Moderation,
            Surface::Gemini,
            TransportMode::Unary,
        ));

        let options = ProviderKind::OpenAiCompatible
            .supported_capabilities()
            .collect::<Vec<_>>();
        assert!(options.contains(&(
            OperationKind::Generation,
            Surface::OpenAi,
            TransportMode::Streaming,
        )));
        assert!(!options.contains(&(
            OperationKind::Moderation,
            Surface::Gemini,
            TransportMode::Unary,
        )));
    }
}

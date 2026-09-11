use std::num::NonZeroU32;

use sha2::Digest;
use sha2::Sha256;
use thiserror::Error;

use crate::ids::DurationMs;
use crate::ids::ProviderId;
use crate::ids::RouteId;
use crate::ids::RouteSlug;
use crate::ids::RuntimeGenerationId;
use crate::ids::TargetId;
use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::identity::TransportMode;

use crate::providers::runtime_model::ProviderKind;
use crate::runtime::snapshot::Snapshot;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AttemptPlan {
    /// Effective bound after routing preferences narrow the route's budget.
    pub attempt_limit: Option<std::num::NonZeroU16>,
    /// Current published quotas pinned for this request. Standalone probes can
    /// leave these absent and use the connector's release-time configuration.
    pub connection_limits: Option<crate::providers::options::ConnectionLimits>,
    pub credential_limits: Option<crate::providers::options::ConnectionLimits>,
    pub routing_policy: Option<crate::routes::policy::AppliedRoutingPolicy>,
    pub credential_slot_id: Option<uuid::Uuid>,
    pub credential_version_id: Option<uuid::Uuid>,
    pub pricing_revision_id: Option<uuid::Uuid>,
    pub generation_id: RuntimeGenerationId,
    pub route_id: RouteId,
    pub target_id: TargetId,
    /// The target's revision-stable identity. `target_id` is minted fresh on
    /// every route activation, so anything that must outlive a republish —
    /// circuit-breaker health above all — keys on this instead.
    pub routing_id: TargetId,
    pub provider_id: ProviderId,
    pub provider_revision_id: uuid::Uuid,
    pub provider_kind: ProviderKind,
    pub upstream_model: String,
    pub timeout: DurationMs,
    pub priority: u16,
}

/// Selects deterministic attempts for a capability probe: default routing
/// preferences, no API key, and no request-level eligibility predicate.
pub fn select_attempts(
    snapshot: &Snapshot,
    route_slug: &RouteSlug,
    operation: OperationKind,
    surface: Surface,
    mode: TransportMode,
    affinity_key: &[u8],
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
    crate::inference::provider_selection::explain_capability(
        snapshot,
        route_slug,
        operation,
        surface,
        mode,
        affinity_key,
        None,
        &crate::routes::policy::RoutingPreferences::default(),
    )
    .ok()
    .filter(|selection| !selection.attempts.is_empty())
    .map(|selection| selection.attempts)
    .ok_or(RoutingError::NoEligibleTargets {
        route: route_slug.clone(),
        operation,
        surface,
        mode,
    })
}

/// Returns the deterministic weighted-rendezvous score used for route target
/// ordering. Configuration simulations call this same primitive as live
/// routing.
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

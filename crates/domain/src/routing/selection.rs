use std::{cmp::Ordering, collections::BTreeMap, num::NonZeroU32};

use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{
    DurationMs, OperationKind, ProviderId, RouteId, RouteSlug, RuntimeGenerationId, Surface,
    TargetId, TransportMode,
};

use super::{Provider, ProviderKind, RuntimeSnapshot, Target};

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AttemptPlan {
    pub generation_id: RuntimeGenerationId,
    pub route_id: RouteId,
    /// Stable identity used for health overlays across runtime revisions.
    pub target_routing_id: TargetId,
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

    let route_id = route.routing_id.unwrap_or(route.id);
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
                    route_id,
                    target.stable_id(),
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
                .then_with(|| left.target.stable_id().cmp(&right.target.stable_id()))
        });

        for ranked in group {
            attempts.push(AttemptPlan {
                generation_id: snapshot.generation.id,
                route_id: route.id,
                target_routing_id: ranked.target.stable_id(),
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

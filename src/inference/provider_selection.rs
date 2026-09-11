//! One policy-aware selection engine for execution, previews, and the playground.
use crate::ids::{ProviderId, RouteSlug};
use crate::inference::error::Error;
use crate::protocols::canonical::{
    identity::{Surface, TransportMode},
    requests::Operation,
};
use crate::providers::{options::ModelMetadata, runtime_model::Provider};
use crate::routes::{
    model::Target,
    policy::*,
    selection::{AttemptPlan, weighted_rendezvous_score},
};
use crate::runtime::snapshot::Snapshot;
use serde::Serialize;
use std::collections::BTreeSet;
use std::num::NonZeroU32;
use utoipa::ToSchema;

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct RoutingDecision {
    pub target_id: uuid::Uuid,
    pub provider_id: uuid::Uuid,
    pub upstream_model: String,
    pub credential_slot_id: Option<uuid::Uuid>,
    pub eligible: bool,
    pub reason: Option<String>,
    pub attempt: Option<usize>,
    pub vendor_id: Option<String>,
    pub priority: u16,
    pub strategy: RoutingStrategy,
    pub price: Option<RoutingPrice>,
    pub performance: Option<crate::inference::performance::Measurement>,
    pub metadata_observed_at: Option<chrono::DateTime<chrono::Utc>>,
}

pub struct Selection {
    pub attempts: Vec<AttemptPlan>,
    pub decisions: Vec<RoutingDecision>,
}

struct Ranked {
    attempt: AttemptPlan,
    decision: usize,
    order: usize,
    score: f64,
    slot_priority: u16,
    slot_score: f64,
    price: Option<rust_decimal::Decimal>,
    metric: Option<u64>,
    threshold: u8,
}

#[allow(clippy::too_many_arguments)]
pub fn select(
    snapshot: &Snapshot,
    route_slug: &RouteSlug,
    operation: &Operation,
    surface: Surface,
    mode: TransportMode,
    affinity: &[u8],
    key: Option<&crate::access::policy::ApiKey>,
    preferences: &RoutingPreferences,
    available: impl FnMut(&Provider, &Target) -> bool,
) -> Result<Selection, Error> {
    let selection = explain(
        snapshot,
        route_slug,
        operation,
        surface,
        mode,
        affinity,
        key,
        preferences,
        available,
    )?;
    if selection.attempts.is_empty() {
        let supported = selection
            .decisions
            .iter()
            .any(|d| d.reason.as_deref() != Some("unsupported_capability"));
        let representable = selection.decisions.iter().any(|d| {
            !matches!(
                d.reason.as_deref(),
                Some("unsupported_capability" | "unrepresentable_request")
            )
        });
        return Err(if supported && !representable {
            Error::invalid_request(
                "No route target can represent this request without semantic loss.",
            )
        } else if representable {
            Error::unavailable("no_eligible_provider")
        } else {
            Error::not_found("No eligible route target supports this operation")
        });
    }
    Ok(selection)
}

#[allow(clippy::too_many_arguments)]
pub fn explain(
    snapshot: &Snapshot,
    route_slug: &RouteSlug,
    operation: &Operation,
    surface: Surface,
    mode: TransportMode,
    affinity: &[u8],
    key: Option<&crate::access::policy::ApiKey>,
    preferences: &RoutingPreferences,
    available: impl FnMut(&Provider, &Target) -> bool,
) -> Result<Selection, Error> {
    explain_inner(
        snapshot,
        route_slug,
        Some(operation),
        operation.kind(),
        surface,
        mode,
        affinity,
        key,
        preferences,
        available,
    )
}

#[allow(clippy::too_many_arguments)]
pub(crate) fn explain_capability(
    snapshot: &Snapshot,
    route_slug: &RouteSlug,
    operation: crate::protocols::canonical::identity::OperationKind,
    surface: Surface,
    mode: TransportMode,
    affinity: &[u8],
    key: Option<&crate::access::policy::ApiKey>,
    preferences: &RoutingPreferences,
) -> Result<Selection, Error> {
    explain_inner(
        snapshot,
        route_slug,
        None,
        operation,
        surface,
        mode,
        affinity,
        key,
        preferences,
        |_, _| true,
    )
}

#[allow(clippy::too_many_arguments)]
fn explain_inner(
    snapshot: &Snapshot,
    route_slug: &RouteSlug,
    operation: Option<&Operation>,
    operation_kind: crate::protocols::canonical::identity::OperationKind,
    surface: Surface,
    mode: TransportMode,
    affinity: &[u8],
    key: Option<&crate::access::policy::ApiKey>,
    preferences: &RoutingPreferences,
    mut available: impl FnMut(&Provider, &Target) -> bool,
) -> Result<Selection, Error> {
    preferences.validate().map_err(Error::invalid_request)?;
    let route = snapshot
        .routes
        .get(route_slug)
        .ok_or_else(|| Error::not_found("Route not found"))?;
    if !route.operations.contains(&operation_kind) {
        return Err(Error::not_found("Operation not supported by route"));
    }
    let empty = RoutingPolicy::default();
    let policies = [
        &snapshot.routing.installation,
        snapshot.routing.routes.get(route_slug).unwrap_or(&empty),
        key.map(|k| &k.routing_policy).unwrap_or(&empty),
    ];
    let mut effective = RoutingPreferences::default();
    for policy in policies {
        effective.overlay(&policy.defaults);
    }
    effective.overlay(preferences);
    let strategy = effective.strategy.unwrap_or_default();
    if policies.iter().any(|p| {
        p.allowed_strategies
            .as_ref()
            .is_some_and(|allowed| !allowed.contains(&strategy))
    }) {
        return Err(Error::invalid_request("Routing strategy is not permitted"));
    }
    if strategy == RoutingStrategy::Throughput
        && operation_kind != crate::protocols::canonical::identity::OperationKind::Generation
    {
        return Err(Error::invalid_request(
            "Throughput routing requires generation",
        ));
    }
    let policy_digest = crate::crypto::key_material::hex_lower(
        &crate::database::idempotency::fingerprint(&(policies, preferences))
            .map_err(|_| Error::invalid_request("Invalid routing policy"))?,
    );
    let attempt_limit = if effective.allow_fallbacks == Some(false) && effective.order.is_none() {
        std::num::NonZeroU16::MIN
    } else {
        route.max_attempts
    };
    let applied = AppliedRoutingPolicy {
        pricing_pinned: snapshot.routing.version > 0 || !snapshot.routing.prices.is_empty(),
        vendor_id: None,
        strategy,
        digest: policy_digest,
    };
    let require_parameters = preferences.constraints.require_parameters
        || policies
            .iter()
            .any(|p| p.constraints.require_parameters || p.defaults.constraints.require_parameters);
    let required = operation
        .filter(|_| require_parameters)
        .map(requested_parameters)
        .unwrap_or_default();
    let default_options = crate::providers::options::ConnectionOptions::default();
    let mut ranked = Vec::new();
    let mut decisions = Vec::new();
    let performance = crate::inference::performance::snapshot();
    for target in &route.targets {
        let Some(provider) = snapshot.providers.get(&target.provider_id) else {
            decisions.push(RoutingDecision {
                target_id: target.id.as_uuid(),
                provider_id: target.provider_id.as_uuid(),
                upstream_model: target.upstream_model.clone(),
                credential_slot_id: None,
                eligible: false,
                reason: Some("provider_unavailable".into()),
                attempt: None,
                vendor_id: None,
                priority: target.priority,
                strategy,
                price: None,
                performance: None,
                metadata_observed_at: None,
            });
            continue;
        };
        let options = snapshot.routing.providers.get(&provider.id);
        // Legacy snapshots do not include endpoint-derived vendor identity.
        // Unknown identity cannot satisfy a newly required vendor constraint.
        let vendor = options.and_then(|o| o.vendor_id.as_deref());
        let metadata = options.and_then(|o| o.models.get(&target.upstream_model));
        let price = snapshot.routing.price(
            provider.id.as_uuid(),
            &target.upstream_model,
            operation_kind.as_str(),
        );
        let measurement = performance.get(
            provider.id.as_uuid(),
            &target.upstream_model,
            operation_kind,
            mode,
        );
        let reason = if !provider.supports(&target.upstream_model, operation_kind, surface, mode) {
            Some("unsupported_capability")
        } else {
            if operation.is_some_and(|operation| {
                let options = options.unwrap_or(&default_options);
                let translated =
                    crate::providers::profiles::operation(operation, options, provider.kind);
                crate::providers::profiles::validate(operation, options).is_err()
                    || crate::inference::selection::validate_for_provider(
                        &translated,
                        provider.kind,
                        &target.upstream_model,
                    )
                    .is_err()
            }) {
                Some("unrepresentable_request")
            } else {
                policies
                    .iter()
                    .flat_map(|p| [&p.constraints, &p.defaults.constraints])
                    .chain(std::iter::once(&preferences.constraints))
                    .find_map(|constraint| {
                        excluded(constraint, provider.id, vendor, metadata, price, &required)
                    })
                    .or_else(|| (!available(provider, target)).then_some("provider_unavailable"))
            }
        };
        let slots = snapshot.routing.credentials.get(&provider.id);
        let pool_empty = slots.is_some_and(Vec::is_empty);
        let candidates: Vec<Option<&crate::providers::pool::CredentialSlot>> = match slots {
            Some(slots) if !slots.is_empty() => slots.iter().map(Some).collect(),
            _ => vec![None],
        };

        let authority = snapshot
            .routing
            .credential_authority
            .as_ref()
            .map(|authority| authority.get(&provider.id));
        for slot in candidates {
            let slot_id = slot.map_or(provider.id.as_uuid(), |s| s.id);
            let authority_slot = authority
                .flatten()
                .and_then(|slots| slots.iter().find(|s| s.id == slot_id));
            // Current published restrictions take precedence over the release's slot.
            let effective_slot = authority_slot.or(slot);
            let credential_version_id = slot
                .and_then(|s| s.credential_version_id)
                .or_else(|| provider.active_credential.map(|c| c.as_uuid()));
            let pinned_reason = preferences
                .required_credential_version
                .filter(|required| credential_version_id != Some(*required))
                .map(|_| "credential_pin_mismatch");
            let cooling = preferences
                .cooling_slots
                .contains(&slot_id)
                .then_some("credential_cooling_down");
            let permits = |s: &crate::providers::pool::CredentialSlot| {
                s.permits(
                    &target.upstream_model,
                    route_slug.as_str(),
                    key.map(|k| k.id.as_uuid()),
                )
            };
            let reason = reason
                .or(pinned_reason)
                .or(cooling)
                .or(pool_empty.then_some("credential_unavailable"))
                .or_else(|| {
                    if preferences.required_credential_version.is_some() {
                        return None;
                    }
                    // A published authority that knows this provider, or is asked
                    // about a release slot, must list the slot; both views must permit.
                    let unlisted = authority.is_some_and(|current| {
                        (slot.is_some() || current.is_some()) && authority_slot.is_none()
                    });
                    (unlisted
                        || effective_slot.is_some_and(|s| !permits(s))
                        || slot.is_some_and(|s| !permits(s)))
                    .then_some("credential_restricted")
                });
            let index = decisions.len();
            decisions.push(RoutingDecision {
                target_id: target.id.as_uuid(),
                provider_id: provider.id.as_uuid(),
                upstream_model: target.upstream_model.clone(),
                credential_slot_id: slot.map(|s| s.id),
                eligible: reason.is_none(),
                reason: reason.map(str::to_owned),
                attempt: None,
                vendor_id: vendor.map(str::to_owned),
                priority: target.priority,
                strategy,
                price: price.cloned(),
                performance: measurement.cloned(),
                metadata_observed_at: metadata.and_then(|m| m.observed_at),
            });
            if reason.is_some() {
                continue;
            }
            let order = effective
                .order
                .as_ref()
                .and_then(|order| order.iter().position(|s| matches(s, provider.id, vendor)))
                .unwrap_or(usize::MAX);
            if effective.allow_fallbacks == Some(false)
                && effective.order.is_some()
                && order == usize::MAX
            {
                decisions[index].eligible = false;
                decisions[index].reason = Some("outside_preferred_order".into());
                continue;
            }
            let score = weighted_rendezvous_score(
                route.routing_id,
                target.routing_id,
                target.weight,
                operation_kind,
                surface,
                mode,
                affinity,
            );
            let slot_score = slot.map_or(0.0, |s| {
                weighted_rendezvous_score(
                    route.routing_id,
                    crate::ids::TargetId::from_uuid(s.id),
                    NonZeroU32::new(s.weight).unwrap_or(NonZeroU32::MIN),
                    operation_kind,
                    surface,
                    mode,
                    affinity,
                )
            });
            ranked.push(Ranked {
                decision: index,
                order,
                score,
                slot_priority: slot.map_or(0, |s| s.priority),
                slot_score,
                price: price.and_then(comparison_price),
                metric: measurement.and_then(|m| {
                    if strategy == RoutingStrategy::Throughput {
                        m.output_tokens_per_second.map(|n| u64::MAX - n)
                    } else {
                        Some(m.latency_ms)
                    }
                }),
                threshold: match measurement {
                    Some(m)
                        if effective
                            .preferred_max_latency_ms
                            .is_none_or(|n| m.latency_ms <= n)
                            && effective.preferred_min_throughput.is_none_or(|n| {
                                m.output_tokens_per_second.is_some_and(|v| v >= n)
                            }) =>
                    {
                        0
                    }
                    Some(_) => 1,
                    None if effective.preferred_max_latency_ms.is_some()
                        || effective.preferred_min_throughput.is_some() =>
                    {
                        2
                    }
                    None => 0,
                },
                attempt: AttemptPlan {
                    attempt_limit: Some(attempt_limit),
                    connection_limits: snapshot
                        .routing
                        .connection_limit_authority
                        .as_ref()
                        .and_then(|limits| limits.get(&provider.id))
                        .cloned()
                        .or_else(|| options.and_then(|options| options.limits.clone())),
                    credential_limits: effective_slot
                        .map(crate::providers::pool::CredentialSlot::limits),
                    routing_policy: Some(AppliedRoutingPolicy {
                        vendor_id: vendor.map(str::to_owned),
                        ..applied.clone()
                    }),
                    credential_slot_id: slot.map(|s| s.id),
                    credential_version_id,
                    pricing_revision_id: price.map(|p| p.revision_id),
                    generation_id: snapshot.generation.id,
                    route_id: route.id,
                    target_id: target.id,
                    routing_id: target.routing_id,
                    provider_id: provider.id,
                    provider_revision_id: provider.revision_id,
                    provider_kind: provider.kind,
                    upstream_model: target.upstream_model.clone(),
                    timeout: target.timeout,
                    priority: target.priority,
                },
            });
        }
    }
    ranked.sort_by(|a, b| {
        a.attempt
            .priority
            .cmp(&b.attempt.priority)
            .then(a.order.cmp(&b.order))
            .then(a.threshold.cmp(&b.threshold))
            .then_with(|| match strategy {
                RoutingStrategy::Price => compare_optional(a.price, b.price),
                RoutingStrategy::Latency | RoutingStrategy::Throughput => {
                    compare_optional(a.metric, b.metric)
                }
                RoutingStrategy::Weighted => std::cmp::Ordering::Equal,
            })
            .then_with(|| b.score.total_cmp(&a.score))
            .then(a.attempt.routing_id.cmp(&b.attempt.routing_id))
            .then(a.slot_priority.cmp(&b.slot_priority))
            .then_with(|| b.slot_score.total_cmp(&a.slot_score))
            .then(
                a.attempt
                    .credential_slot_id
                    .cmp(&b.attempt.credential_slot_id),
            )
    });
    let limit = usize::from(attempt_limit.get());
    let mut attempts = Vec::new();
    let mut used = BTreeSet::new();
    for candidate in ranked {
        let identity = (
            candidate.attempt.provider_id,
            candidate.attempt.upstream_model.clone(),
            candidate.attempt.credential_version_id,
        );
        let decision = &mut decisions[candidate.decision];
        if !used.insert(identity) {
            decision.eligible = false;
            decision.reason = Some("duplicate_destination".into());
            continue;
        }
        if attempts.len() == limit {
            decision.reason = Some("attempt_limit".into());
            continue;
        }
        decision.attempt = Some(attempts.len() + 1);
        attempts.push(candidate.attempt);
    }
    Ok(Selection {
        attempts,
        decisions,
    })
}

fn excluded(
    constraint: &RoutingConstraints,
    provider: ProviderId,
    vendor: Option<&str>,
    metadata: Option<&ModelMetadata>,
    price: Option<&RoutingPrice>,
    parameters: &BTreeSet<String>,
) -> Option<&'static str> {
    if constraint
        .only
        .as_ref()
        .is_some_and(|only| !only.iter().any(|s| matches(s, provider, vendor)))
    {
        return Some("provider_not_allowed");
    }
    if constraint
        .ignore
        .iter()
        .any(|s| matches(s, provider, vendor))
    {
        return Some("provider_denied");
    }
    if constraint.regions.as_ref().is_some_and(|allowed| {
        !metadata
            .and_then(|m| m.region.as_ref())
            .is_some_and(|v| allowed.contains(v))
    }) {
        return Some("region_not_allowed");
    }
    if constraint.quantizations.as_ref().is_some_and(|allowed| {
        !metadata
            .and_then(|m| m.quantization.as_ref())
            .is_some_and(|v| allowed.contains(v))
    }) {
        return Some("quantization_not_allowed");
    }
    if constraint.deny_data_collection && metadata.and_then(|m| m.data_collection) != Some(false) {
        return Some("data_collection_policy");
    }
    if constraint.require_zero_data_retention
        && metadata.and_then(|m| m.zero_data_retention) != Some(true)
    {
        return Some("zero_data_retention_required");
    }
    if constraint.require_parameters
        && !parameters.is_empty()
        && !metadata
            .and_then(|m| m.supported_parameters.as_ref())
            .is_some_and(|supported| parameters.is_subset(supported))
    {
        return Some("parameters_not_supported");
    }
    if let Some(ceiling) = &constraint.max_price {
        let Some(price) = price else {
            return Some("price_unknown");
        };
        for (cap, rate) in [
            (&ceiling.input_per_million, &price.input_per_million),
            (&ceiling.output_per_million, &price.output_per_million),
            (&ceiling.unit_price, &price.unit_price),
        ] {
            if let Some(cap) = cap {
                let Some(rate) = rate.as_deref().and_then(|s| decimal(s).ok()) else {
                    return Some("price_unknown");
                };
                if decimal(cap).is_ok_and(|cap| rate > cap) {
                    return Some("price_ceiling");
                }
            }
        }
    }
    None
}

fn comparison_price(price: &RoutingPrice) -> Option<rust_decimal::Decimal> {
    if price.operation == "generation" {
        decimal(price.input_per_million.as_deref()?)
            .ok()?
            .checked_add(decimal(price.output_per_million.as_deref()?).ok()?)
    } else {
        decimal(
            price
                .input_per_million
                .as_deref()
                .or(price.unit_price.as_deref())?,
        )
        .ok()
    }
}

pub(crate) fn compare_optional<T: Ord>(a: Option<T>, b: Option<T>) -> std::cmp::Ordering {
    match (a, b) {
        (Some(a), Some(b)) => a.cmp(&b),
        (Some(_), None) => std::cmp::Ordering::Less,
        (None, Some(_)) => std::cmp::Ordering::Greater,
        _ => std::cmp::Ordering::Equal,
    }
}

fn requested_parameters(operation: &Operation) -> BTreeSet<String> {
    use crate::protocols::canonical::requests::{ImageOperation, VideoOperation};
    let controls = match operation {
        Operation::Generation(request) => vec![
            (
                "max_output_tokens",
                request.parameters.max_output_tokens.is_some(),
            ),
            ("temperature", request.parameters.temperature.is_some()),
            ("top_p", request.parameters.top_p.is_some()),
            ("stop", !request.parameters.stop_sequences.is_empty()),
            (
                "n",
                request.parameters.candidate_count.is_some()
                    && !(request.extensions.source == Some(Surface::Anthropic)
                        && request.parameters.candidate_count == Some(1)),
            ),
            ("seed", request.parameters.seed.is_some()),
            (
                "parallel_tool_calls",
                request.parameters.parallel_tool_calls.is_some(),
            ),
            ("tools", !request.tools.is_empty()),
            ("tool_choice", request.tool_choice.is_some()),
            ("response_format", request.response_format.is_some()),
        ],
        Operation::Embeddings(request) => vec![("dimensions", request.dimensions.is_some())],
        Operation::Images(ImageOperation::Generation(request)) => vec![
            ("n", request.count.is_some()),
            ("size", request.size.is_some()),
        ],
        Operation::Images(ImageOperation::Variation(request)) => vec![
            ("n", request.count.is_some()),
            ("size", request.size.is_some()),
        ],
        Operation::Images(ImageOperation::Edit(request)) => vec![("mask", request.mask.is_some())],
        Operation::Speech(request) => vec![
            ("voice", true),
            ("response_format", request.format.is_some()),
        ],
        Operation::Transcription(request) => vec![
            ("language", request.language.is_some()),
            ("prompt", request.prompt.is_some()),
        ],
        Operation::Video(VideoOperation::Create(request)) => {
            vec![("input_reference", request.input.is_some())]
        }
        _ => Vec::new(),
    };
    let mut result = controls
        .into_iter()
        .filter(|(_, present)| *present)
        .map(|(name, _)| name.to_owned())
        .collect::<BTreeSet<_>>();
    if let Some(extensions) = operation.extensions() {
        for (path, value) in &extensions.values {
            if let Operation::TokenCount(request) = operation
                && let Some(generation) = counted_generation(request, path, value)
            {
                result.extend(requested_parameters(&Operation::Generation(generation)));
            } else if !crate::protocols::canonical::requests::is_delivery_only_extension(path)
                && path != crate::protocols::canonical::requests::MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION
            {
                result.insert(path.trim_start_matches('/').into());
            }
        }
    }
    result
}

/// Count-token wrappers preserve whole source bodies, so inspect their controls
/// with the same translators used for generation instead of requiring the wrapper.
fn counted_generation(
    request: &crate::protocols::canonical::requests::TokenCountRequest,
    path: &str,
    value: &serde_json::Value,
) -> Option<crate::protocols::canonical::requests::GenerationRequest> {
    use crate::protocols::{anthropic, gemini};
    match (request.extensions.source, path) {
        (Some(Surface::Anthropic), anthropic::count::ANTHROPIC_COUNT_REQUEST_EXTENSION) => {
            let source: anthropic::dto::CountTokensRequest =
                serde_json::from_value(value.clone()).ok()?;
            let Operation::Generation(mut generation) =
                anthropic::translate::decode::request(anthropic::dto::MessagesRequest {
                    model: source.model,
                    messages: source.messages,
                    max_tokens: 1,
                    system: source.system,
                    stop_sequences: Vec::new(),
                    temperature: None,
                    top_p: None,
                    tools: source.tools,
                    tool_choice: source.tool_choice,
                    stream: false,
                    extra: source.extra,
                })
                .ok()?
            else {
                return None;
            };
            // These are synthetic generation requirements, absent on countTokens.
            generation.parameters.max_output_tokens = None;
            generation.parameters.candidate_count = None;
            Some(generation)
        }
        (Some(Surface::Gemini), gemini::count::GEMINI_COUNT_REQUEST_EXTENSION) => {
            let source: gemini::dto::CountTokensRequest =
                serde_json::from_value(value.clone()).ok()?;
            let mut input =
                source
                    .generate_content_request
                    .unwrap_or(gemini::dto::GenerateContentRequest {
                        contents: source.contents,
                        ..Default::default()
                    });
            // The body model identifies the counted input, not an optional control.
            input.model = None;
            let Operation::Generation(mut generation) =
                gemini::translate::decode::request(request.route.as_str(), input, false).ok()?
            else {
                return None;
            };
            crate::protocols::extensions::collect_extra(
                "",
                &source.extra,
                &mut generation.extensions.values,
            );
            Some(generation)
        }
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ids::*;
    use crate::protocols::canonical::identity::OperationKind;
    use crate::providers::{
        pool::CredentialSlot,
        runtime_model::{Capability, ProviderKind},
    };
    use crate::routes::model::Route;
    use std::collections::BTreeMap;

    fn fixture() -> (Snapshot, Operation, Vec<ProviderId>) {
        let operation = Operation::Generation(
            crate::providers::openai::certification::probe_generation_request(
                TransportMode::Unary,
                crate::protocols::canonical::requests::SourceExtensions::default(),
            ),
        );
        let slug = operation.route().unwrap().clone();
        let ids = vec![ProviderId::new(), ProviderId::new()];
        let route = Route {
            id: RouteId::new(),
            routing_id: RouteId::new(),
            slug: slug.clone(),
            operations: BTreeSet::from([OperationKind::Generation]),
            overall_timeout: DurationMs::new(1000),
            max_attempts: std::num::NonZeroU16::new(1).unwrap(),
            targets: ids
                .iter()
                .enumerate()
                .map(|(index, id)| Target {
                    id: TargetId::new(),
                    routing_id: TargetId::new(),
                    provider_id: *id,
                    upstream_model: "test-model".into(),
                    priority: index as u16,
                    weight: NonZeroU32::MIN,
                    timeout: DurationMs::new(1000),
                })
                .collect(),
        };
        let snapshot = Snapshot {
            routing: Default::default(),
            generation: crate::runtime::snapshot::RuntimeGeneration {
                id: RuntimeGenerationId::new(),
                ordinal: 1,
                activated_at: chrono::Utc::now(),
            },
            providers: ids
                .iter()
                .map(|id| {
                    (
                        *id,
                        Provider {
                            id: *id,
                            revision_id: uuid::Uuid::now_v7(),
                            name: "test".into(),
                            kind: ProviderKind::OpenAi,
                            enabled: true,
                            active_credential: None,
                            capabilities: BTreeSet::from([Capability::new(
                                "test-model",
                                OperationKind::Generation,
                                Surface::OpenAi,
                                TransportMode::Unary,
                            )]),
                        },
                    )
                })
                .collect(),
            routes: BTreeMap::from([(slug, route)]),
            api_keys: BTreeMap::new(),
        };
        (snapshot, operation, ids)
    }
    fn run(
        snapshot: &Snapshot,
        operation: &Operation,
        preferences: &RoutingPreferences,
    ) -> Result<Selection, Error> {
        select(
            snapshot,
            operation.route().unwrap(),
            operation,
            Surface::OpenAi,
            TransportMode::Unary,
            b"seed",
            None,
            preferences,
            |_, _| true,
        )
    }

    #[test]
    fn strict_parameters_cover_image_audio_and_video_controls() {
        use crate::protocols::canonical::requests::*;
        let (base, generation, ids) = fixture();
        let slug = generation.route().unwrap().clone();
        let cases: Vec<(Operation, &[&str])> = vec![
            (
                Operation::Images(ImageOperation::Generation(ImageGenerationRequest {
                    route: slug.clone(),
                    prompt: "draw".into(),
                    count: Some(2),
                    size: Some("1024x1024".into()),
                    stream: false,
                    extensions: Default::default(),
                })),
                &["n", "size"],
            ),
            (
                Operation::Images(ImageOperation::Variation(ImageVariationRequest {
                    route: slug.clone(),
                    image: MediaHandle::new("image"),
                    count: Some(2),
                    size: Some("1024x1024".into()),
                    extensions: Default::default(),
                })),
                &["n", "size"],
            ),
            (
                Operation::Images(ImageOperation::Edit(ImageEditRequest {
                    route: slug.clone(),
                    images: vec![MediaHandle::new("image")],
                    mask: Some(MediaHandle::new("mask")),
                    prompt: "edit".into(),
                    stream: false,
                    extensions: Default::default(),
                })),
                &["mask"],
            ),
            (
                Operation::Speech(SpeechRequest {
                    route: slug.clone(),
                    input: "hello".into(),
                    voice: "alloy".into(),
                    format: Some("mp3".into()),
                    stream: false,
                    extensions: Default::default(),
                }),
                &["voice", "response_format"],
            ),
            (
                Operation::Transcription(TranscriptionRequest {
                    route: slug.clone(),
                    audio: MediaHandle::new("audio"),
                    language: Some("en".into()),
                    prompt: Some("names".into()),
                    stream: false,
                    extensions: Default::default(),
                }),
                &["language", "prompt"],
            ),
            (
                Operation::Video(VideoOperation::Create(VideoCreateRequest {
                    route: slug.clone(),
                    prompt: "animate".into(),
                    input: Some(MediaHandle::new("reference")),
                    extensions: Default::default(),
                })),
                &["input_reference"],
            ),
        ];
        for (operation, parameters) in cases {
            let mut snapshot = base.clone();
            let mode = if operation.kind() == OperationKind::VideoCreate {
                TransportMode::Async
            } else {
                TransportMode::Unary
            };
            snapshot.routes.get_mut(&slug).unwrap().operations = BTreeSet::from([operation.kind()]);
            for provider in snapshot.providers.values_mut() {
                provider.capabilities = BTreeSet::from([Capability::new(
                    "test-model",
                    operation.kind(),
                    Surface::OpenAi,
                    mode,
                )]);
            }
            let evaluate = |snapshot: &Snapshot| {
                select(
                    snapshot,
                    &slug,
                    &operation,
                    Surface::OpenAi,
                    mode,
                    b"seed",
                    None,
                    &Default::default(),
                    |_, _| true,
                )
            };
            assert!(
                evaluate(&snapshot).is_ok(),
                "{:?} should be representable",
                operation.kind()
            );
            snapshot.routing.installation.constraints.require_parameters = true;
            assert!(
                evaluate(&snapshot).is_err(),
                "{:?} requires affirmative parameter support",
                operation.kind()
            );
            let metadata = snapshot
                .routing
                .providers
                .entry(ids[0])
                .or_default()
                .models
                .entry("test-model".into())
                .or_default();
            metadata.supported_parameters = Some(
                parameters
                    .iter()
                    .skip(1)
                    .map(|name| (*name).into())
                    .collect(),
            );
            assert!(
                evaluate(&snapshot).is_err(),
                "every supplied control must be supported"
            );
            snapshot
                .routing
                .providers
                .get_mut(&ids[0])
                .unwrap()
                .models
                .get_mut("test-model")
                .unwrap()
                .supported_parameters =
                Some(parameters.iter().map(|name| (*name).into()).collect());
            assert_eq!(evaluate(&snapshot).unwrap().attempts[0].provider_id, ids[0]);
        }
    }

    #[test]
    fn strict_parameters_distinguish_synthetic_and_explicit_single_candidates() {
        use crate::protocols::{anthropic, openai};
        use serde_json::json;
        let (base, generation, ids) = fixture();
        let slug = generation.route().unwrap();
        for surface in [Surface::Anthropic, Surface::OpenAi] {
            let operation = if surface == Surface::Anthropic {
                anthropic::translate::decode::request(
                    serde_json::from_value(json!({"model":slug.as_str(),"max_tokens":64,
                        "temperature":0.5,"messages":[{"role":"user","content":"hello"}]}))
                    .unwrap(),
                )
                .unwrap()
            } else {
                openai::chat::decode::chat_completion(
                    serde_json::from_value(json!({"model":slug.as_str(),"max_tokens":64,
                        "temperature":0.5,"n":1,"messages":[{"role":"user","content":"hello"}]}))
                    .unwrap(),
                )
                .unwrap()
            };
            let mut snapshot = base.clone();
            snapshot.routing.installation.constraints.require_parameters = true;
            for provider in snapshot.providers.values_mut() {
                provider.kind = if surface == Surface::Anthropic {
                    ProviderKind::Anthropic
                } else {
                    ProviderKind::OpenAi
                };
                provider.capabilities = BTreeSet::from([Capability::new(
                    "test-model",
                    OperationKind::Generation,
                    surface,
                    TransportMode::Unary,
                )]);
            }
            snapshot
                .routing
                .providers
                .entry(ids[0])
                .or_default()
                .models
                .entry("test-model".into())
                .or_default()
                .supported_parameters = Some(BTreeSet::from([
                "max_output_tokens".into(),
                "temperature".into(),
            ]));
            let evaluate = |snapshot: &Snapshot| {
                select(
                    snapshot,
                    slug,
                    &operation,
                    surface,
                    TransportMode::Unary,
                    b"seed",
                    None,
                    &Default::default(),
                    |_, _| true,
                )
            };
            assert_eq!(evaluate(&snapshot).is_ok(), surface == Surface::Anthropic);
            snapshot
                .routing
                .providers
                .get_mut(&ids[0])
                .unwrap()
                .models
                .get_mut("test-model")
                .unwrap()
                .supported_parameters
                .as_mut()
                .unwrap()
                .insert("n".into());
            assert!(evaluate(&snapshot).is_ok());
        }
    }

    #[test]
    fn strict_token_count_parameters_inspect_controls_inside_preserved_requests() {
        use crate::protocols::{anthropic, gemini};
        use serde_json::json;
        let (base, generation, ids) = fixture();
        let slug = generation.route().unwrap().clone();
        let cases: Vec<(Surface, serde_json::Value, &[&str])> = vec![
            (
                Surface::Anthropic,
                json!({"model":slug.as_str(),"system":"Be brief","messages":[
                    {"role":"user","content":[{"type":"text","text":"hello"}]}
                ]}),
                &[],
            ),
            (
                Surface::Anthropic,
                json!({"model":slug.as_str(),"messages":[
                    {"role":"user","content":[{"type":"text","text":"hello"}]}
                ],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],
                "tool_choice":{"type":"auto"}}),
                &["tools", "tool_choice"],
            ),
            (
                Surface::Anthropic,
                json!({"model":slug.as_str(),"messages":[
                    {"role":"user","content":[{"type":"text","text":"hello",
                        "cache_control":{"type":"ephemeral"}}]}
                ]}),
                &["messages/0/content/0/cache_control"],
            ),
            (
                Surface::Gemini,
                json!({"contents":[
                    {"role":"user","parts":[{"text":"hello"}]},
                    {"role":"model","parts":[{"text":"hi"}]}
                ]}),
                &[],
            ),
            (
                Surface::Gemini,
                json!({"generateContentRequest":{"model":slug.as_str(),
                    "systemInstruction":{"parts":[{"text":"Be brief"}]},
                    "contents":[{"role":"user","parts":[{"text":"hello"}]}]}}),
                &[],
            ),
            (
                Surface::Gemini,
                json!({"generateContentRequest":{
                    "contents":[{"parts":[{"text":"hello"}]}],
                    "generationConfig":{"temperature":0.5},
                    "tools":[{"functionDeclarations":[{"name":"lookup",
                        "parameters":{"type":"object"}}]}],
                    "safetySettings":[{"category":"HARM_CATEGORY_HATE_SPEECH",
                        "threshold":"BLOCK_ONLY_HIGH"}]
                }}),
                &["temperature", "tools", "safetySettings"],
            ),
        ];
        for (surface, body, required) in cases {
            let operation = match surface {
                Surface::Anthropic => anthropic::count::decode_count_tokens_request(
                    serde_json::from_value(body).unwrap(),
                )
                .unwrap(),
                Surface::Gemini => gemini::count::decode_count_tokens_request(
                    slug.as_str(),
                    serde_json::from_value(body).unwrap(),
                )
                .unwrap(),
                _ => unreachable!(),
            };
            let mut snapshot = base.clone();
            snapshot.routes.get_mut(&slug).unwrap().operations =
                BTreeSet::from([OperationKind::TokenCount]);
            snapshot.routing.installation.constraints.require_parameters = true;
            for provider in snapshot.providers.values_mut() {
                provider.kind = if surface == Surface::Anthropic {
                    ProviderKind::Anthropic
                } else {
                    ProviderKind::Gemini
                };
                provider.capabilities = BTreeSet::from([Capability::new(
                    "test-model",
                    OperationKind::TokenCount,
                    surface,
                    TransportMode::Unary,
                )]);
            }
            let evaluate = |snapshot: &Snapshot| {
                select(
                    snapshot,
                    &slug,
                    &operation,
                    surface,
                    TransportMode::Unary,
                    b"seed",
                    None,
                    &Default::default(),
                    |_, _| true,
                )
            };
            assert_eq!(
                evaluate(&snapshot).is_ok(),
                required.is_empty(),
                "{surface}"
            );
            snapshot
                .routing
                .providers
                .entry(ids[0])
                .or_default()
                .models
                .entry("test-model".into())
                .or_default()
                .supported_parameters = Some(required.iter().map(|name| (*name).into()).collect());
            assert!(evaluate(&snapshot).is_ok(), "{surface}: {required:?}");
        }
    }

    #[test]
    fn strict_parameters_do_not_block_internal_video_deletion_reconciliation() {
        use crate::protocols::canonical::requests::{
            MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION, VideoJobRequest, VideoOperation,
        };
        let (mut snapshot, generation, _) = fixture();
        let slug = generation.route().unwrap().clone();
        snapshot.routes.get_mut(&slug).unwrap().operations =
            BTreeSet::from([OperationKind::VideoDelete]);
        snapshot.routing.installation.constraints.require_parameters = true;
        for provider in snapshot.providers.values_mut() {
            provider.capabilities = BTreeSet::from([Capability::new(
                "test-model",
                OperationKind::VideoDelete,
                Surface::OpenAi,
                TransportMode::Unary,
            )]);
        }
        let mut request = VideoJobRequest {
            route: Some(slug),
            job_id: "video_1".into(),
            extensions: crate::protocols::canonical::requests::SourceExtensions::new(
                Surface::OpenAi,
                BTreeMap::new(),
            ),
        };
        let ordinary = run(
            &snapshot,
            &Operation::Video(VideoOperation::Delete(request.clone())),
            &Default::default(),
        )
        .unwrap();
        request.extensions.values.insert(
            MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION.into(),
            serde_json::json!(true),
        );
        let cleanup = run(
            &snapshot,
            &Operation::Video(VideoOperation::Delete(request)),
            &Default::default(),
        )
        .unwrap();
        assert_eq!(
            ordinary.attempts[0].provider_id,
            cleanup.attempts[0].provider_id
        );
    }

    #[test]
    fn provider_filter_runs_before_attempt_limit() {
        let (snapshot, operation, ids) = fixture();
        let preferences = RoutingPreferences {
            constraints: RoutingConstraints {
                only: Some(vec![format!("provider:{}", ids[1])]),
                ..Default::default()
            },
            ..Default::default()
        };
        assert_eq!(
            run(&snapshot, &operation, &preferences).unwrap().attempts[0].provider_id,
            ids[1]
        );
    }
    #[test]
    fn request_cannot_expand_installation_allowlist() {
        let (mut snapshot, operation, ids) = fixture();
        snapshot.routing.installation.constraints.only = Some(vec![format!("provider:{}", ids[0])]);
        let preferences = RoutingPreferences {
            constraints: RoutingConstraints {
                only: Some(vec![format!("provider:{}", ids[1])]),
                ..Default::default()
            },
            ..Default::default()
        };
        assert!(run(&snapshot, &operation, &preferences).is_err());
    }
    #[test]
    fn privacy_unknown_is_not_an_attestation() {
        let (snapshot, operation, _) = fixture();
        let preferences = RoutingPreferences {
            constraints: RoutingConstraints {
                require_zero_data_retention: true,
                ..Default::default()
            },
            ..Default::default()
        };
        assert!(run(&snapshot, &operation, &preferences).is_err());
    }
    #[test]
    fn legacy_protocol_identity_does_not_imply_an_official_vendor() {
        let (snapshot, operation, _) = fixture();
        let preferences = RoutingPreferences {
            constraints: RoutingConstraints {
                only: Some(vec!["vendor:openai".into()]),
                ..Default::default()
            },
            ..Default::default()
        };
        assert!(run(&snapshot, &operation, &preferences).is_err());
        assert!(run(&snapshot, &operation, &Default::default()).is_ok());
    }
    #[test]
    fn current_slot_restrictions_constrain_historical_credentials() {
        let (mut snapshot, operation, ids) = fixture();
        let historical = CredentialSlot {
            id: uuid::Uuid::now_v7(),
            name: "historical".into(),
            credential_version_id: Some(uuid::Uuid::now_v7()),
            ..Default::default()
        };
        snapshot
            .routing
            .credentials
            .insert(ids[0], vec![historical.clone()]);
        let mut current = historical.clone();
        current.credential_version_id = Some(uuid::Uuid::now_v7());
        current.requests_per_minute = Some(2);
        current.tokens_per_minute = Some(1_000);
        current.max_concurrency = Some(1);
        let connection_limits = crate::providers::options::ConnectionLimits {
            requests_per_minute: Some(3),
            ..Default::default()
        };
        snapshot.routing.connection_limit_authority =
            Some(BTreeMap::from([(ids[0], connection_limits.clone())]));
        snapshot.routing.credential_authority =
            Some(BTreeMap::from([(ids[0], vec![current.clone()])]));
        let selection = run(&snapshot, &operation, &Default::default()).unwrap();
        assert_eq!(
            selection.attempts[0].credential_version_id,
            historical.credential_version_id
        );
        assert_eq!(
            selection.attempts[0].credential_limits,
            Some(current.limits())
        );
        assert_eq!(
            selection.attempts[0].connection_limits,
            Some(connection_limits)
        );
        assert_eq!(
            snapshot.routing.credentials[&ids[0]][0].requests_per_minute,
            None
        );
        for restricted in [
            CredentialSlot {
                enabled: false,
                ..current.clone()
            },
            CredentialSlot {
                allowed_models: vec!["other-model".into()],
                ..current.clone()
            },
            CredentialSlot {
                allowed_routes: vec!["other-route".into()],
                ..current.clone()
            },
            CredentialSlot {
                allowed_api_keys: vec![uuid::Uuid::now_v7()],
                ..current
            },
        ] {
            snapshot.routing.credential_authority =
                Some(BTreeMap::from([(ids[0], vec![restricted])]));
            let selection = run(&snapshot, &operation, &Default::default()).unwrap();
            assert_eq!(selection.attempts[0].provider_id, ids[1]);
            assert_eq!(
                selection.decisions[0].reason.as_deref(),
                Some("credential_restricted")
            );
        }
        snapshot.routing.credential_authority = Some(BTreeMap::new());
        assert_eq!(
            run(&snapshot, &operation, &Default::default())
                .unwrap()
                .attempts[0]
                .provider_id,
            ids[1]
        );
    }

    #[test]
    fn anthropic_token_defaults_make_an_omitted_limit_representable() {
        let (mut snapshot, mut operation, ids) = fixture();
        snapshot.providers.get_mut(&ids[0]).unwrap().kind = ProviderKind::Anthropic;
        let Operation::Generation(request) = &mut operation else {
            unreachable!()
        };
        request.parameters.max_output_tokens = None;
        assert_eq!(
            run(&snapshot, &operation, &Default::default())
                .unwrap()
                .attempts[0]
                .provider_id,
            ids[1]
        );
        snapshot
            .routing
            .providers
            .entry(ids[0])
            .or_default()
            .parameter_defaults
            .insert("max_tokens".into(), serde_json::json!(64));
        assert_eq!(
            run(&snapshot, &operation, &Default::default())
                .unwrap()
                .attempts[0]
                .provider_id,
            ids[0]
        );
    }

    #[test]
    fn credential_filters_precede_the_attempt_budget() {
        let (mut snapshot, operation, ids) = fixture();
        let mut restricted = CredentialSlot {
            id: uuid::Uuid::now_v7(),
            name: "Restricted".into(),
            allowed_models: vec!["another-model".into()],
            ..Default::default()
        };
        restricted.credential_version_id = Some(uuid::Uuid::now_v7());
        let allowed = CredentialSlot {
            id: uuid::Uuid::now_v7(),
            name: "Allowed".into(),
            credential_version_id: Some(uuid::Uuid::now_v7()),
            ..Default::default()
        };
        snapshot
            .routing
            .credentials
            .insert(ids[0], vec![restricted, allowed.clone()]);
        let selection = run(&snapshot, &operation, &RoutingPreferences::default()).unwrap();
        assert_eq!(selection.attempts[0].credential_slot_id, Some(allowed.id));
        assert!(
            selection
                .decisions
                .iter()
                .any(|d| d.reason.as_deref() == Some("credential_restricted"))
        );
    }
    #[test]
    fn slot_priority_wins_before_weight_and_cooldowns_precede_the_limit() {
        let (mut snapshot, operation, ids) = fixture();
        let primary = CredentialSlot {
            id: uuid::Uuid::now_v7(),
            name: "primary".into(),
            priority: 0,
            weight: 1,
            credential_version_id: Some(uuid::Uuid::now_v7()),
            ..Default::default()
        };
        let secondary = CredentialSlot {
            id: uuid::Uuid::now_v7(),
            name: "secondary".into(),
            priority: 1,
            weight: 100000,
            credential_version_id: Some(uuid::Uuid::now_v7()),
            ..Default::default()
        };
        snapshot
            .routing
            .credentials
            .insert(ids[0], vec![secondary.clone(), primary.clone()]);
        assert_eq!(
            run(&snapshot, &operation, &Default::default())
                .unwrap()
                .attempts[0]
                .credential_slot_id,
            Some(primary.id)
        );
        let preferences = RoutingPreferences {
            cooling_slots: BTreeSet::from([primary.id]),
            ..Default::default()
        };
        assert_eq!(
            run(&snapshot, &operation, &preferences).unwrap().attempts[0].credential_slot_id,
            Some(secondary.id)
        );
    }

    #[test]
    fn multiple_credentials_can_fill_more_attempts_than_targets() {
        let (mut snapshot, operation, ids) = fixture();
        let route = snapshot.routes.get_mut(operation.route().unwrap()).unwrap();
        route.targets.truncate(1);
        route.max_attempts = std::num::NonZeroU16::new(2).unwrap();
        snapshot.routing.credentials.insert(
            ids[0],
            (0..2)
                .map(|i| CredentialSlot {
                    id: uuid::Uuid::now_v7(),
                    name: format!("slot-{i}"),
                    credential_version_id: Some(uuid::Uuid::now_v7()),
                    ..Default::default()
                })
                .collect(),
        );
        let selection = run(&snapshot, &operation, &Default::default()).unwrap();
        assert_eq!(selection.attempts.len(), 2);
        assert_ne!(
            selection.attempts[0].credential_version_id,
            selection.attempts[1].credential_version_id
        );
    }

    #[test]
    fn explanations_survive_an_empty_pool_and_unknown_required_metadata() {
        let (mut snapshot, operation, ids) = fixture();
        snapshot.routing.credentials.insert(ids[0], vec![]);
        snapshot
            .routing
            .installation
            .constraints
            .require_zero_data_retention = true;
        let selection = explain(
            &snapshot,
            operation.route().unwrap(),
            &operation,
            Surface::OpenAi,
            TransportMode::Unary,
            b"seed",
            None,
            &Default::default(),
            |_, _| true,
        )
        .unwrap();
        assert!(selection.attempts.is_empty());
        assert_eq!(selection.decisions.len(), 2);
        assert!(
            selection
                .decisions
                .iter()
                .all(|d| d.reason.as_deref() == Some("zero_data_retention_required"))
        );
    }

    fn rate(id: ProviderId, input: &str, output: &str) -> RoutingPrice {
        RoutingPrice {
            effective_at: chrono::Utc::now() - chrono::Duration::hours(1),
            scope_priority: 0,
            revision: 1,
            revision_id: uuid::Uuid::now_v7(),
            provider_id: id.as_uuid(),
            model: "test-model".into(),
            operation: "generation".into(),
            currency: "USD".into(),
            input_per_million: Some(input.into()),
            output_per_million: Some(output.into()),
            unit_price: None,
        }
    }

    #[test]
    fn prices_use_decimal_precision_scope_and_effective_time() {
        let (mut snapshot, operation, ids) = fixture();
        snapshot
            .routes
            .get_mut(operation.route().unwrap())
            .unwrap()
            .targets[1]
            .priority = 0;
        let first = rate(ids[0], "0.100000000001", "0.1");
        let second = rate(ids[1], "0.100000000000", "0.1");
        snapshot.routing.prices = vec![first, second];
        let preferences = RoutingPreferences {
            strategy: Some(RoutingStrategy::Price),
            ..Default::default()
        };
        assert_eq!(
            run(&snapshot, &operation, &preferences).unwrap().attempts[0].provider_id,
            ids[1]
        );
        let mut future = rate(ids[0], "0", "0");
        future.effective_at = chrono::Utc::now() + chrono::Duration::hours(1);
        future.scope_priority = 2;
        snapshot.routing.prices.push(future);
        assert_eq!(
            run(&snapshot, &operation, &preferences).unwrap().attempts[0].provider_id,
            ids[1]
        );
        let scoped = RoutingPrice {
            scope_priority: 2,
            ..rate(ids[0], "0", "0")
        };
        let revision = scoped.revision_id;
        snapshot.routing.prices.push(scoped);
        assert_eq!(
            run(&snapshot, &operation, &preferences).unwrap().attempts[0].pricing_revision_id,
            Some(revision)
        );
    }

    #[test]
    fn request_order_precedes_strategy_within_operator_priority_tiers() {
        let (mut snapshot, operation, ids) = fixture();
        snapshot.routing.prices = vec![rate(ids[0], "1", "1"), rate(ids[1], "0", "0")];
        let preferences = RoutingPreferences {
            strategy: Some(RoutingStrategy::Price),
            order: Some(vec![format!("provider:{}", ids[1])]),
            ..Default::default()
        };
        assert_eq!(
            run(&snapshot, &operation, &preferences).unwrap().attempts[0].provider_id,
            ids[0]
        );
        snapshot
            .routes
            .get_mut(operation.route().unwrap())
            .unwrap()
            .targets[1]
            .priority = 0;
        assert_eq!(
            run(&snapshot, &operation, &preferences).unwrap().attempts[0].provider_id,
            ids[1]
        );
        snapshot.routing.installation.allowed_strategies =
            Some(BTreeSet::from([RoutingStrategy::Weighted]));
        assert!(run(&snapshot, &operation, &preferences).is_err());
    }
}

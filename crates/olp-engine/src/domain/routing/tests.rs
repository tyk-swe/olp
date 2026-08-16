use std::num::NonZeroU32;

use uuid::Uuid;

use super::{provider::ProviderKind, selection::weighted_rendezvous_score};
use crate::domain::{
    canonical::identity::{OperationKind, Surface, TransportMode},
    ids::{RouteId, TargetId},
};

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

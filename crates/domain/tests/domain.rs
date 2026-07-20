use std::{
    collections::{BTreeMap, BTreeSet},
    num::{NonZeroU16, NonZeroU32},
};

use chrono::{Duration, TimeZone, Utc};
use olp_domain::*;
use proptest::prelude::*;
use serde_json::json;
use uuid::Uuid;

fn id(value: u128) -> Uuid {
    Uuid::from_u128(value)
}

fn provider(
    provider_number: u128,
    target_number: u128,
    priority: u16,
    weight: u32,
) -> (Provider, Target) {
    let provider_id = ProviderId::from_uuid(id(provider_number));
    let model = format!("upstream-{provider_number}");
    let provider = Provider {
        id: provider_id,
        name: format!("provider-{provider_number}"),
        kind: ProviderKind::OpenAi,
        enabled: true,
        active_credential: Some(CredentialVersionId::from_uuid(id(provider_number + 100))),
        capabilities: BTreeSet::from([Capability::new(
            &model,
            OperationKind::Generation,
            Surface::OpenAi,
            TransportMode::Streaming,
        )]),
    };
    let target = Target {
        id: TargetId::from_uuid(id(target_number)),
        routing_id: None,
        provider_id,
        provider_model: model,
        priority,
        weight: NonZeroU32::new(weight).expect("fixture weight is non-zero"),
        timeout: DurationMs::new(2_000),
    };
    (provider, target)
}

fn snapshot(targets: Vec<(Provider, Target)>, max_attempts: u16) -> RuntimeSnapshot {
    let slug = RouteSlug::parse("default").expect("fixture slug is valid");
    let providers = targets
        .iter()
        .map(|(provider, _)| (provider.id, provider.clone()))
        .collect();
    let route = Route {
        id: RouteId::from_uuid(id(500)),
        routing_id: None,
        slug: slug.clone(),
        operations: BTreeSet::from([OperationKind::Generation]),
        overall_timeout: DurationMs::new(10_000),
        max_attempts: NonZeroU16::new(max_attempts).expect("fixture attempts are non-zero"),
        targets: targets.into_iter().map(|(_, target)| target).collect(),
    };

    RuntimeSnapshot {
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::from_uuid(id(600)),
            ordinal: 7,
            activated_at: Utc.timestamp_opt(1_800_000_000, 0).unwrap(),
        },
        providers,
        routes: BTreeMap::from([(slug, route)]),
        api_keys: BTreeMap::new(),
    }
}

fn select(snapshot: &RuntimeSnapshot, affinity: &[u8]) -> Vec<AttemptPlan> {
    select_attempts(
        snapshot,
        &RouteSlug::parse("default").unwrap(),
        OperationKind::Generation,
        Surface::OpenAi,
        TransportMode::Streaming,
        affinity,
    )
    .unwrap()
}

#[test]
fn persisted_snapshot_decoder_is_narrow_and_public_serde_stays_strict() {
    let (mut provider, mut target) = provider(1, 2, 0, 1);
    provider.name = "open_ai".to_owned();
    provider.capabilities = BTreeSet::from([Capability::new(
        "open_ai",
        OperationKind::Generation,
        Surface::OpenAi,
        TransportMode::Streaming,
    )]);
    target.provider_model = "open_ai".to_owned();
    let current = snapshot(vec![(provider, target)], 1);
    let current_value = serde_json::to_value(&current).unwrap();
    let current_provider = current_value["providers"]
        .as_object()
        .unwrap()
        .values()
        .next()
        .unwrap();
    assert_eq!(current_provider["kind"], "openai");
    assert_eq!(current_provider["capabilities"][0]["surface"], "openai");

    let mut legacy_value = current_value;
    let legacy_provider = legacy_value["providers"]
        .as_object_mut()
        .unwrap()
        .values_mut()
        .next()
        .unwrap();
    legacy_provider["kind"] = json!("open_ai");
    legacy_provider["capabilities"][0]["surface"] = json!("open_ai");
    let legacy_payload = serde_json::to_vec(&legacy_value).unwrap();

    assert!(serde_json::from_slice::<RuntimeSnapshot>(&legacy_payload).is_err());
    let decoded = RuntimeSnapshot::from_persisted_slice(&legacy_payload).unwrap();
    decoded.validate().unwrap();
    let decoded_provider = decoded.providers.values().next().unwrap();
    assert_eq!(decoded_provider.name, "open_ai");
    assert_eq!(
        decoded_provider.capabilities.iter().next().unwrap().model,
        "open_ai"
    );
    assert_eq!(
        decoded.routes.values().next().unwrap().targets[0].provider_model,
        "open_ai"
    );

    let reserialized = serde_json::to_value(decoded).unwrap();
    let reserialized_provider = reserialized["providers"]
        .as_object()
        .unwrap()
        .values()
        .next()
        .unwrap();
    assert_eq!(reserialized_provider["kind"], "openai");
    assert_eq!(
        reserialized_provider["capabilities"][0]["surface"],
        "openai"
    );
    assert_eq!(reserialized_provider["name"], "open_ai");
    assert_eq!(reserialized_provider["capabilities"][0]["model"], "open_ai");
}

#[test]
fn priority_groups_are_exhausted_before_lower_priorities() {
    let runtime = snapshot(
        vec![
            provider(1, 11, 10, 1),
            provider(2, 12, 0, 1),
            provider(3, 13, 0, 1),
            provider(4, 14, 20, 1),
        ],
        4,
    );

    let attempts = select(&runtime, b"request-1");
    assert_eq!(attempts.len(), 4);
    assert_eq!(
        attempts
            .iter()
            .map(|attempt| attempt.priority)
            .collect::<Vec<_>>(),
        [0, 0, 10, 20]
    );
}

#[test]
fn capability_filter_is_exact_across_surface_and_mode() {
    let (provider, target) = provider(1, 11, 0, 1);
    let runtime = snapshot(vec![(provider, target)], 1);
    let route = RouteSlug::parse("default").unwrap();

    let wrong_surface = select_attempts(
        &runtime,
        &route,
        OperationKind::Generation,
        Surface::Anthropic,
        TransportMode::Streaming,
        b"key",
    );
    let wrong_mode = select_attempts(
        &runtime,
        &route,
        OperationKind::Generation,
        Surface::OpenAi,
        TransportMode::Unary,
        b"key",
    );

    assert!(matches!(
        wrong_surface,
        Err(RoutingError::NoEligibleTargets { .. })
    ));
    assert!(matches!(
        wrong_mode,
        Err(RoutingError::NoEligibleTargets { .. })
    ));
}

#[test]
fn weighted_rendezvous_is_stable_and_respects_weight_over_many_keys() {
    let runtime = snapshot(vec![provider(1, 11, 0, 9), provider(2, 12, 0, 1)], 1);
    let first = select(&runtime, b"same-affinity");
    for _ in 0..100 {
        assert_eq!(select(&runtime, b"same-affinity"), first);
    }

    let heavy_target = TargetId::from_uuid(id(11));
    let heavy_wins = (0_u64..5_000)
        .filter(|index| select(&runtime, &index.to_be_bytes())[0].target_id == heavy_target)
        .count();
    assert!(
        heavy_wins > 4_000,
        "heavy target won only {heavy_wins}/5000 selections"
    );
}

#[test]
fn persisted_routing_ids_drive_rendezvous_and_legacy_payloads_default_to_row_ids() {
    let mut runtime = snapshot(
        vec![
            provider(1, 11, 0, 1),
            provider(2, 12, 0, 1),
            provider(3, 13, 0, 1),
            provider(4, 14, 0, 1),
        ],
        4,
    );
    let affinity = b"persisted-routing-identity";
    let legacy_attempts = select(&runtime, affinity);
    let route = runtime
        .routes
        .get(&RouteSlug::parse("default").unwrap())
        .unwrap();
    let mut expected_legacy = route
        .targets
        .iter()
        .map(|target| {
            (
                weighted_rendezvous_score(
                    route.id,
                    target.id,
                    target.weight,
                    OperationKind::Generation,
                    Surface::OpenAi,
                    TransportMode::Streaming,
                    affinity,
                ),
                target.id,
            )
        })
        .collect::<Vec<_>>();
    expected_legacy.sort_by(|left, right| {
        right
            .0
            .total_cmp(&left.0)
            .then_with(|| left.1.cmp(&right.1))
    });
    assert_eq!(
        legacy_attempts
            .iter()
            .map(|attempt| attempt.target_id)
            .collect::<Vec<_>>(),
        expected_legacy
            .into_iter()
            .map(|(_, target_id)| target_id)
            .collect::<Vec<_>>()
    );
    let legacy_payload = serde_json::to_value(&runtime).unwrap();
    let legacy_route = legacy_payload["routes"]["default"].as_object().unwrap();
    assert!(!legacy_route.contains_key("routing_id"));
    assert!(
        legacy_route["targets"]
            .as_array()
            .unwrap()
            .iter()
            .all(|target| !target.as_object().unwrap().contains_key("routing_id"))
    );
    let legacy_runtime: RuntimeSnapshot = serde_json::from_value(legacy_payload).unwrap();
    assert_eq!(select(&legacy_runtime, affinity), legacy_attempts);

    let route = runtime
        .routes
        .get_mut(&RouteSlug::parse("default").unwrap())
        .unwrap();
    route.routing_id = Some(RouteId::from_uuid(id(700)));
    for (target, routing_id) in route.targets.iter_mut().zip([701, 702, 703, 704]) {
        target.routing_id = Some(TargetId::from_uuid(id(routing_id)));
    }
    let mut expected = route
        .targets
        .iter()
        .map(|target| {
            (
                weighted_rendezvous_score(
                    route.routing_id.unwrap(),
                    target.routing_id.unwrap(),
                    target.weight,
                    OperationKind::Generation,
                    Surface::OpenAi,
                    TransportMode::Streaming,
                    affinity,
                ),
                target.routing_id.unwrap(),
                target.id,
            )
        })
        .collect::<Vec<_>>();
    expected.sort_by(|left, right| {
        right
            .0
            .total_cmp(&left.0)
            .then_with(|| left.1.cmp(&right.1))
    });
    let persisted_attempts = select(&runtime, affinity)
        .into_iter()
        .map(|attempt| attempt.target_id)
        .collect::<Vec<_>>();
    assert_eq!(
        persisted_attempts,
        expected
            .into_iter()
            .map(|(_, _, target_id)| target_id)
            .collect::<Vec<_>>()
    );
}

proptest! {
    #[test]
    fn target_storage_order_does_not_change_rendezvous_order(affinity in prop::collection::vec(any::<u8>(), 0..128)) {
        let targets = vec![provider(1, 11, 0, 1), provider(2, 12, 0, 3), provider(3, 13, 0, 7)];
        let mut reversed = targets.clone();
        reversed.reverse();
        let normal = snapshot(targets, 3);
        let reversed = snapshot(reversed, 3);

        let normal_ids = select(&normal, &affinity).into_iter().map(|attempt| attempt.target_id).collect::<Vec<_>>();
        let reversed_ids = select(&reversed, &affinity).into_iter().map(|attempt| attempt.target_id).collect::<Vec<_>>();
        prop_assert_eq!(normal_ids, reversed_ids);
    }

    #[test]
    fn accepted_route_slugs_round_trip(segments in prop::collection::vec("[a-z0-9]{1,8}", 1..6)) {
        let candidate = segments.join("-");
        prop_assume!(candidate.len() <= RouteSlug::MAX_LENGTH);
        let slug = RouteSlug::parse(candidate.clone()).unwrap();
        prop_assert_eq!(slug.as_str(), candidate);
    }
}

#[test]
fn invalid_snapshot_cannot_reference_an_unknown_provider() {
    let pair = provider(1, 11, 0, 1);
    let mut runtime = snapshot(vec![pair], 1);
    runtime.providers.clear();

    assert!(matches!(
        runtime.validate(),
        Err(SnapshotValidationError::UnknownProvider { .. })
    ));
}

#[test]
fn invalid_snapshot_cannot_publish_an_operation_without_an_eligible_target() {
    let pair = provider(1, 11, 0, 1);
    let mut runtime = snapshot(vec![pair], 1);
    let provider = runtime.providers.values_mut().next().unwrap();
    provider.capabilities.clear();

    assert!(matches!(
        runtime.validate(),
        Err(SnapshotValidationError::NoEligibleTarget {
            operation: OperationKind::Generation,
            ..
        })
    ));
}

#[test]
fn snapshot_eligibility_is_operation_specific_but_surface_agnostic() {
    let pair = provider(1, 11, 0, 1);
    let mut runtime = snapshot(vec![pair], 1);
    let provider = runtime.providers.values_mut().next().unwrap();
    provider.capabilities = BTreeSet::from([Capability::new(
        "upstream-1",
        OperationKind::Generation,
        Surface::Anthropic,
        TransportMode::Unary,
    )]);

    assert_eq!(runtime.validate(), Ok(()));
}

#[test]
fn route_attempt_budget_cannot_exceed_target_count() {
    let runtime = snapshot(vec![provider(1, 11, 0, 1)], 2);
    assert!(matches!(
        runtime.validate(),
        Err(SnapshotValidationError::InvalidRoute {
            source: RouteValidationError::AttemptsExceedTargets,
            ..
        })
    ));
}

#[test]
fn source_extensions_are_forwardable_only_on_the_same_surface() {
    let extensions = SourceExtensions::new(
        Surface::OpenAi,
        BTreeMap::from([("/service_tier".into(), json!("priority"))]),
    );

    assert_eq!(extensions.ensure_representable_on(Surface::OpenAi), Ok(()));
    assert!(matches!(
        extensions.ensure_representable_on(Surface::Gemini),
        Err(ExtensionError::CrossProtocol { .. })
    ));
}

#[test]
fn canonical_event_sequences_require_order_and_exactly_one_terminal_done() {
    let valid = vec![
        CanonicalEvent::new(
            0,
            CanonicalEventKind::ResponseStart {
                response_id: Some("response-1".into()),
                provider_model: None,
            },
        ),
        CanonicalEvent::new(1, CanonicalEventKind::Done),
    ];
    assert_eq!(validate_event_sequence(&valid), Ok(()));

    let gap = vec![CanonicalEvent::new(1, CanonicalEventKind::Done)];
    assert_eq!(
        validate_event_sequence(&gap),
        Err(EventSequenceError::OutOfOrder {
            expected: 0,
            actual: 1
        })
    );

    let after_done = vec![
        CanonicalEvent::new(0, CanonicalEventKind::Done),
        CanonicalEvent::new(1, CanonicalEventKind::Done),
    ];
    assert_eq!(
        validate_event_sequence(&after_done),
        Err(EventSequenceError::AfterDone { sequence: 1 })
    );

    let missing_done = vec![CanonicalEvent::new(
        0,
        CanonicalEventKind::ResponseStart {
            response_id: None,
            provider_model: None,
        },
    )];
    assert_eq!(
        validate_event_sequence(&missing_done),
        Err(EventSequenceError::MissingDone { next_sequence: 1 })
    );

    let mut incremental = EventSequenceValidator::new();
    incremental.push(&valid[0]).unwrap();
    incremental.push(&valid[1]).unwrap();
    assert_eq!(incremental.finish(), Ok(()));
}

fn api_key(status: ApiKeyStatus, expires_at: Option<chrono::DateTime<Utc>>) -> ApiKey {
    ApiKey {
        id: ApiKeyId::from_uuid(id(900)),
        lookup_id: ApiKeyLookupId::parse("lookup_123").unwrap(),
        digest: ApiKeyDigest::new([7; 32]),
        status,
        expires_at,
        scopes: BTreeSet::from([ApiKeyScope::Inference]),
        allowed_routes: BTreeSet::from([RouteSlug::parse("allowed").unwrap()]),
        limits: ApiKeyLimits::default(),
    }
}

#[test]
fn key_authorization_enforces_status_expiry_scope_and_route() {
    let now = Utc.timestamp_opt(1_800_000_000, 0).unwrap();
    let allowed = RouteSlug::parse("allowed").unwrap();
    assert_eq!(
        authorize_api_key(
            &api_key(ApiKeyStatus::Active, Some(now + Duration::minutes(1))),
            Some(&allowed),
            OperationKind::Generation,
            now,
        ),
        Ok(())
    );
    assert_eq!(
        authorize_api_key(
            &api_key(ApiKeyStatus::Revoked, None),
            Some(&allowed),
            OperationKind::Generation,
            now,
        ),
        Err(ApiKeyAuthorizationError::Revoked)
    );
    assert_eq!(
        authorize_api_key(
            &api_key(ApiKeyStatus::Active, Some(now)),
            Some(&allowed),
            OperationKind::Generation,
            now,
        ),
        Err(ApiKeyAuthorizationError::Expired)
    );
    assert!(matches!(
        authorize_api_key(
            &api_key(ApiKeyStatus::Active, None),
            Some(&allowed),
            OperationKind::ModelList,
            now,
        ),
        Err(ApiKeyAuthorizationError::MissingScope { .. })
    ));
    assert!(matches!(
        authorize_api_key(
            &api_key(ApiKeyStatus::Active, None),
            Some(&RouteSlug::parse("blocked").unwrap()),
            OperationKind::Generation,
            now,
        ),
        Err(ApiKeyAuthorizationError::RouteNotAllowed { .. })
    ));
}

#[test]
fn last_owner_cannot_be_demoted_or_removed() {
    assert_eq!(
        validate_owner_change(Role::Owner, Some(Role::Operator), 1),
        Err(OwnerInvariantError::LastOwner)
    );
    assert_eq!(validate_owner_change(Role::Owner, None, 2), Ok(()));
    assert!(Role::Operator.allows(Permission::ManageRoutes));
    assert!(!Role::Viewer.allows(Permission::ManageRoutes));
}

#[test]
fn failover_is_never_allowed_after_commit() {
    let retryable = TransportError {
        phase: TransportPhase::FirstByte,
        class: AttemptFailureClass::Timeout,
        response_committed: false,
        message: "upstream timeout".into(),
    };
    assert!(retryable.allows_failover());
    assert!(
        !TransportError {
            response_committed: true,
            ..retryable
        }
        .allows_failover()
    );
}

#[test]
fn transport_error_diagnostics_never_include_upstream_text() {
    let error = TransportError {
        phase: TransportPhase::Body,
        class: AttemptFailureClass::Protocol,
        response_committed: true,
        message: "sensitive upstream response".into(),
    };

    assert!(!format!("{error:?}").contains("sensitive upstream response"));
    assert!(!error.to_string().contains("sensitive upstream response"));
    assert!(format!("{error:?}").contains("[REDACTED]"));
}

#[test]
fn api_key_digest_debug_output_is_redacted() {
    let digest = ApiKeyDigest::new([0xAB; 32]);
    let debug = format!("{digest:?}");
    assert!(debug.contains("REDACTED"));
    assert!(!debug.contains("171"));
}

#[test]
fn provider_request_debug_never_includes_prompt_content() {
    let runtime = snapshot(vec![provider(1, 11, 0, 1)], 1);
    let operation = Operation::Generation(GenerationRequest {
        route: RouteSlug::parse("default").unwrap(),
        messages: vec![Message {
            role: MessageRole::User,
            content: vec![ContentPart::Text {
                text: "private prompt marker".into(),
            }],
            name: None,
            tool_call_id: None,
            tool_calls: Vec::new(),
        }],
        parameters: GenerationParameters::default(),
        tools: Vec::new(),
        tool_choice: None,
        response_format: None,
        extensions: SourceExtensions::default(),
    });
    let request = ProviderRequest {
        metadata: RequestMetadata {
            request_id: RequestId::from_uuid(id(901)),
            operation: OperationKind::Generation,
            surface: Surface::OpenAi,
            mode: TransportMode::Streaming,
        },
        attempt: select(&runtime, b"debug").remove(0),
        operation,
        media: None,
    };

    let debug = format!("{request:?}");
    assert!(debug.contains("Generation"));
    assert!(!debug.contains("private prompt marker"));
}

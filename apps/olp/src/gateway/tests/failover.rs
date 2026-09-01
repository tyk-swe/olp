use super::*;

#[derive(Clone)]
struct FirstEventPendingTransport {
    calls: Arc<AtomicUsize>,
}

#[derive(Clone)]
struct CountingFiniteTransport {
    calls: Arc<AtomicUsize>,
    events: Vec<Event>,
}

impl ProviderTransport for FirstEventPendingTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        Box::pin(async {
            Ok(ProviderOutput::Events(
                Box::pin(stream::pending()) as ProviderEventStream
            ))
        })
    }
}

impl ProviderTransport for CountingFiniteTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        let events = self.events.clone();
        Box::pin(async move {
            Ok(ProviderOutput::Events(Box::pin(stream::iter(
                events.into_iter().map(Ok),
            ))))
        })
    }
}

fn install_hard_limits(state: &GatewayState) {
    let pinned = state.runtime().pin();
    let mut api_keys = pinned.api_keys.clone();
    api_keys
        .values_mut()
        .next()
        .unwrap()
        .limits
        .requests_per_minute = NonZeroU32::new(10);
    api_keys.values_mut().next().unwrap().limits.concurrency = NonZeroU32::new(2);
    reinstall_api_keys(state, api_keys);
}

fn install_two_target_streams(
    state: &GatewayState,
    operation: OperationKind,
    first: Arc<dyn ProviderTransport>,
    second: Arc<dyn ProviderTransport>,
) -> (
    Arc<Bundle>,
    Vec<olp_engine::domain::routing::selection::AttemptPlan>,
) {
    let pinned = state.runtime().pin();
    let first_provider_id = *pinned.providers.keys().next().unwrap();
    let second_provider_id = ProviderId::new();
    let mut providers = pinned.providers.clone();
    let capability = BTreeSet::from([Capability::new(
        "upstream-model",
        operation,
        Surface::OpenAi,
        TransportMode::Streaming,
    )]);
    providers.get_mut(&first_provider_id).unwrap().capabilities = capability.clone();
    let mut second_provider = providers[&first_provider_id].clone();
    second_provider.id = second_provider_id;
    second_provider.name = "mock-openai-failover".to_owned();
    second_provider.capabilities = capability;
    providers.insert(second_provider_id, second_provider);

    let route_slug = RouteSlug::parse("default").unwrap();
    let mut routes = pinned.routes.clone();
    let route = routes.get_mut(&route_slug).unwrap();
    route.operations = BTreeSet::from([operation]);
    route.max_attempts = NonZeroU16::new(2).unwrap();
    route.targets[0].timeout = DurationMs::new(20);
    route.targets.push(Target {
        id: TargetId::new(),
        routing_id: None,
        provider_id: second_provider_id,
        upstream_model: "upstream-model".to_owned(),
        priority: 1,
        weight: NonZeroU32::new(1).unwrap(),
        timeout: DurationMs::new(100),
    });
    let snapshot = Snapshot {
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::new(),
            ordinal: pinned.generation.ordinal + 1,
            activated_at: Utc::now(),
        },
        providers,
        routes,
        api_keys: pinned.api_keys.clone(),
    };
    state
        .runtime()
        .install(
            snapshot,
            BTreeMap::from([(first_provider_id, first), (second_provider_id, second)]),
        )
        .unwrap();
    let runtime = state.runtime().pin();
    let attempts = select_attempts(
        &runtime,
        &route_slug,
        operation,
        Surface::OpenAi,
        TransportMode::Streaming,
        b"failover-test",
    )
    .unwrap();
    (runtime, attempts)
}

fn streaming_generation_operation() -> Operation {
    let request: CompletionRequest = serde_json::from_value(json!({
        "model": "default",
        "messages": [{"role": "user", "content": "hello"}],
        "stream": true
    }))
    .unwrap();
    decode::chat_completion(request).unwrap()
}

fn streaming_image_generation_operation() -> Operation {
    Operation::Images(ImageOperation::Generation(ImageGenerationRequest {
        route: RouteSlug::parse("default").unwrap(),
        prompt: "draw a test".to_owned(),
        count: Some(1),
        size: None,
        stream: true,
        extensions: SourceExtensions::default(),
    }))
}

fn streaming_request_metadata(operation: OperationKind) -> RequestMetadata {
    RequestMetadata {
        request_id: RequestId::new(),
        operation,
        surface: Surface::OpenAi,
        mode: TransportMode::Streaming,
    }
}

#[tokio::test]
async fn first_event_timeout_obeys_media_ambiguity_policy() {
    let (media_state, _) = test_state(true);
    let media_first_calls = Arc::new(AtomicUsize::new(0));
    let media_second_calls = Arc::new(AtomicUsize::new(0));
    let (runtime, attempts) = install_two_target_streams(
        &media_state,
        OperationKind::ImageGeneration,
        Arc::new(FirstEventPendingTransport {
            calls: media_first_calls.clone(),
        }),
        Arc::new(CountingFiniteTransport {
            calls: media_second_calls.clone(),
            events: Vec::new(),
        }),
    );
    let failure = match execute(
        Context {
            runtime: &runtime,
            overall_timeout: Duration::from_millis(200),
            max_attempts: std::num::NonZeroU16::new(2).unwrap(),
            media_spool: media_state.media_spool().clone(),
            max_inline_media_bytes: 1024 * 1024,
            circuits: &Breaker::default(),
            on_attempt_started: None,
            trace: None,
        },
        attempts,
        streaming_request_metadata(OperationKind::ImageGeneration),
        streaming_image_generation_operation(),
    )
    .await
    {
        Ok(_) => panic!("a committed media timeout must be terminal"),
        Err(failure) => failure,
    };
    assert_eq!(failure.error.code(), "ambiguous_upstream_result");
    assert_eq!(failure.attempts.len(), 1);
    assert!(failure.attempts[0].committed);
    assert_eq!(media_first_calls.load(Ordering::SeqCst), 1);
    assert_eq!(media_second_calls.load(Ordering::SeqCst), 0);

    let (generation_state, _) = test_state(true);
    let generation_second_calls = Arc::new(AtomicUsize::new(0));
    let (runtime, attempts) = install_two_target_streams(
        &generation_state,
        OperationKind::Generation,
        Arc::new(FirstEventPendingTransport {
            calls: Arc::new(AtomicUsize::new(0)),
        }),
        Arc::new(CountingFiniteTransport {
            calls: generation_second_calls.clone(),
            events: generation_stream_events("retried"),
        }),
    );
    let success = match execute(
        Context {
            runtime: &runtime,
            overall_timeout: Duration::from_millis(200),
            max_attempts: std::num::NonZeroU16::new(2).unwrap(),
            media_spool: generation_state.media_spool().clone(),
            max_inline_media_bytes: 1024 * 1024,
            circuits: &Breaker::default(),
            on_attempt_started: None,
            trace: None,
        },
        attempts,
        streaming_request_metadata(OperationKind::Generation),
        streaming_generation_operation(),
    )
    .await
    {
        Ok(success) => success,
        Err(_) => {
            panic!("generation keeps availability-first failover after a first-event timeout")
        }
    };
    assert_eq!(success.attempts.len(), 2);
    assert_eq!(generation_second_calls.load(Ordering::SeqCst), 1);
}

#[test]
fn post_connect_failure_obeys_media_ambiguity_policy() {
    let failure = TransportError {
        upstream: Default::default(),
        phase: olp_engine::domain::ports::TransportPhase::FirstByte,
        class: AttemptFailureClass::Connect,
        response_committed: false,
        message: "connection closed before response headers".to_owned(),
    };

    let media =
        reclassify_ambiguous_transport_failure(failure.clone(), OperationKind::ImageGeneration);
    assert_eq!(media.class, AttemptFailureClass::Ambiguous);
    assert!(media.response_committed);
    assert!(!media.allows_failover());

    let generation = reclassify_ambiguous_transport_failure(failure, OperationKind::Generation);
    assert_eq!(generation.class, AttemptFailureClass::Connect);
    assert!(!generation.response_committed);
    assert!(generation.allows_failover());

    let connect = reclassify_ambiguous_transport_failure(
        TransportError {
            upstream: Default::default(),
            phase: olp_engine::domain::ports::TransportPhase::Connect,
            class: AttemptFailureClass::Connect,
            response_committed: false,
            message: "connection failed".to_owned(),
        },
        OperationKind::ImageGeneration,
    );
    assert_eq!(connect.class, AttemptFailureClass::Connect);
    assert!(!connect.response_committed);
    assert!(connect.allows_failover());
}

#[tokio::test]
async fn retryable_first_canonical_error_fails_over_before_commit() {
    let (state, _) = test_state(true);
    let second_calls = Arc::new(AtomicUsize::new(0));
    let (runtime, attempts) = install_two_target_streams(
        &state,
        OperationKind::Generation,
        Arc::new(CountingFiniteTransport {
            calls: Arc::new(AtomicUsize::new(0)),
            events: vec![
                Event::new(
                    0,
                    Kind::Error {
                        error: Error {
                            class: ErrorClass::RateLimit,
                            message: "provider throttled the request".to_owned(),
                            provider_code: Some("rate_limit".to_owned()),
                            retryable: true,
                        },
                    },
                ),
                Event::new(1, Kind::Done),
            ],
        }),
        Arc::new(CountingFiniteTransport {
            calls: second_calls.clone(),
            events: generation_stream_events("recovered"),
        }),
    );
    let success = match execute(
        Context {
            runtime: &runtime,
            overall_timeout: Duration::from_millis(200),
            max_attempts: std::num::NonZeroU16::new(2).unwrap(),
            media_spool: state.media_spool().clone(),
            max_inline_media_bytes: 1024 * 1024,
            circuits: &Breaker::default(),
            on_attempt_started: None,
            trace: None,
        },
        attempts,
        streaming_request_metadata(OperationKind::Generation),
        streaming_generation_operation(),
    )
    .await
    {
        Ok(success) => success,
        Err(_) => panic!("retryable pre-commit canonical error must use the next target"),
    };
    assert_eq!(success.attempts.len(), 2);
    assert_eq!(
        success.attempts[0].error_class.as_deref(),
        Some("rate_limit")
    );
    assert!(!success.attempts[0].committed);
    assert_eq!(second_calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn direct_executor_reserves_hard_limits_before_route_selection() {
    let (state, _) = test_state(false);
    install_hard_limits(&state);
    let request: CompletionRequest = serde_json::from_value(json!({
        "model": "route-does-not-exist",
        "messages": [{"role": "user", "content": "hello"}]
    }))
    .unwrap();
    let operation = decode::chat_completion(request).unwrap();
    let admission = test_admission(&state, Surface::OpenAi);
    let error =
        match execute_event_operation(&state, &admission, operation, TransportMode::Unary).await {
            Ok(_) => panic!("missing limiter must fail closed before route selection"),
            Err(error) => error,
        };
    assert_eq!(error.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(error.code(), "distributed_limits_unavailable");
}

#[tokio::test]
async fn required_target_unavailability_is_normalized_by_shared_execution_kernel() {
    let (state, _) = test_state(false);
    install_result(
        &state,
        OperationKind::TokenCount,
        CanonicalResult::TokenCount(olp_engine::domain::canonical::results::TokenCountResult {
            input_tokens: 1,
            extensions: olp_engine::domain::canonical::requests::SourceExtensions::new(
                Surface::OpenAi,
                BTreeMap::new(),
            ),
        }),
    );
    let request: ResponseInputTokensRequest = serde_json::from_value(json!({
        "model": "default",
        "input": "hello"
    }))
    .unwrap();
    let operation = decode_response_input_tokens(request).unwrap();
    let admission = test_admission(&state, Surface::OpenAi);

    let error = match execute_routed_result(
        &state,
        &admission,
        operation,
        TransportMode::Unary,
        Some(RequiredTarget {
            provider_id: uuid::Uuid::now_v7(),
            upstream_model: "unavailable-model".to_owned(),
        }),
    )
    .await
    {
        Ok(_) => panic!("a missing pinned target must not fall back to another target"),
        Err(error) => error,
    };

    assert_eq!(error.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(error.code(), "media_job_target_unavailable");
}

#[tokio::test]
async fn http_pre_reservation_marker_reuses_the_full_reservation() {
    let (state, key) = test_state(false);
    install_hard_limits(&state);
    let snapshot = state.runtime().pin();
    let api_key = snapshot.api_keys.values().next().unwrap();
    let lookup = state.auth_hmac_key().as_ref().lookup_id(&key).unwrap();
    let operation = decode::chat_completion(
        serde_json::from_value(json!({
            "model": "default",
            "messages": [{"role": "user", "content": "hello"}]
        }))
        .unwrap(),
    )
    .unwrap();
    let lease = reserve(
        state.limiter(),
        api_key,
        &operation,
        lookup,
        Duration::from_secs(30),
        Some(10_000),
    )
    .await
    .expect("the canonical executor must reuse the HTTP reservation");
    assert!(lease.is_none());
}

#[tokio::test]
async fn http_request_above_baseline_requires_token_delta_reservation() {
    let (state, key) = test_state(false);
    let pinned = state.runtime().pin();
    let mut api_keys = pinned.api_keys.clone();
    api_keys
        .values_mut()
        .next()
        .unwrap()
        .limits
        .tokens_per_minute = std::num::NonZeroU64::new(4_000);
    reinstall_api_keys(&state, api_keys);
    let snapshot = state.runtime().pin();
    let api_key = snapshot.api_keys.values().next().unwrap();
    let lookup = state.auth_hmac_key().as_ref().lookup_id(&key).unwrap();
    let operation = Operation::Images(
        olp_engine::domain::canonical::requests::ImageOperation::Edit(
            olp_engine::domain::canonical::requests::ImageEditRequest {
                route: RouteSlug::parse("default").unwrap(),
                images: vec![MediaHandle::new("bounded-image")],
                mask: None,
                prompt: "x".repeat(8_500),
                stream: false,
                extensions: olp_engine::domain::canonical::requests::SourceExtensions::default(),
            },
        ),
    );
    let error = reserve(
        state.limiter(),
        api_key,
        &operation,
        lookup,
        Duration::from_secs(30),
        Some(2_000),
    )
    .await
    .map_err(InferenceError::from)
    .err()
    .expect("missing delta limiter must fail closed above the HTTP baseline");
    assert_eq!(error.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(error.code(), "distributed_limits_unavailable");
}

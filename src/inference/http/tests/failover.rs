use crate::inference::http::tests::*;

#[tokio::test]
async fn disabling_fallbacks_keeps_the_selected_single_attempt_budget() {
    use std::sync::atomic::{AtomicUsize, Ordering};
    struct Unreachable(Arc<AtomicUsize>);
    impl ProviderTransport for Unreachable {
        fn execute(
            &self,
            _: ProviderRequest,
        ) -> BoxFuture<'_, Result<ProviderOutput, TransportError>> {
            self.0.fetch_add(1, Ordering::SeqCst);
            Box::pin(async {
                Err(TransportError {
                    phase: crate::inference::transport::TransportPhase::Connect,
                    class: crate::inference::transport::AttemptFailureClass::Connect,
                    response_committed: false,
                    message: "connection failed".into(),
                    upstream: Default::default(),
                })
            })
        }
    }
    for (no_fallbacks, expected) in [(true, 1), (false, 2)] {
        let (state, key) = test_state(false);
        let pinned = state.runtime().pin();
        let mut snapshot = Snapshot::clone(&pinned);
        snapshot.generation.id = RuntimeGenerationId::new();
        snapshot.generation.ordinal += 1;
        snapshot.routes.values_mut().next().unwrap().max_attempts = NonZeroU16::new(3).unwrap();
        let provider = *snapshot.providers.keys().next().unwrap();
        let calls = Arc::new(AtomicUsize::new(0));
        state
            .runtime()
            .install(
                snapshot,
                BTreeMap::from([(
                    provider,
                    Arc::new(Unreachable(calls.clone())) as Arc<dyn ProviderTransport>,
                )]),
            )
            .unwrap();
        let mut request = Request::post("/v1/chat/completions")
            .header(header::AUTHORIZATION, format!("Bearer {key}"))
            .header(header::CONTENT_TYPE, "application/json");
        if no_fallbacks {
            request = request.header("x-olp-routing", r#"{"allow_fallbacks":false}"#);
        }
        let response = crate::http::router::gateway_router_for_test(state)
            .oneshot(
                request
                    .body(Body::from(
                        r#"{"model":"default","messages":[{"role":"user","content":"hello"}]}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::BAD_GATEWAY);
        assert_eq!(calls.load(Ordering::SeqCst), expected);
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
        CanonicalResult::TokenCount(crate::protocols::canonical::results::TokenCountResult {
            input_tokens: 1,
            extensions: crate::protocols::canonical::requests::SourceExtensions::new(
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
            credential_version_id: None,
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
    let operation = Operation::Images(crate::protocols::canonical::requests::ImageOperation::Edit(
        crate::protocols::canonical::requests::ImageEditRequest {
            route: RouteSlug::parse("default").unwrap(),
            images: vec![MediaHandle::new("bounded-image")],
            mask: None,
            prompt: "x".repeat(8_500),
            stream: false,
            extensions: crate::protocols::canonical::requests::SourceExtensions::default(),
        },
    ));
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

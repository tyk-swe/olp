use super::*;

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

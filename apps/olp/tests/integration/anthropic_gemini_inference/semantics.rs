use super::*;

#[tokio::test]
async fn inline_media_is_admitted_for_same_protocol_and_rejected_when_malformed_or_oversized() {
    let fixture = test_gateway();
    let app = gateway_router_for_test(fixture.state.clone());
    let response = app
        .clone()
        .oneshot(post_json(
            "/anthropic/v1/messages",
            ("x-api-key", &fixture.key),
            json!({
                "model":"team-default","max_tokens":8,
                "messages":[{"role":"user","content":[{"type":"image","source":{
                    "type":"base64","media_type":"image/png","data":"aGk="
                }}]}]
            }),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);

    let response = app
        .clone()
        .oneshot(post_json(
            "/gemini/v1beta/models/team-default:generateContent",
            ("x-goog-api-key", &fixture.key),
            json!({"contents":[{"role":"user","parts":[{"inlineData":{
                "mimeType":"image/png","data":"aGk="
            }}]}]}),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let successful_calls = fixture.calls.lock().unwrap().len();

    for data in [
        "%%%".to_owned(),
        STANDARD.encode(vec![0_u8; 1024 * 1024 + 1]),
    ] {
        let response = app
            .clone()
            .oneshot(post_json(
                "/anthropic/v1/messages",
                ("x-api-key", &fixture.key),
                json!({
                    "model":"team-default","max_tokens":8,
                    "messages":[{"role":"user","content":[{"type":"image","source":{
                        "type":"base64","media_type":"image/png","data":data
                    }}]}]
                }),
            ))
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    }
    assert_eq!(fixture.calls.lock().unwrap().len(), successful_calls);
}

#[tokio::test]
async fn certified_cross_protocol_tuple_is_runtime_reachable_without_semantic_loss() {
    let fixture = test_gateway();
    let pinned = fixture.state.runtime().pin();
    let mut snapshot = Snapshot {
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::new(),
            ordinal: pinned.generation.ordinal + 1,
            activated_at: Utc::now(),
        },
        providers: pinned.providers.clone(),
        routes: pinned.routes.clone(),
        api_keys: pinned.api_keys.clone(),
    };
    snapshot
        .providers
        .retain(|provider_id, _| *provider_id == fixture.anthropic_provider);
    let provider = snapshot
        .providers
        .get_mut(&fixture.anthropic_provider)
        .unwrap();
    // This is the exact cross-origin tuple admitted by native certification:
    // an OpenAI client surface translated to Anthropic generation transport.
    provider.capabilities = BTreeSet::from([Capability::new(
        "claude-private",
        OperationKind::Generation,
        Surface::OpenAi,
        TransportMode::Unary,
    )]);
    let route = snapshot.routes.values_mut().next().unwrap();
    route.operations = BTreeSet::from([OperationKind::Generation]);
    route.max_attempts = NonZeroU16::new(1).unwrap();
    route
        .targets
        .retain(|target| target.provider_id == fixture.anthropic_provider);
    fixture
        .state
        .runtime()
        .install(
            snapshot,
            BTreeMap::from([(
                fixture.anthropic_provider,
                Arc::new(MockTransport {
                    provider_id: fixture.anthropic_provider,
                    native_surface: Surface::Anthropic,
                    text: "cross-protocol answer",
                    calls: fixture.calls.clone(),
                }) as Arc<dyn ProviderTransport>,
            )]),
        )
        .unwrap();
    let app = gateway_router_for_test(fixture.state.clone());

    let response = app
        .clone()
        .oneshot(post_json(
            "/openai/v1/chat/completions",
            ("authorization", &format!("Bearer {}", fixture.key)),
            json!({
                "model": "team-default",
                "max_tokens": 32,
                "messages": [{"role":"user","content":"hello"}],
                "tools": [{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]
            }),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    assert_eq!(
        body_json(response).await["choices"][0]["message"]["content"],
        "cross-protocol answer"
    );

    // The internal Responses endpoint hint is removed before the Anthropic
    // encoder; it is not treated as client semantics or forwarded upstream.
    let response = app
        .clone()
        .oneshot(post_json(
            "/openai/v1/responses",
            ("authorization", &format!("Bearer {}", fixture.key)),
            json!({"model":"team-default","input":"hello","max_output_tokens":32}),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    assert_eq!(body_json(response).await["object"], "response");
    let successful_calls = fixture.calls.lock().unwrap().len();

    for body in [
        json!({
            "model":"team-default",
            "max_tokens":32,
            "messages":[{"role":"user","content":"hello"}],
            "response_format":{"type":"json_object"}
        }),
        json!({
            "model":"team-default",
            "max_tokens":32,
            "messages":[{"role":"user","content":"hello"}],
            "reasoning":{"effort":"high"}
        }),
        json!({
            "model":"team-default",
            "max_tokens":32,
            "messages":[{"role":"user","content":"hello"}],
            "citations":[{"url":"https://example.test"}]
        }),
        json!({
            "model":"team-default",
            "max_tokens":32,
            "messages":[{"role":"user","content":"hello"}],
            "safety":{"threshold":"strict"}
        }),
        json!({
            "model":"team-default",
            "max_tokens":32,
            "messages":[{"role":"user","content":[{"type":"refusal","refusal":"source-only media result"}]}]
        }),
        json!({
            "model":"team-default",
            "max_tokens":32,
            "messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{
                "data":"aGk=","format":"wav"
            }}]}]
        }),
    ] {
        let response = app
            .clone()
            .oneshot(post_json(
                "/openai/v1/chat/completions",
                ("authorization", &format!("Bearer {}", fixture.key)),
                body,
            ))
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
        let body = body_json(response).await;
        assert_eq!(body["error"]["type"], "invalid_request_error");
    }
    assert_eq!(fixture.calls.lock().unwrap().len(), successful_calls);
}

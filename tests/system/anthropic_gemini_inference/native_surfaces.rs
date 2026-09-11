use super::*;

#[tokio::test]
async fn native_model_discovery_applies_request_and_operator_routing_constraints() {
    for layer in ["request", "installation", "key", "credential"] {
        let fixture = test_gateway();
        if layer != "request" {
            let runtime = fixture.state.runtime().pin();
            let mut snapshot = (**runtime).clone();
            snapshot.generation.ordinal += 1;
            snapshot.generation.id = RuntimeGenerationId::new();
            match layer {
                "installation" => {
                    snapshot
                        .routing
                        .installation
                        .constraints
                        .require_zero_data_retention = true
                }
                "key" => {
                    snapshot
                        .api_keys
                        .values_mut()
                        .next()
                        .unwrap()
                        .routing_policy
                        .constraints
                        .require_zero_data_retention = true
                }
                "credential" => {
                    for id in snapshot.providers.keys() {
                        snapshot.routing.credentials.insert(
                            *id,
                            vec![olp::providers::pool::CredentialSlot {
                                id: uuid::Uuid::now_v7(),
                                name: "restricted".into(),
                                allowed_routes: vec!["other-route".into()],
                                ..Default::default()
                            }],
                        );
                    }
                }
                _ => unreachable!(),
            }
            let transports = snapshot
                .providers
                .keys()
                .map(|id| (*id, runtime.transport(*id).unwrap()))
                .collect();
            fixture
                .state
                .runtime()
                .install(snapshot, transports)
                .unwrap();
        }
        let app = gateway_router_for_test(fixture.state.clone());
        for (prefix, header, collection) in [
            ("/anthropic/v1/models", "x-api-key", "data"),
            ("/gemini/v1/models", "x-goog-api-key", "models"),
            ("/gemini/v1beta/models", "x-goog-api-key", "models"),
        ] {
            for single in [false, true] {
                let uri = if single {
                    format!("{prefix}/team-default")
                } else {
                    prefix.to_owned()
                };
                let mut request = Request::get(uri).header(header, &fixture.key);
                if layer == "request" {
                    request =
                        request.header("x-olp-routing", r#"{"require_zero_data_retention":true}"#);
                }
                let response = app
                    .clone()
                    .oneshot(request.body(Body::empty()).unwrap())
                    .await
                    .unwrap();
                assert_eq!(
                    response.status(),
                    if single {
                        StatusCode::NOT_FOUND
                    } else {
                        StatusCode::OK
                    },
                    "{layer} {prefix}"
                );
                if !single {
                    assert!(
                        body_json(response).await[collection]
                            .as_array()
                            .unwrap()
                            .is_empty(),
                        "{layer} {prefix}"
                    );
                }
            }
        }
        assert!(fixture.calls.lock().unwrap().is_empty());
    }
}

#[tokio::test]
async fn anthropic_unary_count_models_and_native_errors_use_the_shared_pipeline() {
    let fixture = test_gateway();
    let app = gateway_router_for_test(fixture.state.clone());
    let response = app
        .clone()
        .oneshot(post_json(
            "/anthropic/v1/messages",
            ("x-api-key", &fixture.key),
            json!({
                "model": "team-default",
                "max_tokens": 32,
                "messages": [{"role": "user", "content": "hello"}]
            }),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let body = body_json(response).await;
    assert_eq!(body["model"], "team-default");
    assert_eq!(body["content"][0]["text"], "anthropic answer");
    assert_eq!(body["type"], "message");

    let response = app
        .clone()
        .oneshot(post_json(
            "/anthropic/v1/messages",
            ("x-api-key", &fixture.key),
            json!({
                "model": "team-default",
                "max_tokens": 32,
                "stream": true,
                "messages": [{"role": "user", "content": "hello"}]
            }),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let wire = String::from_utf8(
        response
            .into_body()
            .collect()
            .await
            .unwrap()
            .to_bytes()
            .to_vec(),
    )
    .unwrap();
    assert!(wire.contains("event: message_start"));
    assert!(wire.contains("event: content_block_delta"));
    assert!(wire.contains("anthropic answer"));
    assert!(wire.contains("event: message_stop"));

    let response = app
        .clone()
        .oneshot(post_json(
            "/anthropic/v1/messages/count_tokens",
            ("x-api-key", &fixture.key),
            json!({
                "model": "team-default",
                "system": "count all semantics",
                "messages": [{"role": "user", "content": "hello"}],
                "tools": [{"name": "lookup", "input_schema": {"type": "object"}}]
            }),
        ))
        .await
        .unwrap();
    assert_eq!(body_json(response).await["input_tokens"], 13);

    let response = app
        .clone()
        .oneshot(
            Request::get("/anthropic/v1/models/team-default")
                .header("x-api-key", &fixture.key)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    let body = body_json(response).await;
    assert_eq!(body["id"], "team-default");
    assert_eq!(body["type"], "model");

    let response = app
        .clone()
        .oneshot(
            Request::get("/anthropic/v1/models?limit=1")
                .header("x-api-key", &fixture.key)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    let body = body_json(response).await;
    assert_eq!(body["data"][0]["id"], "team-default");
    assert_eq!(body["has_more"], false);

    let stale = app
        .clone()
        .oneshot(
            Request::get("/anthropic/v1/models?after_id=removed-route")
                .header("x-api-key", &fixture.key)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(stale.status(), StatusCode::BAD_REQUEST);

    let response = app
        .oneshot(post_json(
            "/anthropic/v1/messages",
            ("x-api-key", "bad-key"),
            json!({"model":"team-default","max_tokens":1,"messages":[{"role":"user","content":"x"}]}),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
    let body = body_json(response).await;
    assert_eq!(body["type"], "error");
    assert_eq!(body["error"]["type"], "authentication_error");

    let calls = fixture.calls.lock().unwrap();
    assert!(calls.iter().any(|call| {
        call.provider_id == fixture.anthropic_provider
            && call.surface == Surface::Anthropic
            && call.operation == OperationKind::Generation
            && call.route == "team-default"
    }));
    assert!(!calls.iter().any(|call| {
        call.provider_id == fixture.gemini_provider && call.surface == Surface::Anthropic
    }));
}

#[tokio::test]
async fn both_gemini_versions_support_unary_sdk_sse_count_and_models() {
    let fixture = test_gateway();
    let app = gateway_router_for_test(fixture.state.clone());
    for version in ["v1", "v1beta"] {
        let response = app
            .clone()
            .oneshot(post_json(
                &format!("/gemini/{version}/models/team-default:generateContent"),
                ("x-goog-api-key", &fixture.key),
                json!({"contents":[{"role":"user","parts":[{"text":"hello"}]}]}),
            ))
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::OK);
        let body = body_json(response).await;
        assert_eq!(body["modelVersion"], "team-default");
        assert_eq!(
            body["candidates"][0]["content"]["parts"][0]["text"],
            "gemini answer"
        );
    }

    let response = app
        .clone()
        .oneshot(post_json(
            "/gemini/v1beta/models/team-default:streamGenerateContent?alt=sse",
            ("x-goog-api-key", &fixture.key),
            json!({"contents":[{"role":"user","parts":[{"text":"hello"}]}]}),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    assert_eq!(
        response.headers()["content-type"],
        "text/event-stream; charset=utf-8"
    );
    let bytes = response.into_body().collect().await.unwrap().to_bytes();
    let wire = String::from_utf8(bytes.to_vec()).unwrap();
    assert!(wire.contains("data: "));
    assert!(wire.contains("gemini answer"));
    assert!(wire.contains("\"modelVersion\":\"team-default\""));

    let response = app
        .clone()
        .oneshot(post_json(
            "/gemini/v1/models/team-default:countTokens",
            ("x-goog-api-key", &fixture.key),
            json!({"contents":[{"role":"user","parts":[{"text":"hello"}]}]}),
        ))
        .await
        .unwrap();
    assert_eq!(body_json(response).await["totalTokens"], 13);

    let response = app
        .clone()
        .oneshot(
            Request::get("/gemini/v1/models?pageSize=1")
                .header("x-goog-api-key", &fixture.key)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    let body = body_json(response).await;
    assert_eq!(body["models"][0]["name"], "models/team-default");
    assert!(
        body["models"][0]["supportedGenerationMethods"]
            .as_array()
            .unwrap()
            .iter()
            .any(|method| method == "generateContent")
    );

    let stale = app
        .clone()
        .oneshot(
            Request::get("/gemini/v1/models?pageToken=b2xwLXYxOmdvbmU")
                .header("x-goog-api-key", &fixture.key)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(stale.status(), StatusCode::BAD_REQUEST);

    let response = app
        .clone()
        .oneshot(
            Request::get("/gemini/v1beta/models/team-default")
                .header("x-goog-api-key", &fixture.key)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    let body = body_json(response).await;
    assert_eq!(body["name"], "models/team-default");
    assert_eq!(body["baseModelId"], "team-default");

    let response = app
        .oneshot(post_json(
            "/gemini/v1/models/provider/model:generateContent",
            ("x-goog-api-key", &fixture.key),
            json!({"contents":[{"parts":[{"text":"x"}]}]}),
        ))
        .await
        .unwrap();
    assert!(matches!(
        response.status(),
        StatusCode::BAD_REQUEST | StatusCode::NOT_FOUND
    ));

    let calls = fixture.calls.lock().unwrap();
    assert!(calls.iter().any(|call| {
        call.provider_id == fixture.gemini_provider
            && call.surface == Surface::Gemini
            && call.mode == TransportMode::Streaming
    }));
    assert!(!calls.iter().any(|call| {
        call.provider_id == fixture.anthropic_provider && call.surface == Surface::Gemini
    }));
}

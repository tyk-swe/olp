use super::*;

#[tokio::test]
async fn bootstrap_token_digest_is_verified_then_cleared() {
    let mut state = ProcessComposition::new(
        ApiMode::Control,
        None,
        Arc::new(RuntimeManager::empty()),
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    );
    let auth_hmac_key = Arc::new(AuthHmacKey::new([3; 32]));
    let token = base64::engine::general_purpose::STANDARD.encode([7_u8; 32]);
    let digest = auth_hmac_key
        .bootstrap_token_digest_from_base64(&token)
        .unwrap();
    state.auth_hmac_key = Some(auth_hmac_key);
    state.set_bootstrap_token_digest(digest);
    let state = state.gateway_state_for_test();
    assert_eq!(state.verify_bootstrap_token(Some(&token)).await, Some(true));
    assert_eq!(
        state.verify_bootstrap_token(Some("not-a-token")).await,
        Some(false)
    );
    state.clear_bootstrap_token().await;
    assert_eq!(state.verify_bootstrap_token(Some(&token)).await, None);
}

#[tokio::test]
async fn inference_authentication_precedes_body_decode_with_native_errors() {
    let mut state = ProcessComposition::new(
        ApiMode::Gateway,
        None,
        Arc::new(RuntimeManager::empty()),
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    );
    state.auth_hmac_key = Some(Arc::new(AuthHmacKey::new([3; 32])));
    let app = public_router(state.gateway_state_for_test());
    let too_deep = format!("{}0{}", "[".repeat(65), "]".repeat(65));
    for (path, header_name, value, expected_pointer) in [
        (
            "/openai/v1/chat/completions",
            axum::http::header::AUTHORIZATION,
            "Bearer invalid-key",
            "/error/code",
        ),
        (
            "/anthropic/v1/messages",
            HeaderName::from_static("x-api-key"),
            "invalid-key",
            "/error/type",
        ),
        (
            "/gemini/v1beta/models/test:generateContent",
            HeaderName::from_static("x-goog-api-key"),
            "invalid-key",
            "/error/status",
        ),
        (
            "/openai/v1/chat/completions",
            HeaderName::from_static("x-litellm-api-key"),
            "Bearer invalid-key",
            "/error/code",
        ),
    ] {
        let response = app
            .clone()
            .oneshot(
                Request::post(path)
                    .header(axum::http::header::CONTENT_TYPE, "application/json")
                    .header(header_name, value)
                    .body(Body::from(too_deep.clone()))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), axum::http::StatusCode::UNAUTHORIZED);
        assert_ne!(
            response.headers()[axum::http::header::CONTENT_TYPE],
            "application/problem+json"
        );
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert!(body.pointer(expected_pointer).is_some(), "body was {body}");
    }
}

#[tokio::test]
async fn every_inference_surface_and_models_endpoint_requires_its_own_well_formed_header() {
    let (state, key) = inference_state(false);
    let app = public_router(state.gateway_state_for_test());
    let cases = [
        (
            axum::http::Method::POST,
            "/openai/v1/chat/completions",
            axum::http::header::AUTHORIZATION,
            "Bearer malformed key",
            "/error/code",
            "invalid_api_key",
        ),
        (
            axum::http::Method::GET,
            "/openai/v1/models",
            axum::http::header::AUTHORIZATION,
            "Bearer malformed key",
            "/error/code",
            "invalid_api_key",
        ),
        (
            axum::http::Method::GET,
            "/openai/v1/models/default",
            axum::http::header::AUTHORIZATION,
            "Bearer malformed key",
            "/error/code",
            "invalid_api_key",
        ),
        (
            axum::http::Method::POST,
            "/anthropic/v1/messages",
            HeaderName::from_static("x-api-key"),
            "malformed key",
            "/error/type",
            "authentication_error",
        ),
        (
            axum::http::Method::GET,
            "/anthropic/v1/models",
            HeaderName::from_static("x-api-key"),
            "malformed key",
            "/error/type",
            "authentication_error",
        ),
        (
            axum::http::Method::GET,
            "/anthropic/v1/models/default",
            HeaderName::from_static("x-api-key"),
            "malformed key",
            "/error/type",
            "authentication_error",
        ),
        (
            axum::http::Method::POST,
            "/gemini/v1/models/default:generateContent",
            HeaderName::from_static("x-goog-api-key"),
            "malformed key",
            "/error/status",
            "UNAUTHENTICATED",
        ),
        (
            axum::http::Method::GET,
            "/gemini/v1/models",
            HeaderName::from_static("x-goog-api-key"),
            "malformed key",
            "/error/status",
            "UNAUTHENTICATED",
        ),
        (
            axum::http::Method::GET,
            "/gemini/v1/models/default",
            HeaderName::from_static("x-goog-api-key"),
            "malformed key",
            "/error/status",
            "UNAUTHENTICATED",
        ),
        (
            axum::http::Method::GET,
            "/gemini/v1beta/models",
            HeaderName::from_static("x-goog-api-key"),
            "malformed key",
            "/error/status",
            "UNAUTHENTICATED",
        ),
        (
            axum::http::Method::GET,
            "/gemini/v1beta/models/default",
            HeaderName::from_static("x-goog-api-key"),
            "malformed key",
            "/error/status",
            "UNAUTHENTICATED",
        ),
    ];

    for (method, path, required_header, malformed, pointer, expected) in cases {
        for supplied in [None, Some(malformed)] {
            let mut request = Request::builder()
                .method(method.clone())
                .uri(path)
                .header(axum::http::header::CONTENT_TYPE, "application/json")
                .body(Body::from("{}"))
                .unwrap();
            request.headers_mut().insert(
                axum::http::header::AUTHORIZATION,
                HeaderValue::from_str(&format!("Bearer {key}")).unwrap(),
            );
            request.headers_mut().insert(
                HeaderName::from_static("x-api-key"),
                HeaderValue::from_str(&key).unwrap(),
            );
            request.headers_mut().insert(
                HeaderName::from_static("x-goog-api-key"),
                HeaderValue::from_str(&key).unwrap(),
            );
            match supplied {
                Some(value) => {
                    request
                        .headers_mut()
                        .insert(required_header.clone(), HeaderValue::from_static(value));
                }
                None => {
                    request.headers_mut().remove(&required_header);
                }
            }
            let response = app.clone().oneshot(request).await.unwrap();
            assert_eq!(
                response.status(),
                axum::http::StatusCode::UNAUTHORIZED,
                "{method} {path}"
            );
            if required_header == axum::http::header::AUTHORIZATION {
                assert_eq!(
                    response.headers()[axum::http::header::WWW_AUTHENTICATE],
                    "Bearer"
                );
            }
            let body = response.into_body().collect().await.unwrap().to_bytes();
            let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
            assert_eq!(
                body.pointer(pointer).and_then(serde_json::Value::as_str),
                Some(expected),
                "{method} {path}: {body}"
            );
        }
    }
}

#[tokio::test]
async fn litellm_gateway_credentials_authenticate_each_surface_in_both_forms() {
    let (state, key) = inference_state(false);
    let app = public_router(state.gateway_state_for_test());
    for (path, value) in [
        ("/openai/v1/models", format!("Bearer {key}")),
        ("/anthropic/v1/models", key.clone()),
        ("/gemini/v1/models", format!("Bearer {key}")),
    ] {
        let response = app
            .clone()
            .oneshot(
                Request::get(path)
                    .header("x-litellm-api-key", value)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), axum::http::StatusCode::OK, "{path}");
    }

    let response = app
        .clone()
        .oneshot(
            Request::get("/openai/v1/models")
                .header("x-litellm-api-key", format!("Bearer {key}"))
                .header(
                    axum::http::header::AUTHORIZATION,
                    "Bearer upstream-oauth-token",
                )
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), axum::http::StatusCode::OK);

    for (path, native_header, native_value) in [
        (
            "/openai/v1/models",
            axum::http::header::AUTHORIZATION,
            format!("Bearer {key}"),
        ),
        (
            "/anthropic/v1/models",
            HeaderName::from_static("x-api-key"),
            key.clone(),
        ),
        (
            "/gemini/v1/models",
            HeaderName::from_static("x-goog-api-key"),
            key.clone(),
        ),
    ] {
        let response = app
            .clone()
            .oneshot(
                Request::get(path)
                    .header("x-litellm-api-key", format!("Bearer {key}"))
                    .header(native_header, native_value)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), axum::http::StatusCode::OK, "{path}");
    }
}

#[tokio::test]
async fn litellm_gateway_credentials_are_authoritative_and_conflicts_fail_closed() {
    let (state, key) = inference_state(false);
    let other_key = AuthHmacKey::new([19; 32])
        .generate_api_key()
        .expose_once()
        .to_owned();
    let app = public_router(state.gateway_state_for_test());

    for (path, native_header, native_value, pointer, expected) in [
        (
            "/openai/v1/models",
            axum::http::header::AUTHORIZATION,
            format!("Bearer {other_key}"),
            "/error/code",
            "invalid_api_key",
        ),
        (
            "/anthropic/v1/models",
            HeaderName::from_static("x-api-key"),
            other_key.clone(),
            "/error/type",
            "authentication_error",
        ),
        (
            "/gemini/v1/models",
            HeaderName::from_static("x-goog-api-key"),
            other_key.clone(),
            "/error/status",
            "UNAUTHENTICATED",
        ),
    ] {
        let response = app
            .clone()
            .oneshot(
                Request::get(path)
                    .header("x-litellm-api-key", format!("Bearer {key}"))
                    .header(native_header, native_value)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(
            response.status(),
            axum::http::StatusCode::UNAUTHORIZED,
            "{path}"
        );
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let body_text = String::from_utf8(body.to_vec()).unwrap();
        let body: serde_json::Value = serde_json::from_str(&body_text).unwrap();
        assert_eq!(
            body.pointer(pointer).and_then(serde_json::Value::as_str),
            Some(expected)
        );
        assert!(!body_text.contains(&key));
        assert!(!body_text.contains(&other_key));
    }

    for (path, native_header, native_value, pointer, expected) in [
        (
            "/openai/v1/models",
            axum::http::header::AUTHORIZATION,
            format!("Bearer {key}"),
            "/error/code",
            "invalid_api_key",
        ),
        (
            "/anthropic/v1/models",
            HeaderName::from_static("x-api-key"),
            key.clone(),
            "/error/type",
            "authentication_error",
        ),
        (
            "/gemini/v1/models",
            HeaderName::from_static("x-goog-api-key"),
            key.clone(),
            "/error/status",
            "UNAUTHENTICATED",
        ),
    ] {
        let response = app
            .clone()
            .oneshot(
                Request::get(path)
                    .header("x-litellm-api-key", "Bearer invalid-key")
                    .header(native_header, native_value)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(
            response.status(),
            axum::http::StatusCode::UNAUTHORIZED,
            "{path}"
        );
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(
            body.pointer(pointer).and_then(serde_json::Value::as_str),
            Some(expected)
        );
    }
}

#[tokio::test]
async fn revoked_and_expired_keys_are_rejected_by_admission() {
    for (status, expires_at) in [
        (ApiKeyStatus::Revoked, None),
        (
            ApiKeyStatus::Active,
            Some(chrono::Utc::now() - chrono::Duration::seconds(1)),
        ),
    ] {
        let (state, key) = inference_state(false);
        let pinned = state.runtime.pin();
        let mut api_keys = pinned.api_keys.clone();
        let api_key = api_keys.values_mut().next().unwrap();
        api_key.status = status;
        api_key.expires_at = expires_at;
        state
            .runtime
            .install(
                RuntimeSnapshot {
                    generation: RuntimeGeneration {
                        id: RuntimeGenerationId::new(),
                        ordinal: pinned.generation.ordinal + 1,
                        activated_at: chrono::Utc::now(),
                    },
                    providers: pinned.providers.clone(),
                    routes: pinned.routes.clone(),
                    api_keys,
                },
                BTreeMap::new(),
            )
            .unwrap();
        let app = public_router(state.gateway_state_for_test());
        let litellm_response = app
            .clone()
            .oneshot(
                Request::get("/openai/v1/models")
                    .header("x-litellm-api-key", format!("Bearer {key}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(
            litellm_response.status(),
            axum::http::StatusCode::UNAUTHORIZED
        );
        let response = app
            .oneshot(
                Request::get("/openai/v1/models")
                    .header(axum::http::header::AUTHORIZATION, format!("Bearer {key}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), axum::http::StatusCode::UNAUTHORIZED);
    }
}

#[tokio::test]
async fn authenticated_unknown_protocol_paths_keep_the_router_fallback_behavior() {
    let (state, key) = inference_state(false);
    let app = public_router(state.gateway_state_for_test());
    for (path, header_name, header_value) in [
        (
            "/openai/v1/not-enabled",
            axum::http::header::AUTHORIZATION,
            format!("Bearer {key}"),
        ),
        (
            "/anthropic/v2/not-enabled",
            HeaderName::from_static("x-api-key"),
            key.clone(),
        ),
        (
            "/gemini/v2/not-enabled",
            HeaderName::from_static("x-goog-api-key"),
            key.clone(),
        ),
        (
            "/openai/v1/videos/video-id/extra",
            axum::http::header::AUTHORIZATION,
            format!("Bearer {key}"),
        ),
    ] {
        let response = app
            .clone()
            .oneshot(
                Request::get(path)
                    .header(header_name, header_value)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(
            response.status(),
            axum::http::StatusCode::NOT_FOUND,
            "{path}"
        );
        assert_eq!(
            response.headers()[axum::http::header::CONTENT_TYPE],
            "application/problem+json",
            "{path}"
        );
    }

    let response = app
        .oneshot(
            Request::get("/openai/v1/chat/completions")
                .header(axum::http::header::AUTHORIZATION, format!("Bearer {key}"))
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(
        response.status(),
        axum::http::StatusCode::METHOD_NOT_ALLOWED
    );
}

#[tokio::test]
async fn malformed_inference_requests_with_hard_limits_fail_closed_before_decode() {
    let (state, key) = inference_state(true);
    let app = public_router(state.gateway_state_for_test());
    for (path, header_name, header_value, content_type, body, pointer, expected) in [
        (
            "/openai/v1/chat/completions",
            axum::http::header::AUTHORIZATION,
            format!("Bearer {key}"),
            "application/json",
            "{",
            "/error/code",
            "distributed_limits_unavailable",
        ),
        (
            "/anthropic/v1/messages",
            HeaderName::from_static("x-api-key"),
            key.clone(),
            "application/json",
            "{",
            "/error/type",
            "api_error",
        ),
        (
            "/gemini/v1beta/models/default:generateContent",
            HeaderName::from_static("x-goog-api-key"),
            key.clone(),
            "application/json",
            "{",
            "/error/status",
            "UNAVAILABLE",
        ),
        (
            "/openai/v1/audio/transcriptions",
            axum::http::header::AUTHORIZATION,
            format!("Bearer {key}"),
            "multipart/form-data",
            "not-multipart",
            "/error/code",
            "distributed_limits_unavailable",
        ),
    ] {
        let response = app
            .clone()
            .oneshot(
                Request::post(path)
                    .header(header_name, header_value)
                    .header(axum::http::header::CONTENT_TYPE, content_type)
                    .body(Body::from(body))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(
            response.status(),
            axum::http::StatusCode::SERVICE_UNAVAILABLE,
            "{path} bypassed hard-limit fail-closed behavior"
        );
        assert_ne!(
            response.headers()[axum::http::header::CONTENT_TYPE],
            "application/problem+json",
            "{path} did not retain its native protocol error envelope"
        );
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(
            body.pointer(pointer).and_then(|value| value.as_str()),
            Some(expected)
        );
    }
}

#[tokio::test]
async fn malformed_inference_json_without_hard_limits_reaches_native_decoder() {
    let (mut state, key) = inference_state(false);
    let (request_metadata, mut receiver) = RequestMetadataEmitter::bounded(2);
    state.request_metadata = Some(request_metadata);
    let response = public_router(state.gateway_state_for_test())
        .oneshot(
            Request::post("/openai/v1/chat/completions")
                .header(axum::http::header::AUTHORIZATION, format!("Bearer {key}"))
                .header(axum::http::header::CONTENT_TYPE, "application/json")
                .body(Body::from("{"))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), axum::http::StatusCode::BAD_REQUEST);
    let event = receiver.recv_next().await.unwrap();
    assert_eq!(event.status_code, Some(400));
    assert_eq!(event.operation, OperationKind::Generation);
    assert_eq!(event.route_slug, "invalid-request");
    assert!(event.attempts.is_empty());
    assert!(!event.committed);
}

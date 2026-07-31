use super::*;

#[test]
fn public_auth_source_uses_forwarding_only_from_trusted_peers() {
    let mut state = ApiState::new(
        ApiMode::Control,
        None,
        Arc::new(RuntimeManager::empty()),
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    );
    state.set_trusted_proxy_cidrs(vec!["10.0.0.0/8".parse().unwrap()]);
    let state = state.gateway_state_for_test();
    let mut forwarded = HeaderMap::new();
    forwarded.insert(
        "x-forwarded-for",
        HeaderValue::from_static("198.51.100.24, 10.1.2.3"),
    );
    assert_eq!(
        public_auth_source(&state, &forwarded, Some("10.2.3.4:443".parse().unwrap()),).unwrap(),
        "198.51.100.24"
    );

    let mut spoofed = HeaderMap::new();
    spoofed.insert("x-forwarded-for", HeaderValue::from_static("not-an-ip"));
    assert_eq!(
        public_auth_source(&state, &spoofed, Some("203.0.113.30:443".parse().unwrap()),).unwrap(),
        "203.0.113.30"
    );
    assert_eq!(
        public_auth_source(
            &state,
            &HeaderMap::new(),
            Some("10.2.3.4:443".parse().unwrap()),
        )
        .unwrap_err()
        .status,
        400
    );
    assert_eq!(
        public_auth_source(&state, &spoofed, Some("10.2.3.4:443".parse().unwrap()),)
            .unwrap_err()
            .status,
        400
    );
    assert_eq!(
        public_auth_source(&state, &HeaderMap::new(), None)
            .unwrap_err()
            .status,
        503
    );
}

#[test]
fn multipart_admission_is_post_only_and_recovers_after_a_parser_drops() {
    assert!(
        InferenceEndpoint::classify(&axum::http::Method::GET, "/openai/v1/videos")
            .unwrap()
            .multipart()
            .is_none()
    );
    assert!(
        InferenceEndpoint::classify(&axum::http::Method::POST, "/openai/v1/videos")
            .unwrap()
            .multipart()
            .is_some()
    );

    // With a 256-byte spool, untrusted multipart parsers may reserve at
    // most its 128-byte half-budget. A key gets at most one live parser,
    // and releasing/dropping a parser promptly admits the next one.
    let admission = MultipartAdmissionState::new(256);
    let first_key = uuid::Uuid::now_v7();
    let second_key = uuid::Uuid::now_v7();
    let first = admission.try_admit(first_key, 64).unwrap();
    assert!(admission.try_admit(first_key, 64).is_none());
    let second = admission.try_admit(second_key, 64).unwrap();
    assert!(admission.try_admit(uuid::Uuid::now_v7(), 64).is_none());

    first.release();
    assert!(admission.try_admit(first_key, 64).is_some());
    drop(second);
}

#[tokio::test]
async fn malformed_trusted_proxy_chain_is_rejected_before_public_auth_body_handling() {
    let mut state = ApiState::new(
        ApiMode::Control,
        None,
        Arc::new(RuntimeManager::empty()),
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    );
    state.set_trusted_proxy_cidrs(vec!["10.0.0.0/8".parse().unwrap()]);
    let response = public_router(state.management_state_for_test())
        .oneshot(
            Request::post("/api/v1/sessions")
                .header(axum::http::header::CONTENT_TYPE, "application/json")
                .header("x-forwarded-for", "not-an-ip")
                .extension(axum::extract::ConnectInfo(
                    "10.2.3.4:443".parse::<SocketAddr>().unwrap(),
                ))
                .body(Body::from("{"))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), axum::http::StatusCode::BAD_REQUEST);
}

#[tokio::test]
async fn malformed_trusted_proxy_chain_is_rejected_before_oidc_login_post_json_handling() {
    let mut state = ApiState::new(
        ApiMode::Control,
        None,
        Arc::new(RuntimeManager::empty()),
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    );
    state.set_trusted_proxy_cidrs(vec!["10.0.0.0/8".parse().unwrap()]);
    let response = public_router(state.management_state_for_test())
        .oneshot(
            Request::post("/api/v1/oidc/login")
                .header(axum::http::header::CONTENT_TYPE, "application/json")
                .header(axum::http::header::ORIGIN, "https://olp.example.test")
                .header("x-forwarded-for", "not-an-ip")
                .extension(axum::extract::ConnectInfo(
                    "10.2.3.4:443".parse::<SocketAddr>().unwrap(),
                ))
                .body(Body::from("{"))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), axum::http::StatusCode::BAD_REQUEST);
    let problem: Problem =
        serde_json::from_slice(&response.into_body().collect().await.unwrap().to_bytes()).unwrap();
    assert_eq!(
        problem.problem_type.as_ref(),
        "https://openllmproxy.dev/problems/forwarded_for_invalid"
    );
}

#[test]
fn local_metadata_detection_is_method_and_surface_exact() {
    assert_eq!(
        InferenceEndpoint::classify(&axum::http::Method::GET, "/openai/v1/models")
            .unwrap()
            .metadata()
            .map(|policy| (policy.operation, policy.fallback_route)),
        Some((OperationKind::ModelList, "models"))
    );
    assert_eq!(
        InferenceEndpoint::classify(&axum::http::Method::GET, "/gemini/v1beta/models/team-route")
            .unwrap()
            .metadata()
            .map(|policy| (policy.operation, policy.fallback_route)),
        Some((OperationKind::ModelGet, "models"))
    );
    assert_eq!(
        InferenceEndpoint::classify(&axum::http::Method::GET, "/openai/v1/videos")
            .unwrap()
            .metadata()
            .map(|policy| (policy.operation, policy.fallback_route)),
        Some((OperationKind::VideoList, "videos"))
    );
    assert_eq!(
        InferenceEndpoint::classify(&axum::http::Method::POST, "/openai/v1/videos")
            .unwrap()
            .metadata()
            .map(|policy| (policy.operation, policy.fallback_route)),
        Some((OperationKind::VideoCreate, "invalid-request"))
    );
}

#[tokio::test]
async fn local_metadata_event_is_content_free_and_reconcilable() {
    let (request_metadata, mut receiver) = RequestMetadataEmitter::bounded(1);
    let generation_id = uuid::Uuid::now_v7();
    let api_key_id = uuid::Uuid::now_v7();
    LocalRequestMetadata {
        request_metadata: Some(request_metadata),
        request_started_at: chrono::Utc::now(),
        runtime_generation_id: generation_id,
        api_key_id,
        route_slug: "models".to_owned(),
        operation: OperationKind::ModelList,
        surface: Surface::OpenAi,
        always_emit: true,
    }
    .emit(axum::http::StatusCode::OK);
    let event = receiver.recv_next().await.unwrap();
    assert_eq!(event.runtime_generation_id, generation_id);
    assert_eq!(event.api_key_id, api_key_id);
    assert_eq!(event.operation, OperationKind::ModelList);
    assert_eq!(event.route_slug, "models");
    assert!(event.provider_id.is_none());
    assert!(event.upstream_model.is_none());
    assert!(event.attempts.is_empty());
    assert!(!event.usage_complete);
}

#[test]
fn json_depth_scanner_ignores_strings_and_rejects_excessive_nesting() {
    validate_json_depth(br#"{"text":"[[[[{{{{","nested":[{"ok":true}]} }"#).unwrap();
    let too_deep = format!("{}0{}", "[".repeat(65), "]".repeat(65));
    assert_eq!(
        validate_json_depth(too_deep.as_bytes()).unwrap_err().status,
        axum::http::StatusCode::BAD_REQUEST.as_u16()
    );
}

#[test]
fn multipart_boundary_is_required_and_bounded() {
    validate_multipart_boundary("multipart/form-data; boundary=olp-boundary").unwrap();
    assert!(validate_multipart_boundary("multipart/form-data").is_err());
    assert!(
        validate_multipart_boundary(&format!(
            "multipart/form-data; boundary={}",
            "x".repeat(201)
        ))
        .is_err()
    );
}

#[test]
fn raw_json_tpm_estimate_includes_requested_output_and_candidates() {
    let body = br#"{"max_completion_tokens":8192,"n":3,"messages":[]}"#;
    let estimate = estimate_http_json_request_tokens(TokenEstimate::Generation, body);
    assert!(estimate >= 8_192 * 3);
    assert!(
        estimate_http_json_request_tokens(TokenEstimate::Generation, b"{") >= 4_096,
        "malformed generation requests retain a fail-safe output estimate"
    );
    assert!(
        estimate_http_json_request_tokens(TokenEstimate::Embeddings, body) < 4_096,
        "non-generation operations do not inherit generation output tokens"
    );
}

#[test]
fn raw_json_tpm_estimate_counts_compact_embedding_token_arrays() {
    let flat = serde_json::json!({
        "model": "default",
        "input": vec![0_u32; 100],
    });
    let nested = serde_json::json!({
        "model": "default",
        "input": vec![vec![0_u32; 40], vec![0_u32; 60]],
    });
    for body in [flat, nested] {
        let body = serde_json::to_vec(&body).unwrap();
        assert_eq!(
            estimate_http_json_request_tokens(TokenEstimate::Embeddings, &body),
            100
        );
    }
}

#[tokio::test]
async fn json_body_read_has_its_own_deadline_outside_route_layers() {
    let body = Body::from_stream(futures::stream::pending::<Result<bytes::Bytes, Infallible>>());
    let result = read_json_body(body, MAX_JSON_BODY_BYTES, Duration::from_millis(5)).await;
    assert_eq!(result.unwrap_err(), JsonBodyReadError::Timeout);
}

#[tokio::test]
async fn json_body_read_distinguishes_overflow_from_transport_failure() {
    let overflow = read_json_body(
        Body::from(bytes::Bytes::from_static(b"too large")),
        3,
        Duration::from_secs(1),
    )
    .await;
    assert_eq!(overflow.unwrap_err(), JsonBodyReadError::Rejected);

    let failed = Body::from_stream(futures::stream::once(async {
        Err::<bytes::Bytes, _>(std::io::Error::other("client disconnected"))
    }));
    let failed = read_json_body(failed, 100, Duration::from_secs(1)).await;
    assert_eq!(failed.unwrap_err(), JsonBodyReadError::Transport);
}

#[tokio::test]
async fn zero_byte_body_limit_accepts_only_an_empty_body() {
    assert!(
        read_json_body(Body::empty(), 0, Duration::from_secs(1))
            .await
            .unwrap()
            .is_empty()
    );
    assert_eq!(
        read_json_body(Body::from("x"), 0, Duration::from_secs(1))
            .await
            .unwrap_err(),
        JsonBodyReadError::Rejected
    );
}

#[test]
fn singleton_and_api_credential_headers_are_unambiguous() {
    let mut headers = HeaderMap::new();
    headers.append(
        axum::http::header::CONTENT_TYPE,
        HeaderValue::from_static("application/json"),
    );
    headers.append(
        axum::http::header::CONTENT_TYPE,
        HeaderValue::from_static("text/plain"),
    );
    assert_eq!(
        validate_singleton_headers(&headers).unwrap_err().status,
        axum::http::StatusCode::BAD_REQUEST
    );

    let mut headers = HeaderMap::new();
    headers.insert(
        axum::http::header::AUTHORIZATION,
        HeaderValue::from_static("Bearer token"),
    );
    headers.insert("x-api-key", HeaderValue::from_static("token"));
    assert_eq!(
        validate_singleton_headers(&headers).unwrap_err().status,
        axum::http::StatusCode::BAD_REQUEST
    );
}

#[tokio::test]
async fn request_limit_matrix_rejects_depth_size_encoding_and_bad_multipart() {
    let (state, key) = inference_state(false);
    let app = public_router(state.gateway_state_for_test());

    let too_deep = format!("{}0{}", "[".repeat(65), "]".repeat(65));
    let response = app
        .clone()
        .oneshot(
            Request::post("/api/not-found")
                .header(axum::http::header::CONTENT_TYPE, "application/json")
                .body(Body::from(too_deep))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), axum::http::StatusCode::BAD_REQUEST);

    let response = app
        .clone()
        .oneshot(
            Request::post("/openai/not-found")
                .header(axum::http::header::CONTENT_TYPE, "application/json")
                .header(
                    axum::http::header::CONTENT_LENGTH,
                    (MAX_JSON_BODY_BYTES + 1).to_string(),
                )
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), axum::http::StatusCode::PAYLOAD_TOO_LARGE);

    let response = app
        .clone()
        .oneshot(
            Request::post("/openai/not-found")
                .header(axum::http::header::CONTENT_TYPE, "application/json")
                .header(axum::http::header::CONTENT_ENCODING, "gzip")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(
        response.status(),
        axum::http::StatusCode::UNSUPPORTED_MEDIA_TYPE
    );

    let response = app
        .clone()
        .oneshot(
            Request::get("/openai/v1/models")
                .header(axum::http::header::CONTENT_TYPE, "application/json")
                .header(axum::http::header::AUTHORIZATION, format!("Bearer {key}"))
                .body(Body::from("{}"))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), axum::http::StatusCode::BAD_REQUEST);
    let error: serde_json::Value =
        serde_json::from_slice(&response.into_body().collect().await.unwrap().to_bytes()).unwrap();
    assert_eq!(
        error
            .pointer("/error/code")
            .and_then(serde_json::Value::as_str),
        Some("request_body_unsupported")
    );

    let response = app
        .oneshot(
            Request::post("/openai/v1/audio/transcriptions")
                .header(axum::http::header::CONTENT_TYPE, "multipart/form-data")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(
        response.status(),
        axum::http::StatusCode::UNAUTHORIZED,
        "inference authentication precedes multipart decoding"
    );
}

#[tokio::test]
async fn authenticated_multipart_routes_reject_non_multipart_content_types() {
    let (state, key) = inference_state(false);
    let app = public_router(state.gateway_state_for_test());
    for content_type in [None, Some("application/json")] {
        let mut request = Request::post("/openai/v1/images/edits")
            .header(axum::http::header::AUTHORIZATION, format!("Bearer {key}"));
        if let Some(content_type) = content_type {
            request = request.header(axum::http::header::CONTENT_TYPE, content_type);
        }
        let response = app
            .clone()
            .oneshot(request.body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(response.status(), axum::http::StatusCode::BAD_REQUEST);
    }
}

#[tokio::test]
async fn multipart_reserves_request_limits_before_route_parsing() {
    let (state, key) = inference_state(true);
    let app = public_router(state.gateway_state_for_test());
    let authorization = format!("Bearer {key}");

    let malformed = app
        .clone()
        .oneshot(
            Request::post("/openai/v1/images/edits")
                .header(axum::http::header::AUTHORIZATION, authorization.clone())
                .header(
                    axum::http::header::CONTENT_TYPE,
                    "multipart/form-data; boundary=olp-test-boundary",
                )
                .body(Body::from("not-a-multipart-body"))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(
        malformed.status(),
        axum::http::StatusCode::SERVICE_UNAVAILABLE,
        "malformed multipart must consume request limits before parser failure"
    );

    let body = concat!(
        "--olp-test-boundary\r\n",
        "Content-Disposition: form-data; name=\"model\"\r\n\r\n",
        "default\r\n",
        "--olp-test-boundary\r\n",
        "Content-Disposition: form-data; name=\"prompt\"\r\n\r\n",
        "edit this\r\n",
        "--olp-test-boundary\r\n",
        "Content-Disposition: form-data; name=\"image\"; filename=\"fixture.bin\"\r\n",
        "Content-Type: application/octet-stream\r\n\r\n",
        "image-bytes\r\n",
        "--olp-test-boundary--\r\n"
    );
    let parsed_multipart = app
        .clone()
        .oneshot(
            Request::post("/openai/v1/images/edits")
                .header(axum::http::header::AUTHORIZATION, authorization.clone())
                .header(
                    axum::http::header::CONTENT_TYPE,
                    "multipart/form-data; boundary=olp-test-boundary",
                )
                .body(Body::from(body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(
        parsed_multipart.status(),
        axum::http::StatusCode::SERVICE_UNAVAILABLE,
        "canonical execution must reserve after parsing the multipart route"
    );

    let malformed_json = app
        .oneshot(
            Request::post("/openai/v1/chat/completions")
                .header(axum::http::header::AUTHORIZATION, authorization)
                .header(axum::http::header::CONTENT_TYPE, "application/json")
                .body(Body::from("{"))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(
        malformed_json.status(),
        axum::http::StatusCode::SERVICE_UNAVAILABLE,
        "ordinary JSON admission must reserve before handler decoding"
    );
}

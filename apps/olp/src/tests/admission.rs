use super::*;

#[test]
fn public_auth_source_policy_is_exact_and_complete() {
    let expected = [
        (axum::http::Method::POST, "/api/v1/setup"),
        (axum::http::Method::POST, "/api/v1/sessions"),
        (axum::http::Method::POST, "/api/v1/invitations/accept"),
        (axum::http::Method::GET, "/api/v1/oidc/login"),
        (axum::http::Method::POST, "/api/v1/oidc/login"),
    ];

    assert_eq!(PublicAuthRoute::ALL.len(), expected.len());
    for (method, path) in expected {
        let route = PublicAuthRoute::classify(&method, path)
            .unwrap_or_else(|| panic!("missing source policy for {method} {path}"));
        assert_eq!(route.method(), method);
        assert_eq!(route.path(), path);
        assert_eq!(
            PublicAuthRoute::classify(&method, &format!("{path}?ignored=true")),
            None,
            "classification accepts URI paths, not path-and-query strings"
        );
    }

    for (method, path) in [
        (axum::http::Method::GET, "/api/v1/setup"),
        (axum::http::Method::GET, "/api/v1/sessions"),
        (axum::http::Method::HEAD, "/api/v1/oidc/login"),
        (axum::http::Method::OPTIONS, "/api/v1/oidc/login"),
        (axum::http::Method::PUT, "/api/v1/oidc/login"),
        (axum::http::Method::POST, "/api/v1/sessions/"),
        (axum::http::Method::POST, "/api/v1/sessions/current"),
        (axum::http::Method::POST, "/api/v1/sessions-extra"),
        (axum::http::Method::POST, "/prefix/api/v1/sessions"),
        (axum::http::Method::POST, "/api/v1/%73essions"),
        (axum::http::Method::POST, "/api/v1/oidc//login"),
        (axum::http::Method::POST, "/api/v1/oidc/../oidc/login"),
        (axum::http::Method::POST, "/api/v1/profile/reauthenticate"),
        (axum::http::Method::POST, "/api/v1/oidc/link"),
    ] {
        assert_eq!(
            PublicAuthRoute::classify(&method, path),
            None,
            "unexpected source policy for {method} {path}"
        );
    }

    let unknown = axum::http::Method::from_bytes(b"BREW").unwrap();
    assert_eq!(
        PublicAuthRoute::classify(&unknown, "/api/v1/sessions"),
        None
    );
}

#[test]
fn public_auth_query_strings_do_not_change_source_policy() {
    let request = Request::get("/api/v1/oidc/login?return_to=%2Fproviders")
        .body(Body::empty())
        .unwrap();
    assert_eq!(request.uri().path(), PublicAuthRoute::OidcLoginGet.path());
    assert_eq!(
        PublicAuthRoute::classify(request.method(), request.uri().path()),
        Some(PublicAuthRoute::OidcLoginGet)
    );
}

#[test]
fn public_auth_source_uses_forwarding_only_from_trusted_peers() {
    let mut state = ProcessComposition::new(
        ApiMode::Control,
        crate::bootstrap::mode_dependencies::test_store(),
        Arc::new(Manager::empty()),
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
        public_auth_source(
            state.request_boundary(),
            &forwarded,
            Some("10.2.3.4:443".parse().unwrap()),
        )
        .unwrap(),
        "198.51.100.24"
    );

    let mut spoofed = HeaderMap::new();
    spoofed.insert("x-forwarded-for", HeaderValue::from_static("not-an-ip"));
    assert_eq!(
        public_auth_source(
            state.request_boundary(),
            &spoofed,
            Some("203.0.113.30:443".parse().unwrap()),
        )
        .unwrap(),
        "203.0.113.30"
    );
    assert_eq!(
        public_auth_source(
            state.request_boundary(),
            &HeaderMap::new(),
            Some("10.2.3.4:443".parse().unwrap()),
        )
        .unwrap_err()
        .status,
        400
    );
    assert_eq!(
        public_auth_source(
            state.request_boundary(),
            &spoofed,
            Some("10.2.3.4:443".parse().unwrap()),
        )
        .unwrap_err()
        .status,
        400
    );
    assert_eq!(
        public_auth_source(state.request_boundary(), &HeaderMap::new(), None)
            .unwrap_err()
            .status,
        503
    );
}

#[test]
fn audit_request_provenance_records_only_what_the_boundary_proves() {
    let mut state = ProcessComposition::new(
        ApiMode::Control,
        crate::bootstrap::mode_dependencies::test_store(),
        Arc::new(Manager::empty()),
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    );
    state.set_trusted_proxy_cidrs(vec!["10.0.0.0/8".parse().unwrap()]);
    let state = state.gateway_state_for_test();

    // A trusted proxy that forwards no chain leaves no client address to
    // record. The request is still audited, with a null source address.
    let mut curl = HeaderMap::new();
    curl.insert(
        axum::http::header::USER_AGENT,
        HeaderValue::from_static("curl/8.5.0 (x86_64-pc-linux-gnu) libcurl/8.5.0"),
    );
    let provenance = audit_request_provenance(
        state.request_boundary(),
        &curl,
        Some("10.2.3.4:443".parse().unwrap()),
    );
    assert_eq!(provenance.source_ip, None);
    assert_eq!(provenance.user_agent_family.as_deref(), Some("curl"));

    // A direct client cannot claim someone else's address by sending the
    // forwarding header itself.
    let mut spoofed = curl.clone();
    spoofed.insert("x-forwarded-for", HeaderValue::from_static("10.9.9.9"));
    let provenance = audit_request_provenance(
        state.request_boundary(),
        &spoofed,
        Some("203.0.113.30:443".parse().unwrap()),
    );
    assert_eq!(
        provenance.source_ip,
        Some("203.0.113.30".parse::<std::net::IpAddr>().unwrap())
    );

    // Without a connected peer there is nothing to attribute the request to,
    // and the audit row records neither field rather than guessing.
    let provenance = audit_request_provenance(state.request_boundary(), &HeaderMap::new(), None);
    assert_eq!(provenance.source_ip, None);
    assert_eq!(provenance.user_agent_family, None);
}

#[test]
fn multipart_reservations_scale_with_media_cap() {
    let reservation = |path: &str, limits: BodyLimits| {
        InferenceEndpoint::classify(&axum::http::Method::POST, path)
            .unwrap()
            .multipart(limits)
            .unwrap()
            .1
    };
    let mib: usize = 1024 * 1024;
    let default = BodyLimits::default();
    let reservation = |path, limits| usize::try_from(reservation(path, limits)).unwrap();
    assert_eq!(reservation("/openai/v1/images/edits", default), 64 * mib);
    assert_eq!(
        reservation("/openai/v1/images/variations", default),
        55 * mib
    );
    assert_eq!(
        reservation("/openai/v1/audio/transcriptions", default),
        30 * mib
    );
    assert_eq!(reservation("/openai/v1/videos", default), 25 * mib);

    let doubled = BodyLimits {
        media_body_bytes: 128 * mib,
        ..default
    };
    assert_eq!(reservation("/openai/v1/images/edits", doubled), 128 * mib);
    assert_eq!(
        reservation("/openai/v1/images/variations", doubled),
        110 * mib
    );
    assert_eq!(
        reservation("/openai/v1/audio/transcriptions", doubled),
        60 * mib
    );
    assert_eq!(reservation("/openai/v1/videos", doubled), 50 * mib);
}

#[test]
fn body_limits_validate_against_spool_and_json_caps() {
    let default = BodyLimits::default();
    default.validate(1024 * 1024 * 1024).unwrap();
    assert!(default.validate(127 * 1024 * 1024).is_err());
    default.validate(128 * 1024 * 1024).unwrap();
    assert!(
        BodyLimits {
            inline_media_item_bytes: 3 * 1024 * 1024,
            ..default
        }
        .validate(u64::MAX)
        .is_err()
    );
    assert!(
        BodyLimits {
            json_body_bytes: 2 * 1024 * 1024 - 1,
            ..default
        }
        .validate(u64::MAX)
        .is_err()
    );
}

#[test]
fn multipart_admission_is_post_only_and_recovers_after_a_parser_drops() {
    assert!(
        InferenceEndpoint::classify(&axum::http::Method::GET, "/openai/v1/videos")
            .unwrap()
            .multipart(BodyLimits::default())
            .is_none()
    );
    assert!(
        InferenceEndpoint::classify(&axum::http::Method::POST, "/openai/v1/videos")
            .unwrap()
            .multipart(BodyLimits::default())
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
async fn malformed_trusted_proxy_chain_precedes_all_public_auth_json_handling() {
    let mut state = ProcessComposition::new(
        ApiMode::Control,
        crate::bootstrap::mode_dependencies::test_store(),
        Arc::new(Manager::empty()),
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    );
    state.set_trusted_proxy_cidrs(vec!["10.0.0.0/8".parse().unwrap()]);
    let app = management_router_for_test(state.management_state_for_test());

    for (path, requires_origin) in [("/api/v1/sessions", false), ("/api/v1/oidc/login", true)] {
        let mut request = Request::post(path)
            .header(axum::http::header::CONTENT_TYPE, "application/json")
            .header("x-forwarded-for", "not-an-ip")
            .extension(axum::extract::ConnectInfo(
                "10.2.3.4:443".parse::<SocketAddr>().unwrap(),
            ));
        if requires_origin {
            request = request.header(axum::http::header::ORIGIN, "https://olp.example.test");
        }
        let response = app
            .clone()
            .oneshot(request.body(Body::from("{")).unwrap())
            .await
            .unwrap();
        assert_eq!(response.status(), axum::http::StatusCode::BAD_REQUEST);
        let problem: Problem =
            serde_json::from_slice(&response.into_body().collect().await.unwrap().to_bytes())
                .unwrap();
        assert_eq!(
            problem.problem_type.as_ref(),
            "https://openllmproxy.dev/problems/forwarded_for_invalid",
            "source validation must precede JSON handling for {path}"
        );
    }
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
    let (request_metadata, mut receiver) = Emitter::bounded(1);
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
    let result = read_json_body(
        body,
        BodyLimits::default().json_body_bytes,
        Duration::from_millis(5),
    )
    .await;
    assert_eq!(result.unwrap_err(), JsonBodyReadError::Timeout);
}

#[tokio::test]
async fn request_limit_matrix_rejects_depth_size_encoding_and_bad_multipart() {
    let app = gateway_router_for_test(
        ProcessComposition::new(
            ApiMode::Gateway,
            crate::bootstrap::mode_dependencies::test_store(),
            Arc::new(Manager::empty()),
            "https://olp.example.test",
            PathBuf::from("missing-console"),
        )
        .gateway_state_for_test(),
    );

    let mut ambiguous_length = Request::post("/openai/not-found")
        .body(Body::empty())
        .unwrap();
    ambiguous_length.headers_mut().append(
        axum::http::header::CONTENT_LENGTH,
        HeaderValue::from_static("0"),
    );
    ambiguous_length.headers_mut().append(
        axum::http::header::CONTENT_LENGTH,
        HeaderValue::from_static("0"),
    );

    let cases = [
        (
            "excessive JSON depth",
            Request::post("/api/not-found")
                .header(axum::http::header::CONTENT_TYPE, "application/json")
                .body(Body::from(format!("{}0{}", "[".repeat(65), "]".repeat(65))))
                .unwrap(),
            axum::http::StatusCode::BAD_REQUEST,
        ),
        (
            "declared body size",
            Request::post("/openai/not-found")
                .header(axum::http::header::CONTENT_TYPE, "application/json")
                .header(
                    axum::http::header::CONTENT_LENGTH,
                    (BodyLimits::default().json_body_bytes + 1).to_string(),
                )
                .body(Body::empty())
                .unwrap(),
            axum::http::StatusCode::PAYLOAD_TOO_LARGE,
        ),
        (
            "content encoding",
            Request::post("/openai/not-found")
                .header(axum::http::header::CONTENT_TYPE, "application/json")
                .header(axum::http::header::CONTENT_ENCODING, "br")
                .body(Body::empty())
                .unwrap(),
            axum::http::StatusCode::UNSUPPORTED_MEDIA_TYPE,
        ),
        (
            "ambiguous body framing",
            ambiguous_length,
            axum::http::StatusCode::BAD_REQUEST,
        ),
        (
            "multipart authentication precedence",
            Request::post("/openai/v1/audio/transcriptions")
                .header(axum::http::header::CONTENT_TYPE, "multipart/form-data")
                .body(Body::empty())
                .unwrap(),
            axum::http::StatusCode::UNAUTHORIZED,
        ),
    ];

    for (case, request, expected) in cases {
        let response = app.clone().oneshot(request).await.unwrap();
        assert_eq!(response.status(), expected, "failed case: {case}");
    }
}

#[tokio::test]
async fn authenticated_multipart_routes_reject_non_multipart_content_types() {
    let (state, key) = inference_state(false);
    let app = gateway_router_for_test(state.gateway_state_for_test());
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

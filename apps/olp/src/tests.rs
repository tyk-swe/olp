use std::{
    collections::{BTreeMap, BTreeSet},
    convert::Infallible,
    net::SocketAddr,
    num::NonZeroU32,
    path::PathBuf,
    sync::atomic::{AtomicBool, Ordering},
    time::Duration,
};

use axum::{
    Router,
    body::Body,
    extract::{Extension, State},
    http::{HeaderMap, HeaderName, HeaderValue, Request, Response, Uri},
    middleware,
    routing::get,
};
use base64::Engine as _;
use http_body_util::BodyExt as _;
use olp_domain::{
    ApiKey, ApiKeyDigest, ApiKeyId, ApiKeyLimits, ApiKeyLookupId, ApiKeyScope, ApiKeyStatus,
    OperationKind, RuntimeGeneration, RuntimeGenerationId, RuntimeSnapshot, Surface,
};
use tower::{ServiceBuilder, ServiceExt, service_fn};
use tower_http::{
    sensitive_headers::{SetSensitiveRequestHeadersLayer, SetSensitiveResponseHeadersLayer},
    trace::TraceLayer,
};
use uuid::Uuid;

use super::*;
use super::{
    gateway::{InferenceEndpoint, TokenEstimate},
    observability::{OBSERVABILITY_SNAPSHOT_STALE_AFTER, prometheus_label},
    request_admission::{
        HTTP_INFERENCE_LIMITS_RESERVED, HTTP_INFERENCE_METADATA_CLAIMED, HTTP_INFERENCE_PRINCIPAL,
        HTTP_INFERENCE_RESERVATION_HOLD, HTTP_INFERENCE_RUNTIME, InferenceReservation,
        JsonBodyReadError, LocalRequestMetadata, MultipartAdmissionState, ReleaseReservationBody,
        enforce_request_limits, estimate_http_json_request_tokens, http_inference_principal,
        read_json_body, validate_json_depth, validate_multipart_boundary,
    },
    router::{
        http_request_span, request_trace_path, sensitive_request_headers,
        sensitive_response_headers,
    },
};

#[test]
fn prometheus_labels_escape_control_syntax() {
    assert_eq!(
        prometheus_label("provider\\\"name\nnext"),
        "provider\\\\\\\"name\\nnext"
    );
}

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
    let response = public_router(state)
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
    let response = public_router(state)
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

#[tokio::test]
async fn bootstrap_token_digest_is_verified_then_cleared() {
    let mut state = ApiState::new(
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
    assert_eq!(state.verify_bootstrap_token(Some(&token)).await, Some(true));
    assert_eq!(
        state.verify_bootstrap_token(Some("not-a-token")).await,
        Some(false)
    );
    state.clear_bootstrap_token().await;
    assert_eq!(state.verify_bootstrap_token(Some(&token)).await, None);
}

#[tokio::test]
async fn public_router_serves_console_health_and_hides_observability_paths() {
    let console_dir =
        std::env::temp_dir().join(format!("olp-public-router-test-{}", Uuid::now_v7()));
    std::fs::create_dir(&console_dir).unwrap();
    std::fs::write(
        console_dir.join("index.html"),
        "<!doctype html><title>OLP console</title>",
    )
    .unwrap();
    let app = public_router(ApiState::new(
        ApiMode::Control,
        None,
        Arc::new(RuntimeManager::empty()),
        "https://olp.example.test",
        console_dir.clone(),
    ));

    let health = app
        .clone()
        .oneshot(Request::get("/health").body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(health.status(), axum::http::StatusCode::OK);
    let health = health.into_body().collect().await.unwrap().to_bytes();
    assert!(String::from_utf8_lossy(&health).contains("OLP console"));

    for path in [
        "/health/",
        "/health/live",
        "/health/ready",
        "/metrics",
        "/metrics/",
    ] {
        let response = app
            .clone()
            .oneshot(Request::get(path).body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(
            response.status(),
            axum::http::StatusCode::NOT_FOUND,
            "{path}"
        );
    }
    std::fs::remove_dir_all(console_dir).unwrap();
}

#[tokio::test]
async fn observability_router_serves_cached_snapshots_and_freshness_telemetry() {
    let (state, _) = inference_state(false);
    refresh_observability_cache(&state).await;
    let app = observability_router(state.clone());

    let live = app
        .clone()
        .oneshot(Request::get("/health/live").body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(live.status(), axum::http::StatusCode::OK);

    let ready = app
        .clone()
        .oneshot(Request::get("/health/ready").body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(ready.status(), axum::http::StatusCode::OK);
    assert_eq!(ready.headers()["x-olp-observability-snapshot-fresh"], "1");

    let metrics = app
        .clone()
        .oneshot(Request::get("/metrics").body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(metrics.status(), axum::http::StatusCode::OK);
    let metrics = metrics.into_body().collect().await.unwrap().to_bytes();
    let metrics = String::from_utf8(metrics.to_vec()).unwrap();
    assert!(metrics.contains("olp_ready 1"));
    assert!(metrics.contains("olp_observability_metrics_snapshot_fresh 1"));

    let private_only = app
        .oneshot(
            Request::get("/api/v1/providers")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(private_only.status(), axum::http::StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn stale_observability_snapshots_force_unready_metrics_and_readiness() {
    let (state, _) = inference_state(false);
    refresh_observability_cache(&state).await;
    let stale_at = Instant::now() - OBSERVABILITY_SNAPSHOT_STALE_AFTER - Duration::from_secs(1);
    {
        let mut readiness = state.observability.readiness.write().unwrap();
        readiness.last_success_at = Some(stale_at);
        readiness.last_attempt_at = Some(stale_at);
    }
    {
        let mut metrics = state.observability.metrics.write().unwrap();
        metrics.last_success_at = Some(stale_at);
        metrics.last_attempt_at = Some(stale_at);
    }
    let app = observability_router(state);

    let ready = app
        .clone()
        .oneshot(Request::get("/health/ready").body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(ready.status(), axum::http::StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(ready.headers()["x-olp-observability-snapshot-fresh"], "0");

    let metrics = app
        .oneshot(Request::get("/metrics").body(Body::empty()).unwrap())
        .await
        .unwrap();
    let metrics = metrics.into_body().collect().await.unwrap().to_bytes();
    let metrics = String::from_utf8(metrics.to_vec()).unwrap();
    assert!(metrics.contains("olp_ready 0"));
    assert!(metrics.contains("olp_observability_metrics_snapshot_fresh 0"));
}

#[tokio::test]
async fn stale_metrics_do_not_change_the_readiness_contract() {
    let (state, _) = inference_state(false);
    refresh_observability_cache(&state).await;
    state.observability.record_metrics_failure();

    let response = observability_router(state)
        .oneshot(Request::get("/metrics").body(Body::empty()).unwrap())
        .await
        .unwrap();
    let body = response.into_body().collect().await.unwrap().to_bytes();
    let body = String::from_utf8(body.to_vec()).unwrap();
    assert!(body.contains("olp_ready 1"));
    assert!(body.contains("olp_observability_metrics_snapshot_fresh 0"));
}

fn inference_state(limited: bool) -> (ApiState, String) {
    let auth_hmac_key = Arc::new(AuthHmacKey::new([19; 32]));
    let material = auth_hmac_key.generate_api_key();
    let plaintext = material.expose_once().to_owned();
    let lookup_id = ApiKeyLookupId::parse(material.lookup_id.clone()).unwrap();
    let runtime = Arc::new(RuntimeManager::empty());
    runtime
        .install(
            RuntimeSnapshot {
                generation: RuntimeGeneration {
                    id: RuntimeGenerationId::new(),
                    ordinal: 1,
                    activated_at: chrono::Utc::now(),
                },
                providers: BTreeMap::new(),
                routes: BTreeMap::new(),
                api_keys: BTreeMap::from([(
                    lookup_id.clone(),
                    ApiKey {
                        id: ApiKeyId::new(),
                        lookup_id,
                        digest: ApiKeyDigest::new(material.digest),
                        status: ApiKeyStatus::Active,
                        expires_at: None,
                        scopes: BTreeSet::from([ApiKeyScope::Inference, ApiKeyScope::ModelsRead]),
                        allowed_routes: BTreeSet::new(),
                        limits: ApiKeyLimits {
                            requests_per_minute: limited.then(|| NonZeroU32::new(10).unwrap()),
                            tokens_per_minute: None,
                            concurrency: limited.then(|| NonZeroU32::new(2).unwrap()),
                        },
                    },
                )]),
            },
            BTreeMap::new(),
        )
        .unwrap();
    let mut state = ApiState::new(
        ApiMode::Gateway,
        None,
        runtime,
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    );
    state.auth_hmac_key = Some(auth_hmac_key);
    (state, plaintext)
}

#[test]
fn local_metadata_detection_is_method_and_surface_exact() {
    assert_eq!(
        InferenceEndpoint::classify(&axum::http::Method::GET, "/openai/v1/models")
            .unwrap()
            .metadata()
            .map(|policy| (policy.operation, policy.fallback_route)),
        Some(("model_list", "models"))
    );
    assert_eq!(
        InferenceEndpoint::classify(&axum::http::Method::GET, "/gemini/v1beta/models/team-route")
            .unwrap()
            .metadata()
            .map(|policy| (policy.operation, policy.fallback_route)),
        Some(("model_get", "models"))
    );
    assert_eq!(
        InferenceEndpoint::classify(&axum::http::Method::GET, "/openai/v1/videos")
            .unwrap()
            .metadata()
            .map(|policy| (policy.operation, policy.fallback_route)),
        Some(("video_list", "videos"))
    );
    assert_eq!(
        InferenceEndpoint::classify(&axum::http::Method::POST, "/openai/v1/videos")
            .unwrap()
            .metadata()
            .map(|policy| (policy.operation, policy.fallback_route)),
        Some(("video_create", "invalid-request"))
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
        operation: "model_list",
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

#[tokio::test]
async fn trace_boundary_marks_authentication_headers_sensitive() {
    let service = ServiceBuilder::new()
        .layer(SetSensitiveRequestHeadersLayer::new(
            sensitive_request_headers(),
        ))
        .layer(TraceLayer::new_for_http().make_span_with(http_request_span))
        .layer(SetSensitiveResponseHeadersLayer::new(
            sensitive_response_headers(),
        ))
        .service(service_fn(|request: Request<Body>| async move {
            for header in sensitive_request_headers() {
                assert!(request.headers()[header].is_sensitive());
            }
            let mut response = Response::new(Body::empty());
            response.headers_mut().insert(
                axum::http::header::SET_COOKIE,
                HeaderValue::from_static("session=secret"),
            );
            response.headers_mut().insert(
                HeaderName::from_static(management_api::CSRF_HEADER),
                HeaderValue::from_static("csrf-secret"),
            );
            Ok::<_, Infallible>(response)
        }));

    let mut request = Request::new(Body::empty());
    request.headers_mut().insert(
        axum::http::header::AUTHORIZATION,
        HeaderValue::from_static("Bearer secret"),
    );
    request.headers_mut().insert(
        axum::http::header::COOKIE,
        HeaderValue::from_static("session=secret"),
    );
    request.headers_mut().insert(
        HeaderName::from_static(management_api::CSRF_HEADER),
        HeaderValue::from_static("csrf-secret"),
    );
    request.headers_mut().insert(
        HeaderName::from_static(management_api::SETUP_TOKEN_HEADER),
        HeaderValue::from_static("bootstrap-secret"),
    );
    request.headers_mut().insert(
        HeaderName::from_static("x-api-key"),
        HeaderValue::from_static("anthropic-secret"),
    );
    request.headers_mut().insert(
        HeaderName::from_static("x-goog-api-key"),
        HeaderValue::from_static("gemini-secret"),
    );
    let response = service.oneshot(request).await.unwrap();
    assert!(
        response.headers()[axum::http::header::SET_COOKIE].is_sensitive(),
        "TraceLayer must observe Set-Cookie only after it is marked sensitive"
    );
    assert!(
        response
            .headers()
            .get(HeaderName::from_static(management_api::CSRF_HEADER))
            .unwrap()
            .is_sensitive(),
        "TraceLayer must observe rotated CSRF credentials only after they are marked sensitive"
    );
}

#[test]
fn request_trace_path_omits_query_parameters() {
    let uri: Uri = "/openai/v1/models?key=must-not-be-logged".parse().unwrap();
    assert_eq!(request_trace_path(&uri), "/openai/v1/models");
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
async fn management_openapi_is_only_served_on_the_versioned_route() {
    let console_dir = std::env::temp_dir().join(format!("olp-console-test-{}", Uuid::now_v7()));
    std::fs::create_dir(&console_dir).unwrap();
    std::fs::write(
        console_dir.join("index.html"),
        "<!doctype html><title>OLP</title>",
    )
    .unwrap();
    let app = public_router(ApiState::new(
        ApiMode::Control,
        None,
        Arc::new(RuntimeManager::empty()),
        "https://olp.example.test",
        &console_dir,
    ));

    let versioned = app
        .clone()
        .oneshot(
            Request::get("/api/v1/openapi.json")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(versioned.status(), axum::http::StatusCode::OK);

    let legacy = app
        .oneshot(Request::get("/openapi.json").body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(legacy.status(), axum::http::StatusCode::NOT_FOUND);
    std::fs::remove_dir_all(console_dir).unwrap();
}

#[tokio::test]
async fn request_limit_matrix_rejects_depth_size_encoding_and_bad_multipart() {
    let app = public_router(ApiState::new(
        ApiMode::Gateway,
        None,
        Arc::new(RuntimeManager::empty()),
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    ));

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
    let app = public_router(state);
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
async fn management_extractor_rejections_are_rfc9457_without_query_reflection() {
    let app = public_router(ApiState::new(
        ApiMode::Control,
        None,
        Arc::new(RuntimeManager::empty()),
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    ));
    for (uri, expected_instance) in [
        (
            "/api/v1/providers?limit=not-a-number&secret=must-not-reflect",
            "/api/v1/providers",
        ),
        (
            "/api/v1/providers/not-a-uuid",
            "/api/v1/providers/not-a-uuid",
        ),
    ] {
        let response = app
            .clone()
            .oneshot(Request::get(uri).body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(response.status(), axum::http::StatusCode::BAD_REQUEST);
        assert_eq!(
            response.headers()[axum::http::header::CONTENT_TYPE],
            "application/problem+json"
        );
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let problem: Problem = serde_json::from_slice(&body).unwrap();
        assert_eq!(problem.instance.as_deref(), Some(expected_instance));
        assert!(problem.errors.contains_key("request"));
        assert!(!String::from_utf8_lossy(&body).contains("must-not-reflect"));
    }
}

#[tokio::test]
async fn inference_authentication_precedes_body_decode_with_native_errors() {
    let mut state = ApiState::new(
        ApiMode::Gateway,
        None,
        Arc::new(RuntimeManager::empty()),
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    );
    state.auth_hmac_key = Some(Arc::new(AuthHmacKey::new([3; 32])));
    let app = public_router(state);
    let too_deep = format!("{}0{}", "[".repeat(65), "]".repeat(65));
    for (path, header_name, expected_pointer) in [
        (
            "/openai/v1/chat/completions",
            axum::http::header::AUTHORIZATION,
            "/error/code",
        ),
        (
            "/anthropic/v1/messages",
            HeaderName::from_static("x-api-key"),
            "/error/type",
        ),
        (
            "/gemini/v1beta/models/test:generateContent",
            HeaderName::from_static("x-goog-api-key"),
            "/error/status",
        ),
    ] {
        let value = if header_name == axum::http::header::AUTHORIZATION {
            "Bearer invalid-key"
        } else {
            "invalid-key"
        };
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
    let app = public_router(state);
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
        let response = public_router(state)
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
    let app = public_router(state);
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
    let app = public_router(state);
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
    let response = public_router(state)
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

async fn activate_runtime_inside_handler(
    State(state): State<ApiState>,
    Extension(principal): Extension<InferencePrincipal>,
) -> String {
    let pinned_before_activation = pin_inference_runtime(&state);
    let pinned_generation = pinned_before_activation.generation.id;
    assert_eq!(principal.runtime().generation.id, pinned_generation);
    state
        .runtime
        .install(
            RuntimeSnapshot {
                generation: RuntimeGeneration {
                    id: RuntimeGenerationId::new(),
                    ordinal: pinned_before_activation.generation.ordinal + 1,
                    activated_at: chrono::Utc::now(),
                },
                providers: pinned_before_activation.providers.clone(),
                routes: pinned_before_activation.routes.clone(),
                api_keys: pinned_before_activation.api_keys.clone(),
            },
            BTreeMap::new(),
        )
        .unwrap();
    assert_ne!(state.runtime.pin().generation.id, pinned_generation);
    assert_eq!(
        pin_inference_runtime(&state).generation.id,
        pinned_generation,
        "a request must not mix authentication and route generations"
    );
    let detached_state = state.clone();
    let (detached_runtime, detached_principal) = spawn_http_inference_task(&state, async move {
        (
            pin_inference_runtime(&detached_state).generation.id,
            http_inference_principal()
                .expect("admitted principal must cross the detached task boundary")
                .runtime()
                .generation
                .id,
        )
    })
    .await
    .unwrap();
    assert_eq!(detached_runtime, pinned_generation);
    assert_eq!(detached_principal, pinned_generation);
    pinned_generation.to_string()
}

#[tokio::test]
async fn inference_http_boundary_pins_one_generation_across_activation() {
    let (state, key) = inference_state(false);
    let original_generation = state.runtime.pin().generation.id;
    let app = Router::new()
        .route(
            "/openai/test-generation-pin",
            get(activate_runtime_inside_handler),
        )
        .layer(middleware::from_fn_with_state(
            state.clone(),
            enforce_request_limits,
        ))
        .with_state(state.clone());

    let response = app
        .oneshot(
            Request::get("/openai/test-generation-pin")
                .header(axum::http::header::AUTHORIZATION, format!("Bearer {key}"))
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), axum::http::StatusCode::OK);
    let body = response.into_body().collect().await.unwrap().to_bytes();
    assert_eq!(body.as_ref(), original_generation.to_string().as_bytes());
    assert_ne!(state.runtime.pin().generation.id, original_generation);
}

#[tokio::test]
async fn response_completion_and_drop_release_the_http_concurrency_reservation() {
    for consume in [true, false] {
        let released = Arc::new(AtomicBool::new(false));
        let release_signal = released.clone();
        let body = Body::new(ReleaseReservationBody {
            inner: Body::from("response"),
            reservation: InferenceReservation::for_test(async move {
                release_signal.store(true, Ordering::Release);
            }),
        });
        if consume {
            body.collect().await.unwrap();
        } else {
            drop(body);
        }
        tokio::task::yield_now().await;
        assert!(
            released.load(Ordering::Acquire),
            "reservation was not released when consume={consume}"
        );
    }
}

#[tokio::test]
async fn concurrent_final_reservation_drops_release_once() {
    let released = Arc::new(AtomicBool::new(false));
    let release_signal = Arc::clone(&released);
    let reservation = InferenceReservation::for_test(async move {
        release_signal.store(true, Ordering::Release);
    });
    let left = reservation.clone();
    let right = reservation.clone();
    drop(reservation);
    let barrier = Arc::new(tokio::sync::Barrier::new(2));
    let left_task = tokio::spawn({
        let barrier = Arc::clone(&barrier);
        async move {
            barrier.wait().await;
            drop(left);
        }
    });
    let right_task = tokio::spawn(async move {
        barrier.wait().await;
        drop(right);
    });
    left_task.await.unwrap();
    right_task.await.unwrap();
    tokio::time::timeout(Duration::from_secs(1), async {
        while !released.load(Ordering::Acquire) {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("the final reservation drop must schedule its release");
}

#[tokio::test]
async fn detached_inference_task_holds_the_http_reservation_after_request_cancellation() {
    let released = Arc::new(AtomicBool::new(false));
    let release_signal = Arc::clone(&released);
    let reservation = InferenceReservation::for_test(async move {
        release_signal.store(true, Ordering::Release);
    });
    let runtime = Arc::new(RuntimeManager::empty());
    let state = ApiState::new(
        ApiMode::Gateway,
        None,
        runtime,
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    );
    let started = Arc::new(tokio::sync::Notify::new());
    let release_child = Arc::new(tokio::sync::Notify::new());
    let (completed_sender, completed) = tokio::sync::oneshot::channel();
    let started_wait = started.notified();
    let outer = tokio::spawn({
        let state = state.clone();
        let reservation = reservation.clone();
        let started = Arc::clone(&started);
        let release_child = Arc::clone(&release_child);
        async move {
            HTTP_INFERENCE_RESERVATION_HOLD
                .scope(reservation, async move {
                    let _task = spawn_http_inference_task(&state, async move {
                        started.notify_one();
                        release_child.notified().await;
                        let _ = completed_sender.send(());
                    });
                    futures::future::pending::<()>().await;
                })
                .await;
        }
    });
    drop(reservation);
    started_wait.await;
    outer.abort();
    let _ = outer.await;
    assert!(
        !released.load(Ordering::Acquire),
        "the detached task must retain the reservation after outer cancellation"
    );

    release_child.notify_one();
    completed.await.unwrap();
    tokio::time::timeout(Duration::from_secs(1), async {
        while !released.load(Ordering::Acquire) {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("the final detached reservation owner must release the lease");
}

#[tokio::test]
async fn spawned_inference_task_inherits_the_http_execution_context() {
    let (state, _) = inference_state(false);
    let pinned = state.runtime.pin();
    let (lookup_id, _) = pinned.api_keys.iter().next().unwrap();
    let principal =
        InferencePrincipal::for_test(Arc::clone(&pinned), lookup_id.clone(), Surface::OpenAi);
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
                api_keys: pinned.api_keys.clone(),
            },
            BTreeMap::new(),
        )
        .unwrap();
    let metadata_claimed = Arc::new(AtomicBool::new(false));
    let reservation = InferenceReservation::for_test(async {});
    let child_state = state.clone();
    let spawn_state = state.clone();
    let (
        generation,
        principal_generation,
        principal_surface,
        reserved_tokens,
        has_reservation_hold,
    ) = HTTP_INFERENCE_PRINCIPAL
        .scope(
            principal,
            HTTP_INFERENCE_RUNTIME.scope(
                Arc::clone(&pinned),
                HTTP_INFERENCE_METADATA_CLAIMED.scope(
                    Arc::clone(&metadata_claimed),
                    HTTP_INFERENCE_LIMITS_RESERVED.scope(
                        2_000,
                        HTTP_INFERENCE_RESERVATION_HOLD.scope(reservation, async move {
                            let task = spawn_http_inference_task(&spawn_state, async move {
                                claim_http_inference_metadata();
                                let principal = http_inference_principal()
                                    .expect("the detached task inherits the admitted principal");
                                (
                                    pin_inference_runtime(&child_state).generation.id,
                                    principal.runtime().generation.id,
                                    principal.surface(),
                                    http_inference_reserved_tokens(),
                                    HTTP_INFERENCE_RESERVATION_HOLD.try_with(|_| ()).is_ok(),
                                )
                            });
                            task.await.unwrap()
                        }),
                    ),
                ),
            ),
        )
        .await;
    assert_eq!(generation, pinned.generation.id);
    assert_eq!(principal_generation, pinned.generation.id);
    assert_eq!(principal_surface, Surface::OpenAi);
    assert_ne!(state.runtime.pin().generation.id, pinned.generation.id);
    assert_eq!(reserved_tokens, Some(2_000));
    assert!(has_reservation_hold);
    assert!(metadata_claimed.load(Ordering::Acquire));
}

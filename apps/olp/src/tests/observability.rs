use super::*;

#[test]
fn prometheus_labels_escape_control_syntax() {
    assert_eq!(
        prometheus_label("provider\\\"name\nnext"),
        "provider\\\\\\\"name\\nnext"
    );
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
    let state = ProcessComposition::new(
        ApiMode::Control,
        None,
        Arc::new(RuntimeManager::empty()),
        "https://olp.example.test",
        console_dir.clone(),
    );
    let app = public_router(state.management_state_for_test());

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
    let state = state.observability_state_for_test();
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
    let state = state.observability_state_for_test();
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
    let state = state.observability_state_for_test();
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
                HeaderName::from_static(management::CSRF_HEADER),
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
        HeaderName::from_static(management::CSRF_HEADER),
        HeaderValue::from_static("csrf-secret"),
    );
    request.headers_mut().insert(
        HeaderName::from_static(management::SETUP_TOKEN_HEADER),
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
            .get(HeaderName::from_static(management::CSRF_HEADER))
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

#[tokio::test]
async fn management_openapi_is_only_served_on_the_versioned_route() {
    let console_dir = std::env::temp_dir().join(format!("olp-console-test-{}", Uuid::now_v7()));
    std::fs::create_dir(&console_dir).unwrap();
    std::fs::write(
        console_dir.join("index.html"),
        "<!doctype html><title>OLP</title>",
    )
    .unwrap();
    let app = public_router(
        ProcessComposition::new(
            ApiMode::Control,
            None,
            Arc::new(RuntimeManager::empty()),
            "https://olp.example.test",
            &console_dir,
        )
        .management_state_for_test(),
    );

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
async fn management_extractor_rejections_are_rfc9457_without_query_reflection() {
    let app = public_router(
        ProcessComposition::new(
            ApiMode::Control,
            None,
            Arc::new(RuntimeManager::empty()),
            "https://olp.example.test",
            PathBuf::from("missing-console"),
        )
        .management_state_for_test(),
    );
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

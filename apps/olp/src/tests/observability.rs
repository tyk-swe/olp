use super::*;

#[test]
fn prometheus_labels_escape_control_syntax() {
    assert_eq!(
        prometheus_label("provider\\\"name\nnext"),
        "provider\\\\\\\"name\\nnext"
    );
}

#[test]
fn replicated_worker_metrics_declare_types_and_render_durable_values() {
    use olp_db::{
        request_metadata::delivery_health::ConsumerStatus,
        runtime::outbox::{RuntimeOutboxState, RuntimeOutboxStatus},
        worker_health::{
            WorkerRecoveryCounters, WorkerTask, WorkerTaskHealthSummary, WorkerTaskState,
            WorkerTaskStatus,
        },
    };

    use crate::observability::append_async_worker_metrics;

    let now = chrono::DateTime::parse_from_rfc3339("2026-08-08T12:00:00Z")
        .unwrap()
        .with_timezone(&chrono::Utc);
    let consumer = ConsumerStatus::from_health(
        Some(olp_db::request_metadata::delivery_health::ConsumerHealth {
            pending_events: 2,
            lag_events: 3,
            oldest_pending_at: Some(now - chrono::Duration::seconds(30)),
            checked_at: now - chrono::Duration::seconds(2),
        }),
        now,
    );
    let outbox = RuntimeOutboxStatus {
        state: RuntimeOutboxState::Backlogged,
        pending_rows: 4,
        oldest_pending_at: Some(now - chrono::Duration::seconds(45)),
        owner_active: true,
        claimed_rows: 1,
        checked_at: Some(now - chrono::Duration::seconds(2)),
        heartbeat_age_seconds: Some(2),
        last_progress_at: Some(now - chrono::Duration::seconds(10)),
        last_progress_age_seconds: Some(10),
    };
    let tasks = WorkerTaskHealthSummary {
        tasks: WorkerTask::ALL
            .into_iter()
            .map(|task| WorkerTaskStatus {
                task,
                state: WorkerTaskState::Healthy,
                checked_at: Some(now - chrono::Duration::seconds(2)),
                last_success_at: Some(now - chrono::Duration::seconds(2)),
                last_progress_at: Some(now - chrono::Duration::seconds(10)),
                heartbeat_age_seconds: Some(2),
                last_success_age_seconds: Some(2),
                successes_total: 7,
                failures_total: 1,
                skipped_total: 5,
            })
            .collect(),
    };
    let counters = WorkerRecoveryCounters {
        request_metadata_reclaimed: 11,
        request_metadata_recovered: 12,
        request_metadata_duplicates: 13,
        request_metadata_processed: 14,
        runtime_outbox_attempts: 15,
        runtime_outbox_retry_scheduled: 16,
        runtime_outbox_repeated_attempts: 17,
        runtime_outbox_published: 18,
        runtime_outbox_duplicate_publications: 19,
        runtime_outbox_abandoned_ownership: 20,
        runtime_outbox_abandoned_claims: 21,
        runtime_outbox_failed_takeovers: 22,
    };
    let mut metrics = String::new();
    append_async_worker_metrics(
        &mut metrics,
        now,
        true,
        consumer,
        Some(outbox),
        Some(&tasks),
        Some(counters),
    );

    for (name, metric_type) in [
        ("olp_async_worker_observability_available", "gauge"),
        ("olp_async_plane_current", "gauge"),
        ("olp_async_plane_drained", "gauge"),
        ("olp_async_plane_healthy", "gauge"),
        ("olp_async_plane_last_progress_timestamp_seconds", "gauge"),
        (
            "olp_request_metadata_consumer_oldest_pending_age_seconds",
            "gauge",
        ),
        ("olp_request_metadata_events_reclaimed_total", "counter"),
        ("olp_request_metadata_events_recovered_total", "counter"),
        (
            "olp_request_metadata_persistence_duplicates_total",
            "counter",
        ),
        ("olp_request_metadata_events_processed_total", "counter"),
        ("olp_runtime_outbox_pending_rows", "gauge"),
        ("olp_runtime_outbox_oldest_pending_age_seconds", "gauge"),
        ("olp_runtime_outbox_owner_active", "gauge"),
        ("olp_runtime_outbox_claimed_rows", "gauge"),
        ("olp_runtime_outbox_owner_stale", "gauge"),
        ("olp_runtime_outbox_heartbeat_age_seconds", "gauge"),
        ("olp_runtime_outbox_publication_attempts_total", "counter"),
        ("olp_runtime_outbox_publication_retries_total", "counter"),
        (
            "olp_runtime_outbox_repeated_publication_attempts_total",
            "counter",
        ),
        ("olp_runtime_outbox_published_total", "counter"),
        ("olp_runtime_outbox_duplicate_publications_total", "counter"),
        ("olp_runtime_outbox_abandoned_ownership_total", "counter"),
        ("olp_runtime_outbox_abandoned_claims_total", "counter"),
        ("olp_runtime_outbox_failed_takeovers_total", "counter"),
        ("olp_worker_task_healthy", "gauge"),
        ("olp_worker_task_heartbeat_age_seconds", "gauge"),
        ("olp_worker_task_last_success_age_seconds", "gauge"),
        ("olp_worker_task_runs_total", "counter"),
    ] {
        assert!(
            metrics.contains(&format!("# HELP {name} ")),
            "missing HELP for {name}"
        );
        assert!(
            metrics.contains(&format!("# TYPE {name} {metric_type}\n")),
            "missing TYPE for {name}"
        );
    }
    for sample in [
        "olp_async_worker_observability_available 1",
        "olp_async_plane_current 1",
        "olp_async_plane_drained 0",
        "olp_async_plane_healthy 0",
        "olp_request_metadata_consumer_oldest_pending_age_seconds 30",
        "olp_request_metadata_events_reclaimed_total 11",
        "olp_request_metadata_events_recovered_total 12",
        "olp_request_metadata_persistence_duplicates_total 13",
        "olp_request_metadata_events_processed_total 14",
        "olp_runtime_outbox_pending_rows 4",
        "olp_runtime_outbox_oldest_pending_age_seconds 45",
        "olp_runtime_outbox_owner_active 1",
        "olp_runtime_outbox_claimed_rows 1",
        "olp_runtime_outbox_owner_stale 0",
        "olp_runtime_outbox_heartbeat_age_seconds 2",
        "olp_runtime_outbox_publication_attempts_total 15",
        "olp_runtime_outbox_publication_retries_total 16",
        "olp_runtime_outbox_repeated_publication_attempts_total 17",
        "olp_runtime_outbox_published_total 18",
        "olp_runtime_outbox_duplicate_publications_total 19",
        "olp_runtime_outbox_abandoned_ownership_total 20",
        "olp_runtime_outbox_abandoned_claims_total 21",
        "olp_runtime_outbox_failed_takeovers_total 22",
        "olp_worker_task_healthy{task=\"maintenance\"} 1",
        "olp_worker_task_heartbeat_age_seconds{task=\"maintenance\"} 2",
        "olp_worker_task_last_success_age_seconds{task=\"maintenance\"} 2",
        "olp_worker_task_runs_total{task=\"maintenance\",outcome=\"success\"} 7",
        "olp_worker_task_runs_total{task=\"maintenance\",outcome=\"failure\"} 1",
        "olp_worker_task_runs_total{task=\"maintenance\",outcome=\"skipped\"} 5",
    ] {
        assert!(metrics.contains(sample), "missing sample {sample}");
    }
    assert!(metrics.contains(&format!(
        "olp_async_plane_last_progress_timestamp_seconds {}",
        (now - chrono::Duration::seconds(10)).timestamp()
    )));
    for forbidden_label in [
        "worker_id=",
        "stream_id=",
        "event_id=",
        "api_key=",
        "route=",
        "provider=",
        "installation=",
    ] {
        assert!(!metrics.contains(forbidden_label));
    }

    let mut unavailable = String::new();
    append_async_worker_metrics(
        &mut unavailable,
        now,
        false,
        consumer,
        Some(outbox),
        Some(&tasks),
        Some(counters),
    );
    assert!(unavailable.contains("olp_async_worker_observability_available 0"));
    assert!(unavailable.contains("olp_async_plane_current 0"));
    assert!(unavailable.contains("olp_runtime_outbox_publication_attempts_total 15"));
    assert!(unavailable.contains("olp_request_metadata_events_reclaimed_total 11"));

    let mut missing_summaries = String::new();
    append_async_worker_metrics(
        &mut missing_summaries,
        now,
        false,
        consumer,
        None,
        None,
        None,
    );
    assert!(missing_summaries.contains("olp_async_worker_observability_available 0"));
    assert!(!missing_summaries.contains("olp_runtime_outbox_publication_attempts_total"));
    assert!(!missing_summaries.contains("olp_request_metadata_events_reclaimed_total"));
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
        Arc::new(Manager::empty()),
        "https://olp.example.test",
        console_dir.clone(),
    );
    let app = for_state(state.management_state_for_test());

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
                HeaderName::from_static(management::sessions::CSRF_HEADER),
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
        HeaderName::from_static(management::sessions::CSRF_HEADER),
        HeaderValue::from_static("csrf-secret"),
    );
    request.headers_mut().insert(
        HeaderName::from_static(management::sessions::SETUP_TOKEN_HEADER),
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
    request.headers_mut().insert(
        HeaderName::from_static("x-litellm-api-key"),
        HeaderValue::from_static("litellm-secret"),
    );
    let response = service.oneshot(request).await.unwrap();
    assert!(
        response.headers()[axum::http::header::SET_COOKIE].is_sensitive(),
        "TraceLayer must observe Set-Cookie only after it is marked sensitive"
    );
    assert!(
        response
            .headers()
            .get(HeaderName::from_static(management::sessions::CSRF_HEADER))
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
    let app = for_state(
        ProcessComposition::new(
            ApiMode::Control,
            None,
            Arc::new(Manager::empty()),
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
    let app = for_state(
        ProcessComposition::new(
            ApiMode::Control,
            None,
            Arc::new(Manager::empty()),
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

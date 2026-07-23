use std::{path::PathBuf, sync::Arc};

use axum::{
    Router,
    body::Body,
    http::{Method, Request, Response, StatusCode, header},
};
use chrono::{Duration, SecondsFormat, Timelike, Utc};
use http_body_util::BodyExt as _;
use olp::{
    ApiMode, ApiState, RuntimeManager, observability_router, public_router,
    refresh_observability_cache,
};
use olp_domain::Surface;
use olp_storage::{
    MasterKey, PgStore, RequestAttemptMetadata, RequestMetadataEvent, RequestMetadataGap,
};
use serde_json::{Value, json};
use tower::ServiceExt as _;
use uuid::Uuid;

mod common;
use common::{BOOTSTRAP_TOKEN, configure_bootstrap};

const ORIGIN: &str = "https://olp.example.test";

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn operations_http_contract_is_authorized_paginated_exact_and_metadata_only() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 5).await.unwrap();
    store.migrate().await.unwrap();
    let mut state = ApiState::new(
        ApiMode::Control,
        Some(store.clone()),
        Arc::new(RuntimeManager::empty()),
        ORIGIN,
        PathBuf::from("missing-console-for-operations-test"),
    );
    state.master_key = Some(Arc::new(MasterKey::new(1, [31; 32])));
    configure_bootstrap(&mut state, [32; 32]);
    let dependencies = state.mode_dependencies().unwrap();
    let observability_state = dependencies.observability();
    let app = public_router(dependencies.management().unwrap());
    let observability = observability_router(observability_state.clone());

    let setup = send(
        &app,
        Method::POST,
        "/api/v1/setup",
        Some(json!({
            "email": "owner@example.test",
            "password": "correct horse battery staple",
            "display_name": "Owner",
            "installation_name": "Operations HTTP test"
        })),
        RequestHeaders::default(),
    )
    .await;
    assert_eq!(setup.status(), StatusCode::CREATED);
    let cookie = session_cookie(&setup);
    let setup_body = response_json(setup).await;
    let csrf = setup_body["csrf_token"].as_str().unwrap().to_owned();
    let owner_id = Uuid::parse_str(setup_body["user"]["id"].as_str().unwrap()).unwrap();

    let provider_id = Uuid::now_v7();
    let api_key_id = Uuid::now_v7();
    let generation_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers
         (id, name, kind, state, auth_mode, etag, created_by,
          last_probe_at, last_probe_status, last_probe_detail)
         VALUES ($1, 'operations-http-provider', 'openai', 'active', 'api_key', $2, $3,
                 now(), 'succeeded', 'mock probe succeeded')",
    )
    .bind(provider_id)
    .bind(Uuid::now_v7())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO api_keys
         (id, lookup_id, secret_digest, name, created_by)
         VALUES ($1, 'olpv2oper002', $2, 'operations HTTP test', $3)",
    )
    .bind(api_key_id)
    .bind([10_u8; 32].as_slice())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO runtime_generations
         (id, compiled_release, release_sha256, created_by)
         VALUES ($1, $2, $3, $4)",
    )
    .bind(generation_id)
    .bind([1_u8].as_slice())
    .bind([3_u8; 32].as_slice())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();

    let observed_at = Utc::now() - Duration::hours(2);
    let pricing = send(
        &app,
        Method::POST,
        "/api/v1/pricing/revisions",
        Some(json!({
            "effective_at": (observed_at - Duration::minutes(1)).to_rfc3339(),
            "prices": [{
                "provider_kind": "openai",
                "provider_id": provider_id,
                "model": "mock-model",
                "operation": "generation",
                "input_per_million": "1.000000000000",
                "output_per_million": "2.000000000000",
                "currency": "USD"
            }]
        })),
        RequestHeaders {
            cookie: Some(&cookie),
            csrf: Some(&csrf),
            idempotency_key: Some("pricing-http-operations-001"),
            if_match: None,
        },
    )
    .await;
    assert_eq!(pricing.status(), StatusCode::CREATED);
    let pricing_body = response_json(pricing).await;
    assert_eq!(
        pricing_body["prices"][0]["provider_id"],
        provider_id.to_string()
    );
    assert_eq!(pricing_body["revision"], 1);
    let pricing_replay = send(
        &app,
        Method::POST,
        "/api/v1/pricing/revisions",
        Some(json!({
            "effective_at": (observed_at - Duration::minutes(1)).to_rfc3339(),
            "prices": [{
                "provider_kind": "openai",
                "provider_id": provider_id,
                "model": "mock-model",
                "operation": "generation",
                "input_per_million": "1.000000000000",
                "output_per_million": "2.000000000000",
                "currency": "USD"
            }]
        })),
        RequestHeaders {
            cookie: Some(&cookie),
            csrf: Some(&csrf),
            idempotency_key: Some("pricing-http-operations-001"),
            if_match: None,
        },
    )
    .await;
    assert_eq!(pricing_replay.status(), StatusCode::CREATED);
    assert_eq!(response_json(pricing_replay).await, pricing_body);
    let pricing_mismatch = send(
        &app,
        Method::POST,
        "/api/v1/pricing/revisions",
        Some(json!({
            "effective_at": (observed_at - Duration::minutes(1)).to_rfc3339(),
            "prices": [{
                "provider_kind": "openai",
                "provider_id": provider_id,
                "model": "mock-model",
                "operation": "generation",
                "input_per_million": "9.000000000000",
                "output_per_million": "2.000000000000",
                "currency": "USD"
            }]
        })),
        RequestHeaders {
            cookie: Some(&cookie),
            csrf: Some(&csrf),
            idempotency_key: Some("pricing-http-operations-001"),
            if_match: None,
        },
    )
    .await;
    assert_eq!(pricing_mismatch.status(), StatusCode::CONFLICT);

    let future_pricing = send(
        &app,
        Method::POST,
        "/api/v1/pricing/revisions",
        Some(json!({
            "effective_at": (observed_at + Duration::days(1)).to_rfc3339(),
            "prices": [{
                "provider_kind": "openai",
                "provider_id": provider_id,
                "model": "mock-model",
                "operation": "generation",
                "input_per_million": "3.000000000000",
                "output_per_million": "4.000000000000",
                "currency": "USD"
            }]
        })),
        RequestHeaders {
            cookie: Some(&cookie),
            csrf: Some(&csrf),
            idempotency_key: Some("pricing-http-operations-002"),
            if_match: None,
        },
    )
    .await;
    assert_eq!(future_pricing.status(), StatusCode::CREATED);
    assert_eq!(response_json(future_pricing).await["revision"], 2);

    let request_id = Uuid::now_v7();
    let started_at = observed_at - Duration::milliseconds(25);
    store
        .persist_request_metadata_event(&RequestMetadataEvent {
            event_id: Uuid::now_v7(),
            request_id,
            runtime_generation_id: generation_id,
            api_key_id,
            provider_id: Some(provider_id),
            route_slug: "default".to_owned(),
            upstream_model: Some("mock-model".to_owned()),
            operation: "generation".parse().unwrap(),
            surface: Surface::OpenAi,
            request_started_at: started_at,
            request_completed_at: observed_at,
            observed_at,
            status_code: Some(200),
            error_class: None,
            committed: true,
            latency_ms: 25,
            first_byte_ms: Some(5),
            input_tokens: Some(100),
            output_tokens: Some(50),
            cached_input_tokens: None,
            media_units: None,
            usage_complete: true,
            unpriced: false,
            attempts: vec![RequestAttemptMetadata {
                id: Uuid::now_v7(),
                ordinal: 1,
                provider_id,
                upstream_model: "mock-model".to_owned(),
                started_at,
                completed_at: observed_at,
                status_code: Some(200),
                error_class: None,
                committed: true,
                latency_ms: 25,
                first_byte_ms: Some(5),
            }],
        })
        .await
        .unwrap();
    store
        .report_request_metadata_gap_once(
            RequestMetadataGap {
                gateway_instance: "http-integration".to_owned(),
                event_count: 2,
                reason: "injected_http_gap".to_owned(),
                first_observed_at: observed_at,
                last_observed_at: observed_at,
            },
            "operations-http-injected-gap",
        )
        .await
        .unwrap();
    store
        .report_request_metadata_consumer_health(2, 3, Some(Utc::now() - Duration::seconds(30)))
        .await
        .unwrap();

    let requests = get(&app, "/api/v1/requests?limit=1", &cookie).await;
    assert_eq!(requests.status(), StatusCode::OK);
    let requests_body = response_json(requests).await;
    assert_eq!(requests_body["data"][0]["id"], request_id.to_string());
    assert_eq!(requests_body["data"][0]["estimated_cost"], "0.000200000000");
    assert!(requests_body["data"][0].get("prompt").is_none());

    let detail = get(&app, &format!("/api/v1/requests/{request_id}"), &cookie).await;
    assert_eq!(detail.status(), StatusCode::OK);
    let detail_body = response_json(detail).await;
    assert_eq!(detail_body["attempts"].as_array().unwrap().len(), 1);
    assert!(detail_body.get("output").is_none());

    let start = (observed_at - Duration::hours(1)).to_rfc3339_opts(SecondsFormat::Secs, true);
    let end = (observed_at + Duration::hours(1)).to_rfc3339_opts(SecondsFormat::Secs, true);
    let summary = get(
        &app,
        &format!("/api/v1/usage/summary?start={start}&end={end}"),
        &cookie,
    )
    .await;
    assert_eq!(summary.status(), StatusCode::OK);
    let summary_body = response_json(summary).await;
    assert_eq!(
        summary_body["request_metadata_consumer"]["state"],
        "backlogged"
    );
    assert_eq!(
        summary_body["request_metadata_consumer"]["pending_events"],
        2
    );
    assert_eq!(summary_body["request_metadata_consumer"]["lag_events"], 3);
    assert_eq!(summary_body["complete"], false);

    let series = get(
        &app,
        &format!("/api/v1/usage/time-series?start={start}&end={end}&granularity=hour"),
        &cookie,
    )
    .await;
    assert_eq!(series.status(), StatusCode::OK);
    assert_eq!(
        response_json(series).await["coverage"]["range_complete"],
        true
    );

    let breakdown = get(
        &app,
        &format!("/api/v1/usage/breakdown?start={start}&end={end}&dimension=provider"),
        &cookie,
    )
    .await;
    assert_eq!(breakdown.status(), StatusCode::OK);
    assert_eq!(
        response_json(breakdown).await["coverage"]["range_complete"],
        true
    );

    let completeness = get(
        &app,
        &format!("/api/v1/usage/completeness?start={start}&end={end}"),
        &cookie,
    )
    .await;
    let completeness_body = response_json(completeness).await;
    assert_eq!(completeness_body["request_metadata_gap_events"], 2);
    assert_eq!(
        completeness_body["request_metadata_consumer"]["state"],
        "backlogged"
    );
    assert_eq!(completeness_body["complete"], false);

    refresh_observability_cache(&observability_state).await;
    let ready = get(&observability, "/health/ready", &cookie).await;
    assert_eq!(ready.status(), StatusCode::OK);
    let ready_body = response_json(ready).await;
    assert_eq!(ready_body["status"], "degraded");
    assert_eq!(ready_body["request_metadata_complete"], false);
    assert_eq!(ready_body["request_metadata_consumer"], "backlogged");
    assert_eq!(ready_body["request_metadata_consumer_pending_events"], 2);
    assert_eq!(ready_body["request_metadata_consumer_lag_events"], 3);

    let management_ready = get(&app, "/api/v1/health/ready", &cookie).await;
    assert_eq!(management_ready.status(), StatusCode::OK);
    let management_ready_body = response_json(management_ready).await;
    assert_eq!(management_ready_body["status"], "degraded");
    assert_eq!(management_ready_body["request_metadata_complete"], false);
    assert_eq!(
        management_ready_body["request_metadata_consumer"],
        "backlogged"
    );

    sqlx::query(
        "UPDATE request_metadata_consumer_health SET checked_at = now() - interval '1 minute'",
    )
    .execute(store.pool())
    .await
    .unwrap();
    refresh_observability_cache(&observability_state).await;
    let stale_ready = get(&observability, "/health/ready", &cookie).await;
    assert_eq!(stale_ready.status(), StatusCode::OK);
    let stale_ready_body = response_json(stale_ready).await;
    assert_eq!(stale_ready_body["status"], "degraded");
    assert_eq!(stale_ready_body["request_metadata_consumer"], "stale");
    assert_eq!(stale_ready_body["request_metadata_complete"], false);

    refresh_observability_cache(&observability_state).await;
    let metrics = get(&observability, "/metrics", &cookie).await;
    assert_eq!(metrics.status(), StatusCode::OK);
    let metrics = String::from_utf8(
        metrics
            .into_body()
            .collect()
            .await
            .unwrap()
            .to_bytes()
            .to_vec(),
    )
    .unwrap();
    assert!(metrics.contains("olp_ready 1"));
    assert!(metrics.contains("olp_request_metadata_consumer_pending_events 2"));
    assert!(metrics.contains("olp_request_metadata_consumer_lag_events 3"));
    assert!(metrics.contains("olp_request_metadata_consumer_healthy 0"));
    assert!(metrics.contains("olp_request_metadata_consumer_stale 1"));
    assert!(metrics.contains("olp_operational_metrics_available 1"));
    assert!(metrics.contains("olp_request_success_ratio_5m"));
    assert!(metrics.contains("olp_request_latency_seconds{quantile=\"0.95\"}"));
    assert!(metrics.contains("olp_upstream_cancellations_5m"));
    assert!(metrics.contains("olp_provider_health{"));

    store
        .report_request_metadata_consumer_health(0, 0, None)
        .await
        .unwrap();

    let archived_bucket = (observed_at - Duration::days(3))
        .with_minute(0)
        .unwrap()
        .with_second(0)
        .unwrap()
        .with_nanosecond(0)
        .unwrap();
    let mut aggregate_fixture = store.pool().begin().await.unwrap();
    sqlx::query("SELECT set_config('olp.usage_rollup_writer', 'additive-v2', true)")
        .execute(&mut *aggregate_fixture)
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO usage_hourly (
            bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id,
            request_count, input_tokens, output_tokens, cached_input_tokens, media_units,
            estimated_cost, unpriced_count, incomplete_count, currency
         ) VALUES (
            $1, 'archived-http', $2, 'mock-model', 'generation', 'openai', $3,
            4, 40, 20, 5, 0, 0.000080000000, 0, 0, 'USD'
         )",
    )
    .bind(archived_bucket)
    .bind(provider_id)
    .bind(api_key_id)
    .execute(&mut *aggregate_fixture)
    .await
    .unwrap();
    aggregate_fixture.commit().await.unwrap();

    let archived_full_start = archived_bucket.to_rfc3339_opts(SecondsFormat::Secs, true);
    let archived_full_end =
        (archived_bucket + Duration::hours(1)).to_rfc3339_opts(SecondsFormat::Secs, true);
    let archived_full = get(
        &app,
        &format!(
            "/api/v1/usage/summary?start={archived_full_start}&end={archived_full_end}&route=archived-http"
        ),
        &cookie,
    )
    .await;
    assert_eq!(archived_full.status(), StatusCode::OK);
    let archived_full = response_json(archived_full).await;
    assert_eq!(archived_full["request_count"], 4);
    assert_eq!(archived_full["coverage"]["range_complete"], true);
    assert_eq!(archived_full["complete"], true);

    let archived_partial_start =
        (archived_bucket + Duration::minutes(15)).to_rfc3339_opts(SecondsFormat::Secs, true);
    let archived_partial_end =
        (archived_bucket + Duration::minutes(45)).to_rfc3339_opts(SecondsFormat::Secs, true);
    let archived_partial = get(
        &app,
        &format!(
            "/api/v1/usage/summary?start={archived_partial_start}&end={archived_partial_end}&route=archived-http"
        ),
        &cookie,
    )
    .await;
    assert_eq!(archived_partial.status(), StatusCode::OK);
    let archived_partial = response_json(archived_partial).await;
    assert_eq!(archived_partial["request_count"], 0);
    assert_eq!(archived_partial["coverage"]["range_complete"], false);
    assert_eq!(archived_partial["coverage"]["approximate"], true);
    assert_eq!(
        archived_partial["coverage"]["excluded_partial_aggregate_boundaries"],
        1
    );
    assert_eq!(archived_partial["complete"], false);

    let archived_partial_series = get(
        &app,
        &format!(
            "/api/v1/usage/time-series?start={archived_partial_start}&end={archived_partial_end}&route=archived-http&granularity=hour"
        ),
        &cookie,
    )
    .await;
    assert_eq!(archived_partial_series.status(), StatusCode::OK);
    let archived_partial_series = response_json(archived_partial_series).await;
    assert!(
        archived_partial_series["data"]
            .as_array()
            .unwrap()
            .is_empty()
    );
    assert_eq!(archived_partial_series["coverage"]["range_complete"], false);

    let invalid_range = get(
        &app,
        &format!("/api/v1/usage/summary?start={end}&end={start}"),
        &cookie,
    )
    .await;
    assert_eq!(invalid_range.status(), StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(
        invalid_range.headers()[header::CONTENT_TYPE],
        "application/problem+json"
    );

    let invalid_limit = get(&app, "/api/v1/requests?limit=0", &cookie).await;
    assert_eq!(invalid_limit.status(), StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(
        invalid_limit.headers()[header::CONTENT_TYPE],
        "application/problem+json"
    );

    for endpoint in [
        "/api/v1/provider-health?window_minutes=180".to_owned(),
        "/api/v1/audit?limit=50".to_owned(),
        "/api/v1/runtime-generations?limit=50".to_owned(),
        "/api/v1/settings".to_owned(),
    ] {
        let response = get(&app, &endpoint, &cookie).await;
        assert_eq!(response.status(), StatusCode::OK, "{endpoint}");
    }

    let pricing_page = get(&app, "/api/v1/pricing/revisions?limit=1", &cookie).await;
    assert_eq!(pricing_page.status(), StatusCode::OK);
    let pricing_page = response_json(pricing_page).await;
    assert_eq!(pricing_page["data"].as_array().unwrap().len(), 1);
    assert_eq!(pricing_page["data"][0]["revision"], 2);
    assert_eq!(pricing_page["next_cursor"], "2");

    let older_pricing = get(
        &app,
        &format!(
            "/api/v1/pricing/revisions?limit=1&cursor={}",
            pricing_page["next_cursor"].as_str().unwrap()
        ),
        &cookie,
    )
    .await;
    assert_eq!(older_pricing.status(), StatusCode::OK);
    let older_pricing = response_json(older_pricing).await;
    assert_eq!(older_pricing["data"].as_array().unwrap().len(), 1);
    assert_eq!(older_pricing["data"][0]["revision"], 1);
    assert!(older_pricing["next_cursor"].is_null());

    let setting = get(&app, "/api/v1/settings/retention.requests_days", &cookie).await;
    assert_eq!(setting.status(), StatusCode::OK);
    let etag = setting.headers()[header::ETAG].to_str().unwrap().to_owned();
    let updated = send(
        &app,
        Method::PUT,
        "/api/v1/settings/retention.requests_days",
        Some(json!({ "value": "31" })),
        RequestHeaders {
            cookie: Some(&cookie),
            csrf: Some(&csrf),
            idempotency_key: None,
            if_match: Some(&etag),
        },
    )
    .await;
    assert_eq!(updated.status(), StatusCode::OK);

    let unauthenticated = app
        .clone()
        .oneshot(
            Request::get("/api/v1/requests")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(unauthenticated.status(), StatusCode::UNAUTHORIZED);

    let unauthenticated_readiness = app
        .clone()
        .oneshot(
            Request::get("/api/v1/health/ready")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(unauthenticated_readiness.status(), StatusCode::UNAUTHORIZED);
}

async fn get(app: &Router, uri: &str, cookie: &str) -> Response<Body> {
    send(
        app,
        Method::GET,
        uri,
        None,
        RequestHeaders {
            cookie: Some(cookie),
            ..RequestHeaders::default()
        },
    )
    .await
}

#[derive(Default)]
struct RequestHeaders<'a> {
    cookie: Option<&'a str>,
    csrf: Option<&'a str>,
    idempotency_key: Option<&'a str>,
    if_match: Option<&'a str>,
}

async fn send(
    app: &Router,
    method: Method,
    uri: &str,
    body: Option<Value>,
    headers: RequestHeaders<'_>,
) -> Response<Body> {
    let is_mutation = method != Method::GET;
    let mut request = Request::builder().method(method).uri(uri);
    if is_mutation {
        request = request.header(header::ORIGIN, ORIGIN);
    }
    if body.is_some() {
        request = request.header(header::CONTENT_TYPE, "application/json");
    }
    if uri == "/api/v1/setup" {
        request = request.header("x-olp-setup-token", BOOTSTRAP_TOKEN);
    }
    if let Some(cookie) = headers.cookie {
        request = request.header(header::COOKIE, cookie);
    }
    if let Some(csrf) = headers.csrf {
        request = request.header("x-csrf-token", csrf);
    }
    if let Some(value) = headers.idempotency_key {
        request = request.header("idempotency-key", value);
    }
    if let Some(value) = headers.if_match {
        request = request.header(header::IF_MATCH, value);
    }
    let mut request = request
        .body(body.map_or_else(Body::empty, |body| {
            Body::from(serde_json::to_vec(&body).unwrap())
        }))
        .unwrap();
    request.extensions_mut().insert(axum::extract::ConnectInfo(
        "198.51.100.12:443".parse::<std::net::SocketAddr>().unwrap(),
    ));
    app.clone().oneshot(request).await.unwrap()
}

fn session_cookie(response: &Response<Body>) -> String {
    response
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .find_map(|value| {
            value
                .to_str()
                .ok()?
                .split(';')
                .next()
                .filter(|cookie| cookie.starts_with("__Host-olp_session="))
                .map(str::to_owned)
        })
        .unwrap()
}

async fn response_json(response: Response<Body>) -> Value {
    serde_json::from_slice(&response.into_body().collect().await.unwrap().to_bytes()).unwrap()
}

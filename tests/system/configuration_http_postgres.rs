use std::path::PathBuf;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::atomic::AtomicUsize;
use std::sync::atomic::Ordering;
use std::time::Duration;

use axum::Json;
use axum::Router;
use axum::body::Body;
use axum::extract::State;
use axum::http::HeaderMap;
use axum::http::Method;
use axum::http::Request;
use axum::http::Response;
use axum::http::StatusCode;
use axum::http::header;
use axum::routing::get;
use axum::routing::post;
use http_body_util::BodyExt as _;
use olp::crypto::envelope::MasterKey;
use olp::http::router::management_router_for_test;
use olp::net::egress::EgressPolicy;
use olp::process::mode::ApiMode;
use olp::process::state::ProcessComposition;
use olp::providers::connector::Timeouts;
use olp::providers::openai::ApiKey;
use olp::providers::openai::ConnectorConfig;
use olp::providers::openai::transport::Connector;
use olp::runtime::manager::Manager;
use serde_json::Value;
use serde_json::json;
use tokio::net::TcpListener;
use tower::ServiceExt as _;
use uuid::Uuid;

use crate::common::BOOTSTRAP_TOKEN;
use crate::common::configure_bootstrap;

mod api_key_issuers;
mod api_keys;
mod provider_kinds;
mod providers;
mod routes;
mod support;

use support::*;

const ORIGIN: &str = "https://olp.configuration.test";

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn configuration_http_flow_enforces_etags_roles_idempotency_and_one_time_secrets() {
    let db = olp::test_support::TestDb::create_migrated("configuration_http").await;
    let pool = db.pool(5).await;
    let mut state = ProcessComposition::new(
        ApiMode::Control,
        pool.clone(),
        Arc::new(Manager::empty()),
        ORIGIN,
        PathBuf::from("missing-console-for-configuration-test"),
    );
    state.master_key = Some(Arc::new(MasterKey::new(1, [7; 32])));
    configure_bootstrap(&mut state, [9; 32]);
    state.set_provider_egress_policy(EgressPolicy::new(
        vec!["127.0.0.0/8".parse().unwrap(), "::1/128".parse().unwrap()],
        vec!["127.0.0.1".to_owned()],
    ));
    let configuration_state = state.clone();
    let app = management_router_for_test(state.mode_dependencies().management().unwrap());
    let mock_provider = MockOpenAiProvider::spawn().await;

    let setup = send(
        &app,
        Method::POST,
        "/api/v3/setup",
        Some(json!({
            "email": "owner@configuration.test",
            "password": "correct horse battery staple",
            "display_name": "Owner",
            "installation_name": "Configuration HTTP test"
        })),
        None,
        None,
        None,
        None,
    )
    .await;
    assert_eq!(setup.status(), StatusCode::CREATED);
    let cookie = cookie_header(&setup);
    let setup_body = response_json(setup).await;
    let csrf = setup_body["csrf_token"].as_str().unwrap().to_owned();

    provider_kinds::verify(&app, &cookie, &csrf).await;
    provider_kinds::verify_egress_policy(&app, &mock_provider, &cookie, &csrf).await;
    let (provider_id, model_id) =
        providers::exercise(&app, &configuration_state, &mock_provider, &cookie, &csrf).await;
    let draft_id = routes::exercise(&app, &cookie, &csrf, &provider_id, &model_id).await;
    api_keys::exercise(&app, &cookie, &csrf, &draft_id, &model_id).await;
    api_key_issuers::verify(&app, &pool, &cookie).await;
}

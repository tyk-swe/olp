use std::{
    path::PathBuf,
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};

use axum::{
    Json, Router,
    body::Body,
    extract::State,
    http::{HeaderMap, Method, Request, Response, StatusCode, header},
    routing::{get, post},
};
use http_body_util::BodyExt as _;
use olp::{
    bootstrap::state::{ApiMode, ProcessComposition},
    public_http::router::management_router_for_test,
};
use olp_db::security::envelope::MasterKey;
use olp_engine::inference::runtime::Manager;
use olp_engine::providers::{
    connector::Timeouts,
    openai::{ApiKey, ConnectorConfig, transport::Connector},
};
use serde_json::{Value, json};
use tokio::net::TcpListener;
use tower::ServiceExt as _;
use uuid::Uuid;

use crate::common::{BOOTSTRAP_TOKEN, configure_bootstrap};

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
    let db = olp_db::test_support::TestDb::create_migrated("configuration_http").await;
    let store = db.store(5).await;
    let mut state = ProcessComposition::new(
        ApiMode::Control,
        store.clone(),
        Arc::new(Manager::empty()),
        ORIGIN,
        PathBuf::from("missing-console-for-configuration-test"),
    );
    state.master_key = Some(Arc::new(MasterKey::new(1, [7; 32])));
    configure_bootstrap(&mut state, [9; 32]);
    let configuration_state = state.clone();
    let app = management_router_for_test(state.mode_dependencies().management().unwrap());
    let mock_provider = MockOpenAiProvider::spawn().await;

    let setup = send(
        &app,
        Method::POST,
        "/api/v1/setup",
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
    let (provider_id, model_id) =
        providers::exercise(&app, &configuration_state, &mock_provider, &cookie, &csrf).await;
    let draft_id = routes::exercise(&app, &cookie, &csrf, &provider_id, &model_id).await;
    api_keys::exercise(&app, &cookie, &csrf, &draft_id, &model_id).await;
}

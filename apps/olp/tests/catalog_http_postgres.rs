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
use olp::{ApiMode, ApiState, RuntimeManager, public_router};
use olp_providers::openai::{ConnectorConfig, ConnectorTimeouts, OpenAiApiKey, OpenAiConnector};
use olp_storage::{MasterKey, PgStore};
use serde_json::{Value, json};
use tokio::net::TcpListener;
use tower::ServiceExt as _;
use uuid::Uuid;

mod common;
use common::{BOOTSTRAP_TOKEN, configure_bootstrap};

const ORIGIN: &str = "https://olp.catalog.test";

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn catalog_http_flow_enforces_etags_roles_idempotency_and_one_time_secrets() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 5).await.unwrap();
    store.migrate().await.unwrap();
    let mut state = ApiState::new(
        ApiMode::Control,
        Some(store.clone()),
        Arc::new(RuntimeManager::empty()),
        ORIGIN,
        PathBuf::from("missing-console-for-catalog-test"),
    );
    state.master_key = Some(Arc::new(MasterKey::new(1, [7; 32])));
    configure_bootstrap(&mut state, [9; 32]);
    let catalog_state = state.clone();
    let app = public_router(state);
    let mock_provider = MockOpenAiProvider::spawn().await;

    let setup = send(
        &app,
        Method::POST,
        "/api/v1/setup",
        Some(json!({
            "email": "owner@catalog.test",
            "password": "correct horse battery staple",
            "display_name": "Owner",
            "organization_name": "Catalog HTTP test"
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

    let vertex = send(
        &app,
        Method::POST,
        "/api/v1/providers",
        Some(json!({
            "name": "vertex-draft",
            "kind": "vertex_ai",
            "cloud_project": "project-test",
            "cloud_region": "us-central1",
            "auth_mode": "adc",
            "model": "gemini-test",
            "display_name": "Gemini Test"
        })),
        Some(&cookie),
        Some(&csrf),
        Some("provider-vertex-create-01"),
        None,
    )
    .await;
    assert_eq!(vertex.status(), StatusCode::CREATED);
    let vertex_etag = etag(&vertex);
    let vertex_id = response_json(vertex).await["id"]
        .as_str()
        .unwrap()
        .to_owned();
    let vertex_detail = send(
        &app,
        Method::GET,
        &format!("/api/v1/providers/{vertex_id}"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    let vertex_body = response_json(vertex_detail).await;
    assert_eq!(vertex_body["connector_ready"], true);
    assert_eq!(vertex_body["model_count"], 1);
    assert!(vertex_body.get("models").is_none());
    let vertex_models = send(
        &app,
        Method::GET,
        &format!("/api/v1/providers/{vertex_id}/models?limit=100"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(
        response_json(vertex_models).await["items"][0]["enabled"],
        true
    );
    let vertex_probe = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{vertex_id}/probe"),
        None,
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&vertex_etag),
    )
    .await;
    assert_eq!(vertex_probe.status(), StatusCode::UNPROCESSABLE_ENTITY);
    assert!(!vertex_etag.is_empty());

    let invalid_azure = send(
        &app,
        Method::POST,
        "/api/v1/providers",
        Some(json!({
            "name": "invalid-azure",
            "kind": "azure_open_ai",
            "model": "deployment-model"
        })),
        Some(&cookie),
        Some(&csrf),
        Some("provider-azure-invalid-01"),
        None,
    )
    .await;
    assert_eq!(invalid_azure.status(), StatusCode::UNPROCESSABLE_ENTITY);

    let azure = send(
        &app,
        Method::POST,
        "/api/v1/providers",
        Some(json!({
            "name": "azure-primary",
            "kind": "azure_open_ai",
            "endpoint": "https://resource.openai.azure.com",
            "deployment": "team-chat",
            "api_version": "2024-10-21",
            "credential": "azure-test-secret"
        })),
        Some(&cookie),
        Some(&csrf),
        Some("provider-azure-create-01"),
        None,
    )
    .await;
    assert_eq!(azure.status(), StatusCode::CREATED);
    let azure_etag = etag(&azure);
    let azure_id = response_json(azure).await["id"]
        .as_str()
        .unwrap()
        .to_owned();
    let azure_detail = send(
        &app,
        Method::GET,
        &format!("/api/v1/providers/{azure_id}"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    let azure_body = response_json(azure_detail).await;
    assert_eq!(azure_body["connector_ready"], true);
    assert_eq!(azure_body["deployment"], "team-chat");
    assert_eq!(azure_body["api_version"], "2024-10-21");
    assert_eq!(azure_body["model_count"], 0);
    assert!(azure_body.get("models").is_none());
    let azure_probe = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{azure_id}/probe"),
        None,
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&azure_etag),
    )
    .await;
    assert_eq!(azure_probe.status(), StatusCode::UNPROCESSABLE_ENTITY);
    assert!(!azure_etag.is_empty());

    for (idempotency_key, request) in [
        (
            "provider-http-legacy-vertex-key-0001",
            json!({
                "name": "legacy-vertex",
                "kind": "vertex_ai",
                "cloud_project": "project-test",
                "cloud_region": "us-central1",
                "auth_mode": "adc",
                "model": "gemini-test",
                "api_key": "legacy-secret"
            }),
        ),
        (
            "provider-http-legacy-bedrock-key-0001",
            json!({
                "name": "legacy-bedrock",
                "kind": "bedrock",
                "cloud_region": "us-east-1",
                "auth_mode": "default_chain",
                "api_key": "legacy-secret"
            }),
        ),
    ] {
        let legacy_provider_request = send(
            &app,
            Method::POST,
            "/api/v1/providers",
            Some(request),
            Some(&cookie),
            Some(&csrf),
            Some(idempotency_key),
            None,
        )
        .await;
        assert_eq!(
            legacy_provider_request.status(),
            StatusCode::UNPROCESSABLE_ENTITY
        );
        assert!(response_json(legacy_provider_request).await["errors"]["api_key"].is_array());
    }

    let provider = send(
        &app,
        Method::POST,
        "/api/v1/providers",
        Some(json!({
            "name": "openai-primary",
            "kind": "open_ai",
            "credential": "sk-openai-test-secret",
            "model": "compatible-model",
            "display_name": "Compatible Model"
        })),
        Some(&cookie),
        Some(&csrf),
        Some("provider-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(provider.status(), StatusCode::CREATED);
    let mut provider_etag = etag(&provider);
    let provider_body = response_json(provider).await;
    let provider_id = provider_body["id"].as_str().unwrap().to_owned();
    catalog_state.register_catalog_openai_connector_for_test(
        Uuid::parse_str(&provider_id).unwrap(),
        mock_provider.connector("sk-openai-test-secret"),
    );
    let provider_replay = send(
        &app,
        Method::POST,
        "/api/v1/providers",
        Some(json!({
            "name": "openai-primary",
            "kind": "open_ai",
            "credential": "sk-openai-test-secret",
            "model": "compatible-model",
            "display_name": "Compatible Model"
        })),
        Some(&cookie),
        Some(&csrf),
        Some("provider-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(provider_replay.status(), StatusCode::CREATED);
    assert_eq!(etag(&provider_replay), provider_etag);
    assert_eq!(response_json(provider_replay).await, provider_body);
    let provider_mismatch = send(
        &app,
        Method::POST,
        "/api/v1/providers",
        Some(json!({
            "name": "openai-changed",
            "kind": "open_ai",
            "credential": "sk-openai-test-secret",
            "model": "compatible-model",
            "display_name": "Compatible Model"
        })),
        Some(&cookie),
        Some(&csrf),
        Some("provider-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(provider_mismatch.status(), StatusCode::CONFLICT);

    let provider_update = send(
        &app,
        Method::PATCH,
        &format!("/api/v1/providers/{provider_id}"),
        Some(json!({
            "name": "openai-primary-updated",
            "auth_mode": "api_key"
        })),
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&provider_etag),
    )
    .await;
    assert_eq!(provider_update.status(), StatusCode::OK);
    provider_etag = etag(&provider_update);
    assert_eq!(
        response_json(provider_update).await["name"],
        "openai-primary-updated"
    );

    let providers = send(
        &app,
        Method::GET,
        "/api/v1/providers?limit=10",
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(providers.status(), StatusCode::OK);
    let providers_body = response_json(providers).await;
    let open_ai = providers_body["items"]
        .as_array()
        .unwrap()
        .iter()
        .find(|provider| provider["kind"] == "open_ai")
        .unwrap();
    assert!(open_ai.get("credential").is_none());
    assert!(open_ai.get("models").is_none());
    assert!(open_ai.get("endpoint").is_none());
    assert_eq!(open_ai["model_count"], 1);

    let missing_probe_precondition = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/probe"),
        None,
        Some(&cookie),
        Some(&csrf),
        None,
        None,
    )
    .await;
    assert_eq!(
        missing_probe_precondition.status(),
        StatusCode::PRECONDITION_REQUIRED
    );
    let probe = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/probe"),
        None,
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&provider_etag),
    )
    .await;
    assert_eq!(probe.status(), StatusCode::OK);
    let probe_body = response_json(probe).await;
    assert_eq!(probe_body["succeeded"], true);
    assert_eq!(probe_body["probe_type"], "connector_connectivity");
    assert_eq!(mock_provider.model_requests(), 1);

    let discovery = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/discovery"),
        Some(json!({
            "mode": "manual",
            "models": [
                {
                    "upstream_model": "compatible-model",
                    "display_name": "Compatible Model"
                },
                {
                    "upstream_model": "compatible-model-secondary",
                    "display_name": "Compatible Model Secondary"
                }
            ]
        })),
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&provider_etag),
    )
    .await;
    assert_eq!(discovery.status(), StatusCode::OK);
    let discovery_etag = etag(&discovery);
    let discovery_body = response_json(discovery).await;
    assert_eq!(discovery_body["model_count"], 2);
    assert_eq!(discovery_body["added_model_count"], 2);
    assert!(discovery_body.get("models").is_none());
    let first_model_page = send(
        &app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/models?limit=1"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(first_model_page.status(), StatusCode::OK);
    let first_model_page = response_json(first_model_page).await;
    assert_eq!(first_model_page["items"].as_array().unwrap().len(), 1);
    let model_cursor = first_model_page["next_cursor"].as_str().unwrap();
    let second_model_page = send(
        &app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/models?limit=1&cursor={model_cursor}"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(second_model_page.status(), StatusCode::OK);
    let second_model_page = response_json(second_model_page).await;
    assert_eq!(second_model_page["items"].as_array().unwrap().len(), 1);
    assert!(second_model_page["next_cursor"].is_null());
    let model_id = first_model_page["items"]
        .as_array()
        .unwrap()
        .iter()
        .chain(second_model_page["items"].as_array().unwrap())
        .find(|model| model["upstream_model"] == "compatible-model")
        .and_then(|model| model["id"].as_str())
        .unwrap()
        .to_owned();

    let stale_discovery = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/discovery"),
        Some(json!({
            "mode": "manual",
            "models": [{
                "upstream_model": "compatible-model",
                "display_name": "Compatible Model"
            }]
        })),
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&provider_etag),
    )
    .await;
    assert_eq!(stale_discovery.status(), StatusCode::PRECONDITION_FAILED);

    let reviewed_model = send(
        &app,
        Method::PATCH,
        &format!("/api/v1/providers/{provider_id}/models/{model_id}"),
        Some(json!({
            "enabled": true,
            "capabilities": [
                {"operation": "embeddings", "surface": "open_ai", "mode": "unary"}
            ]
        })),
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&discovery_etag),
    )
    .await;
    assert_eq!(reviewed_model.status(), StatusCode::OK);
    let reviewed_etag = etag(&reviewed_model);
    let reviewed_body = response_json(reviewed_model).await;
    assert_eq!(reviewed_body["enabled_model_count"], 1);
    assert!(reviewed_body.get("models").is_none());
    let reviewed_models = send(
        &app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/models?limit=100"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    let reviewed_models = response_json(reviewed_models).await;
    let reviewed_model = reviewed_models["items"]
        .as_array()
        .unwrap()
        .iter()
        .find(|model| model["id"] == model_id)
        .unwrap();
    assert_eq!(reviewed_model["enabled"], true);
    assert_eq!(reviewed_model["capabilities"][0]["source"], "declared");
    let eligible_inventory = send(
        &app,
        Method::GET,
        "/api/v1/provider-models?enabled=true&limit=100",
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(eligible_inventory.status(), StatusCode::OK);
    let eligible_inventory = response_json(eligible_inventory).await;
    let inventory_model = eligible_inventory["items"]
        .as_array()
        .unwrap()
        .iter()
        .find(|item| item["provider_id"] == provider_id && item["model"]["id"] == model_id)
        .unwrap();
    assert_eq!(inventory_model["provider_kind"], "open_ai");
    assert_eq!(inventory_model["model"]["enabled"], true);

    let reviewed_probe = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/probe"),
        None,
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&reviewed_etag),
    )
    .await;
    assert_eq!(reviewed_probe.status(), StatusCode::OK);
    assert_eq!(response_json(reviewed_probe).await["succeeded"], true);
    assert_eq!(mock_provider.model_requests(), 2);

    let native_certification = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/models/{model_id}/certify"),
        None,
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&reviewed_etag),
    )
    .await;
    assert_eq!(native_certification.status(), StatusCode::OK);
    let certified_etag = etag(&native_certification);
    assert_eq!(
        response_json(native_certification).await["certified_count"],
        1
    );

    let missing_activation_key = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/activate"),
        None,
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&certified_etag),
    )
    .await;
    assert_eq!(missing_activation_key.status(), StatusCode::BAD_REQUEST);

    let activation = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/activate"),
        None,
        Some(&cookie),
        Some(&csrf),
        Some("provider-http-activate-01"),
        Some(&certified_etag),
    )
    .await;
    assert_eq!(activation.status(), StatusCode::OK);
    let active_etag = etag(&activation);
    let duplicate_activation = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/activate"),
        None,
        Some(&cookie),
        Some(&csrf),
        Some("provider-http-activate-01"),
        Some(&reviewed_etag),
    )
    .await;
    assert_eq!(duplicate_activation.status(), StatusCode::CONFLICT);

    let rotated_provider = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/credentials"),
        Some(json!({"credential": "sk-openai-rotated-secret"})),
        Some(&cookie),
        Some(&csrf),
        Some("provider-http-rotate-0001"),
        Some(&active_etag),
    )
    .await;
    assert_eq!(rotated_provider.status(), StatusCode::CREATED);
    let rotated_provider_etag = etag(&rotated_provider);
    let rotated_provider_body = response_json(rotated_provider).await;
    assert!(rotated_provider_body["runtime_generation"].is_null());
    let rotated_credential_id = rotated_provider_body["credential_id"]
        .as_str()
        .unwrap()
        .to_owned();
    let rotated_provider_replay = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/credentials"),
        Some(json!({"credential": "sk-openai-rotated-secret"})),
        Some(&cookie),
        Some(&csrf),
        Some("provider-http-rotate-0001"),
        Some(&active_etag),
    )
    .await;
    assert_eq!(rotated_provider_replay.status(), StatusCode::CREATED);
    assert_eq!(etag(&rotated_provider_replay), rotated_provider_etag);
    assert_eq!(
        response_json(rotated_provider_replay).await,
        rotated_provider_body
    );
    let rotated_provider_mismatch = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/credentials"),
        Some(json!({"credential": "sk-openai-different-secret"})),
        Some(&cookie),
        Some(&csrf),
        Some("provider-http-rotate-0001"),
        Some(&active_etag),
    )
    .await;
    assert_eq!(rotated_provider_mismatch.status(), StatusCode::CONFLICT);

    let staged_provider = send(
        &app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(staged_provider.status(), StatusCode::OK);
    let staged_provider_body = response_json(staged_provider).await;
    assert_eq!(staged_provider_body["state"], "draft");
    assert_eq!(staged_provider_body["active_revision"], 1);
    assert_eq!(staged_provider_body["pending_activation"], true);
    assert_eq!(
        staged_provider_body["draft_credential_id"],
        rotated_credential_id
    );
    assert_ne!(
        staged_provider_body["runtime_credential_id"],
        staged_provider_body["draft_credential_id"]
    );
    let runtime_credential_id = staged_provider_body["runtime_credential_id"]
        .as_str()
        .unwrap()
        .to_owned();

    let credential_page = send(
        &app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/credentials?limit=1"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(credential_page.status(), StatusCode::OK);
    let credential_page = response_json(credential_page).await;
    assert_eq!(credential_page["items"].as_array().unwrap().len(), 1);
    assert_eq!(credential_page["items"][0]["id"], rotated_credential_id);
    assert_eq!(credential_page["items"][0]["active"], false);
    assert_eq!(credential_page["items"][0]["draft_selected"], true);
    let credential_cursor = credential_page["next_cursor"].as_str().unwrap();
    let older_credentials = send(
        &app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/credentials?limit=1&cursor={credential_cursor}"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(older_credentials.status(), StatusCode::OK);
    let older_credentials = response_json(older_credentials).await;
    assert_eq!(older_credentials["items"].as_array().unwrap().len(), 1);
    assert_eq!(older_credentials["items"][0]["id"], runtime_credential_id);
    assert_eq!(older_credentials["items"][0]["active"], true);
    assert_eq!(older_credentials["items"][0]["draft_selected"], false);
    assert!(older_credentials["next_cursor"].is_null());

    let cannot_revoke_runtime_credential = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/credentials/{runtime_credential_id}/revoke"),
        None,
        Some(&cookie),
        Some(&csrf),
        Some("provider-runtime-credential-revoke-blocked-01"),
        Some(&rotated_provider_etag),
    )
    .await;
    assert_eq!(
        cannot_revoke_runtime_credential.status(),
        StatusCode::CONFLICT
    );

    catalog_state.register_catalog_openai_connector_for_test(
        Uuid::parse_str(&provider_id).unwrap(),
        mock_provider.connector("sk-openai-rotated-secret"),
    );
    let rotated_probe = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/probe"),
        None,
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&rotated_provider_etag),
    )
    .await;
    assert_eq!(rotated_probe.status(), StatusCode::OK);
    assert_eq!(response_json(rotated_probe).await["succeeded"], true);
    assert_eq!(mock_provider.model_requests(), 3);
    assert_eq!(
        mock_provider.last_authorization().as_deref(),
        Some("Bearer sk-openai-rotated-secret")
    );
    let rotated_certification = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/models/{model_id}/certify"),
        None,
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&rotated_provider_etag),
    )
    .await;
    assert_eq!(rotated_certification.status(), StatusCode::OK);
    let rotated_certified_etag = etag(&rotated_certification);
    assert_eq!(
        response_json(rotated_certification).await["certified_count"],
        1
    );
    let rotated_activation = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/activate"),
        None,
        Some(&cookie),
        Some(&csrf),
        Some("provider-http-activate-02"),
        Some(&rotated_certified_etag),
    )
    .await;
    assert_eq!(rotated_activation.status(), StatusCode::OK);
    let reactivated_provider_etag = etag(&rotated_activation);

    let reactivated_provider = send(
        &app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    let reactivated_provider_body = response_json(reactivated_provider).await;
    assert_eq!(reactivated_provider_body["state"], "active");
    assert_eq!(reactivated_provider_body["active_revision"], 2);
    assert_eq!(reactivated_provider_body["pending_activation"], false);
    assert_eq!(
        reactivated_provider_body["runtime_credential_id"],
        rotated_credential_id
    );
    assert_eq!(
        reactivated_provider_body["draft_credential_id"],
        rotated_credential_id
    );

    let provider_revisions = send(
        &app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/revisions?limit=1"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(provider_revisions.status(), StatusCode::OK);
    let provider_revisions = response_json(provider_revisions).await;
    let latest_provider_revision = &provider_revisions["items"][0];
    assert_eq!(latest_provider_revision["revision"], 2);
    assert_eq!(latest_provider_revision["model_count"], 2);
    assert!(latest_provider_revision.get("models").is_none());
    assert!(provider_revisions["next_cursor"].is_string());
    let latest_provider_revision_id = latest_provider_revision["id"].as_str().unwrap();
    let provider_revision_detail = send(
        &app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/revisions/{latest_provider_revision_id}"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    let provider_revision_detail = response_json(provider_revision_detail).await;
    assert_eq!(provider_revision_detail["model_count"], 2);
    assert!(provider_revision_detail.get("models").is_none());
    let revision_models = send(
        &app,
        Method::GET,
        &format!(
            "/api/v1/providers/{provider_id}/revisions/{latest_provider_revision_id}/models?limit=1"
        ),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(revision_models.status(), StatusCode::OK);
    let revision_models = response_json(revision_models).await;
    assert_eq!(revision_models["items"].as_array().unwrap().len(), 1);
    assert!(revision_models["next_cursor"].is_string());

    let active_credentials = send(
        &app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/credentials?limit=100"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    let active_credentials = response_json(active_credentials).await;
    let active_credential = active_credentials["items"]
        .as_array()
        .unwrap()
        .iter()
        .find(|credential| credential["id"] == rotated_credential_id)
        .unwrap();
    assert_eq!(active_credential["active"], true);
    assert_eq!(active_credential["draft_selected"], false);
    let obsolete_credential = active_credentials["items"]
        .as_array()
        .unwrap()
        .iter()
        .find(|credential| credential["id"] == runtime_credential_id)
        .unwrap();
    assert_eq!(obsolete_credential["active"], false);
    assert_eq!(obsolete_credential["draft_selected"], false);

    let revoked_credential = send(
        &app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/credentials/{runtime_credential_id}/revoke"),
        None,
        Some(&cookie),
        Some(&csrf),
        Some("provider-credential-revoke-01"),
        Some(&reactivated_provider_etag),
    )
    .await;
    assert_eq!(revoked_credential.status(), StatusCode::OK);

    let route = send(
        &app,
        Method::POST,
        "/api/v1/route-drafts",
        Some(json!({
            "slug": "default",
            "operations": ["embeddings"],
            "overall_timeout_ms": 30000,
            "max_attempts": 1,
            "targets": [{
                "provider_id": provider_id,
                "provider_model": "compatible-model",
                "priority": 0,
                "weight": 1,
                "timeout_ms": 20000
            }]
        })),
        Some(&cookie),
        Some(&csrf),
        Some("route-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(route.status(), StatusCode::CREATED);
    let mut route_etag = etag(&route);
    let route_body = response_json(route).await;
    let draft_id = route_body["id"].as_str().unwrap().to_owned();
    let route_replay = send(
        &app,
        Method::POST,
        "/api/v1/route-drafts",
        Some(json!({
            "slug": "default",
            "operations": ["embeddings"],
            "overall_timeout_ms": 30000,
            "max_attempts": 1,
            "targets": [{
                "provider_id": provider_id,
                "provider_model": "compatible-model",
                "priority": 0,
                "weight": 1,
                "timeout_ms": 20000
            }]
        })),
        Some(&cookie),
        Some(&csrf),
        Some("route-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(route_replay.status(), StatusCode::CREATED);
    assert_eq!(etag(&route_replay), route_etag);
    assert_eq!(response_json(route_replay).await, route_body);
    let route_mismatch = send(
        &app,
        Method::POST,
        "/api/v1/route-drafts",
        Some(json!({
            "slug": "default",
            "operations": ["embeddings"],
            "overall_timeout_ms": 31000,
            "max_attempts": 1,
            "targets": [{
                "provider_id": provider_id,
                "provider_model": "compatible-model",
                "priority": 0,
                "weight": 1,
                "timeout_ms": 20000
            }]
        })),
        Some(&cookie),
        Some(&csrf),
        Some("route-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(route_mismatch.status(), StatusCode::CONFLICT);

    let route_update = send(
        &app,
        Method::PUT,
        &format!("/api/v1/route-drafts/{draft_id}"),
        Some(json!({
            "slug": "default",
            "operations": ["embeddings"],
            "overall_timeout_ms": 35000,
            "max_attempts": 1,
            "targets": [{
                "provider_model_id": model_id,
                "priority": 0,
                "weight": 5,
                "timeout_ms": 22000
            }]
        })),
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&route_etag),
    )
    .await;
    assert_eq!(route_update.status(), StatusCode::OK);
    route_etag = etag(&route_update);

    let simulation = send(
        &app,
        Method::POST,
        &format!("/api/v1/route-drafts/{draft_id}/simulate"),
        Some(json!({
            "operation": "embeddings",
            "surface": "open_ai",
            "mode": "unary",
            "seed": "sdk-request-1"
        })),
        Some(&cookie),
        Some(&csrf),
        None,
        None,
    )
    .await;
    assert_eq!(simulation.status(), StatusCode::OK);
    assert_eq!(response_json(simulation).await["targets"][0]["attempt"], 1);

    let validation = send(
        &app,
        Method::POST,
        &format!("/api/v1/route-drafts/{draft_id}/validate"),
        None,
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&route_etag),
    )
    .await;
    assert_eq!(validation.status(), StatusCode::OK);
    let validated_etag = etag(&validation);
    let activation = send(
        &app,
        Method::POST,
        &format!("/api/v1/route-drafts/{draft_id}/activate"),
        None,
        Some(&cookie),
        Some(&csrf),
        Some("route-http-activate-0001"),
        Some(&validated_etag),
    )
    .await;
    assert_eq!(activation.status(), StatusCode::OK);
    let activation_body = response_json(activation).await;
    let route_id = activation_body["route_id"].as_str().unwrap();
    let first_revision_id = activation_body["revision_id"].as_str().unwrap();

    let revisions = send(
        &app,
        Method::GET,
        &format!("/api/v1/routes/{route_id}/revisions"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(revisions.status(), StatusCode::OK);
    assert_eq!(response_json(revisions).await["items"][0]["revision"], 1);

    let routes = send(
        &app,
        Method::GET,
        "/api/v1/routes?limit=1",
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(routes.status(), StatusCode::OK);
    let routes_body = response_json(routes).await;
    assert_eq!(routes_body["items"][0]["id"], route_id);
    assert_eq!(routes_body["items"][0]["latest_revision"]["revision"], 1);
    assert_eq!(routes_body["items"][0]["revision_count"], 1);
    let route_detail = send(
        &app,
        Method::GET,
        &format!("/api/v1/routes/{route_id}"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(route_detail.status(), StatusCode::OK);
    assert_eq!(response_json(route_detail).await["slug"], "default");

    let active_draft = send(
        &app,
        Method::GET,
        &format!("/api/v1/route-drafts/{draft_id}"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    let active_draft_etag = etag(&active_draft);
    let second_draft = send(
        &app,
        Method::PUT,
        &format!("/api/v1/route-drafts/{draft_id}"),
        Some(json!({
            "slug": "default",
            "operations": ["embeddings"],
            "overall_timeout_ms": 45000,
            "max_attempts": 1,
            "targets": [{
                "provider_model_id": model_id,
                "priority": 0,
                "weight": 9,
                "timeout_ms": 25000
            }]
        })),
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&active_draft_etag),
    )
    .await;
    let second_draft_etag = etag(&second_draft);
    let second_validation = send(
        &app,
        Method::POST,
        &format!("/api/v1/route-drafts/{draft_id}/validate"),
        None,
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&second_draft_etag),
    )
    .await;
    let second_validated_etag = etag(&second_validation);
    let second_activation = send(
        &app,
        Method::POST,
        &format!("/api/v1/route-drafts/{draft_id}/activate"),
        None,
        Some(&cookie),
        Some(&csrf),
        Some("route-http-activate-0002"),
        Some(&second_validated_etag),
    )
    .await;
    assert_eq!(second_activation.status(), StatusCode::OK);
    let second_activation_body = response_json(second_activation).await;
    let second_revision_id = second_activation_body["revision_id"].as_str().unwrap();
    let revision_page = send(
        &app,
        Method::GET,
        &format!("/api/v1/routes/{route_id}/revisions?limit=1"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(revision_page.status(), StatusCode::OK);
    let revision_page = response_json(revision_page).await;
    assert_eq!(revision_page["items"].as_array().unwrap().len(), 1);
    assert_eq!(revision_page["items"][0]["revision"], 2);
    let revision_cursor = revision_page["next_cursor"].as_str().unwrap();
    let older_revisions = send(
        &app,
        Method::GET,
        &format!("/api/v1/routes/{route_id}/revisions?limit=1&cursor={revision_cursor}"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(older_revisions.status(), StatusCode::OK);
    let older_revisions = response_json(older_revisions).await;
    assert_eq!(older_revisions["items"].as_array().unwrap().len(), 1);
    assert_eq!(older_revisions["items"][0]["revision"], 1);
    assert!(older_revisions["next_cursor"].is_null());
    let revision_diff = send(
        &app,
        Method::GET,
        &format!(
            "/api/v1/routes/{route_id}/revisions/diff?from={first_revision_id}&to={second_revision_id}"
        ),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(revision_diff.status(), StatusCode::OK);
    assert_eq!(response_json(revision_diff).await["timeout_changed"], true);
    let restored = send(
        &app,
        Method::POST,
        &format!("/api/v1/routes/{route_id}/revisions/{first_revision_id}/restore-as-draft"),
        None,
        Some(&cookie),
        Some(&csrf),
        Some("route-http-restore-0001"),
        None,
    )
    .await;
    assert_eq!(restored.status(), StatusCode::CREATED);
    let restored_etag = etag(&restored);
    let restored_id = response_json(restored).await["id"]
        .as_str()
        .unwrap()
        .to_owned();
    let deleted_restored = send(
        &app,
        Method::DELETE,
        &format!("/api/v1/route-drafts/{restored_id}"),
        None,
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&restored_etag),
    )
    .await;
    assert_eq!(deleted_restored.status(), StatusCode::NO_CONTENT);

    let api_key = send(
        &app,
        Method::POST,
        "/api/v1/api-keys",
        Some(json!({
            "name": "SDK key",
            "scopes": ["inference", "models_read"],
            "allowed_routes": ["default"]
        })),
        Some(&cookie),
        Some(&csrf),
        Some("api-key-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(api_key.status(), StatusCode::CREATED);
    let api_key_create_etag = etag(&api_key);
    let api_key_body = response_json(api_key).await;
    let api_key_id = api_key_body["id"].as_str().unwrap().to_owned();
    assert!(
        api_key_body["secret"]
            .as_str()
            .unwrap()
            .starts_with("olp_v2_")
    );
    let api_key_replay = send(
        &app,
        Method::POST,
        "/api/v1/api-keys",
        Some(json!({
            "name": "SDK key",
            "scopes": ["inference", "models_read"],
            "allowed_routes": ["default"]
        })),
        Some(&cookie),
        Some(&csrf),
        Some("api-key-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(api_key_replay.status(), StatusCode::CREATED);
    assert_eq!(etag(&api_key_replay), api_key_create_etag);
    assert_eq!(response_json(api_key_replay).await, api_key_body);
    let api_key_mismatch = send(
        &app,
        Method::POST,
        "/api/v1/api-keys",
        Some(json!({
            "name": "Changed SDK key",
            "scopes": ["inference", "models_read"],
            "allowed_routes": ["default"]
        })),
        Some(&cookie),
        Some(&csrf),
        Some("api-key-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(api_key_mismatch.status(), StatusCode::CONFLICT);

    let key_detail = send(
        &app,
        Method::GET,
        &format!("/api/v1/api-keys/{api_key_id}"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(key_detail.status(), StatusCode::OK);
    let mut key_etag = etag(&key_detail);
    let key_detail_body = response_json(key_detail).await;
    assert!(key_detail_body.get("secret").is_none());
    assert_eq!(key_detail_body["allowed_routes"][0], "default");

    let duplicate_policy = send(
        &app,
        Method::PATCH,
        &format!("/api/v1/api-keys/{api_key_id}"),
        Some(json!({
            "name": "Duplicate policy",
            "scopes": ["inference", "inference"],
            "allowed_routes": []
        })),
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&key_etag),
    )
    .await;
    assert_eq!(duplicate_policy.status(), StatusCode::UNPROCESSABLE_ENTITY);
    let updated_key = send(
        &app,
        Method::PATCH,
        &format!("/api/v1/api-keys/{api_key_id}"),
        Some(json!({
            "name": "Updated SDK key",
            "scopes": ["inference"],
            "allowed_routes": [],
            "requests_per_minute": 60,
            "tokens_per_minute": 10000,
            "max_concurrency": 4,
            "expires_at": null
        })),
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&key_etag),
    )
    .await;
    assert_eq!(updated_key.status(), StatusCode::OK);
    let original_key_etag = key_etag;
    key_etag = etag(&updated_key);
    assert!(response_json(updated_key).await["runtime_generation"].is_object());
    let updated_key_detail = send(
        &app,
        Method::GET,
        &format!("/api/v1/api-keys/{api_key_id}"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(updated_key_detail.status(), StatusCode::OK);
    let updated_key_body = response_json(updated_key_detail).await;
    assert_eq!(updated_key_body["name"], "Updated SDK key");
    assert_eq!(updated_key_body["scopes"], json!(["inference"]));
    assert_eq!(updated_key_body["allowed_routes"], json!([]));
    assert_eq!(updated_key_body["requests_per_minute"], 60);
    let stale_key_update = send(
        &app,
        Method::PATCH,
        &format!("/api/v1/api-keys/{api_key_id}"),
        Some(json!({
            "name": "Stale update",
            "scopes": ["inference"],
            "allowed_routes": []
        })),
        Some(&cookie),
        Some(&csrf),
        None,
        Some(&original_key_etag),
    )
    .await;
    assert_eq!(stale_key_update.status(), StatusCode::PRECONDITION_FAILED);

    let rotated_key = send(
        &app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/rotate"),
        None,
        Some(&cookie),
        Some(&csrf),
        Some("api-key-http-rotate-0001"),
        Some(&key_etag),
    )
    .await;
    assert_eq!(rotated_key.status(), StatusCode::OK);
    let rotated_key_etag = etag(&rotated_key);
    let rotated_key_body = response_json(rotated_key).await;
    assert!(
        rotated_key_body["secret"]
            .as_str()
            .unwrap()
            .starts_with("olp_v2_")
    );
    assert!(rotated_key_body["runtime_generation"].is_object());
    let rotated_key_replay = send(
        &app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/rotate"),
        None,
        Some(&cookie),
        Some(&csrf),
        Some("api-key-http-rotate-0001"),
        Some(&key_etag),
    )
    .await;
    assert_eq!(rotated_key_replay.status(), StatusCode::OK);
    assert_eq!(etag(&rotated_key_replay), rotated_key_etag);
    assert_eq!(response_json(rotated_key_replay).await, rotated_key_body);
    let rotated_key_mismatch = send(
        &app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/rotate"),
        None,
        Some(&cookie),
        Some(&csrf),
        Some("api-key-http-rotate-0001"),
        Some(&rotated_key_etag),
    )
    .await;
    assert_eq!(rotated_key_mismatch.status(), StatusCode::CONFLICT);

    let stale_revoke = send(
        &app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/revoke"),
        None,
        Some(&cookie),
        Some(&csrf),
        Some("api-key-http-revoke-0001"),
        Some(&key_etag),
    )
    .await;
    assert_eq!(stale_revoke.status(), StatusCode::PRECONDITION_FAILED);
    let revoked_key = send(
        &app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/revoke"),
        None,
        Some(&cookie),
        Some(&csrf),
        Some("api-key-http-revoke-0001"),
        Some(&rotated_key_etag),
    )
    .await;
    assert_eq!(revoked_key.status(), StatusCode::OK);
    assert!(etag(&revoked_key).starts_with('"'));
    let duplicate_revoke = send(
        &app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/revoke"),
        None,
        Some(&cookie),
        Some(&csrf),
        Some("api-key-http-revoke-0001"),
        Some(&rotated_key_etag),
    )
    .await;
    assert_eq!(duplicate_revoke.status(), StatusCode::CONFLICT);

    // The route target remains identified by its durable model ID in catalog responses.
    let draft_detail = send(
        &app,
        Method::GET,
        &format!("/api/v1/route-drafts/{draft_id}"),
        None,
        Some(&cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(
        response_json(draft_detail).await["targets"][0]["provider_model_id"],
        model_id
    );
}

#[derive(Clone)]
struct MockOpenAiState {
    model_requests: Arc<AtomicUsize>,
    authorizations: Arc<Mutex<Vec<String>>>,
}

struct MockOpenAiProvider {
    base_url: String,
    state: MockOpenAiState,
}

impl MockOpenAiProvider {
    async fn spawn() -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let state = MockOpenAiState {
            model_requests: Arc::new(AtomicUsize::new(0)),
            authorizations: Arc::new(Mutex::new(Vec::new())),
        };
        let app = Router::new()
            .route("/v1/models", get(mock_openai_models))
            .route("/v1/embeddings", post(mock_openai_embeddings))
            .with_state(state.clone());
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        Self {
            base_url: format!("http://{address}/v1/"),
            state,
        }
    }

    fn connector(&self, api_key: &str) -> OpenAiConnector {
        OpenAiConnector::new(
            ConnectorConfig::for_local_test(
                &self.base_url,
                ConnectorTimeouts {
                    connect: Duration::from_secs(1),
                    first_byte: Duration::from_secs(1),
                    idle: Duration::from_secs(1),
                },
            ),
            OpenAiApiKey::new(api_key).unwrap(),
        )
    }

    fn model_requests(&self) -> usize {
        self.state.model_requests.load(Ordering::SeqCst)
    }

    fn last_authorization(&self) -> Option<String> {
        self.state.authorizations.lock().unwrap().last().cloned()
    }
}

async fn mock_openai_models(
    State(state): State<MockOpenAiState>,
    headers: HeaderMap,
) -> Json<Value> {
    state.model_requests.fetch_add(1, Ordering::SeqCst);
    state.authorizations.lock().unwrap().push(
        headers
            .get(header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default()
            .to_owned(),
    );
    Json(json!({
        "object": "list",
        "data": [{"id": "compatible-model", "object": "model"}]
    }))
}

async fn mock_openai_embeddings(
    State(state): State<MockOpenAiState>,
    headers: HeaderMap,
    Json(body): Json<Value>,
) -> Json<Value> {
    state.authorizations.lock().unwrap().push(
        headers
            .get(header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default()
            .to_owned(),
    );
    Json(json!({
        "object": "list",
        "model": body["model"],
        "data": [{"object": "embedding", "index": 0, "embedding": [0.25]}],
        "usage": {"prompt_tokens": 1, "total_tokens": 1}
    }))
}

#[allow(clippy::too_many_arguments)]
async fn send(
    app: &Router,
    method: Method,
    uri: &str,
    body: Option<Value>,
    cookie: Option<&str>,
    csrf: Option<&str>,
    idempotency_key: Option<&str>,
    if_match: Option<&str>,
) -> Response<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if body.is_some() {
        builder = builder.header(header::CONTENT_TYPE, "application/json");
    }
    if let Some(cookie) = cookie {
        builder = builder.header(header::COOKIE, cookie);
    }
    if let Some(csrf) = csrf {
        builder = builder
            .header("x-csrf-token", csrf)
            .header(header::ORIGIN, ORIGIN);
    } else if body.is_some() {
        builder = builder.header(header::ORIGIN, ORIGIN);
    }
    if let Some(idempotency_key) = idempotency_key {
        builder = builder.header("idempotency-key", idempotency_key);
    }
    if let Some(if_match) = if_match {
        builder = builder.header(header::IF_MATCH, if_match);
    }
    if uri == "/api/v1/setup" {
        builder = builder.header("x-olp-setup-token", BOOTSTRAP_TOKEN);
    }
    let body = body.map_or_else(Body::empty, |value| Body::from(value.to_string()));
    let mut request = builder.body(body).unwrap();
    request.extensions_mut().insert(axum::extract::ConnectInfo(
        "198.51.100.10:443".parse::<std::net::SocketAddr>().unwrap(),
    ));
    app.clone().oneshot(request).await.unwrap()
}

fn cookie_header(response: &Response<Body>) -> String {
    response
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .filter_map(|value| value.to_str().ok())
        .filter_map(|cookie| cookie.split(';').next())
        .collect::<Vec<_>>()
        .join("; ")
}

fn etag(response: &Response<Body>) -> String {
    response
        .headers()
        .get(header::ETAG)
        .unwrap()
        .to_str()
        .unwrap()
        .to_owned()
}

async fn response_json(response: Response<Body>) -> Value {
    let bytes = response.into_body().collect().await.unwrap().to_bytes();
    serde_json::from_slice(&bytes).unwrap()
}

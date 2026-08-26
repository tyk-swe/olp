use std::{path::PathBuf, sync::Arc};

use axum::{
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::IntoResponse,
};
use chrono::Utc;
use olp_db::{
    authentication::SessionPrincipal, configuration::Error, idempotency::Outcome,
    idempotency::Response, security::session_material::SessionMaterial,
};
use olp_engine::domain::{
    auth::{Permission, Role},
    provider::ProviderAuthMode,
    provider_configuration::{Configuration, validate},
    routing::provider::ProviderKind,
};
use olp_engine::inference::runtime::Manager;
use utoipa::OpenApi;
use uuid::Uuid;

use olp_db::store::RequestProvenance;

use crate::management::provenance::Provenance;

use super::{
    ApiDoc,
    access::invitations::{
        AcceptInvitationRequest, INVALID_INVITATION_RATE_LIMIT_TARGET, invitation_rate_limit_target,
    },
    auth::{
        INVALID_LOGIN_RATE_LIMIT_TARGET, LoginRequest, SetupRequest, acquire_password_work,
        csrf_recovery_cas_failure_response, installation_name, local_login_rate_limit_target,
        logout, password_work_concurrency, spawn_password_work, validate_setup,
    },
    configuration::{
        api_keys::create::CreateApiKeyResponse, providers::create::CreateProviderRequest,
    },
    cookies::append_session_cookies,
    error_mapping::map_configuration,
    idempotency::{idempotency_http_response, require_idempotency_key},
    openapi::document,
    permissions::require_permission,
    preconditions::if_match,
    response_policy::RuntimeGenerationResponse,
    secrets::WriteOnlySecret,
    sessions::{enforce_origin, session_cookie},
};
use crate::bootstrap::mode_dependencies::ManagementState;

fn state() -> ManagementState {
    ManagementState::new(
        crate::bootstrap::state::ApiMode::Control,
        None,
        Arc::new(Manager::empty()),
        "https://olp.example.test",
        PathBuf::from("console"),
    )
}

fn principal(role: &str) -> SessionPrincipal {
    SessionPrincipal {
        session_id: Uuid::now_v7(),
        user_id: Uuid::now_v7(),
        email: "person@example.test".to_owned(),
        display_name: "Person".to_owned(),
        role: role.to_owned(),
        security_version: 1,
        csrf_digest: vec![0; 32],
        expires_at: Utc::now() + chrono::Duration::hours(1),
    }
}

#[test]
fn setup_validation_returns_field_errors() {
    let problem = validate_setup(&SetupRequest {
        email: "bad".into(),
        password: WriteOnlySecret("short".into()),
        display_name: "".into(),
        installation_name: "".into(),
    })
    .unwrap_err();
    assert_eq!(problem.status, 422);
    assert_eq!(problem.errors.len(), 4);
}

#[test]
fn malformed_public_auth_targets_use_bounded_source_local_sentinels() {
    assert_eq!(
        local_login_rate_limit_target(&"a".repeat(255)),
        INVALID_LOGIN_RATE_LIMIT_TARGET
    );
    assert_eq!(
        local_login_rate_limit_target(" Owner@Example.test "),
        "owner@example.test"
    );
    assert_eq!(
        invitation_rate_limit_target(&"x".repeat(44)),
        INVALID_INVITATION_RATE_LIMIT_TARGET
    );
    assert_eq!(
        invitation_rate_limit_target(&"x".repeat(43)),
        "x".repeat(43)
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn unauthenticated_password_work_remains_bounded_after_request_cancellation() {
    let barrier = Arc::new(std::sync::Barrier::new(2));
    let task_barrier = Arc::clone(&barrier);
    let (started, started_receiver) = std::sync::mpsc::channel();
    let (completed, completed_receiver) = std::sync::mpsc::channel();
    let task = spawn_password_work(move || {
        started.send(()).unwrap();
        task_barrier.wait();
        completed.send(()).unwrap();
    })
    .unwrap();
    started_receiver
        .recv_timeout(std::time::Duration::from_secs(1))
        .unwrap();
    drop(task);

    let permits = (1..password_work_concurrency())
        .map(|_| acquire_password_work().unwrap())
        .collect::<Vec<_>>();
    assert_eq!(acquire_password_work().unwrap_err().status, 429);
    barrier.wait();
    completed_receiver
        .recv_timeout(std::time::Duration::from_secs(1))
        .unwrap();
    drop(permits);
    tokio::time::timeout(std::time::Duration::from_secs(1), async {
        loop {
            if acquire_password_work().is_ok() {
                break;
            }
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
}

#[test]
fn native_provider_create_shape_rejects_custom_and_cloud_fields() {
    let request = CreateProviderRequest {
        name: "native".to_owned(),
        kind: ProviderKind::OpenAi,
        endpoint: Some("https://proxy.example.test/v1".to_owned()),
        cloud_region: Some("region".to_owned()),
        cloud_project: None,
        deployment: None,
        api_version: None,
        auth_mode: Some(ProviderAuthMode::ApplicationDefault),
        credential: Some(WriteOnlySecret("sk-test-secret".to_owned())),
        legacy_api_key: None,
        model: None,
        display_name: None,
    };
    let errors = validate(Configuration {
        kind: request.kind,
        auth_mode: request.auth_mode.unwrap(),
        endpoint: request.endpoint.as_deref(),
        cloud_region: request.cloud_region.as_deref(),
        cloud_project: request.cloud_project.as_deref(),
        deployment: request.deployment.as_deref(),
        api_version: request.api_version.as_deref(),
        model: request.model.as_deref(),
        credential_present: Some(request.credential.is_some()),
    });
    assert!(
        errors
            .iter()
            .any(|error| error.field.as_str() == "endpoint")
    );
    assert!(
        errors
            .iter()
            .any(|error| error.field.as_str() == "cloud_region")
    );
    assert!(
        errors
            .iter()
            .any(|error| error.field.as_str() == "auth_mode")
    );
}

#[test]
fn mutations_require_exact_origin() {
    let state = state();
    let mut headers = HeaderMap::new();
    assert!(enforce_origin(&state.public_origin, &headers).is_err());
    headers.insert(
        header::ORIGIN,
        HeaderValue::from_static("https://evil.test"),
    );
    assert!(enforce_origin(&state.public_origin, &headers).is_err());
    headers.insert(
        header::ORIGIN,
        HeaderValue::from_static("https://olp.example.test"),
    );
    assert!(enforce_origin(&state.public_origin, &headers).is_ok());
}

#[test]
fn cookie_parser_uses_only_host_session_cookie() {
    let mut headers = HeaderMap::new();
    headers.insert(
        header::COOKIE,
        HeaderValue::from_static("other=x; __Host-olp_session=secret; theme=dark"),
    );
    assert_eq!(session_cookie(&headers).unwrap(), "secret");
}

#[test]
fn session_cookie_lifetime_uses_the_configured_ttl() {
    let material = SessionMaterial::generate();
    let mut response = StatusCode::NO_CONTENT.into_response();
    append_session_cookies(&mut response, &material, chrono::Duration::seconds(1_234)).unwrap();
    let cookies = response
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .map(|value| value.to_str().unwrap())
        .collect::<Vec<_>>();
    assert_eq!(cookies.len(), 2);
    assert!(cookies.iter().all(|cookie| cookie.contains("Max-Age=1234")));
}

#[tokio::test]
async fn logout_without_a_server_side_session_still_expires_every_browser_credential() {
    let mut headers = HeaderMap::new();
    headers.insert(
        header::ORIGIN,
        HeaderValue::from_static("https://olp.example.test"),
    );
    headers.insert(
        header::COOKIE,
        HeaderValue::from_static("__Host-olp_session=already-revoked"),
    );
    let response = logout(
        axum::extract::State(state()),
        Provenance(RequestProvenance::default()),
        headers,
    )
    .await
    .unwrap();
    assert_eq!(response.status(), StatusCode::NO_CONTENT);
    let cookies = response
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .map(|value| value.to_str().unwrap())
        .collect::<Vec<_>>();
    assert_eq!(cookies.len(), 3);
    assert!(cookies.iter().all(|cookie| cookie.contains("Max-Age=0")));
    assert!(
        cookies
            .iter()
            .any(|cookie| cookie.starts_with("__Host-olp_session="))
    );
    assert!(
        cookies
            .iter()
            .any(|cookie| cookie.starts_with("__Host-olp_csrf="))
    );
    assert!(
        cookies
            .iter()
            .any(|cookie| cookie.starts_with("__Host-olp_recent_auth="))
    );
}

#[tokio::test]
async fn logout_rejects_conflicting_cookies_but_still_expires_browser_credentials() {
    let mut headers = HeaderMap::new();
    headers.insert(
        header::ORIGIN,
        HeaderValue::from_static("https://olp.example.test"),
    );
    headers.append(
        header::COOKIE,
        HeaderValue::from_static("__Host-olp_session=first"),
    );
    headers.append(
        header::COOKIE,
        HeaderValue::from_static("__Host-olp_session=second"),
    );
    let response = logout(
        axum::extract::State(state()),
        Provenance(RequestProvenance::default()),
        headers,
    )
    .await
    .unwrap();
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    let expired = response
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .filter_map(|value| value.to_str().ok())
        .filter(|value| value.contains("Max-Age=0"))
        .count();
    assert_eq!(expired, 3);
}

#[test]
fn csrf_recovery_failures_never_expire_browser_wide_credentials() {
    for (session_is_current, status) in [
        (true, StatusCode::CONFLICT),
        (false, StatusCode::UNAUTHORIZED),
    ] {
        let response = csrf_recovery_cas_failure_response(session_is_current);

        assert_eq!(response.status(), status);
        assert!(
            response
                .headers()
                .get_all(header::SET_COOKIE)
                .iter()
                .next()
                .is_none()
        );
        assert_eq!(
            response.headers().get(header::CACHE_CONTROL).unwrap(),
            "no-store"
        );
    }
}

#[test]
fn cookie_parser_combines_repeated_fields_and_rejects_conflicts() {
    let mut headers = HeaderMap::new();
    headers.append(
        header::COOKIE,
        HeaderValue::from_static("other=x; malformed pair"),
    );
    headers.append(
        header::COOKIE,
        HeaderValue::from_static("__Host-olp_session=secret"),
    );
    assert_eq!(session_cookie(&headers).unwrap(), "secret");

    headers.append(
        header::COOKIE,
        HeaderValue::from_static("__Host-olp_session=different"),
    );
    assert_eq!(session_cookie(&headers).unwrap_err().status, 400);
}

#[test]
fn idempotency_key_requires_url_safe_header_value() {
    let mut headers = HeaderMap::new();
    assert_eq!(require_idempotency_key(&headers).unwrap_err().status, 400);

    headers.insert("idempotency-key", HeaderValue::from_static("1234567"));
    assert_eq!(require_idempotency_key(&headers).unwrap_err().status, 400);

    headers.insert("idempotency-key", HeaderValue::from_static("12345678"));
    assert_eq!(require_idempotency_key(&headers).unwrap(), "12345678");

    headers.insert(
        "idempotency-key",
        HeaderValue::from_static("contains/slash"),
    );
    assert_eq!(require_idempotency_key(&headers).unwrap_err().status, 400);

    headers.insert(
        "idempotency-key",
        HeaderValue::from_static("provider-create_01.v2"),
    );
    assert_eq!(
        require_idempotency_key(&headers).unwrap(),
        "provider-create_01.v2"
    );
}

#[test]
fn if_match_requires_one_strong_quoted_uuid_etag() {
    let id = Uuid::now_v7();
    let mut headers = HeaderMap::new();
    assert_eq!(if_match(&headers).unwrap_err().status, 428);
    headers.insert(
        header::IF_MATCH,
        HeaderValue::from_str(&format!("\"{id}\"")).unwrap(),
    );
    assert_eq!(if_match(&headers).unwrap(), id);
    headers.insert(
        header::IF_MATCH,
        HeaderValue::from_str(&id.to_string()).unwrap(),
    );
    assert_eq!(if_match(&headers).unwrap_err().status, 400);
    headers.insert(header::IF_MATCH, HeaderValue::from_static("*"));
    assert_eq!(if_match(&headers).unwrap_err().status, 400);
}

#[test]
fn create_draft_openapi_contract_requires_idempotency_and_documents_conflict() {
    let document = document();
    for path in ["/api/v1/providers", "/api/v1/route-drafts"] {
        let post = &document["paths"][path]["post"];
        let parameters = post["parameters"].as_array().unwrap();
        assert!(parameters.iter().any(|parameter| {
            parameter["name"] == "Idempotency-Key"
                && parameter["in"] == "header"
                && parameter["required"] == true
        }));
        assert!(post["responses"].get("409").is_some());
    }
}

#[test]
fn openapi_document_includes_its_public_serving_endpoint() {
    let document = document();
    let get = &document["paths"]["/api/v1/openapi.json"]["get"];
    assert!(
        get["responses"]["200"]["content"]["application/json"]["schema"].is_object(),
        "the successful response must advertise an application/json body"
    );
    assert_eq!(get["security"], serde_json::json!([]));
}

#[test]
fn idempotency_reuse_is_an_rfc9457_conflict() {
    let response = map_configuration(Error::IdempotencyConflict).into_response();
    assert_eq!(response.status(), StatusCode::CONFLICT);
    assert_eq!(
        response.headers().get(header::CONTENT_TYPE).unwrap(),
        "application/problem+json"
    );
}

#[test]
fn replayable_responses_are_never_cacheable() {
    let response = idempotency_http_response(Outcome::<()>::Replayed(
        Response::new(
            StatusCode::CREATED.as_u16(),
            Some("application/json".to_owned()),
            None,
            br#"{"secret":"shown-once"}"#.to_vec(),
        )
        .expect("fixed replay fixture is within response bounds"),
    ))
    .unwrap();
    assert_eq!(
        response.headers().get(header::CACHE_CONTROL).unwrap(),
        "no-store"
    );
    assert_eq!(response.headers().get(header::PRAGMA).unwrap(), "no-cache");
}

#[test]
fn route_guard_delegates_every_role_permission_pair_to_core() {
    for role in Role::ALL {
        let principal = principal(role.as_str());
        for permission in Permission::ALL {
            assert_eq!(
                require_permission(&principal, permission).is_ok(),
                role.allows(permission),
                "HTTP guard diverged for {role}/{permission:?}"
            );
        }
    }
    assert!(require_permission(&principal("unknown"), Permission::ReadOperations).is_err());
}

#[test]
fn identity_and_setup_contracts_use_current_names() {
    let document = serde_json::to_value(ApiDoc::openapi()).unwrap();
    let setup = &document["components"]["schemas"]["SetupRequest"]["properties"];
    assert!(setup.get("installation_name").is_some());
    assert!(setup.get("organization_name").is_none());
    let update = &document["paths"]["/api/v1/users/{user_id}"]["patch"];
    assert!(
        update["parameters"]
            .as_array()
            .unwrap()
            .iter()
            .any(|parameter| {
                parameter["name"] == "If-Match"
                    && parameter["in"] == "header"
                    && parameter["required"] == true
            })
    );
    let create = &document["paths"]["/api/v1/invitations"]["post"];
    assert!(
        create["parameters"]
            .as_array()
            .unwrap()
            .iter()
            .any(|parameter| {
                parameter["name"] == "Idempotency-Key"
                    && parameter["in"] == "header"
                    && parameter["required"] == true
            })
    );
    assert_eq!(
        document["components"]["schemas"]["CreateInvitationResponse"]["properties"]["token"]["readOnly"],
        true
    );
}

#[test]
fn management_dto_debug_output_redacts_plaintext_secrets() {
    let setup = SetupRequest {
        email: "owner@example.test".into(),
        password: WriteOnlySecret("correct horse battery staple".into()),
        display_name: "Owner".into(),
        installation_name: "OLP".into(),
    };
    assert!(!format!("{setup:?}").contains("correct horse"));

    let login = LoginRequest {
        email: "owner@example.test".into(),
        password: WriteOnlySecret("another plaintext password".into()),
    };
    assert!(!format!("{login:?}").contains("another plaintext"));

    let response = CreateApiKeyResponse {
        id: Uuid::now_v7(),
        lookup_id: "olp_lookup".into(),
        secret: WriteOnlySecret("olp_secret_once".into()),
        runtime_generation: RuntimeGenerationResponse {
            id: Uuid::now_v7(),
            sequence: 1,
        },
    };
    assert!(!format!("{response:?}").contains("olp_secret_once"));

    let acceptance = AcceptInvitationRequest {
        token: WriteOnlySecret("sensitive-invitation-token".into()),
        display_name: "Invited person".into(),
        password: WriteOnlySecret("sensitive-local-password".into()),
    };
    let output = format!("{acceptance:?}");
    assert!(!output.contains("sensitive-invitation-token"));
    assert!(!output.contains("sensitive-local-password"));
}

/// Every authenticated response carries the installation name, so the helper
/// treats a missing installation row as a broken invariant rather than an
/// absent optional value. Only a database can produce that state, so the arm
/// is exercised against a migrated but unseeded schema.
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn installation_name_fails_when_the_installation_row_is_missing() {
    let db = olp_db::test_support::TestDb::create_migrated("instname").await;
    let store = db.store(1).await;
    assert!(store.installation_name().await.unwrap().is_none());

    let problem = installation_name(&store).await.unwrap_err();
    assert_eq!(problem.status, StatusCode::INTERNAL_SERVER_ERROR.as_u16());
    assert_eq!(
        problem.problem_type.as_ref(),
        "https://openllmproxy.dev/problems/internal_error"
    );
}

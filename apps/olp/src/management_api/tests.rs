use std::{path::PathBuf, sync::Arc};

use axum::{
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::IntoResponse,
};
use chrono::Utc;
use olp_domain::{Permission, Role};
use olp_storage::{
    ConfigurationError, IdempotencyOutcome, IdempotencyResponse, SessionMaterial, SessionPrincipal,
};
use utoipa::OpenApi;
use uuid::Uuid;

use super::{
    ManagementApiDoc,
    access::{
        AcceptInvitationRequest, INVALID_INVITATION_RATE_LIMIT_TARGET, invitation_rate_limit_target,
    },
    auth::{
        INVALID_LOGIN_RATE_LIMIT_TARGET, LoginRequest, PASSWORD_WORK_CONCURRENCY, SetupRequest,
        acquire_password_work, csrf_recovery_cas_failure_response, local_login_rate_limit_target,
        logout, spawn_password_work, validate_setup,
    },
    common::{
        RuntimeGenerationResponse, WriteOnlySecret, append_session_cookies, enforce_origin,
        idempotency_http_response, if_match, map_configuration, require_idempotency_key,
        require_permission, session_cookie,
    },
    configuration::{
        api_keys::CreateApiKeyResponse,
        providers::{
            CreateProviderRequest, reject_create_cloud_fields, reject_create_field,
            require_create_auth_mode,
        },
    },
    management_openapi,
};
use crate::{ApiState, FieldErrors};

fn state() -> ApiState {
    ApiState::new(
        crate::ApiMode::Control,
        None,
        Arc::new(crate::RuntimeManager::empty()),
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

    let permits = (1..PASSWORD_WORK_CONCURRENCY)
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
        kind: "openai".to_owned(),
        endpoint: Some("https://proxy.example.test/v1".to_owned()),
        cloud_region: Some("region".to_owned()),
        cloud_project: None,
        deployment: None,
        api_version: None,
        auth_mode: Some("custom".to_owned()),
        credential: Some(WriteOnlySecret("sk-test-secret".to_owned())),
        legacy_api_key: None,
        model: None,
        display_name: None,
    };
    let mut errors = FieldErrors::new();
    require_create_auth_mode(
        &mut errors,
        request.auth_mode.as_deref().unwrap(),
        "api_key",
    );
    reject_create_field(
        &mut errors,
        "endpoint",
        request.endpoint.is_some(),
        "Native OpenAI uses the official endpoint.",
    );
    reject_create_cloud_fields(&mut errors, &request);
    assert!(errors.contains_key("endpoint"));
    assert!(errors.contains_key("cloud_region"));
    assert!(errors.contains_key("auth_mode"));
}

#[test]
fn mutations_require_exact_origin() {
    let state = state();
    let mut headers = HeaderMap::new();
    assert!(enforce_origin(&state, &headers).is_err());
    headers.insert(
        header::ORIGIN,
        HeaderValue::from_static("https://evil.test"),
    );
    assert!(enforce_origin(&state, &headers).is_err());
    headers.insert(
        header::ORIGIN,
        HeaderValue::from_static("https://olp.example.test"),
    );
    assert!(enforce_origin(&state, &headers).is_ok());
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
    let response = logout(axum::extract::State(state()), headers)
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

#[test]
fn concurrent_csrf_recovery_does_not_expire_a_still_valid_session() {
    let response = csrf_recovery_cas_failure_response(true);

    assert_eq!(response.status(), StatusCode::CONFLICT);
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
    let document = management_openapi();
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
fn idempotency_reuse_is_an_rfc9457_conflict() {
    let response = map_configuration(ConfigurationError::IdempotencyConflict).into_response();
    assert_eq!(response.status(), StatusCode::CONFLICT);
    assert_eq!(
        response.headers().get(header::CONTENT_TYPE).unwrap(),
        "application/problem+json"
    );
}

#[test]
fn replayable_responses_are_never_cacheable() {
    let response = idempotency_http_response(IdempotencyOutcome::<()>::Replayed(
        IdempotencyResponse::new(
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
    let document = serde_json::to_value(ManagementApiDoc::openapi()).unwrap();
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

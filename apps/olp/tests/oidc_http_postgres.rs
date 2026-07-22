use std::{collections::BTreeMap, path::PathBuf, sync::Arc};

use axum::{
    Form, Json, Router,
    body::Body,
    extract::State,
    http::{HeaderMap, Method, Request, Response, StatusCode, header},
    routing::{get, post},
};
use base64::{
    Engine as _,
    engine::general_purpose::{STANDARD, URL_SAFE_NO_PAD},
};
use chrono::Utc;
use http_body_util::BodyExt as _;
use jsonwebtoken::{Algorithm, EncodingKey, Header, encode};
use olp::{ApiMode, ApiState, RuntimeManager, public_router};
use olp_storage::{MasterKey, PgStore};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use tokio::{net::TcpListener, sync::Mutex};
use tower::ServiceExt as _;
use url::Url;
use uuid::Uuid;

mod common;
use common::{BOOTSTRAP_TOKEN, configure_bootstrap};

const ORIGIN: &str = "https://olp.example.test";
const CLIENT_ID: &str = "olp-client";
const CLIENT_SECRET: &str = "mock-client-secret";
// Public, test-only Ed25519 fixture. It is generated solely for the in-process
// mock IdP and is never used by runtime code.
const ED25519_PRIVATE_DER_B64: &str =
    "MC4CAQAwBQYDK2VwBCIEIBrf5enAkeYcV99WmDtSpbEHFio5SdSot7TRRtzNDW11";
const ED25519_PUBLIC_X: &str = "WOts4ZqTyrsFm_sqwXTJZQngsj3-LQRk-4kz9WFJaYc";

#[derive(Clone)]
struct MockIdentity {
    subject: String,
    email: String,
    name: String,
    groups: Vec<String>,
}

struct ExpectedAuthorization {
    nonce: String,
    challenge: String,
    wrong_nonce: bool,
}

struct MockInner {
    identity: MockIdentity,
    expected: Option<ExpectedAuthorization>,
    pkce_verified: bool,
}

#[derive(Clone)]
struct MockIdp {
    issuer: String,
    encoding_key: Arc<EncodingKey>,
    public_x: String,
    inner: Arc<Mutex<MockInner>>,
}

#[tokio::test]
#[ignore = "requires an empty PostgreSQL database in OLP_TEST_DATABASE_URL"]
async fn oidc_code_flow_is_bound_validated_mapped_linked_and_session_backed() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty test database");
    let store = PgStore::connect(&database_url, 8).await.unwrap();
    store.migrate().await.unwrap();
    let (idp, _idp_task) = spawn_mock_idp().await;

    let mut api_state = ApiState::new(
        ApiMode::Control,
        Some(store.clone()),
        Arc::new(RuntimeManager::empty()),
        ORIGIN,
        PathBuf::from("missing-console-for-oidc-test"),
    );
    api_state.master_key = Some(Arc::new(MasterKey::new(1, [42_u8; 32])));
    configure_bootstrap(&mut api_state, [43_u8; 32]);
    api_state.oidc_allow_insecure_test_endpoints = true;
    let app = public_router(api_state);

    let setup = send_json(
        &app,
        Method::POST,
        "/api/v1/setup",
        json!({
            "email": "owner@example.test",
            "password": "correct horse battery staple",
            "display_name": "Local Owner",
            "installation_name": "OIDC integration"
        }),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(setup.status(), StatusCode::CREATED);
    let mut owner_cookies = cookie_header(&setup);
    let setup_body = response_json(setup).await;
    let mut owner_csrf = setup_body["csrf_token"].as_str().unwrap().to_owned();
    let owner_id = setup_body["user"]["id"].as_str().unwrap().to_owned();

    let configured = send_json(
        &app,
        Method::PUT,
        "/api/v1/oidc/configuration",
        json!({
            "discovery_url": format!("{}/.well-known/openid-configuration", idp.issuer),
            "issuer": idp.issuer,
            "client_id": CLIENT_ID,
            "client_secret": CLIENT_SECRET,
            "enabled": true,
            "scopes": ["openid", "email", "profile", "groups"],
            "group_role_mappings": [{"claim_value": "engineering", "role": "developer"}]
        }),
        Some(&owner_cookies),
        Some(&owner_csrf),
        None,
    )
    .await;
    assert_eq!(configured.status(), StatusCode::CREATED);

    let redacted = send_empty(
        &app,
        Method::GET,
        "/api/v1/oidc/configuration",
        Some(&owner_cookies),
        None,
    )
    .await;
    assert_eq!(redacted.status(), StatusCode::OK);
    let configuration_etag = redacted
        .headers()
        .get(header::ETAG)
        .unwrap()
        .to_str()
        .unwrap()
        .to_owned();
    let redacted_text = response_text(redacted).await;
    assert!(redacted_text.contains("\"has_client_secret\":true"));
    assert!(!redacted_text.contains(CLIENT_SECRET));
    let preserved_secret = send_json(
        &app,
        Method::PUT,
        "/api/v1/oidc/configuration",
        json!({
            "discovery_url": format!("{}/.well-known/openid-configuration", idp.issuer),
            "issuer": idp.issuer,
            "client_id": CLIENT_ID,
            "enabled": true,
            "scopes": ["openid", "email", "profile", "groups"],
            "group_role_mappings": [{"claim_value": "engineering", "role": "developer"}]
        }),
        Some(&owner_cookies),
        Some(&owner_csrf),
        Some(&configuration_etag),
    )
    .await;
    assert_eq!(preserved_secret.status(), StatusCode::OK);
    assert!(
        !response_text(preserved_secret)
            .await
            .contains(CLIENT_SECRET)
    );

    // Model the narrow consume/update interleaving where configuration
    // invalidation no longer finds the already-consumed row. The encrypted
    // flow material must still reject the changed configuration ETag before
    // any token exchange.
    let active_configuration_etag: Uuid =
        sqlx::query_scalar("SELECT etag FROM oidc_configurations WHERE singleton")
            .fetch_one(store.pool())
            .await
            .unwrap();
    let stale_flow = begin_login(&app).await;
    sqlx::query("UPDATE oidc_configurations SET etag = $1 WHERE singleton")
        .bind(Uuid::now_v7())
        .execute(store.pool())
        .await
        .unwrap();
    let stale_callback =
        callback_request(&app, &stale_flow.state, &stale_flow.flow_cookie, None).await;
    assert_eq!(stale_callback.status(), StatusCode::BAD_REQUEST);
    assert_eq!(
        response_json(stale_callback).await["type"],
        "https://openllmproxy.dev/problems/oidc_flow_stale"
    );
    sqlx::query("UPDATE oidc_configurations SET etag = $1 WHERE singleton")
        .bind(active_configuration_etag)
        .execute(store.pool())
        .await
        .unwrap();

    // Browser state binding rejects the wrong cookie without consuming the
    // legitimate one-time state.
    let first_flow = begin_login(&app).await;
    let login_flow_rows: i64 =
        sqlx::query_scalar("SELECT count(*) FROM oidc_authorization_flows WHERE purpose = 'login'")
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(login_flow_rows, 0, "new login starts must be stateless");
    let nonce = query_value(&first_flow.authorization_url, "nonce");
    assert!(!first_flow.flow_cookie.contains(&nonce));
    arm_idp(&idp, &first_flow.authorization_url, false).await;
    let wrong_binding = callback_request(
        &app,
        &first_flow.state,
        "0000000000000000000000000000000000000000000",
        None,
    )
    .await;
    assert_eq!(wrong_binding.status(), StatusCode::BAD_REQUEST);

    // A correctly signed token with the wrong nonce is still rejected.
    arm_idp(&idp, &first_flow.authorization_url, true).await;
    let wrong_nonce =
        callback_request(&app, &first_flow.state, &first_flow.flow_cookie, None).await;
    assert_eq!(wrong_nonce.status(), StatusCode::UNAUTHORIZED);
    arm_idp(&idp, &first_flow.authorization_url, false).await;
    let failed_callback_replay =
        callback_request(&app, &first_flow.state, &first_flow.flow_cookie, None).await;
    assert_eq!(failed_callback_replay.status(), StatusCode::BAD_REQUEST);
    assert_eq!(
        response_json(failed_callback_replay).await["type"],
        "https://openllmproxy.dev/problems/oidc_flow_unavailable"
    );
    assert!(
        idp.inner.lock().await.expected.is_some(),
        "a consumed login flow must be rejected before another token exchange"
    );

    // A new flow proves S256 at the mock token endpoint, provisions from the
    // asserted group, and issues opaque local session/CSRF cookies.
    let login_flow = begin_login(&app).await;
    arm_idp(&idp, &login_flow.authorization_url, false).await;
    let login = callback_request(&app, &login_flow.state, &login_flow.flow_cookie, None).await;
    assert_eq!(login.status(), StatusCode::SEE_OTHER);
    let mut developer_cookies = cookie_header(&login);
    assert!(idp.inner.lock().await.pkce_verified);
    arm_idp(&idp, &login_flow.authorization_url, false).await;
    let successful_callback_replay =
        callback_request(&app, &login_flow.state, &login_flow.flow_cookie, None).await;
    assert_eq!(successful_callback_replay.status(), StatusCode::BAD_REQUEST);
    assert_eq!(
        response_json(successful_callback_replay).await["type"],
        "https://openllmproxy.dev/problems/oidc_flow_unavailable"
    );
    assert!(
        idp.inner.lock().await.expected.is_some(),
        "replaying an authorization URL must not perform another token exchange"
    );
    let current = send_empty(
        &app,
        Method::GET,
        "/api/v1/sessions/current",
        Some(&developer_cookies),
        None,
    )
    .await;
    assert_eq!(current.status(), StatusCode::OK);
    let current_body = response_json(current).await;
    assert_eq!(current_body["user"]["email"], "developer@example.test");
    assert_eq!(current_body["user"]["role"], "developer");
    assert!(current_body["csrf_token"].as_str().unwrap().len() >= 40);
    let mut developer_csrf = current_body["csrf_token"].as_str().unwrap().to_owned();
    let developer_identities = send_empty(
        &app,
        Method::GET,
        "/api/v1/oidc/identities",
        Some(&developer_cookies),
        None,
    )
    .await;
    assert_eq!(developer_identities.status(), StatusCode::OK);
    let developer_identity = response_json(developer_identities).await;
    assert_eq!(developer_identity["data"].as_array().unwrap().len(), 1);
    assert_eq!(developer_identity["data"][0]["can_unlink"], false);
    let developer_identity_id = developer_identity["data"][0]["id"]
        .as_str()
        .unwrap()
        .to_owned();
    let blocked_unlink_reauthentication = begin_oidc_reauthentication(
        &app,
        &developer_cookies,
        &developer_csrf,
        "oidc_unlink",
        Some(&developer_identity_id),
    )
    .await;
    arm_idp(
        &idp,
        &blocked_unlink_reauthentication.authorization_url,
        false,
    )
    .await;
    let blocked_unlink_grant = callback_request(
        &app,
        &blocked_unlink_reauthentication.state,
        &blocked_unlink_reauthentication.flow_cookie,
        Some(&developer_cookies),
    )
    .await;
    assert_eq!(blocked_unlink_grant.status(), StatusCode::SEE_OTHER);
    developer_cookies = apply_response_cookies(&developer_cookies, &blocked_unlink_grant);
    let blocked_unlink = send_empty_with_origin(
        &app,
        Method::DELETE,
        &format!("/api/v1/oidc/identities/{developer_identity_id}"),
        Some(&developer_cookies),
        Some(&developer_csrf),
    )
    .await;
    assert_eq!(blocked_unlink.status(), StatusCode::CONFLICT);

    // Existing-password changes remain unavailable to an OIDC-only account.
    // A separate CSRF/origin/ETag-protected enrollment operation atomically
    // adds the first local password, after which the final OIDC identity can
    // be removed without stranding the user.
    let developer_profile = send_empty(
        &app,
        Method::GET,
        "/api/v1/profile",
        Some(&developer_cookies),
        None,
    )
    .await;
    assert_eq!(developer_profile.status(), StatusCode::OK);
    let developer_profile_etag = developer_profile.headers()[header::ETAG]
        .to_str()
        .unwrap()
        .to_owned();
    let ordinary_change = send_json(
        &app,
        Method::POST,
        "/api/v1/profile/password",
        json!({
            "current_password": "not-a-local-password",
            "new_password": "replacement local password"
        }),
        Some(&developer_cookies),
        Some(&developer_csrf),
        Some(&developer_profile_etag),
    )
    .await;
    assert_eq!(ordinary_change.status(), StatusCode::FORBIDDEN);

    let enrollment_reauthentication = begin_oidc_reauthentication(
        &app,
        &developer_cookies,
        &developer_csrf,
        "password_enrollment",
        None,
    )
    .await;
    arm_idp(&idp, &enrollment_reauthentication.authorization_url, false).await;
    let enrollment_grant = callback_request(
        &app,
        &enrollment_reauthentication.state,
        &enrollment_reauthentication.flow_cookie,
        Some(&developer_cookies),
    )
    .await;
    assert_eq!(enrollment_grant.status(), StatusCode::SEE_OTHER);
    developer_cookies = apply_response_cookies(&developer_cookies, &enrollment_grant);
    let enrolled = send_json(
        &app,
        Method::POST,
        "/api/v1/profile/password/enroll",
        json!({"new_password": "developer enrolled local password"}),
        Some(&developer_cookies),
        Some(&developer_csrf),
        Some(&developer_profile_etag),
    )
    .await;
    assert_eq!(enrolled.status(), StatusCode::OK);
    let enrolled_etag = enrolled.headers()[header::ETAG]
        .to_str()
        .unwrap()
        .to_owned();
    developer_csrf = enrolled
        .headers()
        .get("x-csrf-token")
        .unwrap()
        .to_str()
        .unwrap()
        .to_owned();
    developer_cookies = apply_response_cookies(&developer_cookies, &enrolled);

    let duplicate_reauthentication = send_json(
        &app,
        Method::POST,
        "/api/v1/profile/reauthenticate",
        json!({
            "current_password": "developer enrolled local password",
            "purpose": "password_enrollment"
        }),
        Some(&developer_cookies),
        Some(&developer_csrf),
        None,
    )
    .await;
    assert_eq!(duplicate_reauthentication.status(), StatusCode::NO_CONTENT);
    developer_cookies = apply_response_cookies(&developer_cookies, &duplicate_reauthentication);
    let duplicate_enrollment = send_json(
        &app,
        Method::POST,
        "/api/v1/profile/password/enroll",
        json!({"new_password": "another enrolled local password"}),
        Some(&developer_cookies),
        Some(&developer_csrf),
        Some(&enrolled_etag),
    )
    .await;
    assert_eq!(duplicate_enrollment.status(), StatusCode::CONFLICT);
    let developer_identities = send_empty(
        &app,
        Method::GET,
        "/api/v1/oidc/identities",
        Some(&developer_cookies),
        None,
    )
    .await;
    let developer_identity = response_json(developer_identities).await;
    assert_eq!(developer_identity["data"][0]["can_unlink"], true);
    let developer_unlink_reauthentication = send_json(
        &app,
        Method::POST,
        "/api/v1/profile/reauthenticate",
        json!({
            "current_password": "developer enrolled local password",
            "purpose": "oidc_unlink",
            "resource_id": developer_identity_id
        }),
        Some(&developer_cookies),
        Some(&developer_csrf),
        None,
    )
    .await;
    assert_eq!(
        developer_unlink_reauthentication.status(),
        StatusCode::NO_CONTENT
    );
    developer_cookies =
        apply_response_cookies(&developer_cookies, &developer_unlink_reauthentication);
    let developer_unlinked = send_empty_with_origin(
        &app,
        Method::DELETE,
        &format!("/api/v1/oidc/identities/{developer_identity_id}"),
        Some(&developer_cookies),
        Some(&developer_csrf),
    )
    .await;
    assert_eq!(developer_unlinked.status(), StatusCode::NO_CONTENT);
    assert!(developer_unlinked.headers().contains_key("x-csrf-token"));
    let local_login = send_json(
        &app,
        Method::POST,
        "/api/v1/sessions",
        json!({
            "email": "developer@example.test",
            "password": "developer enrolled local password"
        }),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(local_login.status(), StatusCode::CREATED);

    let replay = callback_request(&app, &login_flow.state, &login_flow.flow_cookie, None).await;
    assert_eq!(replay.status(), StatusCode::BAD_REQUEST);

    // Switching the IdP identity to the owner's email cannot silently link by
    // email. Ordinary login reports the explicit-link requirement.
    {
        let mut inner = idp.inner.lock().await;
        inner.identity = MockIdentity {
            subject: "idp-owner-subject".to_owned(),
            email: "owner@example.test".to_owned(),
            name: "OIDC Owner".to_owned(),
            groups: vec!["engineering".to_owned()],
        };
    }
    let collision_flow = begin_login(&app).await;
    arm_idp(&idp, &collision_flow.authorization_url, false).await;
    let collision = callback_request(
        &app,
        &collision_flow.state,
        &collision_flow.flow_cookie,
        None,
    )
    .await;
    assert_eq!(collision.status(), StatusCode::CONFLICT);

    // Link initiation is a session mutation and therefore requires Origin,
    // CSRF, and a purpose-bound recent-authentication grant. A valid owner
    // request binds the callback to that same local user.
    let missing_csrf = send_empty(
        &app,
        Method::POST,
        "/api/v1/oidc/link",
        Some(&owner_cookies),
        None,
    )
    .await;
    assert_eq!(missing_csrf.status(), StatusCode::FORBIDDEN);
    let missing_recent_auth = send_empty_with_origin(
        &app,
        Method::POST,
        "/api/v1/oidc/link",
        Some(&owner_cookies),
        Some(&owner_csrf),
    )
    .await;
    assert_eq!(
        missing_recent_auth.status(),
        StatusCode::PRECONDITION_REQUIRED
    );
    let owner_link_reauthentication = send_json(
        &app,
        Method::POST,
        "/api/v1/profile/reauthenticate",
        json!({
            "current_password": "correct horse battery staple",
            "purpose": "oidc_link"
        }),
        Some(&owner_cookies),
        Some(&owner_csrf),
        None,
    )
    .await;
    assert_eq!(owner_link_reauthentication.status(), StatusCode::NO_CONTENT);
    owner_cookies = apply_response_cookies(&owner_cookies, &owner_link_reauthentication);
    let abandoned_login = begin_login(&app).await;
    let link = send_empty_with_origin(
        &app,
        Method::POST,
        "/api/v1/oidc/link",
        Some(&owner_cookies),
        Some(&owner_csrf),
    )
    .await;
    assert_eq!(link.status(), StatusCode::OK);
    assert_host_cookie_contract(&link, "__Host-olp_oidc_flow");
    let link_cookie = named_cookie(&link, "__Host-olp_oidc_flow");
    let link_body = response_json(link).await;
    let link_url = link_body["authorization_url"].as_str().unwrap().to_owned();
    let link_state = query_value(&link_url, "state");
    arm_idp(&idp, &link_url, false).await;
    let cookies_with_abandoned_login = format!(
        "{owner_cookies}; __Host-olp_oidc_login_flow={}",
        abandoned_login.flow_cookie
    );
    let linked = callback_request(
        &app,
        &link_state,
        &link_cookie,
        Some(&cookies_with_abandoned_login),
    )
    .await;
    assert_eq!(linked.status(), StatusCode::SEE_OTHER);
    assert_host_cookie_contract(&linked, "__Host-olp_oidc_flow");
    assert_host_cookie_contract(&linked, "__Host-olp_oidc_login_flow");
    owner_csrf = linked
        .headers()
        .get("x-csrf-token")
        .unwrap()
        .to_str()
        .unwrap()
        .to_owned();
    owner_cookies = apply_response_cookies(&owner_cookies, &linked);

    let linked_user: String = sqlx::query_scalar(
        "SELECT user_id::text FROM oidc_identities WHERE issuer = $1 AND subject = 'idp-owner-subject'",
    )
    .bind(&idp.issuer)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(linked_user, owner_id);
    let owner_identities = send_empty(
        &app,
        Method::GET,
        "/api/v1/oidc/identities",
        Some(&owner_cookies),
        None,
    )
    .await;
    assert_eq!(owner_identities.status(), StatusCode::OK);
    let owner_identity_body = response_json(owner_identities).await;
    assert_eq!(owner_identity_body["data"].as_array().unwrap().len(), 1);
    assert_eq!(owner_identity_body["data"][0]["can_unlink"], true);
    assert_eq!(owner_identity_body["linking_available"], true);
    let owner_identity_id = owner_identity_body["data"][0]["id"].as_str().unwrap();
    let owner_unlink_reauthentication = send_json(
        &app,
        Method::POST,
        "/api/v1/profile/reauthenticate",
        json!({
            "current_password": "correct horse battery staple",
            "purpose": "oidc_unlink",
            "resource_id": owner_identity_id
        }),
        Some(&owner_cookies),
        Some(&owner_csrf),
        None,
    )
    .await;
    assert_eq!(
        owner_unlink_reauthentication.status(),
        StatusCode::NO_CONTENT
    );
    owner_cookies = apply_response_cookies(&owner_cookies, &owner_unlink_reauthentication);
    let unlinked = send_empty_with_origin(
        &app,
        Method::DELETE,
        &format!("/api/v1/oidc/identities/{owner_identity_id}"),
        Some(&owner_cookies),
        Some(&owner_csrf),
    )
    .await;
    assert_eq!(unlinked.status(), StatusCode::NO_CONTENT);
    assert!(unlinked.headers().contains_key("x-csrf-token"));
    let owner_uuid = Uuid::parse_str(&owner_id).unwrap();
    let remaining: i64 =
        sqlx::query_scalar("SELECT count(*) FROM oidc_identities WHERE user_id = $1")
            .bind(owner_uuid)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(remaining, 0);
    let unlink_audit: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM audit_events WHERE action = 'oidc.identity_unlink' AND actor_user_id = $1",
    )
    .bind(owner_uuid)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(unlink_audit, 1);
    let flow_rows: i64 = sqlx::query_scalar("SELECT count(*) FROM oidc_authorization_flows")
        .fetch_one(store.pool())
        .await
        .unwrap();
    assert_eq!(flow_rows, 0);

    // Repeated anonymous starts retain no login-flow rows. Source-aware
    // admission is exercised separately with distinct peer identities.
    for _ in 0..2 {
        let flow = begin_login(&app).await;
        assert!(flow.flow_cookie.starts_with("v2."));
    }
    let login_flow_rows: i64 =
        sqlx::query_scalar("SELECT count(*) FROM oidc_authorization_flows WHERE purpose = 'login'")
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(login_flow_rows, 0);
}

struct BrowserFlow {
    authorization_url: String,
    state: String,
    flow_cookie: String,
}

async fn begin_login(app: &Router) -> BrowserFlow {
    let response = send_empty(app, Method::GET, "/api/v1/oidc/login", None, None).await;
    assert_eq!(response.status(), StatusCode::SEE_OTHER);
    assert_host_cookie_contract(&response, "__Host-olp_oidc_login_flow");
    let authorization_url = response
        .headers()
        .get(header::LOCATION)
        .unwrap()
        .to_str()
        .unwrap()
        .to_owned();
    let flow_cookie = named_cookie(&response, "__Host-olp_oidc_login_flow");
    let state = query_value(&authorization_url, "state");
    assert_eq!(
        query_value(&authorization_url, "code_challenge_method"),
        "S256"
    );
    BrowserFlow {
        authorization_url,
        state,
        flow_cookie,
    }
}

async fn begin_oidc_reauthentication(
    app: &Router,
    cookies: &str,
    csrf: &str,
    purpose: &str,
    resource_id: Option<&str>,
) -> BrowserFlow {
    let body = match resource_id {
        Some(resource_id) => json!({"purpose": purpose, "resource_id": resource_id}),
        None => json!({"purpose": purpose}),
    };
    let response = send_json(
        app,
        Method::POST,
        "/api/v1/oidc/reauthenticate",
        body,
        Some(cookies),
        Some(csrf),
        None,
    )
    .await;
    assert_eq!(response.status(), StatusCode::OK);
    assert_host_cookie_contract(&response, "__Host-olp_oidc_flow");
    let flow_cookie = named_cookie(&response, "__Host-olp_oidc_flow");
    let authorization_url = response_json(response).await["authorization_url"]
        .as_str()
        .unwrap()
        .to_owned();
    let state = query_value(&authorization_url, "state");
    BrowserFlow {
        authorization_url,
        state,
        flow_cookie,
    }
}

async fn arm_idp(idp: &MockIdp, authorization_url: &str, wrong_nonce: bool) {
    let mut inner = idp.inner.lock().await;
    inner.expected = Some(ExpectedAuthorization {
        nonce: query_value(authorization_url, "nonce"),
        challenge: query_value(authorization_url, "code_challenge"),
        wrong_nonce,
    });
    inner.pkce_verified = false;
}

async fn spawn_mock_idp() -> (MockIdp, tokio::task::JoinHandle<()>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let issuer = format!("http://{}", listener.local_addr().unwrap());
    let private_der = STANDARD.decode(ED25519_PRIVATE_DER_B64).unwrap();
    let idp = MockIdp {
        issuer,
        encoding_key: Arc::new(EncodingKey::from_ed_der(&private_der)),
        public_x: ED25519_PUBLIC_X.to_owned(),
        inner: Arc::new(Mutex::new(MockInner {
            identity: MockIdentity {
                subject: "jit-developer-subject".to_owned(),
                email: "developer@example.test".to_owned(),
                name: "OIDC Developer".to_owned(),
                groups: vec!["engineering".to_owned()],
            },
            expected: None,
            pkce_verified: false,
        })),
    };
    let app = Router::new()
        .route("/.well-known/openid-configuration", get(mock_discovery))
        .route("/jwks", get(mock_jwks))
        .route("/authorize", get(|| async { StatusCode::NO_CONTENT }))
        .route("/token", post(mock_token))
        .with_state(idp.clone());
    let task = tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    (idp, task)
}

async fn mock_discovery(State(idp): State<MockIdp>) -> Json<Value> {
    Json(json!({
        "issuer": idp.issuer,
        "authorization_endpoint": format!("{}/authorize", idp.issuer),
        "token_endpoint": format!("{}/token", idp.issuer),
        "jwks_uri": format!("{}/jwks", idp.issuer),
        "response_types_supported": ["code"],
        "code_challenge_methods_supported": ["S256"],
        "token_endpoint_auth_methods_supported": ["client_secret_basic"],
        "id_token_signing_alg_values_supported": ["EdDSA"]
    }))
}

async fn mock_jwks(State(idp): State<MockIdp>) -> Json<Value> {
    Json(json!({"keys": [{
        "kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA", "kid": "mock-key",
        "x": idp.public_x
    }]}))
}

async fn mock_token(
    State(idp): State<MockIdp>,
    headers: HeaderMap,
    Form(form): Form<BTreeMap<String, String>>,
) -> Result<Json<Value>, StatusCode> {
    let expected_basic = format!(
        "Basic {}",
        STANDARD.encode(format!("{CLIENT_ID}:{CLIENT_SECRET}"))
    );
    if headers
        .get(header::AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        != Some(expected_basic.as_str())
        || form.get("grant_type").map(String::as_str) != Some("authorization_code")
        || form.get("code").map(String::as_str) != Some("mock-code")
        || form.get("client_id").map(String::as_str) != Some(CLIENT_ID)
    {
        return Err(StatusCode::UNAUTHORIZED);
    }
    let verifier = form.get("code_verifier").ok_or(StatusCode::BAD_REQUEST)?;
    let challenge = URL_SAFE_NO_PAD.encode(Sha256::digest(verifier.as_bytes()));
    let mut inner = idp.inner.lock().await;
    let expected = inner.expected.take().ok_or(StatusCode::BAD_REQUEST)?;
    if challenge != expected.challenge {
        return Err(StatusCode::BAD_REQUEST);
    }
    inner.pkce_verified = true;
    let nonce = if expected.wrong_nonce {
        "wrong-nonce".to_owned()
    } else {
        expected.nonce
    };
    let identity = inner.identity.clone();
    drop(inner);
    let mut header = Header::new(Algorithm::EdDSA);
    header.kid = Some("mock-key".to_owned());
    let now = Utc::now().timestamp();
    let id_token = encode(
        &header,
        &json!({
            "iss": idp.issuer,
            "sub": identity.subject,
            "aud": CLIENT_ID,
            "iat": now,
            "exp": now + 300,
            "nonce": nonce,
            "email": identity.email,
            "email_verified": true,
            "name": identity.name,
            "groups": identity.groups
        }),
        &idp.encoding_key,
    )
    .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;
    Ok(Json(
        json!({"id_token": id_token, "access_token": "never-persist-this", "token_type": "Bearer"}),
    ))
}

async fn callback_request(
    app: &Router,
    state: &str,
    flow_cookie: &str,
    session_cookies: Option<&str>,
) -> Response<Body> {
    let cookie_name = if flow_cookie.starts_with("v2.") {
        "__Host-olp_oidc_login_flow"
    } else {
        "__Host-olp_oidc_flow"
    };
    let cookies = session_cookies.map_or_else(
        || format!("{cookie_name}={flow_cookie}"),
        |session| format!("{session}; {cookie_name}={flow_cookie}"),
    );
    let uri = format!("/api/v1/oidc/callback?code=mock-code&state={state}");
    let request = Request::builder()
        .method(Method::GET)
        .uri(uri)
        .header(header::COOKIE, cookies)
        .body(Body::empty())
        .unwrap();
    app.clone().oneshot(request).await.unwrap()
}

async fn send_json(
    app: &Router,
    method: Method,
    uri: &str,
    body: Value,
    cookies: Option<&str>,
    csrf: Option<&str>,
    if_match: Option<&str>,
) -> Response<Body> {
    request(app, method, uri, Some(body), cookies, csrf, if_match, true).await
}

async fn send_empty(
    app: &Router,
    method: Method,
    uri: &str,
    cookies: Option<&str>,
    csrf: Option<&str>,
) -> Response<Body> {
    request(app, method, uri, None, cookies, csrf, None, false).await
}

async fn send_empty_with_origin(
    app: &Router,
    method: Method,
    uri: &str,
    cookies: Option<&str>,
    csrf: Option<&str>,
) -> Response<Body> {
    request(app, method, uri, None, cookies, csrf, None, true).await
}

#[allow(clippy::too_many_arguments)]
async fn request(
    app: &Router,
    method: Method,
    uri: &str,
    body: Option<Value>,
    cookies: Option<&str>,
    csrf: Option<&str>,
    if_match: Option<&str>,
    origin: bool,
) -> Response<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if origin {
        builder = builder.header(header::ORIGIN, ORIGIN);
    }
    if let Some(cookies) = cookies {
        builder = builder.header(header::COOKIE, cookies);
    }
    if let Some(csrf) = csrf {
        builder = builder.header("x-csrf-token", csrf);
    }
    if let Some(if_match) = if_match {
        builder = builder.header(header::IF_MATCH, if_match);
    }
    if uri == "/api/v1/setup" {
        builder = builder.header("x-olp-setup-token", BOOTSTRAP_TOKEN);
    }
    let body = if let Some(value) = body {
        builder = builder.header(header::CONTENT_TYPE, "application/json");
        Body::from(value.to_string())
    } else {
        Body::empty()
    };
    let mut request = builder.body(body).unwrap();
    request.extensions_mut().insert(axum::extract::ConnectInfo(
        "198.51.100.13:443".parse::<std::net::SocketAddr>().unwrap(),
    ));
    app.clone().oneshot(request).await.unwrap()
}

fn cookie_header(response: &Response<Body>) -> String {
    response
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .map(|cookie| cookie.to_str().unwrap().split(';').next().unwrap())
        .filter(|cookie| {
            !cookie.starts_with("__Host-olp_oidc_flow=")
                && !cookie.starts_with("__Host-olp_oidc_login_flow=")
        })
        .collect::<Vec<_>>()
        .join("; ")
}

fn apply_response_cookies(existing: &str, response: &Response<Body>) -> String {
    let mut cookies = BTreeMap::new();
    for cookie in existing.split(';') {
        let Some((name, value)) = cookie.trim().split_once('=') else {
            continue;
        };
        if value.is_empty() {
            cookies.remove(name);
        } else {
            cookies.insert(name.to_owned(), value.to_owned());
        }
    }
    for cookie in response.headers().get_all(header::SET_COOKIE).iter() {
        let Some((name, value)) = cookie
            .to_str()
            .ok()
            .and_then(|value| value.split(';').next())
            .and_then(|value| value.split_once('='))
        else {
            continue;
        };
        if value.is_empty() {
            cookies.remove(name);
        } else {
            cookies.insert(name.to_owned(), value.to_owned());
        }
    }
    cookies
        .into_iter()
        .map(|(name, value)| format!("{name}={value}"))
        .collect::<Vec<_>>()
        .join("; ")
}

fn named_cookie(response: &Response<Body>, name: &str) -> String {
    response
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .find_map(|cookie| {
            let first = cookie.to_str().ok()?.split(';').next()?;
            first.strip_prefix(&format!("{name}="))
        })
        .unwrap()
        .to_owned()
}

fn assert_host_cookie_contract(response: &Response<Body>, name: &str) {
    let cookie = response
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .filter_map(|value| value.to_str().ok())
        .find(|value| value.starts_with(&format!("{name}=")))
        .unwrap_or_else(|| panic!("missing {name} cookie"));
    assert!(
        cookie.contains("; Path=/;"),
        "invalid __Host Path: {cookie}"
    );
    assert!(cookie.contains("; Secure;"), "missing Secure: {cookie}");
    assert!(
        !cookie.contains("Domain="),
        "__Host cookie has Domain: {cookie}"
    );
}

fn query_value(url: &str, name: &str) -> String {
    Url::parse(url)
        .unwrap()
        .query_pairs()
        .find_map(|(key, value)| (key == name).then(|| value.into_owned()))
        .unwrap()
}

async fn response_json(response: Response<Body>) -> Value {
    serde_json::from_str(&response_text(response).await).unwrap()
}

async fn response_text(response: Response<Body>) -> String {
    let bytes = response.into_body().collect().await.unwrap().to_bytes();
    String::from_utf8(bytes.to_vec()).unwrap()
}

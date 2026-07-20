use std::{path::PathBuf, sync::Arc};

use axum::{
    Router,
    body::Body,
    http::{Method, Request, Response, StatusCode, header},
};
use http_body_util::BodyExt as _;
use olp::{ApiMode, ApiState, RuntimeManager, public_router};
use olp_storage::{MasterKey, PgStore};
use serde_json::{Value, json};
use sqlx::Row as _;
use tower::ServiceExt as _;

mod common;
use common::{BOOTSTRAP_TOKEN, configure_bootstrap};

const ORIGIN: &str = "https://olp.example.test";

#[tokio::test]
#[ignore = "requires an empty PostgreSQL database in OLP_TEST_DATABASE_URL"]
async fn identity_http_flow_enforces_sessions_csrf_roles_and_owner_guard() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty test database");
    let store = PgStore::connect(&database_url, 5).await.unwrap();
    store.migrate().await.unwrap();
    let mut state = ApiState::new(
        ApiMode::Control,
        Some(store.clone()),
        Arc::new(RuntimeManager::empty()),
        ORIGIN,
        PathBuf::from("missing-console-for-api-test"),
    );
    state.master_key = Some(Arc::new(MasterKey::new(1, [7; 32])));
    configure_bootstrap(&mut state, [8; 32]);
    let app = public_router(state);

    let setup = send_json(
        &app,
        Method::POST,
        "/api/v1/setup",
        json!({
            "email": "owner@example.test",
            "password": "correct horse battery staple",
            "display_name": "Owner",
            "installation_name": "HTTP identity test"
        }),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(setup.status(), StatusCode::CREATED);
    let owner_cookie = cookie_header(&setup);
    let setup_body = response_json(setup).await;
    let owner_csrf = setup_body["csrf_token"].as_str().unwrap().to_owned();
    let owner_id = setup_body["user"]["id"].as_str().unwrap().to_owned();
    let owner_uuid = uuid::Uuid::parse_str(&owner_id).unwrap();
    let stale_activity = chrono::Utc::now() - chrono::Duration::minutes(10);
    sqlx::query("UPDATE sessions SET last_seen_at = $1 WHERE user_id = $2")
        .bind(stale_activity)
        .bind(owner_uuid)
        .execute(store.pool())
        .await
        .unwrap();

    let profile = send_empty(
        &app,
        Method::GET,
        "/api/v1/profile",
        Some(&owner_cookie),
        None,
        None,
    )
    .await;
    assert_eq!(profile.status(), StatusCode::OK);
    let touched_activity: chrono::DateTime<chrono::Utc> =
        sqlx::query_scalar("SELECT last_seen_at FROM sessions WHERE user_id = $1")
            .bind(owner_uuid)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert!(touched_activity > stale_activity);
    let profile_etag = profile.headers()[header::ETAG].to_str().unwrap().to_owned();
    let updated_profile = send_json_with_if_match(
        &app,
        Method::PATCH,
        "/api/v1/profile",
        json!({ "display_name": "Renamed Owner" }),
        &owner_cookie,
        &owner_csrf,
        &profile_etag,
    )
    .await;
    assert_eq!(updated_profile.status(), StatusCode::OK);
    let password_etag = updated_profile.headers()[header::ETAG]
        .to_str()
        .unwrap()
        .to_owned();
    assert_eq!(
        response_json(updated_profile).await["display_name"],
        "Renamed Owner"
    );

    let second_owner_login = send_json(
        &app,
        Method::POST,
        "/api/v1/sessions",
        json!({
            "email": "owner@example.test",
            "password": "correct horse battery staple"
        }),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(second_owner_login.status(), StatusCode::CREATED);
    let second_owner_cookie = cookie_header(&second_owner_login);

    let changed_password = send_json_with_if_match(
        &app,
        Method::POST,
        "/api/v1/profile/password",
        json!({
            "current_password": "correct horse battery staple",
            "new_password": "new correct horse battery staple"
        }),
        &owner_cookie,
        &owner_csrf,
        &password_etag,
    )
    .await;
    assert_eq!(changed_password.status(), StatusCode::OK);
    let revoked_other_session = send_empty(
        &app,
        Method::GET,
        "/api/v1/sessions/current",
        Some(&second_owner_cookie),
        None,
        None,
    )
    .await;
    assert_eq!(revoked_other_session.status(), StatusCode::UNAUTHORIZED);
    let old_password = send_json(
        &app,
        Method::POST,
        "/api/v1/sessions",
        json!({
            "email": "owner@example.test",
            "password": "correct horse battery staple"
        }),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(old_password.status(), StatusCode::UNAUTHORIZED);
    let unknown_identity = send_json(
        &app,
        Method::POST,
        "/api/v1/sessions",
        json!({
            "email": "unknown@example.test",
            "password": "a plausible but incorrect local password"
        }),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(unknown_identity.status(), StatusCode::UNAUTHORIZED);
    let new_password = send_json(
        &app,
        Method::POST,
        "/api/v1/sessions",
        json!({
            "email": "owner@example.test",
            "password": "new correct horse battery staple"
        }),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(new_password.status(), StatusCode::CREATED);

    let invitation = send_json(
        &app,
        Method::POST,
        "/api/v1/invitations",
        json!({
            "email": "developer@example.test",
            "role": "developer",
            "expires_in_hours": 24
        }),
        Some(&owner_cookie),
        Some(&owner_csrf),
        Some("invite-developer-http-001"),
    )
    .await;
    assert_eq!(invitation.status(), StatusCode::CREATED);
    let invitation_body = response_json(invitation).await;
    let invitation_token = invitation_body["token"].as_str().unwrap();
    let invitation_replay = send_json(
        &app,
        Method::POST,
        "/api/v1/invitations",
        json!({
            "email": "developer@example.test",
            "role": "developer",
            "expires_in_hours": 24
        }),
        Some(&owner_cookie),
        Some(&owner_csrf),
        Some("invite-developer-http-001"),
    )
    .await;
    assert_eq!(invitation_replay.status(), StatusCode::CREATED);
    assert_eq!(response_json(invitation_replay).await, invitation_body);
    let invitation_mismatch = send_json(
        &app,
        Method::POST,
        "/api/v1/invitations",
        json!({
            "email": "changed@example.test",
            "role": "viewer",
            "expires_in_hours": 24
        }),
        Some(&owner_cookie),
        Some(&owner_csrf),
        Some("invite-developer-http-001"),
    )
    .await;
    assert_eq!(invitation_mismatch.status(), StatusCode::CONFLICT);

    let acceptance = send_json(
        &app,
        Method::POST,
        "/api/v1/invitations/accept",
        json!({
            "token": invitation_token,
            "display_name": "Developer",
            "password": "another correct local password"
        }),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(acceptance.status(), StatusCode::CREATED);
    let developer_cookie = cookie_header(&acceptance);
    let acceptance_body = response_json(acceptance).await;
    let developer_id = acceptance_body["user"]["id"].as_str().unwrap().to_owned();

    let users = send_empty(
        &app,
        Method::GET,
        "/api/v1/users?limit=1",
        Some(&owner_cookie),
        None,
        None,
    )
    .await;
    assert_eq!(users.status(), StatusCode::OK);
    let users_body = response_json(users).await;
    assert_eq!(users_body["data"].as_array().unwrap().len(), 1);
    assert!(users_body["next_cursor"].is_string());

    let developer = send_empty(
        &app,
        Method::GET,
        &format!("/api/v1/users/{developer_id}"),
        Some(&owner_cookie),
        None,
        None,
    )
    .await;
    assert_eq!(developer.status(), StatusCode::OK);
    let developer_etag = developer
        .headers()
        .get(header::ETAG)
        .unwrap()
        .to_str()
        .unwrap()
        .to_owned();

    let sessions = send_empty(
        &app,
        Method::GET,
        &format!("/api/v1/sessions?user_id={developer_id}"),
        Some(&owner_cookie),
        None,
        None,
    )
    .await;
    assert_eq!(sessions.status(), StatusCode::OK);
    let sessions_body = response_json(sessions).await;
    let developer_session_id = sessions_body["data"][0]["id"].as_str().unwrap().to_owned();
    let revoked_session = send_empty(
        &app,
        Method::DELETE,
        &format!("/api/v1/sessions/{developer_session_id}"),
        Some(&owner_cookie),
        Some(&owner_csrf),
        None,
    )
    .await;
    assert_eq!(revoked_session.status(), StatusCode::NO_CONTENT);
    let old_session = send_empty(
        &app,
        Method::GET,
        "/api/v1/sessions/current",
        Some(&developer_cookie),
        None,
        None,
    )
    .await;
    assert_eq!(old_session.status(), StatusCode::UNAUTHORIZED);

    let role_update = send_json_with_if_match(
        &app,
        Method::PATCH,
        &format!("/api/v1/users/{developer_id}"),
        json!({ "role": "viewer" }),
        &owner_cookie,
        &owner_csrf,
        &developer_etag,
    )
    .await;
    assert_eq!(role_update.status(), StatusCode::OK);
    assert_eq!(response_json(role_update).await["role"], "viewer");

    let owner = send_empty(
        &app,
        Method::GET,
        &format!("/api/v1/users/{owner_id}"),
        Some(&owner_cookie),
        None,
        None,
    )
    .await;
    let owner_etag = owner
        .headers()
        .get(header::ETAG)
        .unwrap()
        .to_str()
        .unwrap()
        .to_owned();
    let last_owner = send_json_with_if_match(
        &app,
        Method::PATCH,
        &format!("/api/v1/users/{owner_id}"),
        json!({ "role": "viewer" }),
        &owner_cookie,
        &owner_csrf,
        &owner_etag,
    )
    .await;
    assert_eq!(last_owner.status(), StatusCode::CONFLICT);
    assert!(
        response_json(last_owner).await["type"]
            .as_str()
            .unwrap()
            .ends_with("/last_owner_required")
    );

    let pending = send_json(
        &app,
        Method::POST,
        "/api/v1/invitations",
        json!({ "email": "viewer@example.test", "role": "viewer" }),
        Some(&owner_cookie),
        Some(&owner_csrf),
        Some("invite-viewer-http-0001"),
    )
    .await;
    let pending_body = response_json(pending).await;
    let pending_id = pending_body["invitation"]["id"].as_str().unwrap();
    let revoked_invitation = send_empty(
        &app,
        Method::DELETE,
        &format!("/api/v1/invitations/{pending_id}"),
        Some(&owner_cookie),
        Some(&owner_csrf),
        Some("revoke-viewer-http-0001"),
    )
    .await;
    assert_eq!(revoked_invitation.status(), StatusCode::OK);
    assert_eq!(response_json(revoked_invitation).await["status"], "revoked");

    let audit_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM audit_events WHERE action IN \
         ('invitation.create', 'invitation.accept', 'invitation.revoke', \
          'user.create', 'user.role_update', 'session.create', 'session.revoke')",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert!(audit_count >= 9);
    let profile_audit_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM audit_events WHERE action IN
         ('user.profile_update', 'user.password_update')",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(profile_audit_count, 2);

    for _ in 0..5 {
        let rejected = send_json(
            &app,
            Method::POST,
            "/api/v1/sessions",
            json!({
                "email": "rate-limit-target@example.test",
                "password": "plausible but incorrect password"
            }),
            None,
            None,
            None,
        )
        .await;
        assert_eq!(rejected.status(), StatusCode::UNAUTHORIZED);
    }
    let rate_limited = send_json(
        &app,
        Method::POST,
        "/api/v1/sessions",
        json!({
            "email": "rate-limit-target@example.test",
            "password": "plausible but incorrect password"
        }),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(rate_limited.status(), StatusCode::TOO_MANY_REQUESTS);

    // Oversized passwords must still consume the source bucket before the
    // cheap validation path. Vary the target so the five-attempt
    // source-plus-target ceiling does not hide a source-admission bypass.
    let oversized_password = "x".repeat(1_025);
    let oversized_peer = "198.51.100.200:443".parse().unwrap();
    for attempt in 0..60 {
        let rejected = send_json_from_peer(
            &app,
            Method::POST,
            "/api/v1/sessions",
            json!({
                "email": format!("oversized-password-{attempt}@example.test"),
                "password": oversized_password.as_str(),
            }),
            None,
            None,
            None,
            oversized_peer,
        )
        .await;
        assert_eq!(rejected.status(), StatusCode::UNAUTHORIZED);
    }
    let oversized_rate_limited = send_json_from_peer(
        &app,
        Method::POST,
        "/api/v1/sessions",
        json!({
            "email": "oversized-password-final@example.test",
            "password": oversized_password.as_str(),
        }),
        None,
        None,
        None,
        oversized_peer,
    )
    .await;
    assert_eq!(
        oversized_rate_limited.status(),
        StatusCode::TOO_MANY_REQUESTS
    );

    let login_audits = sqlx::query(
        "SELECT actor_user_id, outcome, resource_id, source_ip::text AS source_ip, \
                user_agent_family \
         FROM audit_events WHERE action = 'local_auth.login' ORDER BY occurred_at",
    )
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert!(login_audits.iter().any(|row| {
        row.get::<String, _>("outcome") == "success"
            && row.get::<Option<uuid::Uuid>, _>("actor_user_id") == Some(owner_uuid)
            && row.get::<Option<String>, _>("resource_id").is_some()
    }));
    assert!(login_audits.iter().any(|row| {
        row.get::<String, _>("outcome") == "failure"
            && row.get::<Option<uuid::Uuid>, _>("actor_user_id") == Some(owner_uuid)
            && row.get::<Option<String>, _>("resource_id").is_none()
    }));
    assert!(login_audits.iter().any(|row| {
        row.get::<String, _>("outcome") == "failure"
            && row.get::<Option<uuid::Uuid>, _>("actor_user_id").is_none()
            && row.get::<Option<String>, _>("resource_id").is_none()
    }));
    assert!(login_audits.iter().all(|row| {
        row.get::<Option<String>, _>("source_ip").is_none()
            && row.get::<Option<String>, _>("user_agent_family").is_none()
    }));

    let audit = send_empty(
        &app,
        Method::GET,
        "/api/v1/audit?limit=200",
        Some(&owner_cookie),
        None,
        None,
    )
    .await;
    assert_eq!(audit.status(), StatusCode::OK);
    for event in response_json(audit).await["data"].as_array().unwrap() {
        assert!(event.get("source_ip").is_none());
        assert!(event.get("user_agent_family").is_none());
    }
}

async fn send_json(
    app: &Router,
    method: Method,
    uri: &str,
    body: Value,
    cookie: Option<&str>,
    csrf: Option<&str>,
    idempotency_key: Option<&str>,
) -> Response<Body> {
    send_json_from_peer(
        app,
        method,
        uri,
        body,
        cookie,
        csrf,
        idempotency_key,
        "198.51.100.11:443".parse().unwrap(),
    )
    .await
}

#[allow(clippy::too_many_arguments)]
async fn send_json_from_peer(
    app: &Router,
    method: Method,
    uri: &str,
    body: Value,
    cookie: Option<&str>,
    csrf: Option<&str>,
    idempotency_key: Option<&str>,
    peer: std::net::SocketAddr,
) -> Response<Body> {
    request(
        app,
        method,
        uri,
        Some(body),
        cookie,
        csrf,
        idempotency_key,
        None,
        peer,
    )
    .await
}

async fn send_json_with_if_match(
    app: &Router,
    method: Method,
    uri: &str,
    body: Value,
    cookie: &str,
    csrf: &str,
    etag: &str,
) -> Response<Body> {
    request(
        app,
        method,
        uri,
        Some(body),
        Some(cookie),
        Some(csrf),
        None,
        Some(etag),
        "198.51.100.11:443".parse().unwrap(),
    )
    .await
}

async fn send_empty(
    app: &Router,
    method: Method,
    uri: &str,
    cookie: Option<&str>,
    csrf: Option<&str>,
    idempotency_key: Option<&str>,
) -> Response<Body> {
    request(
        app,
        method,
        uri,
        None,
        cookie,
        csrf,
        idempotency_key,
        None,
        "198.51.100.11:443".parse().unwrap(),
    )
    .await
}

#[allow(clippy::too_many_arguments)]
async fn request(
    app: &Router,
    method: Method,
    uri: &str,
    body: Option<Value>,
    cookie: Option<&str>,
    csrf: Option<&str>,
    idempotency_key: Option<&str>,
    etag: Option<&str>,
    peer: std::net::SocketAddr,
) -> Response<Body> {
    let mut builder = Request::builder().method(method.clone()).uri(uri);
    if method != Method::GET {
        builder = builder.header(header::ORIGIN, ORIGIN);
    }
    if let Some(cookie) = cookie {
        builder = builder.header(header::COOKIE, cookie);
    }
    if let Some(csrf) = csrf {
        builder = builder.header("x-csrf-token", csrf);
    }
    if let Some(idempotency_key) = idempotency_key {
        builder = builder.header("idempotency-key", idempotency_key);
    }
    if let Some(etag) = etag {
        builder = builder.header(header::IF_MATCH, etag);
    }
    if uri == "/api/v1/setup" {
        builder = builder.header("x-olp-setup-token", BOOTSTRAP_TOKEN);
    }
    let body = if let Some(body) = body {
        builder = builder.header(header::CONTENT_TYPE, "application/json");
        Body::from(body.to_string())
    } else {
        Body::empty()
    };
    let mut request = builder.body(body).unwrap();
    request
        .extensions_mut()
        .insert(axum::extract::ConnectInfo(peer));
    app.clone().oneshot(request).await.unwrap()
}

fn cookie_header(response: &Response<Body>) -> String {
    response
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .map(|cookie| cookie.to_str().unwrap().split(';').next().unwrap())
        .collect::<Vec<_>>()
        .join("; ")
}

async fn response_json(response: Response<Body>) -> Value {
    let bytes = response.into_body().collect().await.unwrap().to_bytes();
    serde_json::from_slice(&bytes).unwrap()
}

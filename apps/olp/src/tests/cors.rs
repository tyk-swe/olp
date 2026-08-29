use std::{path::PathBuf, sync::Arc};

use axum::{
    body::Body,
    http::{HeaderValue, Method, Request, StatusCode},
};
use tower::ServiceExt as _;

use crate::bootstrap::{mode_dependencies::test_store, state::ApiMode};
use olp_db::security::key_material::AuthHmacKey;
use olp_engine::inference::runtime::Manager;

use super::{ProcessComposition, gateway_router_for_test, validated_public_router};

fn composition(origins: Vec<HeaderValue>) -> ProcessComposition {
    let mut state = ProcessComposition::new(
        ApiMode::All,
        test_store(),
        Arc::new(Manager::empty()),
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    );
    state.auth_hmac_key = Arc::new(AuthHmacKey::new([3; 32]));
    state.set_gateway_cors_allowed_origins(origins);
    state
}

fn preflight(path: &str, origin: &'static str) -> Request<axum::body::Body> {
    Request::builder()
        .method(Method::OPTIONS)
        .uri(path)
        .header("origin", origin)
        .header("access-control-request-method", "POST")
        .header(
            "access-control-request-headers",
            "authorization, content-type, anthropic-dangerous-direct-browser-access, \
             x-goog-api-client, x-stainless-lang, x-stainless-package-version",
        )
        .body(axum::body::Body::empty())
        .unwrap()
}

#[tokio::test]
async fn preflight_is_answered_only_for_configured_origins() {
    let state = composition(vec![HeaderValue::from_static("https://app.example.test")]);
    let app = gateway_router_for_test(state.gateway_state_for_test());

    let allowed = app
        .clone()
        .oneshot(preflight(
            "/openai/v1/chat/completions",
            "https://app.example.test",
        ))
        .await
        .unwrap();
    assert_eq!(allowed.status(), StatusCode::OK);
    assert_eq!(
        allowed
            .headers()
            .get("access-control-allow-origin")
            .and_then(|value| value.to_str().ok()),
        Some("https://app.example.test")
    );
    assert_eq!(
        allowed
            .headers()
            .get("access-control-allow-headers")
            .and_then(|value| value.to_str().ok()),
        Some(
            "authorization, content-type, anthropic-dangerous-direct-browser-access, \
             x-goog-api-client, x-stainless-lang, x-stainless-package-version"
        )
    );
    assert!(
        allowed
            .headers()
            .get("access-control-allow-credentials")
            .is_none()
    );

    let denied = app
        .oneshot(preflight(
            "/openai/v1/chat/completions",
            "https://other.example.test",
        ))
        .await
        .unwrap();
    assert!(
        denied
            .headers()
            .get("access-control-allow-origin")
            .is_none()
    );
}

#[tokio::test]
async fn cors_stays_disabled_without_configured_origins() {
    let state = composition(Vec::new());
    let app = gateway_router_for_test(state.gateway_state_for_test());
    let response = app
        .oneshot(preflight(
            "/v1/chat/completions",
            "https://app.example.test",
        ))
        .await
        .unwrap();
    assert!(
        response
            .headers()
            .get("access-control-allow-origin")
            .is_none()
    );
}

#[tokio::test]
async fn admission_rejections_include_cors_headers() {
    let state = composition(vec![HeaderValue::from_static("https://app.example.test")]);
    let app = gateway_router_for_test(state.gateway_state_for_test());
    let response = app
        .oneshot(
            Request::post("/openai/v1/chat/completions")
                .header("origin", "https://app.example.test")
                .header("content-type", "application/json")
                .body(Body::from("{}"))
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
    assert_eq!(
        response
            .headers()
            .get("access-control-allow-origin")
            .and_then(|value| value.to_str().ok()),
        Some("https://app.example.test")
    );
}

#[tokio::test]
async fn management_api_never_receives_cors_headers() {
    let state = composition(vec![HeaderValue::from_static("https://app.example.test")]);
    let app = validated_public_router(state.mode_dependencies());
    let response = app
        .oneshot(preflight(
            "/api/v1/setup/status",
            "https://app.example.test",
        ))
        .await
        .unwrap();
    assert!(
        response
            .headers()
            .get("access-control-allow-origin")
            .is_none()
    );
}

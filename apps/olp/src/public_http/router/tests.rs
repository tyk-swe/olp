use axum::{
    body::Body,
    http::{HeaderValue, Request, StatusCode, header},
};
use tower::ServiceExt as _;

use crate::bootstrap::state::ProcessComposition;

use super::*;

#[tokio::test]
async fn hsts_follows_the_canonical_public_origin_scheme() {
    for mode in [
        crate::application::mode::ApiMode::Gateway,
        crate::application::mode::ApiMode::Control,
    ] {
        for tracing_enabled in [false, true] {
            for (origin, expected) in [
                ("https://console.example.test", true),
                ("http://127.0.0.1:8080", false),
            ] {
                let mut state = ProcessComposition::new(
                    mode,
                    crate::bootstrap::mode_dependencies::test_store(),
                    std::sync::Arc::new(olp_engine::inference::runtime::Manager::empty()),
                    origin,
                    std::path::PathBuf::from("missing-console"),
                );
                state.request_tracing =
                    tracing_enabled.then_some(crate::observability::tracing::RequestConfig {
                        installation_id: uuid::Uuid::nil(),
                        propagate_upstream: true,
                        accept_inbound: true,
                    });
                let router = match mode {
                    crate::application::mode::ApiMode::Gateway => {
                        gateway_router_for_test(state.gateway_state_for_test())
                    }
                    crate::application::mode::ApiMode::Control => {
                        management_router_for_test(state.management_state_for_test())
                    }
                    crate::application::mode::ApiMode::All => {
                        unreachable!("all mode is not part of this test")
                    }
                };
                let response = router
                    .oneshot(
                        Request::builder()
                            .uri("/metrics")
                            .body(Body::empty())
                            .unwrap(),
                    )
                    .await
                    .unwrap();
                assert_public_security_headers(&response, expected);
                assert_eq!(
                    response.headers().contains_key("strict-transport-security"),
                    expected,
                    "{mode:?} {origin} tracing={tracing_enabled}",
                );
            }
        }
    }
}

fn assert_public_security_headers(response: &Response, expect_hsts: bool) {
    assert_eq!(
        response.headers().get("x-content-type-options").unwrap(),
        HeaderValue::from_static("nosniff"),
    );
    assert_eq!(
        response.headers().get("x-frame-options").unwrap(),
        HeaderValue::from_static("DENY"),
    );
    assert_eq!(
        response.headers().get("referrer-policy").unwrap(),
        HeaderValue::from_static("no-referrer"),
    );
    assert!(response.headers().contains_key("content-security-policy"));
    assert_eq!(
        response.headers().contains_key("strict-transport-security"),
        expect_hsts,
    );
}

fn tracing_config() -> crate::observability::tracing::RequestConfig {
    crate::observability::tracing::RequestConfig {
        installation_id: uuid::Uuid::nil(),
        propagate_upstream: true,
        accept_inbound: true,
    }
}

#[tokio::test]
async fn admission_overload_responses_keep_public_boundary_headers() {
    for tracing_enabled in [false, true] {
        for (mode, hold_uri, reject_uri, expected_content_type) in [
            (
                crate::application::mode::ApiMode::Gateway,
                "/openai/v1/models",
                "/openai/v1/models",
                "application/json",
            ),
            (
                crate::application::mode::ApiMode::Control,
                "/metrics",
                "/api/v1/sessions",
                "application/problem+json",
            ),
        ] {
            let mut state = ProcessComposition::new(
                mode,
                crate::bootstrap::mode_dependencies::test_store(),
                std::sync::Arc::new(olp_engine::inference::runtime::Manager::empty()),
                "https://console.example.test",
                std::path::PathBuf::from("missing-console"),
            );
            state.set_public_admission_limits(1, 1);
            state.request_tracing = tracing_enabled.then_some(tracing_config());
            let router = match mode {
                crate::application::mode::ApiMode::Gateway => {
                    gateway_router_for_test(state.gateway_state_for_test())
                }
                crate::application::mode::ApiMode::Control => {
                    management_router_for_test(state.management_state_for_test())
                }
                crate::application::mode::ApiMode::All => {
                    unreachable!("all mode is not part of this test")
                }
            };
            let held_response = router
                .clone()
                .oneshot(Request::get(hold_uri).body(Body::empty()).unwrap())
                .await
                .unwrap();
            let response = router
                .oneshot(
                    Request::get(reject_uri)
                        .header("x-request-id", "test-request-id")
                        .body(Body::empty())
                        .unwrap(),
                )
                .await
                .unwrap();
            assert_eq!(response.status(), StatusCode::SERVICE_UNAVAILABLE);
            assert_eq!(
                response.headers().get("x-request-id").unwrap(),
                HeaderValue::from_static("test-request-id"),
                "{mode:?} tracing={tracing_enabled}",
            );
            assert_eq!(
                response.headers().get(header::CONTENT_TYPE).unwrap(),
                expected_content_type,
                "{mode:?} tracing={tracing_enabled}",
            );
            assert_public_security_headers(&response, true);
            assert_eq!(
                response.headers().get("permissions-policy").unwrap(),
                HeaderValue::from_static("camera=(), microphone=(), geolocation=(), payment=()"),
                "{mode:?} tracing={tracing_enabled}",
            );
            drop(held_response);
        }
    }
}

use axum::body::Body;
use axum::http::HeaderValue;
use axum::http::Request;
use axum::http::StatusCode;
use axum::http::header;
use tower::ServiceExt as _;

use crate::process::state::ProcessComposition;

use crate::http::router::*;

#[tokio::test]
async fn hsts_follows_the_canonical_public_origin_scheme() {
    for mode in [
        crate::process::mode::ApiMode::Gateway,
        crate::process::mode::ApiMode::Control,
    ] {
        for tracing_enabled in [false, true] {
            for (origin, expected) in [
                ("https://console.example.test", true),
                ("http://127.0.0.1:8080", false),
            ] {
                let mut state = ProcessComposition::new(
                    mode,
                    crate::process::mode_dependencies::test_store(),
                    std::sync::Arc::new(crate::runtime::manager::Manager::empty()),
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
                    crate::process::mode::ApiMode::Gateway => {
                        gateway_router_for_test(state.gateway_state_for_test())
                    }
                    crate::process::mode::ApiMode::Control => {
                        management_router_for_test(state.management_state_for_test())
                    }
                    crate::process::mode::ApiMode::All => {
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
                crate::process::mode::ApiMode::Gateway,
                "/v1/models",
                "/v1/models",
                "application/json",
            ),
            (
                crate::process::mode::ApiMode::Control,
                "/metrics",
                "/api/v3/sessions",
                "application/problem+json",
            ),
        ] {
            let mut state = ProcessComposition::new(
                mode,
                crate::process::mode_dependencies::test_store(),
                std::sync::Arc::new(crate::runtime::manager::Manager::empty()),
                "https://console.example.test",
                std::path::PathBuf::from("missing-console"),
            );
            state.set_public_admission_limits(1, 1);
            state.request_tracing = tracing_enabled.then_some(tracing_config());
            let router = match mode {
                crate::process::mode::ApiMode::Gateway => {
                    gateway_router_for_test(state.gateway_state_for_test())
                }
                crate::process::mode::ApiMode::Control => {
                    management_router_for_test(state.management_state_for_test())
                }
                crate::process::mode::ApiMode::All => {
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

#[tokio::test]
async fn api_rejections_are_not_cacheable_and_request_ids_are_bounded() {
    let state = ProcessComposition::new(
        crate::process::mode::ApiMode::Gateway,
        crate::process::mode_dependencies::test_store(),
        std::sync::Arc::new(crate::runtime::manager::Manager::empty()),
        "https://gateway.example.test",
        std::path::PathBuf::from("unused-console"),
    );
    let router = gateway_router_for_test(state.gateway_state_for_test());
    for (incoming, preserve) in [
        ("safe-id_123".to_owned(), true),
        ("x".repeat(129), false),
        ("bad id".to_owned(), false),
    ] {
        for path in [
            "/v1/models",
            "/openai/missing",
            "/anthropic/missing",
            "/gemini/missing",
        ] {
            let response = router
                .clone()
                .oneshot(
                    Request::get(path)
                        .header("x-request-id", &incoming)
                        .body(Body::empty())
                        .unwrap(),
                )
                .await
                .unwrap();
            assert_eq!(response.headers()[header::CACHE_CONTROL], "no-store");
            let id = response.headers()["x-request-id"].to_str().unwrap();
            if preserve {
                assert_eq!(id, incoming);
            } else {
                assert!(uuid::Uuid::parse_str(id).is_ok());
            }
        }
    }
}

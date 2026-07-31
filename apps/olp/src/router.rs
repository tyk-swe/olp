//! Public application route composition and boundary middleware.

use std::time::Duration;

use axum::{
    Router,
    body::Body,
    extract::DefaultBodyLimit,
    http::{HeaderName, Request, Uri},
    middleware,
    response::{IntoResponse, Response},
    routing::any,
};
use tower::ServiceBuilder;
use tower_http::{
    catch_panic::CatchPanicLayer,
    request_id::{MakeRequestUuid, PropagateRequestIdLayer, SetRequestIdLayer},
    sensitive_headers::{SetSensitiveRequestHeadersLayer, SetSensitiveResponseHeadersLayer},
    set_header::SetResponseHeaderLayer,
    timeout::RequestBodyTimeoutLayer,
    trace::TraceLayer,
};

use crate::{
    GatewayState, MAX_JSON_BODY_BYTES, ManagementState, ModeDependencies, Problem, gateway,
    management_api,
    request_admission::{PublicAdmissionMiddleware, admit_public_request, enforce_request_limits},
    static_console,
};

pub(super) const REQUEST_BODY_TIMEOUT: Duration = Duration::from_secs(30);
const MANAGEMENT_API_MOUNT_PATH: &str = "/api";
const MANAGEMENT_API_FALLBACK_PATH: &str = "/api/{*path}";
const MANAGEMENT_OPENAPI_PATH: &str = "/openapi.json";

pub(crate) fn is_management_path(path: &str) -> bool {
    path == MANAGEMENT_API_MOUNT_PATH
        || path
            .strip_prefix(MANAGEMENT_API_MOUNT_PATH)
            .is_some_and(|suffix| suffix.starts_with('/'))
        || path == MANAGEMENT_OPENAPI_PATH
}

/// Builds the public application router. Observability is intentionally served
/// by [`crate::observability_router`] on a separate listener. Public-auth
/// callers must attach [`axum::extract::ConnectInfo`] with the socket peer; the
/// hardened application listener does so automatically.
///
pub trait IntoPublicRouter {
    fn into_public_router(self) -> Router;
}

impl IntoPublicRouter for GatewayState {
    fn into_public_router(self) -> Router {
        compose_public_router(Some(self.clone()), None, self)
    }
}

impl IntoPublicRouter for ManagementState {
    fn into_public_router(self) -> Router {
        compose_public_router(None, Some(self.clone()), self.gateway_state())
    }
}

pub fn public_router(state: impl IntoPublicRouter) -> Router {
    state.into_public_router()
}

pub(crate) fn validated_public_router(dependencies: ModeDependencies) -> Router {
    let (gateway_state, management_state, request_limit_state): (
        Option<GatewayState>,
        Option<ManagementState>,
        GatewayState,
    ) = match dependencies {
        ModeDependencies::All {
            gateway,
            management,
            ..
        } => {
            let gateway = *gateway;
            (Some(gateway.clone()), Some(*management), gateway)
        }
        ModeDependencies::Gateway { gateway, .. } => {
            let gateway = *gateway;
            (Some(gateway.clone()), None, gateway)
        }
        ModeDependencies::Control { management, .. } => {
            let management = *management;
            let request_limit_state = management.gateway_state();
            (None, Some(management), request_limit_state)
        }
    };
    compose_public_router(gateway_state, management_state, request_limit_state)
}

#[cfg(test)]
pub(crate) fn gateway_router_for_test(state: GatewayState) -> Router {
    compose_public_router(Some(state.clone()), None, state)
}

#[cfg(test)]
pub(crate) fn management_router_for_test(state: ManagementState) -> Router {
    compose_public_router(None, Some(state.clone()), state.gateway_state())
}

fn compose_public_router(
    gateway_state: Option<GatewayState>,
    management_state: Option<ManagementState>,
    request_limit_state: GatewayState,
) -> Router {
    let request_id = HeaderName::from_static("x-request-id");
    let public_origin_is_https = request_limit_state.public_origin.is_https();
    let public_admission = PublicAdmissionMiddleware::new(
        request_limit_state.public_admission.clone(),
        gateway_state.is_some(),
    );
    // The request boundary protects public authentication as well as
    // inference, so control-only mode uses the playground's validated gateway
    // capabilities without exposing gateway routes.
    let content_security_policy = management_state.as_ref().map_or_else(
        || static_console::content_security_policy(std::path::Path::new(".")),
        |state| static_console::content_security_policy(&state.console_dir),
    );
    // Keep observability descendants and metrics ahead of the console fallback.
    // The exact `/health` path belongs to the console; probes live below it on
    // the separate observability listener.
    let mut router = Router::new()
        .route("/health/", any(public_observability_not_found))
        .route("/health/{*path}", any(public_observability_not_found))
        .route("/metrics", any(public_observability_not_found))
        .route("/metrics/", any(public_observability_not_found))
        .route("/metrics/{*path}", any(public_observability_not_found));

    if let Some(state) = management_state.as_ref() {
        let control = Router::new()
            .route(MANAGEMENT_OPENAPI_PATH, any(api_not_found))
            .merge(management_api::router())
            .route(MANAGEMENT_API_FALLBACK_PATH, any(api_not_found))
            .layer(middleware::from_fn(normalize_management_rejection))
            .with_state(state.clone());
        router = router
            .merge(control)
            .fallback_service(static_console::spa_service(&state.console_dir));
    }

    if let Some(state) = gateway_state {
        // Protocol routes are merged here by the gateway module once transports
        // have been wired. Keeping mode composition explicit prevents a control
        // deployment from accidentally becoming an inference data plane.
        router = router
            .merge(gateway::router().with_state(state))
            .route("/openai/{*path}", any(protocol_not_found))
            .route("/anthropic/{*path}", any(protocol_not_found))
            .route("/gemini/{*path}", any(protocol_not_found));
    }

    let router = router
        .layer(DefaultBodyLimit::max(MAX_JSON_BODY_BYTES))
        .layer(middleware::from_fn_with_state(
            request_limit_state,
            enforce_request_limits,
        ))
        .layer(middleware::from_fn(normalize_management_rejection))
        // Admission stays inside the cheap public boundary so overload
        // rejections keep request IDs, tracing, and hardened response headers,
        // while still running before authentication, request-body decoding,
        // storage, and transport work.
        .layer(middleware::from_fn_with_state(
            public_admission,
            admit_public_request,
        ))
        .layer(middleware::from_fn(prevent_management_caching))
        .layer(
            ServiceBuilder::new()
                .layer(SetSensitiveRequestHeadersLayer::new(
                    sensitive_request_headers(),
                ))
                .layer(SetRequestIdLayer::new(request_id.clone(), MakeRequestUuid))
                .layer(PropagateRequestIdLayer::new(request_id))
                .layer(TraceLayer::new_for_http().make_span_with(http_request_span))
                .layer(SetSensitiveResponseHeadersLayer::new(
                    sensitive_response_headers(),
                ))
                .layer(CatchPanicLayer::custom(problem_panic_response))
                .layer(RequestBodyTimeoutLayer::new(REQUEST_BODY_TIMEOUT))
                .layer(SetResponseHeaderLayer::if_not_present(
                    HeaderName::from_static("x-content-type-options"),
                    axum::http::HeaderValue::from_static("nosniff"),
                ))
                .layer(SetResponseHeaderLayer::if_not_present(
                    HeaderName::from_static("x-frame-options"),
                    axum::http::HeaderValue::from_static("DENY"),
                ))
                .layer(SetResponseHeaderLayer::if_not_present(
                    HeaderName::from_static("referrer-policy"),
                    axum::http::HeaderValue::from_static("no-referrer"),
                ))
                .layer(SetResponseHeaderLayer::if_not_present(
                    HeaderName::from_static("permissions-policy"),
                    axum::http::HeaderValue::from_static(
                        "camera=(), microphone=(), geolocation=(), payment=()",
                    ),
                ))
                .layer(SetResponseHeaderLayer::if_not_present(
                    HeaderName::from_static("content-security-policy"),
                    content_security_policy,
                )),
        );
    if public_origin_is_https {
        router.layer(SetResponseHeaderLayer::if_not_present(
            HeaderName::from_static("strict-transport-security"),
            axum::http::HeaderValue::from_static("max-age=31536000"),
        ))
    } else {
        router
    }
}

async fn public_observability_not_found() -> axum::http::StatusCode {
    axum::http::StatusCode::NOT_FOUND
}

/// Axum extractor rejections otherwise bypass the RFC 9457 management error
/// contract and return `text/plain`. Normalize malformed path/query values at
/// the management boundary without reflecting their potentially sensitive raw
/// values.
async fn normalize_management_rejection(
    request: Request<Body>,
    next: middleware::Next,
) -> Response {
    let uri = request.uri().clone();
    let response = next.run(request).await;
    if !is_management_path(uri.path())
        || !response.status().is_client_error() && !response.status().is_server_error()
    {
        return response;
    }
    let is_problem = response
        .headers()
        .get(axum::http::header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .is_some_and(|value| value.starts_with("application/problem+json"));
    if is_problem {
        return response;
    }
    let status = response.status();
    let allow = response.headers().get(axum::http::header::ALLOW).cloned();
    let (code, title, detail) = match status {
        axum::http::StatusCode::BAD_REQUEST => (
            "invalid_parameters",
            "Invalid request",
            "One or more path, query, or body parameters are malformed.",
        ),
        axum::http::StatusCode::NOT_FOUND => (
            "management_endpoint_not_found",
            "Endpoint not found",
            "The requested management endpoint does not exist.",
        ),
        axum::http::StatusCode::METHOD_NOT_ALLOWED => (
            "method_not_allowed",
            "Method not allowed",
            "The management endpoint does not support this HTTP method.",
        ),
        axum::http::StatusCode::PAYLOAD_TOO_LARGE => (
            "payload_too_large",
            "Payload too large",
            "The request body exceeds the configured limit.",
        ),
        axum::http::StatusCode::REQUEST_TIMEOUT => (
            "request_timeout",
            "Request timeout",
            "The request body was not received before the deadline.",
        ),
        _ if status.is_server_error() => (
            "internal_error",
            "Internal error",
            "The request could not be completed.",
        ),
        _ => (
            "request_rejected",
            "Request rejected",
            "The management request was rejected.",
        ),
    };
    let mut problem = Problem::new(status, code, title, detail);
    if status == axum::http::StatusCode::BAD_REQUEST {
        problem.errors.insert(
            "request".to_owned(),
            vec!["One or more request parameters are malformed.".to_owned()],
        );
    }
    let mut normalized = problem.with_instance(&uri).into_response();
    if let Some(allow) = allow {
        normalized
            .headers_mut()
            .insert(axum::http::header::ALLOW, allow);
    }
    normalized
}

async fn prevent_management_caching(request: Request<Body>, next: middleware::Next) -> Response {
    let is_management = is_management_path(request.uri().path());
    let mut response = next.run(request).await;
    if is_management {
        response.headers_mut().insert(
            axum::http::header::CACHE_CONTROL,
            axum::http::HeaderValue::from_static("no-store, private"),
        );
    }
    response
}

fn problem_panic_response(_panic: Box<dyn std::any::Any + Send + 'static>) -> Response<Body> {
    // The panic payload can contain request or upstream data. The active HTTP
    // span retains method, path, and request ID without exposing that payload.
    tracing::error!("HTTP request handler panicked");
    Problem::internal().into_response()
}

pub(super) fn sensitive_request_headers() -> [HeaderName; 6] {
    [
        axum::http::header::AUTHORIZATION,
        axum::http::header::COOKIE,
        HeaderName::from_static(management_api::CSRF_HEADER),
        HeaderName::from_static(management_api::SETUP_TOKEN_HEADER),
        HeaderName::from_static("x-api-key"),
        HeaderName::from_static("x-goog-api-key"),
    ]
}

pub(super) fn sensitive_response_headers() -> [HeaderName; 2] {
    [
        axum::http::header::SET_COOKIE,
        HeaderName::from_static(management_api::CSRF_HEADER),
    ]
}

pub(super) fn request_trace_path(uri: &Uri) -> &str {
    uri.path()
}

pub(super) fn http_request_span(request: &Request<Body>) -> tracing::Span {
    let request_id = request
        .headers()
        .get("x-request-id")
        .and_then(|value| value.to_str().ok())
        .unwrap_or("unavailable");
    tracing::info_span!(
        "http_request",
        method = %request.method(),
        path = %request_trace_path(request.uri()),
        request_id = %request_id,
    )
}

async fn api_not_found(uri: Uri) -> Problem {
    Problem::new(
        axum::http::StatusCode::NOT_FOUND,
        "management_endpoint_not_found",
        "Endpoint not found",
        "The requested management endpoint does not exist.",
    )
    .with_instance(&uri)
}

async fn protocol_not_found(uri: Uri) -> Problem {
    Problem::new(
        axum::http::StatusCode::NOT_FOUND,
        "protocol_endpoint_not_found",
        "Endpoint not found",
        "The requested inference endpoint is not enabled in this release.",
    )
    .with_instance(&uri)
}

#[cfg(test)]
mod tests {
    use axum::{
        body::Body,
        http::{HeaderValue, Request, StatusCode, header},
    };
    use tower::ServiceExt as _;

    use crate::ApiState;

    use super::*;

    #[tokio::test]
    async fn hsts_follows_the_canonical_public_origin_scheme() {
        for mode in [crate::ApiMode::Gateway, crate::ApiMode::Control] {
            for (origin, expected) in [
                ("https://console.example.test", true),
                ("http://127.0.0.1:8080", false),
            ] {
                let state = ApiState::new(
                    mode,
                    None,
                    std::sync::Arc::new(crate::RuntimeManager::empty()),
                    origin,
                    std::path::PathBuf::from("missing-console"),
                );
                let router = match mode {
                    crate::ApiMode::Gateway => public_router(state.gateway_state_for_test()),
                    crate::ApiMode::Control => public_router(state.management_state_for_test()),
                    crate::ApiMode::All => unreachable!("all mode is not part of this test"),
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
                assert_eq!(
                    response.headers().contains_key("strict-transport-security"),
                    expected,
                    "{mode:?} {origin}",
                );
            }
        }
    }

    #[tokio::test]
    async fn admission_overload_responses_keep_public_boundary_headers() {
        for (mode, hold_uri, reject_uri, expected_content_type) in [
            (
                crate::ApiMode::Gateway,
                "/openai/v1/models",
                "/openai/v1/models",
                "application/json",
            ),
            (
                crate::ApiMode::Control,
                "/api/v1/sessions",
                "/api/v1/sessions",
                "application/problem+json",
            ),
        ] {
            let mut state = ApiState::new(
                mode,
                None,
                std::sync::Arc::new(crate::RuntimeManager::empty()),
                "https://console.example.test",
                std::path::PathBuf::from("missing-console"),
            );
            state.set_public_admission_limits(1, 1);
            let router = match mode {
                crate::ApiMode::Gateway => public_router(state.gateway_state_for_test()),
                crate::ApiMode::Control => public_router(state.management_state_for_test()),
                crate::ApiMode::All => unreachable!("all mode is not part of this test"),
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
                "{mode:?}",
            );
            assert_eq!(
                response.headers().get(header::CONTENT_TYPE).unwrap(),
                expected_content_type,
                "{mode:?}",
            );
            assert_eq!(
                response.headers().get("x-content-type-options").unwrap(),
                HeaderValue::from_static("nosniff"),
                "{mode:?}",
            );
            assert_eq!(
                response.headers().get("x-frame-options").unwrap(),
                HeaderValue::from_static("DENY"),
                "{mode:?}",
            );
            assert_eq!(
                response.headers().get("referrer-policy").unwrap(),
                HeaderValue::from_static("no-referrer"),
                "{mode:?}",
            );
            assert_eq!(
                response.headers().get("permissions-policy").unwrap(),
                HeaderValue::from_static("camera=(), microphone=(), geolocation=(), payment=()"),
                "{mode:?}",
            );
            assert!(
                response.headers().contains_key("content-security-policy"),
                "{mode:?}",
            );
            if matches!(mode, crate::ApiMode::Control) {
                assert_eq!(
                    response.headers().get(header::CACHE_CONTROL).unwrap(),
                    HeaderValue::from_static("no-store, private"),
                );
            }
            drop(held_response);
        }
    }

    #[tokio::test]
    async fn management_responses_are_never_cacheable() {
        let state = ApiState::new(
            crate::ApiMode::Control,
            None,
            std::sync::Arc::new(crate::RuntimeManager::empty()),
            "https://console.example.test",
            std::path::PathBuf::from("missing-console"),
        );
        let response = public_router(state.management_state_for_test())
            .oneshot(
                Request::get("/api/not-a-route")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(
            response.headers().get(header::CACHE_CONTROL).unwrap(),
            "no-store, private"
        );
    }
}

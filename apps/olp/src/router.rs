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
    management_api, request_admission::enforce_request_limits, static_console,
};

pub(super) const REQUEST_BODY_TIMEOUT: Duration = Duration::from_secs(30);

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
            .route("/openapi.json", any(api_not_found))
            .merge(management_api::router())
            .route("/api/{*path}", any(api_not_found))
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
        )
        .layer(DefaultBodyLimit::max(MAX_JSON_BODY_BYTES))
        .layer(middleware::from_fn_with_state(
            request_limit_state,
            enforce_request_limits,
        ))
        .layer(middleware::from_fn(normalize_management_rejection));
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
    if !uri.path().starts_with("/api/")
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
    use axum::{body::Body, http::Request};
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
}

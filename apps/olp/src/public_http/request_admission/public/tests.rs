use std::{
    convert::Infallible,
    path::PathBuf,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

use axum::{
    Router,
    body::{Body, Bytes},
    http::{Request, StatusCode},
    middleware,
    response::Response,
    routing::{get, post},
};
use futures::stream;
use http_body_util::BodyExt as _;
use hyper_util::rt::{TokioExecutor, TokioIo};
use tokio::{
    net::{TcpListener, TcpStream},
    sync::{Notify, watch},
};
use tower::ServiceExt as _;

use super::*;
use olp_inference::runtime::RuntimeManager;

use crate::{ApiMode, ProcessComposition, observability_router, public_http::listener};

fn app<F>(admission: PublicAdmission, make_body: F) -> Router
where
    F: Fn() -> Body + Clone + Send + Sync + 'static,
{
    Router::new()
        .route(
            "/",
            get(move || {
                let body = make_body();
                async { Response::new(body) }
            }),
        )
        .layer(middleware::from_fn_with_state(
            PublicAdmissionMiddleware::new(admission, false),
            admit_public_request,
        ))
}

#[tokio::test]
async fn unary_response_releases_only_after_body_completion() {
    let admission = PublicAdmission::new(1, 1);
    let response = app(admission.clone(), || Body::from("ok"))
        .oneshot(Request::get("/").body(Body::empty()).unwrap())
        .await
        .unwrap();

    assert_eq!(admission.admitted("management"), 1);
    let body = response.into_body().collect().await.unwrap().to_bytes();
    assert_eq!(body, "ok");
    assert_eq!(admission.admitted("management"), 0);
}

#[tokio::test]
async fn handler_execution_is_admitted_and_early_error_body_releases() {
    let admission = PublicAdmission::new(1, 1);
    let entered = Arc::new(Notify::new());
    let release = Arc::new(Notify::new());
    let app = Router::new()
        .route(
            "/",
            get({
                let entered = Arc::clone(&entered);
                let release = Arc::clone(&release);
                move || {
                    let entered = Arc::clone(&entered);
                    let release = Arc::clone(&release);
                    async move {
                        entered.notify_one();
                        release.notified().await;
                        StatusCode::BAD_REQUEST
                    }
                }
            }),
        )
        .layer(middleware::from_fn_with_state(
            PublicAdmissionMiddleware::new(admission.clone(), false),
            admit_public_request,
        ));
    let request = tokio::spawn(app.oneshot(Request::get("/").body(Body::empty()).unwrap()));
    entered.notified().await;
    assert_eq!(admission.admitted("management"), 1);
    release.notify_one();
    let response = request.await.unwrap().unwrap();
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    assert_eq!(admission.admitted("management"), 1);
    response.into_body().collect().await.unwrap();
    assert_metrics_zero(&admission);
}

#[tokio::test]
async fn cancelling_handler_future_releases_permit() {
    let admission = PublicAdmission::new(1, 1);
    let entered = Arc::new(Notify::new());
    let app = Router::new()
        .route(
            "/",
            get({
                let entered = Arc::clone(&entered);
                move || {
                    let entered = Arc::clone(&entered);
                    async move {
                        entered.notify_one();
                        std::future::pending::<()>().await;
                    }
                }
            }),
        )
        .layer(middleware::from_fn_with_state(
            PublicAdmissionMiddleware::new(admission.clone(), false),
            admit_public_request,
        ));
    let request = tokio::spawn(app.oneshot(Request::get("/").body(Body::empty()).unwrap()));
    entered.notified().await;
    assert_eq!(admission.admitted("management"), 1);
    request.abort();
    assert!(request.await.unwrap_err().is_cancelled());
    assert_metrics_zero(&admission);
}

#[test]
fn panic_unwinding_releases_permit() {
    let admission = PublicAdmission::new(1, 1);
    let unwound = std::panic::catch_unwind({
        let admission = admission.clone();
        move || {
            let _permit = admission
                .try_acquire(AdmissionSurface::Management)
                .expect("management permit");
            panic!("test panic after admission");
        }
    });
    assert!(unwound.is_err());
    assert_metrics_zero(&admission);
}

#[tokio::test]
async fn streaming_body_holds_until_eof_and_drop_releases_unread_body() {
    let admission = PublicAdmission::new(1, 1);
    let response = app(admission.clone(), || {
        Body::from_stream(stream::iter([
            Ok::<_, Infallible>(Bytes::from_static(b"a")),
            Ok(Bytes::from_static(b"b")),
        ]))
    })
    .oneshot(Request::get("/").body(Body::empty()).unwrap())
    .await
    .unwrap();
    assert_eq!(admission.admitted("management"), 1);

    let body = response.into_body().collect().await.unwrap().to_bytes();
    assert_eq!(body, "ab");
    assert_eq!(admission.admitted("management"), 0);

    let response = app(admission.clone(), || Body::from("unread"))
        .oneshot(Request::get("/").body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(admission.admitted("management"), 1);
    drop(response);
    assert_metrics_zero(&admission);
}

#[tokio::test]
async fn dropping_partially_read_body_releases_permit() {
    let admission = PublicAdmission::new(1, 1);
    let response = app(admission.clone(), || {
        Body::from_stream(stream::iter([
            Ok::<_, Infallible>(Bytes::from_static(b"a")),
            Ok(Bytes::from_static(b"b")),
        ]))
    })
    .oneshot(Request::get("/").body(Body::empty()).unwrap())
    .await
    .unwrap();
    let mut body = response.into_body();
    assert_eq!(
        body.frame().await.unwrap().unwrap().into_data().unwrap(),
        "a"
    );
    assert_eq!(admission.admitted("management"), 1);
    drop(body);
    assert_metrics_zero(&admission);
}

#[tokio::test]
async fn terminal_body_error_releases_permit() {
    let admission = PublicAdmission::new(1, 1);
    let response = app(admission.clone(), || {
        Body::from_stream(stream::once(async {
            Err::<Bytes, _>(std::io::Error::other("terminal test error"))
        }))
    })
    .oneshot(Request::get("/").body(Body::empty()).unwrap())
    .await
    .unwrap();
    assert_eq!(admission.admitted("management"), 1);
    assert!(response.into_body().collect().await.is_err());
    assert_metrics_zero(&admission);
}

#[tokio::test]
async fn independent_pools_reject_without_invoking_saturated_handler() {
    let admission = PublicAdmission::new(1, 1);
    let provider_transport_invoked = Arc::new(AtomicBool::new(false));
    let provider_transport_invoked_for_handler = Arc::clone(&provider_transport_invoked);
    let app = Router::new()
        .route(
            "/openai/v1/chat/completions",
            post(move || {
                provider_transport_invoked_for_handler.store(true, Ordering::Release);
                async { "provider" }
            }),
        )
        .route("/api/v1/test", get(|| async { "management" }))
        .layer(middleware::from_fn_with_state(
            PublicAdmissionMiddleware::new(admission.clone(), true),
            admit_public_request,
        ));

    let inference_hold = admission
        .try_acquire(AdmissionSurface::Inference)
        .expect("inference capacity");
    let rejected = app
        .clone()
        .oneshot(
            Request::post("/openai/v1/chat/completions")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(rejected.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(rejected.headers()[header::RETRY_AFTER], RETRY_AFTER_SECONDS);
    assert_eq!(rejected.headers()[header::CONTENT_TYPE], "application/json");
    assert!(!provider_transport_invoked.load(Ordering::Acquire));
    let rejection: serde_json::Value =
        serde_json::from_slice(&rejected.into_body().collect().await.unwrap().to_bytes()).unwrap();
    assert_eq!(rejection["error"]["code"], "request_admission_overloaded");

    let management = app
        .clone()
        .oneshot(Request::get("/api/v1/test").body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(management.status(), StatusCode::OK);
    drop(management);

    let management_hold = admission
        .try_acquire(AdmissionSurface::Management)
        .expect("management capacity");
    let rejected = app
        .oneshot(Request::get("/api/v1/test").body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(rejected.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(
        rejected.headers().get(header::RETRY_AFTER).unwrap(),
        RETRY_AFTER_SECONDS
    );
    assert_eq!(
        rejected.headers()[header::CONTENT_TYPE],
        "application/problem+json"
    );
    let rejection: serde_json::Value =
        serde_json::from_slice(&rejected.into_body().collect().await.unwrap().to_bytes()).unwrap();
    assert_eq!(rejection["status"], 503);
    assert_eq!(
        rejection["type"],
        "https://openllmproxy.dev/problems/request_admission_overloaded"
    );
    drop(management_hold);
    drop(inference_hold);
    assert_metrics_zero(&admission);
}

#[tokio::test]
async fn one_http2_connection_cannot_exceed_request_budget_and_shutdown_drains() {
    let admission = PublicAdmission::new(1, 1);
    let app = Router::new()
        .route(
            "/stream",
            get(|| async {
                Response::new(Body::from_stream(stream::pending::<
                    Result<Bytes, Infallible>,
                >()))
            }),
        )
        .layer(middleware::from_fn_with_state(
            PublicAdmissionMiddleware::new(admission.clone(), false),
            admit_public_request,
        ));
    let listener_socket = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener_socket.local_addr().unwrap();
    let (shutdown, shutdown_receiver) = watch::channel(false);
    let server = tokio::spawn(listener::serve_http(
        listener_socket,
        app,
        listener::HttpServerConfig::standard(8),
        shutdown_receiver,
    ));

    let stream = TcpStream::connect(address).await.unwrap();
    let (mut sender, connection) =
        hyper::client::conn::http2::handshake(TokioExecutor::new(), TokioIo::new(stream))
            .await
            .unwrap();
    let connection = tokio::spawn(connection);
    let first = sender
        .send_request(
            Request::get("http://localhost/stream")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(first.status(), StatusCode::OK);
    assert_eq!(admission.admitted("management"), 1);

    let second = sender
        .send_request(
            Request::get("http://localhost/stream")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(second.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(admission.admitted("management"), 1);
    second.into_body().collect().await.unwrap();

    shutdown.send(true).unwrap();
    tokio::time::sleep(Duration::from_millis(20)).await;
    assert_eq!(
        admission.admitted("management"),
        1,
        "shutdown must not release an active response permit"
    );
    drop(first);
    drop(sender);
    tokio::time::timeout(Duration::from_secs(2), server)
        .await
        .expect("graceful shutdown must not deadlock")
        .unwrap()
        .unwrap();
    connection.await.unwrap().unwrap();
    assert_metrics_zero(&admission);
}

#[tokio::test]
async fn observability_is_independent_while_both_public_pools_are_saturated() {
    let mut state = ProcessComposition::new(
        ApiMode::All,
        None,
        Arc::new(RuntimeManager::empty()),
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    );
    state.set_public_admission_limits(1, 1);
    let observability_state = state.observability_state_for_test();
    let admission = observability_state.public_admission.clone();
    let inference = admission
        .try_acquire(AdmissionSurface::Inference)
        .expect("inference permit");
    let management = admission
        .try_acquire(AdmissionSurface::Management)
        .expect("management permit");

    let app = observability_router(observability_state);
    let response = app
        .clone()
        .oneshot(Request::get("/health/live").body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    drop(response);
    let response = app
        .clone()
        .oneshot(Request::get("/metrics").body(Body::empty()).unwrap())
        .await
        .unwrap();
    let metrics = String::from_utf8(
        response
            .into_body()
            .collect()
            .await
            .unwrap()
            .to_bytes()
            .to_vec(),
    )
    .unwrap();
    assert!(metrics.contains("olp_http_admitted_requests{surface=\"inference\"} 1"));
    assert!(metrics.contains("olp_http_admitted_requests{surface=\"management\"} 1"));
    drop(inference);
    drop(management);
    let response = app
        .oneshot(Request::get("/metrics").body(Body::empty()).unwrap())
        .await
        .unwrap();
    let metrics = String::from_utf8(
        response
            .into_body()
            .collect()
            .await
            .unwrap()
            .to_bytes()
            .to_vec(),
    )
    .unwrap();
    assert!(metrics.contains("olp_http_admitted_requests{surface=\"inference\"} 0"));
    assert!(metrics.contains("olp_http_admitted_requests{surface=\"management\"} 0"));
    assert_metrics_zero(&admission);
}

fn assert_metrics_zero(admission: &PublicAdmission) {
    assert_eq!(admission.admitted("inference"), 0);
    assert_eq!(admission.admitted("management"), 0);
    let metrics = admission.metrics();
    assert!(metrics.contains("olp_http_admitted_requests{surface=\"inference\"} 0"));
    assert!(metrics.contains("olp_http_admitted_requests{surface=\"management\"} 0"));
}

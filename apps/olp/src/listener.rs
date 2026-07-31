//! Hardened HTTP listener shared by the public and observability sockets.
//!
//! Axum's convenience server deliberately does not expose connection-level
//! controls. Keeping them here makes the resource envelope explicit and makes
//! both listeners follow the same shutdown semantics.

use std::{
    io,
    net::SocketAddr,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

use axum::{Router, body::Body};
use hyper_util::{
    rt::{TokioExecutor, TokioIo, TokioTimer},
    server::conn::auto::Builder,
    service::TowerToHyperService,
};
use tokio::{
    net::TcpListener,
    sync::{Notify, Semaphore, watch},
    task::JoinSet,
};
use tower::ServiceExt as _;
use tracing::{debug, warn};

const HTTP1_MAX_HEADERS: usize = crate::MAX_HTTP_HEADER_COUNT;
const HTTP1_MAX_HEADER_BYTES: usize = crate::MAX_HTTP_HEADER_BYTES;
const HTTP2_MAX_CONCURRENT_STREAMS: u32 = 100;
const HTTP2_MAX_HEADER_LIST_BYTES: u32 = 32 * 1024;
const HTTP2_MAX_PENDING_RESET_STREAMS: usize = 32;
const HTTP2_MAX_LOCAL_ERROR_RESET_STREAMS: usize = 32;
const HTTP_CONNECTION_DRAIN_TIMEOUT: Duration = Duration::from_secs(30);

/// Per-listener controls applied before a request reaches Axum.
#[derive(Clone, Copy, Debug)]
pub(crate) struct HttpServerConfig {
    max_connections: usize,
    http1_header_timeout: Duration,
}

impl HttpServerConfig {
    pub(crate) fn standard(max_connections: usize) -> Self {
        Self {
            max_connections,
            http1_header_timeout: Duration::from_secs(10),
        }
    }

    #[cfg(test)]
    fn for_test(max_connections: usize, http1_header_timeout: Duration) -> Self {
        Self {
            max_connections,
            http1_header_timeout,
        }
    }
}

/// Accept and serve a listener with bounded connection admission.
///
/// A full semaphore is load shedding rather than a queue: the accepted socket
/// is immediately dropped. On shutdown the listener stops accepting, every
/// active connection begins Hyper's graceful drain, and this function resolves
/// only once those connection tasks have exited.
pub(crate) async fn serve_http(
    listener: TcpListener,
    router: Router,
    config: HttpServerConfig,
    mut shutdown: watch::Receiver<bool>,
) -> io::Result<()> {
    if config.max_connections == 0 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "HTTP max connections must be greater than zero",
        ));
    }

    let make_service = router.into_make_service_with_connect_info::<SocketAddr>();
    let permits = Arc::new(Semaphore::new(config.max_connections));
    let mut connections = JoinSet::new();

    loop {
        tokio::select! {
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    break;
                }
            }
            Some(result) = connections.join_next(), if !connections.is_empty() => {
                if let Err(error) = result {
                    warn!(%error, "HTTP connection task stopped unexpectedly");
                }
            }
            accepted = listener.accept() => {
                let (stream, peer) = match accepted {
                    Ok(connection) => connection,
                    Err(error) => {
                        // A transient descriptor exhaustion must not take down a
                        // healthy process. Leave shutdown responsive while backing
                        // off before the next accept attempt.
                        warn!(%error, "HTTP accept failed");
                        tokio::select! {
                            () = tokio::time::sleep(Duration::from_secs(1)) => {},
                            changed = shutdown.changed() => {
                                if changed.is_err() || *shutdown.borrow() {
                                    break;
                                }
                            }
                        }
                        continue;
                    }
                };
                let Ok(permit) = Arc::clone(&permits).try_acquire_owned() else {
                    debug!(%peer, max_connections = config.max_connections, "dropping connection above HTTP admission cap");
                    drop(stream);
                    continue;
                };

                let service = make_service.clone();
                let connection_shutdown = shutdown.clone();
                connections.spawn(async move {
                    // The permit must outlive the complete HTTP connection,
                    // including HTTP/2 streams and graceful draining.
                    let _permit = permit;
                    serve_connection(
                        stream,
                        peer,
                        service,
                        config,
                        connection_shutdown,
                    )
                    .await;
                });
            }
        }
    }

    drop(listener);
    while let Some(result) = connections.join_next().await {
        if let Err(error) = result {
            warn!(%error, "HTTP connection task stopped unexpectedly during drain");
        }
    }
    Ok(())
}

async fn serve_connection(
    stream: tokio::net::TcpStream,
    peer: SocketAddr,
    make_service: axum::extract::connect_info::IntoMakeServiceWithConnectInfo<Router, SocketAddr>,
    config: HttpServerConfig,
    mut shutdown: watch::Receiver<bool>,
) {
    let tower_service = match make_service.oneshot(peer).await {
        Ok(service) => service
            .map_request(|request: hyper::Request<hyper::body::Incoming>| request.map(Body::new)),
        Err(never) => match never {},
    };
    // `hyper_util`'s auto protocol detector reads far enough to distinguish
    // the HTTP/2 prior-knowledge preface from HTTP/1. That happens before
    // Hyper installs the HTTP/1 header timer, so a silent socket (or a peer
    // that trickles a prefix of `PRI * HTTP/2.0...`) would otherwise retain a
    // connection permit indefinitely. Signal when the first request reaches
    // Axum and enforce the same absolute deadline until then.
    let first_request_observed = Arc::new(AtomicBool::new(false));
    let first_request_notify = Arc::new(Notify::new());
    let service_first_request_notify = Arc::clone(&first_request_notify);
    let observed_service = tower::service_fn(move |request| {
        let service = tower_service.clone();
        let first_request_observed = Arc::clone(&first_request_observed);
        let first_request_notify = Arc::clone(&service_first_request_notify);
        async move {
            if !first_request_observed.swap(true, Ordering::AcqRel) {
                first_request_notify.notify_one();
            }
            service.oneshot(request).await
        }
    });
    let hyper_service = TowerToHyperService::new(observed_service);
    let mut builder = Builder::new(TokioExecutor::new());
    builder
        .http1()
        .max_headers(HTTP1_MAX_HEADERS)
        .max_buf_size(HTTP1_MAX_HEADER_BYTES)
        .timer(TokioTimer::new())
        .header_read_timeout(Some(config.http1_header_timeout));
    builder
        .http2()
        .max_concurrent_streams(HTTP2_MAX_CONCURRENT_STREAMS)
        .max_header_list_size(HTTP2_MAX_HEADER_LIST_BYTES)
        .max_pending_accept_reset_streams(Some(HTTP2_MAX_PENDING_RESET_STREAMS))
        .max_local_error_reset_streams(Some(HTTP2_MAX_LOCAL_ERROR_RESET_STREAMS))
        .timer(TokioTimer::new());

    let connection = builder.serve_connection_with_upgrades(TokioIo::new(stream), hyper_service);
    tokio::pin!(connection);
    let first_request_deadline = tokio::time::sleep(config.http1_header_timeout);
    tokio::pin!(first_request_deadline);
    let mut draining = false;
    let mut drain_deadline = None;
    let mut first_request_seen = false;
    loop {
        tokio::select! {
            result = connection.as_mut() => {
                if let Err(error) = result {
                    debug!(%peer, %error, "HTTP connection closed with protocol error");
                }
                return;
            }
            () = first_request_notify.notified(), if !first_request_seen => {
                first_request_seen = true;
            }
            () = &mut first_request_deadline, if !first_request_seen => {
                debug!(%peer, "closing connection before its first request exceeded the header/protocol deadline");
                return;
            }
            changed = shutdown.changed(), if !draining => {
                if changed.is_err() || *shutdown.borrow() {
                    draining = true;
                    connection.as_mut().graceful_shutdown();
                    drain_deadline = Some(Box::pin(tokio::time::sleep(HTTP_CONNECTION_DRAIN_TIMEOUT)));
                }
            }
            () = async {
                if let Some(deadline) = drain_deadline.as_mut() {
                    deadline.await;
                }
            }, if draining => {
                warn!(%peer, "forcing close after HTTP connection drain deadline");
                return;
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use std::{
        sync::{
            Arc,
            atomic::{AtomicBool, Ordering},
        },
        time::Duration,
    };

    use axum::{Router, routing::get};
    use tokio::{
        io::{AsyncReadExt as _, AsyncWriteExt as _},
        net::TcpStream,
        sync::{Notify, watch},
    };

    use super::*;

    async fn test_server(
        max_connections: usize,
        header_timeout: Duration,
    ) -> (
        SocketAddr,
        watch::Sender<bool>,
        tokio::task::JoinHandle<io::Result<()>>,
    ) {
        test_server_with_router(
            max_connections,
            header_timeout,
            Router::new().route("/", get(|| async { "ok" })),
        )
        .await
    }

    async fn test_server_with_router(
        max_connections: usize,
        header_timeout: Duration,
        app: Router,
    ) -> (
        SocketAddr,
        watch::Sender<bool>,
        tokio::task::JoinHandle<io::Result<()>>,
    ) {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let (shutdown_sender, shutdown_receiver) = watch::channel(false);
        let task = tokio::spawn(serve_http(
            listener,
            app,
            HttpServerConfig::for_test(max_connections, header_timeout),
            shutdown_receiver,
        ));
        (address, shutdown_sender, task)
    }

    #[tokio::test]
    async fn slow_http1_headers_are_closed_at_the_deadline() {
        let (address, shutdown, task) = test_server(4, Duration::from_millis(40)).await;
        let mut stream = TcpStream::connect(address).await.unwrap();
        stream
            .write_all(b"GET / HTTP/1.1\r\nHost: example")
            .await
            .unwrap();
        let mut byte = [0_u8; 1];
        let read = tokio::time::timeout(Duration::from_secs(1), stream.read(&mut byte)).await;
        assert!(matches!(read, Ok(Ok(0))));
        let _ = shutdown.send(true);
        task.await.unwrap().unwrap();
    }

    #[tokio::test]
    async fn initial_protocol_negotiation_has_the_header_deadline() {
        let (address, shutdown, task) = test_server(4, Duration::from_millis(40)).await;

        // No bytes means the auto-protocol detector cannot choose HTTP/1 or
        // HTTP/2. It still must release the connection permit promptly.
        let mut silent = TcpStream::connect(address).await.unwrap();
        let mut byte = [0_u8; 1];
        let read = tokio::time::timeout(Duration::from_secs(1), silent.read(&mut byte)).await;
        assert!(matches!(read, Ok(Ok(0))));

        // A partial HTTP/2 prior-knowledge preface exercises the detector's
        // otherwise-unbounded `ReadVersion` stage.
        let mut partial_h2 = TcpStream::connect(address).await.unwrap();
        partial_h2
            .write_all(b"PRI * HTTP/2.0\r\n\r\nSM")
            .await
            .unwrap();
        let read = tokio::time::timeout(Duration::from_secs(1), partial_h2.read(&mut byte)).await;
        assert!(matches!(read, Ok(Ok(0))));

        let _ = shutdown.send(true);
        task.await.unwrap().unwrap();
    }

    #[tokio::test]
    async fn oversized_http1_headers_are_rejected_before_reaching_axum() {
        let reached_axum = Arc::new(AtomicBool::new(false));
        let app = Router::new().route(
            "/",
            get({
                let reached_axum = Arc::clone(&reached_axum);
                move || {
                    let reached_axum = Arc::clone(&reached_axum);
                    async move {
                        reached_axum.store(true, Ordering::Release);
                        "ok"
                    }
                }
            }),
        );
        let (address, shutdown, task) =
            test_server_with_router(4, Duration::from_secs(1), app).await;
        let mut stream = TcpStream::connect(address).await.unwrap();
        let request = format!(
            "GET / HTTP/1.1\r\nHost: example\r\nX-Padding: {}\r\n\r\n",
            "a".repeat(HTTP1_MAX_HEADER_BYTES)
        );
        stream.write_all(request.as_bytes()).await.unwrap();
        let mut response = Vec::new();
        tokio::time::timeout(Duration::from_secs(1), stream.read_to_end(&mut response))
            .await
            .expect("an oversized header must be rejected promptly")
            .unwrap();
        // Hyper may choose an HTTP parser error response rather than a bare
        // EOF. The invariant is that the request never reaches Axum.
        assert!(!reached_axum.load(Ordering::Acquire));
        assert!(!response.starts_with(b"HTTP/1.1 200"));
        let _ = shutdown.send(true);
        task.await.unwrap().unwrap();
    }

    #[tokio::test]
    async fn http1_connection_can_be_reused() {
        let (address, shutdown, task) = test_server(1, Duration::from_secs(1)).await;
        let mut stream = TcpStream::connect(address).await.unwrap();
        stream
            .write_all(b"GET / HTTP/1.1\r\nHost: example\r\n\r\n")
            .await
            .unwrap();
        let mut first = Vec::new();
        tokio::time::timeout(Duration::from_secs(1), async {
            let mut buffer = [0_u8; 256];
            while !first.windows(4).any(|window| window == b"\r\n\r\n") {
                let read = stream.read(&mut buffer).await.unwrap();
                assert!(read > 0);
                first.extend_from_slice(&buffer[..read]);
            }
        })
        .await
        .unwrap();

        stream
            .write_all(b"GET / HTTP/1.1\r\nHost: example\r\nConnection: close\r\n\r\n")
            .await
            .unwrap();
        let mut second = Vec::new();
        stream.read_to_end(&mut second).await.unwrap();
        first.extend(second);
        assert_eq!(
            first
                .windows(b"HTTP/1.1 200".len())
                .filter(|window| *window == b"HTTP/1.1 200")
                .count(),
            2
        );

        let _ = shutdown.send(true);
        task.await.unwrap().unwrap();
    }

    #[tokio::test]
    async fn connection_cap_sheds_instead_of_queuing() {
        let (address, shutdown, task) = test_server(1, Duration::from_secs(1)).await;
        let first = TcpStream::connect(address).await.unwrap();
        let mut second = TcpStream::connect(address).await.unwrap();
        second
            .write_all(b"GET / HTTP/1.1\r\nHost: example\r\n\r\n")
            .await
            .unwrap();
        let mut byte = [0_u8; 1];
        let read = tokio::time::timeout(Duration::from_secs(1), second.read(&mut byte)).await;
        assert!(matches!(read, Ok(Ok(0))));
        drop(first);
        let mut third = TcpStream::connect(address).await.unwrap();
        third
            .write_all(b"GET / HTTP/1.1\r\nHost: example\r\nConnection: close\r\n\r\n")
            .await
            .unwrap();
        let mut response = Vec::new();
        third.read_to_end(&mut response).await.unwrap();
        assert!(response.windows(6).any(|window| window == b"\r\n\r\nok"));
        let _ = shutdown.send(true);
        task.await.unwrap().unwrap();
    }

    #[tokio::test]
    async fn shutdown_drains_active_connections() {
        let entered = Arc::new(Notify::new());
        let release = Arc::new(Notify::new());
        let app = Router::new().route(
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
                        "ok"
                    }
                }
            }),
        );
        let (address, shutdown, task) =
            test_server_with_router(2, Duration::from_secs(1), app).await;
        let mut stream = TcpStream::connect(address).await.unwrap();
        stream
            .write_all(b"GET / HTTP/1.1\r\nHost: example\r\n\r\n")
            .await
            .unwrap();
        entered.notified().await;
        let _ = shutdown.send(true);
        release.notify_one();
        let mut response = Vec::new();
        stream.read_to_end(&mut response).await.unwrap();
        assert!(response.starts_with(b"HTTP/1.1 200"));
        task.await.unwrap().unwrap();
    }
}

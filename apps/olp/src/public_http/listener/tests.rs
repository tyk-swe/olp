use std::{
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

use axum::{Router, response::IntoResponse, routing::get};
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
async fn http2_connection_starts_graceful_drain_at_its_maximum_age() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (shutdown, shutdown_receiver) = watch::channel(false);
    let task = tokio::spawn(serve_http(
        listener,
        Router::new().route("/", get(|| async { "ok" })),
        HttpServerConfig {
            max_connections: 1,
            http1_header_timeout: Duration::from_secs(1),
            connection_max_age: Duration::from_millis(100),
            connection_drain_timeout: Duration::from_secs(30),
        },
        shutdown_receiver,
    ));
    let mut stream = TcpStream::connect(address).await.unwrap();
    stream
        .write_all(
            b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\
              \x00\x00\x00\x04\x00\x00\x00\x00\x00\
              \x00\x00\x03\x01\x05\x00\x00\x00\x01\x82\x86\x84",
        )
        .await
        .unwrap();
    // A graceful H2 drain sends GOAWAY but waits for the peer to close the
    // connection. It must not force EOF while a peer can still finish an
    // in-flight stream.
    tokio::time::sleep(Duration::from_millis(150)).await;
    let mut response = [0_u8; 128];
    assert!(
        tokio::time::timeout(Duration::from_secs(1), stream.read(&mut response))
            .await
            .unwrap()
            .unwrap()
            > 0
    );
    drop(stream);
    let _ = shutdown.send(true);
    task.await.unwrap().unwrap();
}

#[tokio::test]
async fn http2_connection_negotiated_after_maximum_age_is_drained() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (shutdown, shutdown_receiver) = watch::channel(false);
    let task = tokio::spawn(serve_http(
        listener,
        Router::new().route("/", get(|| async { "ok" })),
        HttpServerConfig {
            max_connections: 1,
            http1_header_timeout: Duration::from_secs(1),
            connection_max_age: Duration::from_millis(40),
            connection_drain_timeout: Duration::from_millis(40),
        },
        shutdown_receiver,
    ));
    let mut stream = TcpStream::connect(address).await.unwrap();
    stream.write_all(b"PRI * HTTP/2.0\r\n\r\nSM").await.unwrap();
    tokio::time::sleep(Duration::from_millis(80)).await;
    stream
        .write_all(
            b"\r\n\r\n\
              \x00\x00\x00\x04\x00\x00\x00\x00\x00\
              \x00\x00\x03\x01\x05\x00\x00\x00\x01\x82\x86\x84",
        )
        .await
        .unwrap();
    let mut response = Vec::new();
    tokio::time::timeout(Duration::from_secs(1), stream.read_to_end(&mut response))
        .await
        .expect("an HTTP/2 connection negotiated after maximum age must be drained")
        .unwrap();
    let _ = shutdown.send(true);
    task.await.unwrap().unwrap();
}

#[tokio::test]
async fn http1_streaming_response_is_not_cut_by_connection_age() {
    let app = Router::new().route(
        "/",
        get(|| async {
            let chunks = futures::stream::unfold(0_u32, |index| async move {
                if index == 8 {
                    return None;
                }
                tokio::time::sleep(Duration::from_millis(40)).await;
                Some((
                    Ok::<_, std::convert::Infallible>(format!("chunk{index}\n")),
                    index + 1,
                ))
            });
            Body::from_stream(chunks).into_response()
        }),
    );
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (shutdown, shutdown_receiver) = watch::channel(false);
    let task = tokio::spawn(serve_http(
        listener,
        app,
        HttpServerConfig {
            max_connections: 1,
            http1_header_timeout: Duration::from_secs(1),
            connection_max_age: Duration::from_millis(50),
            connection_drain_timeout: Duration::from_millis(20),
        },
        shutdown_receiver,
    ));
    let mut stream = TcpStream::connect(address).await.unwrap();
    stream
        .write_all(b"GET / HTTP/1.1\r\nHost: example\r\n\r\n")
        .await
        .unwrap();
    let mut response = Vec::new();
    stream.read_to_end(&mut response).await.unwrap();
    assert!(response.starts_with(b"HTTP/1.1 200"));
    for index in 0..8 {
        let chunk = format!("chunk{index}\n");
        assert!(
            response
                .windows(chunk.len())
                .any(|window| window == chunk.as_bytes()),
            "missing {chunk:?}"
        );
    }
    let _ = shutdown.send(true);
    task.await.unwrap().unwrap();
}

#[tokio::test]
async fn idle_http2_connection_closes_after_the_drain_timeout() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (shutdown, shutdown_receiver) = watch::channel(false);
    let task = tokio::spawn(serve_http(
        listener,
        Router::new().route("/", get(|| async { "ok" })),
        HttpServerConfig {
            max_connections: 1,
            http1_header_timeout: Duration::from_secs(5),
            connection_max_age: Duration::from_millis(50),
            connection_drain_timeout: Duration::from_millis(50),
        },
        shutdown_receiver,
    ));
    let mut stream = TcpStream::connect(address).await.unwrap();
    stream
        .write_all(
            b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\
              \x00\x00\x00\x04\x00\x00\x00\x00\x00",
        )
        .await
        .unwrap();
    let mut response = Vec::new();
    tokio::time::timeout(Duration::from_secs(2), stream.read_to_end(&mut response))
        .await
        .expect("an idle HTTP/2 connection must be closed after the drain timeout")
        .unwrap();
    let _ = shutdown.send(true);
    task.await.unwrap().unwrap();
}

#[tokio::test]
async fn connection_age_drains_an_active_http1_request() {
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
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (shutdown, shutdown_receiver) = watch::channel(false);
    let task = tokio::spawn(serve_http(
        listener,
        app,
        HttpServerConfig {
            max_connections: 1,
            http1_header_timeout: Duration::from_secs(1),
            connection_max_age: Duration::from_millis(50),
            connection_drain_timeout: Duration::from_millis(20),
        },
        shutdown_receiver,
    ));
    let mut stream = TcpStream::connect(address).await.unwrap();
    stream
        .write_all(b"GET / HTTP/1.1\r\nHost: example\r\n\r\n")
        .await
        .unwrap();
    entered.notified().await;
    tokio::time::sleep(Duration::from_millis(100)).await;
    release.notify_one();
    let mut response = Vec::new();
    stream.read_to_end(&mut response).await.unwrap();
    assert!(response.starts_with(b"HTTP/1.1 200"));
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
    let (address, shutdown, task) = test_server_with_router(4, Duration::from_secs(1), app).await;
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
    let (address, shutdown, task) = test_server_with_router(2, Duration::from_secs(1), app).await;
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

//! Hardened HTTP listener shared by the public and observability sockets.
//!
//! Axum's convenience server deliberately does not expose connection-level
//! controls. Keeping them here makes the resource envelope explicit and makes
//! both listeners follow the same shutdown semantics.

use std::{
    future::Future as _,
    io,
    net::SocketAddr,
    pin::Pin,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    task::{Context, Poll},
    time::Duration,
};

use axum::{Router, body::Body};
use hyper_util::{
    rt::{TokioExecutor, TokioIo, TokioTimer},
    server::conn::auto::Builder,
    service::TowerToHyperService,
};
use tokio::{
    io::{AsyncRead, AsyncWrite, ReadBuf},
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
const HTTP2_MAX_CONNECTION_AGE: Duration = Duration::from_secs(5 * 60);
const HTTP_CONNECTION_DRAIN_TIMEOUT: Duration = Duration::from_secs(30);
const HTTP2_PREFACE: &[u8] = b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n";
const HTTP2_FRAME_DATA: u8 = 0;
const HTTP2_FRAME_HEADERS: u8 = 1;
const HTTP2_FRAME_PUSH_PROMISE: u8 = 5;
const HTTP2_FRAME_CONTINUATION: u8 = 9;
const HTTP2_FLAG_END_HEADERS: u8 = 0x4;

/// Hyper's HTTP/1 `max_buf_size` protects its parser, but it may read beyond
/// the configured threshold before checking it. This wrapper makes the
/// externally visible header limit exact, independent of that read-ahead.
/// It also observes HTTP/2 frame boundaries so an incomplete frame cannot
/// wait forever inside h2 before service dispatch. Header blocks retain one
/// absolute deadline across HEADERS / PUSH_PROMISE / CONTINUATION frames.
struct HeaderLimitedStream {
    inner: tokio::net::TcpStream,
    initial_header_bytes: usize,
    terminator_match_bytes: u8,
    initial_header_complete: bool,
    header_timeout: Duration,
    h2_preface_bytes: usize,
    h2_possible: bool,
    h2_frame_header: [u8; 9],
    h2_frame_header_bytes: usize,
    h2_frame_payload_remaining: usize,
    h2_frame_is_header: bool,
    h2_frame_ends_headers: bool,
    h2_header_block_open: bool,
    h2_frame_deadline: Pin<Box<tokio::time::Sleep>>,
    h2_frame_deadline_active: bool,
}

impl HeaderLimitedStream {
    fn new(inner: tokio::net::TcpStream, header_timeout: Duration) -> Self {
        Self {
            inner,
            initial_header_bytes: 0,
            terminator_match_bytes: 0,
            initial_header_complete: false,
            header_timeout,
            h2_preface_bytes: 0,
            h2_possible: true,
            h2_frame_header: [0; 9],
            h2_frame_header_bytes: 0,
            h2_frame_payload_remaining: 0,
            h2_frame_is_header: false,
            h2_frame_ends_headers: false,
            h2_header_block_open: false,
            h2_frame_deadline: Box::pin(tokio::time::sleep(header_timeout)),
            h2_frame_deadline_active: false,
        }
    }

    fn observe_initial_bytes(&mut self, bytes: &[u8]) -> io::Result<()> {
        if self.initial_header_complete {
            return Ok(());
        }
        for &byte in bytes {
            self.initial_header_bytes = self.initial_header_bytes.saturating_add(1);
            if self.initial_header_bytes > HTTP1_MAX_HEADER_BYTES {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "HTTP/1 headers exceed the 32 KiB limit",
                ));
            }
            self.terminator_match_bytes = match (self.terminator_match_bytes, byte) {
                (0, b'\r') | (1 | 3, b'\r') => 1,
                (1, b'\n') => 2,
                (2, b'\r') => 3,
                (3, b'\n') => 4,
                _ => 0,
            };
            if self.terminator_match_bytes == 4 {
                self.initial_header_complete = true;
                return Ok(());
            }
        }
        Ok(())
    }

    fn observe_http2_bytes(&mut self, bytes: &[u8]) {
        let mut bytes = bytes;
        while self.h2_possible && self.h2_preface_bytes < HTTP2_PREFACE.len() && !bytes.is_empty() {
            if bytes[0] != HTTP2_PREFACE[self.h2_preface_bytes] {
                self.h2_possible = false;
                return;
            }
            self.h2_preface_bytes += 1;
            bytes = &bytes[1..];
        }
        if !self.h2_possible || self.h2_preface_bytes < HTTP2_PREFACE.len() {
            return;
        }

        while !bytes.is_empty() {
            if self.h2_frame_payload_remaining > 0 {
                let consumed = self.h2_frame_payload_remaining.min(bytes.len());
                self.h2_frame_payload_remaining -= consumed;
                bytes = &bytes[consumed..];
                if self.h2_frame_payload_remaining == 0 {
                    self.finish_http2_frame();
                }
                continue;
            }

            if self.h2_frame_header_bytes == 0 {
                self.start_http2_frame_deadline();
            }
            let consumed = (9 - self.h2_frame_header_bytes).min(bytes.len());
            self.h2_frame_header[self.h2_frame_header_bytes..self.h2_frame_header_bytes + consumed]
                .copy_from_slice(&bytes[..consumed]);
            self.h2_frame_header_bytes += consumed;
            bytes = &bytes[consumed..];

            if self.h2_frame_header_bytes < 9 {
                continue;
            }

            let frame_kind = self.h2_frame_header[3];
            let flags = self.h2_frame_header[4];
            self.h2_frame_payload_remaining = (usize::from(self.h2_frame_header[0]) << 16)
                | (usize::from(self.h2_frame_header[1]) << 8)
                | usize::from(self.h2_frame_header[2]);
            self.h2_frame_is_header =
                http2_frame_is_header_block(frame_kind) || self.h2_header_block_open;
            self.h2_frame_ends_headers =
                http2_frame_is_header_block(frame_kind) && flags & HTTP2_FLAG_END_HEADERS != 0;
            if http2_frame_is_header_block(frame_kind) {
                self.h2_header_block_open = true;
            }
            self.h2_frame_header_bytes = 0;
            if frame_kind == HTTP2_FRAME_DATA
                && !self.h2_frame_is_header
                && self.h2_frame_payload_remaining > 0
            {
                self.h2_frame_deadline
                    .as_mut()
                    .reset(tokio::time::Instant::now() + crate::router::REQUEST_BODY_TIMEOUT);
            }
            if self.h2_frame_payload_remaining == 0 {
                self.finish_http2_frame();
            }
        }
    }

    fn start_http2_frame_deadline(&mut self) {
        if !self.h2_frame_deadline_active {
            self.h2_frame_deadline
                .as_mut()
                .reset(tokio::time::Instant::now() + self.header_timeout);
            self.h2_frame_deadline_active = true;
        }
    }

    fn finish_http2_frame(&mut self) {
        if self.h2_frame_is_header && self.h2_frame_ends_headers {
            self.h2_header_block_open = false;
            self.h2_frame_deadline_active = false;
        } else if !self.h2_header_block_open {
            self.h2_frame_deadline_active = false;
        }
        self.h2_frame_is_header = false;
        self.h2_frame_ends_headers = false;
    }

    fn poll_http2_frame_deadline(&mut self, context: &mut Context<'_>) -> io::Result<()> {
        if self.h2_frame_deadline_active && self.h2_frame_deadline.as_mut().poll(context).is_ready()
        {
            return Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "HTTP/2 frame exceeded the establishment deadline",
            ));
        }
        Ok(())
    }
}

fn http2_frame_is_header_block(frame_kind: u8) -> bool {
    matches!(
        frame_kind,
        HTTP2_FRAME_HEADERS | HTTP2_FRAME_PUSH_PROMISE | HTTP2_FRAME_CONTINUATION
    )
}

impl AsyncRead for HeaderLimitedStream {
    fn poll_read(
        mut self: Pin<&mut Self>,
        context: &mut Context<'_>,
        buffer: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        self.poll_http2_frame_deadline(context)?;
        let filled = buffer.filled().len();
        match Pin::new(&mut self.inner).poll_read(context, buffer) {
            Poll::Ready(Ok(())) => {
                let bytes = &buffer.filled()[filled..];
                self.observe_initial_bytes(bytes)?;
                self.observe_http2_bytes(bytes);
                self.poll_http2_frame_deadline(context)?;
                Poll::Ready(Ok(()))
            }
            other => other,
        }
    }
}

impl AsyncWrite for HeaderLimitedStream {
    fn poll_write(
        mut self: Pin<&mut Self>,
        context: &mut Context<'_>,
        buffer: &[u8],
    ) -> Poll<io::Result<usize>> {
        Pin::new(&mut self.inner).poll_write(context, buffer)
    }

    fn poll_flush(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.inner).poll_flush(context)
    }

    fn poll_shutdown(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.inner).poll_shutdown(context)
    }
}

/// Per-listener controls applied before a request reaches Axum.
#[derive(Clone, Copy, Debug)]
pub(crate) struct HttpServerConfig {
    max_connections: usize,
    request_header_timeout: Duration,
    connection_max_age: Duration,
    connection_drain_timeout: Duration,
}

impl HttpServerConfig {
    pub(crate) fn standard(max_connections: usize) -> Self {
        Self {
            max_connections,
            request_header_timeout: Duration::from_secs(10),
            connection_max_age: HTTP2_MAX_CONNECTION_AGE,
            connection_drain_timeout: HTTP_CONNECTION_DRAIN_TIMEOUT,
        }
    }

    #[cfg(test)]
    fn for_test(max_connections: usize, request_header_timeout: Duration) -> Self {
        Self {
            max_connections,
            request_header_timeout,
            connection_max_age: Duration::from_secs(60),
            connection_drain_timeout: HTTP_CONNECTION_DRAIN_TIMEOUT,
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
        // One request per connection keeps the exact raw-header bound in
        // HeaderLimitedStream authoritative; Hyper exposes no per-request raw
        // byte hook for later keep-alive requests.
        .keep_alive(false)
        .max_headers(HTTP1_MAX_HEADERS)
        .max_buf_size(HTTP1_MAX_HEADER_BYTES)
        .timer(TokioTimer::new())
        .header_read_timeout(Some(config.request_header_timeout));
    builder
        .http2()
        .max_concurrent_streams(HTTP2_MAX_CONCURRENT_STREAMS)
        .max_header_list_size(HTTP2_MAX_HEADER_LIST_BYTES)
        .max_pending_accept_reset_streams(Some(HTTP2_MAX_PENDING_RESET_STREAMS))
        .max_local_error_reset_streams(Some(HTTP2_MAX_LOCAL_ERROR_RESET_STREAMS))
        .timer(TokioTimer::new());

    let connection = builder.serve_connection_with_upgrades(
        TokioIo::new(HeaderLimitedStream::new(
            stream,
            config.request_header_timeout,
        )),
        hyper_service,
    );
    tokio::pin!(connection);
    let first_request_deadline = tokio::time::sleep(config.request_header_timeout);
    tokio::pin!(first_request_deadline);
    let connection_deadline = tokio::time::sleep(config.connection_max_age);
    tokio::pin!(connection_deadline);
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
            () = &mut connection_deadline, if !draining => {
                debug!(%peer, "gracefully draining connection at its maximum age");
                draining = true;
                connection.as_mut().graceful_shutdown();
                drain_deadline = Some(Box::pin(tokio::time::sleep(config.connection_drain_timeout)));
            }
            changed = shutdown.changed(), if drain_deadline.is_none() => {
                if changed.is_err() || *shutdown.borrow() {
                    draining = true;
                    connection.as_mut().graceful_shutdown();
                    drain_deadline = Some(Box::pin(tokio::time::sleep(config.connection_drain_timeout)));
                }
            }
            () = async {
                if let Some(deadline) = drain_deadline.as_mut() {
                    deadline.await;
                }
            }, if drain_deadline.is_some() => {
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

    use axum::{
        Router,
        body::Bytes,
        routing::{get, post},
    };
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
    async fn later_http2_header_block_has_the_request_header_deadline() {
        let entered = Arc::new(Notify::new());
        let app = Router::new().route(
            "/",
            get({
                let entered = Arc::clone(&entered);
                move || {
                    let entered = Arc::clone(&entered);
                    async move {
                        entered.notify_one();
                        "ok"
                    }
                }
            }),
        );
        let (address, shutdown, task) =
            test_server_with_router(4, Duration::from_millis(80), app).await;
        let mut stream = TcpStream::connect(address).await.unwrap();
        stream
            .write_all(
                b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\
                  \x00\x00\x00\x04\x00\x00\x00\x00\x00\
                  \x00\x00\x03\x01\x05\x00\x00\x00\x01\x82\x86\x84",
            )
            .await
            .unwrap();
        tokio::time::timeout(Duration::from_secs(1), entered.notified())
            .await
            .expect("the complete first request must reach Axum");

        // A timely continuation remains valid.
        stream
            .write_all(
                b"\x00\x00\x01\x01\x01\x00\x00\x00\x03\x82\
                  \x00\x00\x02\x09\x04\x00\x00\x00\x03\x86\x84",
            )
            .await
            .unwrap();
        tokio::time::timeout(Duration::from_secs(1), entered.notified())
            .await
            .expect("a completed continuation block must reach Axum");

        // Stream 5 supplies HEADERS without END_HEADERS, then never supplies
        // the required CONTINUATION.
        stream
            .write_all(b"\x00\x00\x01\x01\x01\x00\x00\x00\x05\x82")
            .await
            .unwrap();
        let mut response = Vec::new();
        tokio::time::timeout(Duration::from_secs(1), stream.read_to_end(&mut response))
            .await
            .expect("the partial header block must not retain the connection")
            .unwrap();

        let _ = shutdown.send(true);
        task.await.unwrap().unwrap();
    }

    #[tokio::test]
    async fn later_http2_frames_have_an_establishment_deadline() {
        let entered = Arc::new(Notify::new());
        let app = Router::new().route(
            "/",
            get({
                let entered = Arc::clone(&entered);
                move || {
                    let entered = Arc::clone(&entered);
                    async move {
                        entered.notify_one();
                        "ok"
                    }
                }
            }),
        );
        let (address, shutdown, task) =
            test_server_with_router(4, Duration::from_millis(80), app).await;

        for stalled_frame in [
            b"\x00\x00\x00\x04".as_slice(),
            b"\x00\x00\x01\x04\x00\x00\x00\x00\x00".as_slice(),
        ] {
            let mut stream = TcpStream::connect(address).await.unwrap();
            stream
                .write_all(
                    b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\
                      \x00\x00\x00\x04\x00\x00\x00\x00\x00\
                      \x00\x00\x03\x01\x05\x00\x00\x00\x01\x82\x86\x84",
                )
                .await
                .unwrap();
            tokio::time::timeout(Duration::from_secs(1), entered.notified())
                .await
                .expect("the complete first request must reach Axum");

            stream.write_all(stalled_frame).await.unwrap();
            let mut response = Vec::new();
            tokio::time::timeout(Duration::from_secs(1), stream.read_to_end(&mut response))
                .await
                .expect("the partial HTTP/2 frame must not retain the connection")
                .unwrap();
        }

        let _ = shutdown.send(true);
        task.await.unwrap().unwrap();
    }

    #[tokio::test]
    async fn valid_http2_data_uses_the_request_body_deadline() {
        let entered = Arc::new(Notify::new());
        let app = Router::new().route(
            "/",
            post({
                let entered = Arc::clone(&entered);
                move |body: Bytes| {
                    let entered = Arc::clone(&entered);
                    async move {
                        assert_eq!(body.as_ref(), b"x");
                        entered.notify_one();
                        "ok"
                    }
                }
            }),
        );
        let (address, shutdown, task) =
            test_server_with_router(4, Duration::from_millis(80), app).await;
        let mut stream = TcpStream::connect(address).await.unwrap();
        stream
            .write_all(
                b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\
                  \x00\x00\x00\x04\x00\x00\x00\x00\x00\
                  \x00\x00\x03\x01\x04\x00\x00\x00\x01\x83\x86\x84\
                  \x00\x00\x01\x00\x01\x00\x00\x00\x01",
            )
            .await
            .unwrap();

        tokio::time::sleep(Duration::from_millis(150)).await;
        stream.write_all(b"x").await.unwrap();
        tokio::time::timeout(Duration::from_secs(1), entered.notified())
            .await
            .expect("a valid DATA payload may use the longer request body deadline");

        drop(stream);
        let _ = shutdown.send(true);
        task.await.unwrap().unwrap();
    }

    #[tokio::test]
    async fn http2_connection_is_forced_closed_after_max_age_drain_timeout() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let (shutdown, shutdown_receiver) = watch::channel(false);
        let task = tokio::spawn(serve_http(
            listener,
            Router::new().route("/", post(|_: Bytes| async { "ok" })),
            HttpServerConfig {
                max_connections: 1,
                request_header_timeout: Duration::from_secs(1),
                connection_max_age: Duration::from_millis(100),
                connection_drain_timeout: Duration::from_millis(20),
            },
            shutdown_receiver,
        ));
        let mut stream = TcpStream::connect(address).await.unwrap();
        stream
            .write_all(
                b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\
                  \x00\x00\x00\x04\x00\x00\x00\x00\x00\
                  \x00\x00\x03\x01\x04\x00\x00\x00\x01\x83\x86\x84",
            )
            .await
            .unwrap();
        let mut response = Vec::new();
        tokio::time::timeout(Duration::from_secs(1), stream.read_to_end(&mut response))
            .await
            .expect("an active HTTP/2 stream must not retain the connection permit")
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
                request_header_timeout: Duration::from_secs(1),
                connection_max_age: Duration::from_millis(50),
                connection_drain_timeout: Duration::from_millis(200),
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

//! Hardened HTTP listener shared by the public and observability sockets.
//!
//! Axum's convenience server deliberately does not expose connection-level
//! controls. Keeping them here makes the resource envelope explicit and makes
//! both listeners follow the same shutdown semantics.

use std::{
    io,
    net::SocketAddr,
    pin::Pin,
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    task::{Context, Poll},
    time::Duration,
};

use axum::{
    Router,
    body::{Body, HttpBody},
};
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
    time::{Instant, Sleep},
};
use tower::ServiceExt as _;
use tracing::{debug, warn};

const HTTP1_MAX_HEADERS: usize = crate::public_http::body_limits::MAX_HTTP_HEADER_COUNT;
const HTTP1_MAX_HEADER_BYTES: usize = crate::public_http::body_limits::MAX_HTTP_HEADER_BYTES;
const HTTP2_MAX_CONCURRENT_STREAMS: u32 = 100;
const HTTP2_MAX_HEADER_LIST_BYTES: u32 = 32 * 1024;
const HTTP2_MAX_PENDING_RESET_STREAMS: usize = 32;
const HTTP2_MAX_LOCAL_ERROR_RESET_STREAMS: usize = 32;
const HTTP2_PREFACE: &[u8] = b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n";
/// Upper bound on how long a draining connection may wait for in-flight
/// response bodies before its admission permit is reclaimed by force.
pub(crate) const HTTP_CONNECTION_DRAIN_HARD_LIMIT: Duration = Duration::from_secs(60 * 60);

/// Hyper's HTTP/1 `max_buf_size` protects its parser, but it may read beyond
/// the configured threshold before checking it. This wrapper makes the
/// externally visible header limit exact, independent of that read-ahead.
/// The HTTP/2 prior-knowledge preface contains `\r\n\r\n`, so it stops tracking
/// before Hyper takes over HTTP/2 frame/header-list enforcement.
struct HeaderLimitedStream {
    inner: tokio::net::TcpStream,
    initial_header_bytes: usize,
    terminator_match_bytes: u8,
    initial_header_complete: bool,
    preface_match_bytes: Option<usize>,
    http2: Arc<AtomicBool>,
}

impl HeaderLimitedStream {
    fn new(inner: tokio::net::TcpStream, http2: Arc<AtomicBool>) -> Self {
        Self {
            inner,
            initial_header_bytes: 0,
            terminator_match_bytes: 0,
            initial_header_complete: false,
            preface_match_bytes: Some(0),
            http2,
        }
    }

    fn observe_http2_preface(&mut self, bytes: &[u8]) {
        for &byte in bytes {
            let Some(matched) = self.preface_match_bytes else {
                return;
            };
            if HTTP2_PREFACE.get(matched) != Some(&byte) {
                self.preface_match_bytes = None;
                return;
            }
            if matched + 1 == HTTP2_PREFACE.len() {
                self.http2.store(true, Ordering::Release);
                self.preface_match_bytes = None;
                return;
            }
            self.preface_match_bytes = Some(matched + 1);
        }
    }

    fn observe_initial_bytes(&mut self, bytes: &[u8]) -> io::Result<()> {
        self.observe_http2_preface(bytes);
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
}

impl AsyncRead for HeaderLimitedStream {
    fn poll_read(
        mut self: Pin<&mut Self>,
        context: &mut Context<'_>,
        buffer: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        let filled = buffer.filled().len();
        match Pin::new(&mut self.inner).poll_read(context, buffer) {
            Poll::Ready(Ok(())) => {
                let bytes = &buffer.filled()[filled..];
                self.observe_initial_bytes(bytes)?;
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
    http1_header_timeout: Duration,
    connection_max_age: Duration,
    connection_drain_timeout: Duration,
}

impl HttpServerConfig {
    pub(crate) fn standard(
        max_connections: usize,
        connection_max_age: Duration,
        connection_drain_timeout: Duration,
    ) -> Self {
        Self {
            max_connections,
            http1_header_timeout: Duration::from_secs(10),
            connection_max_age,
            connection_drain_timeout,
        }
    }

    #[cfg(test)]
    fn for_test(max_connections: usize, http1_header_timeout: Duration) -> Self {
        Self {
            max_connections,
            http1_header_timeout,
            connection_max_age: Duration::from_secs(60),
            connection_drain_timeout: Duration::from_secs(30),
        }
    }
}

struct InFlightResponse(Arc<AtomicUsize>);

impl InFlightResponse {
    fn begin(counter: &Arc<AtomicUsize>) -> Self {
        counter.fetch_add(1, Ordering::AcqRel);
        Self(Arc::clone(counter))
    }
}

impl Drop for InFlightResponse {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::AcqRel);
    }
}

struct InFlightBody {
    inner: Body,
    _in_flight: InFlightResponse,
}

impl HttpBody for InFlightBody {
    type Data = bytes::Bytes;
    type Error = axum::Error;

    fn poll_frame(
        self: Pin<&mut Self>,
        context: &mut Context<'_>,
    ) -> Poll<Option<Result<http_body::Frame<Self::Data>, Self::Error>>> {
        Pin::new(&mut self.get_mut().inner).poll_frame(context)
    }

    fn is_end_stream(&self) -> bool {
        self.inner.is_end_stream()
    }

    fn size_hint(&self) -> http_body::SizeHint {
        self.inner.size_hint()
    }
}

struct Drain {
    started: Instant,
    timeout: Duration,
    deadline: Pin<Box<Sleep>>,
}

impl Drain {
    fn start(timeout: Duration) -> Self {
        let started = Instant::now();
        Self {
            started,
            timeout,
            deadline: Box::pin(tokio::time::sleep_until(started + timeout)),
        }
    }

    fn extend_for_in_flight_responses(&mut self, in_flight: usize) -> bool {
        let hard_limit = self.started + HTTP_CONNECTION_DRAIN_HARD_LIMIT;
        let now = Instant::now();
        if in_flight == 0 || now >= hard_limit {
            return false;
        }
        self.deadline
            .as_mut()
            .reset((now + self.timeout).min(hard_limit));
        true
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

struct ConnectionSignals {
    first_request: Arc<Notify>,
    http2: Arc<AtomicBool>,
    in_flight: Arc<AtomicUsize>,
}

fn observed_service<S>(
    tower_service: S,
    first_request: Arc<Notify>,
    in_flight: Arc<AtomicUsize>,
) -> impl tower::Service<
    hyper::Request<hyper::body::Incoming>,
    Response = hyper::Response<Body>,
    Error = S::Error,
    Future = impl Send,
> + Clone
where
    S: tower::Service<hyper::Request<hyper::body::Incoming>, Response = hyper::Response<Body>>
        + Clone
        + Send
        + 'static,
    S::Future: Send,
{
    let first_request_observed = Arc::new(AtomicBool::new(false));
    tower::service_fn(move |request| {
        let service = tower_service.clone();
        let first_request_observed = Arc::clone(&first_request_observed);
        let first_request = Arc::clone(&first_request);
        let in_flight = InFlightResponse::begin(&in_flight);
        async move {
            if !first_request_observed.swap(true, Ordering::AcqRel) {
                first_request.notify_one();
            }
            let response = service.oneshot(request).await?;
            Ok(response.map(|inner| {
                Body::new(InFlightBody {
                    inner,
                    _in_flight: in_flight,
                })
            }))
        }
    })
}

fn connection_builder(config: HttpServerConfig) -> Builder<TokioExecutor> {
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
        .header_read_timeout(Some(config.http1_header_timeout));
    builder
        .http2()
        .max_concurrent_streams(HTTP2_MAX_CONCURRENT_STREAMS)
        .max_header_list_size(HTTP2_MAX_HEADER_LIST_BYTES)
        .max_pending_accept_reset_streams(Some(HTTP2_MAX_PENDING_RESET_STREAMS))
        .max_local_error_reset_streams(Some(HTTP2_MAX_LOCAL_ERROR_RESET_STREAMS))
        .timer(TokioTimer::new());
    builder
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
    let signals = ConnectionSignals {
        first_request: Arc::new(Notify::new()),
        http2: Arc::new(AtomicBool::new(false)),
        in_flight: Arc::new(AtomicUsize::new(0)),
    };
    let hyper_service = TowerToHyperService::new(observed_service(
        tower_service,
        Arc::clone(&signals.first_request),
        Arc::clone(&signals.in_flight),
    ));
    let builder = connection_builder(config);
    let connection = builder.serve_connection_with_upgrades(
        TokioIo::new(HeaderLimitedStream::new(stream, Arc::clone(&signals.http2))),
        hyper_service,
    );
    tokio::pin!(connection);
    let first_request_deadline = tokio::time::sleep(config.http1_header_timeout);
    tokio::pin!(first_request_deadline);
    let connection_deadline = tokio::time::sleep(config.connection_max_age);
    tokio::pin!(connection_deadline);
    let mut drain: Option<Drain> = None;
    let mut max_age_reached = false;
    let mut first_request_seen = false;
    loop {
        tokio::select! {
            result = connection.as_mut() => {
                if let Err(error) = result {
                    debug!(%peer, %error, "HTTP connection closed with protocol error");
                }
                return;
            }
            () = signals.first_request.notified(), if !first_request_seen => {
                first_request_seen = true;
                if max_age_reached && signals.http2.load(Ordering::Acquire) && drain.is_none() {
                    debug!(%peer, "gracefully draining HTTP/2 connection at its maximum age");
                    connection.as_mut().graceful_shutdown();
                    drain = Some(Drain::start(config.connection_drain_timeout));
                }
            }
            () = &mut first_request_deadline, if !first_request_seen => {
                debug!(%peer, "closing connection before its first request exceeded the header/protocol deadline");
                return;
            }
            () = &mut connection_deadline, if drain.is_none() && !max_age_reached => {
                max_age_reached = true;
                if signals.http2.load(Ordering::Acquire) {
                    debug!(%peer, "gracefully draining HTTP/2 connection at its maximum age");
                    connection.as_mut().graceful_shutdown();
                    drain = Some(Drain::start(config.connection_drain_timeout));
                }
            }
            changed = shutdown.changed(), if drain.is_none() => {
                if changed.is_err() || *shutdown.borrow() {
                    connection.as_mut().graceful_shutdown();
                    drain = Some(Drain::start(config.connection_drain_timeout));
                }
            }
            () = async {
                if let Some(drain) = drain.as_mut() {
                    drain.deadline.as_mut().await;
                }
            }, if drain.is_some() => {
                let in_flight = signals.in_flight.load(Ordering::Acquire);
                if drain.as_mut().is_some_and(|drain| drain.extend_for_in_flight_responses(in_flight)) {
                    debug!(%peer, in_flight, "extending HTTP connection drain for in-flight responses");
                    continue;
                }
                warn!(%peer, in_flight, "forcing close after HTTP connection drain deadline");
                return;
            }
        }
    }
}

#[cfg(test)]
mod tests;

use std::{
    future::Future,
    io,
    net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr},
    time::Duration,
};

use tokio::net::TcpStream;

use super::AppResult;

const DEFAULT_READY_ADDRESS: &str = "127.0.0.1:9090";
const PROBE_TIMEOUT: Duration = Duration::from_secs(3);
const MAX_STATUS_LINE_BYTES: usize = 256;

/// Resolves the readiness address from the same setting the server binds,
/// mapping a wildcard bind address to loopback for the local probe.
fn ready_address() -> io::Result<SocketAddr> {
    let configured = std::env::var("OLP_OBSERVABILITY_LISTEN_ADDR")
        .ok()
        .filter(|value| !value.trim().is_empty())
        .unwrap_or_else(|| DEFAULT_READY_ADDRESS.to_owned());
    let mut address: SocketAddr = configured.trim().parse().map_err(|_| {
        io::Error::new(
            io::ErrorKind::InvalidInput,
            "OLP_OBSERVABILITY_LISTEN_ADDR is not a valid socket address",
        )
    })?;
    if address.ip().is_unspecified() {
        address.set_ip(match address.ip() {
            IpAddr::V4(_) => IpAddr::V4(Ipv4Addr::LOCALHOST),
            IpAddr::V6(_) => IpAddr::V6(Ipv6Addr::LOCALHOST),
        });
    }
    Ok(address)
}

fn ready_request(address: &SocketAddr) -> Vec<u8> {
    format!("GET /health/ready HTTP/1.1\r\nHost: {address}\r\nConnection: close\r\n\r\n")
        .into_bytes()
}

pub(super) async fn health_probe() -> AppResult<()> {
    let address = ready_address()?;
    let stream = timed(
        TcpStream::connect(address),
        "timed out connecting to the readiness listener",
    )
    .await??;
    timed(
        write_request(&stream, &ready_request(&address)),
        "timed out writing the readiness request",
    )
    .await??;
    let status_line = timed(
        read_status_line(&stream),
        "timed out reading the readiness response",
    )
    .await??;
    if status_line_is_ready(&status_line) {
        return Ok(());
    }
    Err(io::Error::other(format!(
        "readiness probe returned an unhealthy status: {}",
        String::from_utf8_lossy(&status_line).trim_end()
    ))
    .into())
}

async fn timed<T>(future: impl Future<Output = T>, timeout_message: &'static str) -> io::Result<T> {
    tokio::time::timeout(PROBE_TIMEOUT, future)
        .await
        .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, timeout_message))
}

async fn write_request(stream: &TcpStream, request: &[u8]) -> io::Result<()> {
    let mut written = 0;
    while written < request.len() {
        stream.writable().await?;
        match stream.try_write(&request[written..]) {
            Ok(0) => {
                return Err(io::Error::new(
                    io::ErrorKind::WriteZero,
                    "readiness socket closed",
                ));
            }
            Ok(bytes) => written += bytes,
            Err(error) if error.kind() == io::ErrorKind::WouldBlock => {}
            Err(error) => return Err(error),
        }
    }
    Ok(())
}

async fn read_status_line(stream: &TcpStream) -> io::Result<Vec<u8>> {
    let mut status_line = Vec::with_capacity(64);
    let mut buffer = [0_u8; 64];
    loop {
        stream.readable().await?;
        match stream.try_read(&mut buffer) {
            Ok(0) => {
                return Err(io::Error::new(
                    io::ErrorKind::UnexpectedEof,
                    "readiness response ended before its status line",
                ));
            }
            Ok(bytes) => {
                let remaining = MAX_STATUS_LINE_BYTES.saturating_sub(status_line.len());
                status_line.extend_from_slice(&buffer[..bytes.min(remaining)]);
                if let Some(line_end) = status_line.iter().position(|byte| *byte == b'\n') {
                    status_line.truncate(line_end + 1);
                    return Ok(status_line);
                }
                if status_line.len() == MAX_STATUS_LINE_BYTES {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidData,
                        "readiness response status line is too long",
                    ));
                }
            }
            Err(error) if error.kind() == io::ErrorKind::WouldBlock => {}
            Err(error) => return Err(error),
        }
    }
}

fn status_line_is_ready(status_line: &[u8]) -> bool {
    let Ok(status_line) = std::str::from_utf8(status_line) else {
        return false;
    };
    let mut fields = status_line.split_ascii_whitespace();
    matches!(
        (fields.next(), fields.next()),
        (Some("HTTP/1.0" | "HTTP/1.1"), Some("200"))
    )
}

#[cfg(test)]
mod tests {
    use std::io;

    use tokio::net::{TcpListener, TcpStream};

    use super::{MAX_STATUS_LINE_BYTES, read_status_line, status_line_is_ready, write_request};

    async fn loopback_pair() -> (TcpStream, TcpStream) {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let (client, accepted) = tokio::join!(TcpStream::connect(address), listener.accept());
        (client.unwrap(), accepted.unwrap().0)
    }

    #[test]
    fn accepts_only_successful_http_readiness_statuses() {
        for status_line in [
            b"HTTP/1.1 200 OK\r\n".as_slice(),
            b"HTTP/1.0 200 Ready\n".as_slice(),
        ] {
            assert!(status_line_is_ready(status_line));
        }
        for status_line in [
            b"HTTP/1.1 503 Service Unavailable\r\n".as_slice(),
            b"not-http 200 OK\r\n".as_slice(),
            b"HTTP/1.1\r\n".as_slice(),
            b"\xff\r\n".as_slice(),
        ] {
            assert!(!status_line_is_ready(status_line));
        }
    }

    #[tokio::test]
    async fn status_line_reader_returns_only_the_first_line() {
        let (client, server) = loopback_pair().await;
        write_request(&server, b"HTTP/1.1 200 OK\r\nignored body")
            .await
            .unwrap();

        assert_eq!(
            read_status_line(&client).await.unwrap(),
            b"HTTP/1.1 200 OK\r\n"
        );
    }

    #[tokio::test]
    async fn status_line_reader_bounds_untrusted_responses() {
        let (client, server) = loopback_pair().await;
        write_request(&server, &[b'x'; MAX_STATUS_LINE_BYTES])
            .await
            .unwrap();
        assert_eq!(
            read_status_line(&client).await.unwrap_err().kind(),
            io::ErrorKind::InvalidData
        );

        let (client, server) = loopback_pair().await;
        drop(server);
        assert_eq!(
            read_status_line(&client).await.unwrap_err().kind(),
            io::ErrorKind::UnexpectedEof
        );
    }
}

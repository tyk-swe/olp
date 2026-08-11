//! Minimal loopback HTTP/1.1 server shared by provider unit tests.

use std::time::Duration;

use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
    sync::oneshot,
};

pub(in crate::providers) struct MockResponse {
    pub(in crate::providers) chunks: Vec<(Duration, Vec<u8>)>,
}

impl MockResponse {
    pub(in crate::providers) fn immediate(response: Vec<u8>) -> Self {
        Self {
            chunks: vec![(Duration::ZERO, response)],
        }
    }
}

pub(in crate::providers) async fn spawn_mock(
    base_path: &str,
    response: MockResponse,
) -> (String, oneshot::Receiver<Vec<u8>>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (sender, receiver) = oneshot::channel();
    tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let request = read_request(&mut socket).await;
        let _ = sender.send(request);
        write_response(&mut socket, response).await;
    });
    (format!("http://{address}{base_path}"), receiver)
}

pub(in crate::providers) async fn spawn_sequence(
    base_path: &str,
    responses: Vec<MockResponse>,
) -> (String, oneshot::Receiver<Vec<Vec<u8>>>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (sender, receiver) = oneshot::channel();
    tokio::spawn(async move {
        let mut requests = Vec::with_capacity(responses.len());
        for response in responses {
            let (mut socket, _) = listener.accept().await.unwrap();
            requests.push(read_request(&mut socket).await);
            write_response(&mut socket, response).await;
        }
        let _ = sender.send(requests);
    });
    (format!("http://{address}{base_path}"), receiver)
}

async fn write_response(socket: &mut TcpStream, response: MockResponse) {
    for (delay, chunk) in response.chunks {
        tokio::time::sleep(delay).await;
        if socket.write_all(&chunk).await.is_err() {
            return;
        }
        let _ = socket.flush().await;
    }
}

async fn read_request(socket: &mut TcpStream) -> Vec<u8> {
    let mut request = Vec::new();
    let mut buffer = [0_u8; 4096];
    let mut expected = None;
    loop {
        let read = socket.read(&mut buffer).await.unwrap();
        if read == 0 {
            return request;
        }
        request.extend_from_slice(&buffer[..read]);
        if expected.is_none()
            && let Some(end) = find_bytes(&request, b"\r\n\r\n")
        {
            let headers = String::from_utf8_lossy(&request[..end]);
            let length = headers
                .lines()
                .find_map(|line| {
                    let (name, value) = line.split_once(':')?;
                    name.eq_ignore_ascii_case("content-length")
                        .then(|| value.trim().parse::<usize>().ok())
                        .flatten()
                })
                .unwrap_or_default();
            expected = Some(end + 4 + length);
        }
        if expected.is_some_and(|length| request.len() >= length) {
            return request;
        }
    }
}

pub(in crate::providers) fn find_bytes(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    haystack
        .windows(needle.len())
        .position(|window| window == needle)
}

pub(in crate::providers) fn response(content_type: &str, body: impl AsRef<[u8]>) -> Vec<u8> {
    status_response("200 OK", content_type, body)
}

pub(in crate::providers) fn status_response(
    status: &str,
    content_type: &str,
    body: impl AsRef<[u8]>,
) -> Vec<u8> {
    let body = body.as_ref();
    let headers = format!(
        "HTTP/1.1 {status}\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    );
    [headers.as_bytes(), body].concat()
}

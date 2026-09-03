use std::{collections::BTreeMap, sync::Arc};

use aws_smithy_eventstream::frame::write_message_to;
use aws_smithy_types::event_stream::{Header, HeaderValue, Message as EventMessage};
use bytes::Bytes;
use futures::StreamExt as _;
use olp_engine::domain::{
    canonical::{
        events::Event,
        identity::{RequestMetadata, Surface, TransportMode},
        requests::{
            GenerationParameters, GenerationRequest, MediaHandle, Message, MessageRole, Operation,
            SourceExtensions,
        },
        results::MediaArtifact,
    },
    ids::{DurationMs, ProviderId, RequestId, RouteId, RouteSlug, RuntimeGenerationId, TargetId},
    ports::{
        MediaSpool, MediaSpoolError, MediaUpload, OpenedMedia, ProviderOutput, ProviderRequest,
        ProviderTransport, TransportError,
    },
    routing::{provider::ProviderKind, selection::AttemptPlan},
};
use olp_engine::providers::test_support::local_provider;
use tokio::{
    io::{AsyncReadExt as _, AsyncWriteExt as _},
    net::{TcpListener, TcpStream},
    sync::oneshot,
    task::JoinHandle,
};

use super::fixtures::certification_response;

pub(super) const MODEL: &str = "conformance-model";
pub(super) const REQUEST_ID: &str = "01989dc0-2c00-7000-8000-000000000001";

#[derive(Debug)]
pub(super) struct CapturedRequest {
    pub(super) head: String,
    pub(super) body: Vec<u8>,
}

impl CapturedRequest {
    pub(super) fn path(&self) -> &str {
        self.head
            .lines()
            .next()
            .and_then(|line| line.split_ascii_whitespace().nth(1))
            .unwrap()
    }

    pub(super) fn header(&self, expected: &str) -> Option<&str> {
        self.head.lines().skip(1).find_map(|line| {
            let (name, value) = line.split_once(':')?;
            name.eq_ignore_ascii_case(expected).then(|| value.trim())
        })
    }

    pub(super) fn body_text(&self) -> &str {
        std::str::from_utf8(&self.body).unwrap()
    }
}

pub(super) fn http_response(status: &str, content_type: &str, body: impl AsRef<[u8]>) -> Vec<u8> {
    http_response_with_headers(status, content_type, &[], body)
}

pub(super) fn http_stream_response(content_type: &str, body: impl AsRef<[u8]>) -> Vec<u8> {
    let mut response = format!(
        "HTTP/1.1 200 OK\r\nContent-Type: {content_type}\r\nConnection: keep-alive\r\n\r\n"
    )
    .into_bytes();
    response.extend_from_slice(body.as_ref());
    response
}

pub(super) fn http_response_with_headers(
    status: &str,
    content_type: &str,
    headers: &[(&str, &str)],
    body: impl AsRef<[u8]>,
) -> Vec<u8> {
    let body = body.as_ref();
    let extra_headers = headers
        .iter()
        .map(|(name, value)| format!("{name}: {value}\r\n"))
        .collect::<String>();
    let mut response = format!(
        "HTTP/1.1 {status}\r\nContent-Type: {content_type}\r\n{extra_headers}Content-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    )
    .into_bytes();
    response.extend_from_slice(body);
    response
}

pub(super) async fn spawn_server(response: Vec<u8>) -> (String, JoinHandle<CapturedRequest>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let request = read_request(&mut socket).await;
        let _ = socket.write_all(&response).await;
        CapturedRequest {
            head: String::from_utf8(request.head).unwrap(),
            body: request.body,
        }
    });
    (format!("http://{address}"), task)
}

pub(super) async fn spawn_certification_emulator(
    kind: ProviderKind,
) -> (
    String,
    oneshot::Sender<()>,
    JoinHandle<Vec<CapturedRequest>>,
) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    let (shutdown_tx, mut shutdown_rx) = oneshot::channel();
    let server = tokio::spawn(async move {
        let mut requests = Vec::new();
        loop {
            let accepted = tokio::select! {
                _ = &mut shutdown_rx => break,
                accepted = listener.accept() => accepted,
            };
            let (mut socket, _) = accepted.unwrap();
            let raw_request = read_request(&mut socket).await;
            let request = CapturedRequest {
                head: String::from_utf8(raw_request.head).unwrap(),
                body: raw_request.body,
            };
            let response = certification_response(kind, &request);
            socket.write_all(&response).await.unwrap();
            requests.push(request);
        }
        requests
    });
    (origin, shutdown_tx, server)
}

pub(super) async fn spawn_stalling_server() -> (
    String,
    oneshot::Receiver<()>,
    JoinHandle<std::io::Result<usize>>,
) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    let (accepted_tx, accepted_rx) = oneshot::channel();
    let handle = tokio::spawn(async move {
        let (mut stream, _) = listener.accept().await?;
        let _request = read_request(&mut stream).await;
        let _ = accepted_tx.send(());
        let mut remaining = Vec::new();
        stream.read_to_end(&mut remaining).await
    });
    (origin, accepted_rx, handle)
}

pub(super) async fn spawn_streaming_handoff_server(
    response: Vec<u8>,
) -> (String, JoinHandle<(CapturedRequest, usize)>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    let handle = tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let request = read_request(&mut socket).await;
        socket.write_all(&response).await.unwrap();
        socket.flush().await.unwrap();
        let mut remaining = Vec::new();
        let bytes_after_request = socket.read_to_end(&mut remaining).await.unwrap();
        (
            CapturedRequest {
                head: String::from_utf8(request.head).unwrap(),
                body: request.body,
            },
            bytes_after_request,
        )
    });
    (origin, handle)
}

pub(super) struct RawRequest {
    pub(super) head: Vec<u8>,
    pub(super) body: Vec<u8>,
}

pub(super) struct InlineMediaSpool {
    pub(super) filename: &'static str,
    pub(super) content_type: &'static str,
    pub(super) data: &'static [u8],
}

impl MediaSpool for InlineMediaSpool {
    fn put<'a>(
        &'a self,
        _upload: MediaUpload,
    ) -> olp_engine::domain::ports::BoxFuture<'a, Result<MediaArtifact, MediaSpoolError>> {
        Box::pin(async { Err(MediaSpoolError::Unavailable) })
    }

    fn open<'a>(
        &'a self,
        handle: &'a MediaHandle,
    ) -> olp_engine::domain::ports::BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
        let handle = handle.clone();
        let filename = self.filename;
        let content_type = self.content_type;
        let data = self.data;
        Box::pin(async move {
            Ok(OpenedMedia {
                artifact: MediaArtifact {
                    handle,
                    content_type: Some(content_type.to_owned()),
                    content_length: Some(data.len() as u64),
                },
                filename: filename.to_owned(),
                bytes: Box::pin(futures::stream::once(async move {
                    Ok(Bytes::from_static(data))
                })),
            })
        })
    }

    fn remove<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> olp_engine::domain::ports::BoxFuture<'a, Result<(), MediaSpoolError>> {
        Box::pin(async { Err(MediaSpoolError::Unavailable) })
    }
}

pub(super) async fn read_request(socket: &mut TcpStream) -> RawRequest {
    let mut bytes = Vec::new();
    let mut buffer = [0_u8; 8_192];
    let header_end = loop {
        let read = socket.read(&mut buffer).await.unwrap();
        assert_ne!(read, 0, "connection closed before request headers");
        bytes.extend_from_slice(&buffer[..read]);
        if let Some(index) = bytes.windows(4).position(|part| part == b"\r\n\r\n") {
            break index + 4;
        }
    };
    let length = String::from_utf8_lossy(&bytes[..header_end])
        .lines()
        .find_map(|line| {
            let (name, value) = line.split_once(':')?;
            name.eq_ignore_ascii_case("content-length")
                .then(|| value.trim().parse::<usize>().unwrap())
        })
        .unwrap_or_default();
    while bytes.len() < header_end + length {
        let read = socket.read(&mut buffer).await.unwrap();
        assert_ne!(read, 0, "connection closed before request body");
        bytes.extend_from_slice(&buffer[..read]);
    }
    RawRequest {
        head: bytes[..header_end].to_vec(),
        body: bytes[header_end..header_end + length].to_vec(),
    }
}

pub(super) async fn transport_at(
    kind: ProviderKind,
    response: Vec<u8>,
) -> (Arc<dyn ProviderTransport>, JoinHandle<CapturedRequest>) {
    let (origin, server) = spawn_server(response).await;
    let provider = local_provider(kind, &origin).await.unwrap();
    (provider.into_transport(), server)
}

pub(super) fn request_id() -> RequestId {
    REQUEST_ID.parse().unwrap()
}

pub(super) fn generation_request(
    kind: ProviderKind,
    surface: Surface,
    mode: TransportMode,
) -> ProviderRequest {
    let operation = Operation::Generation(GenerationRequest {
        route: RouteSlug::parse("conformance").unwrap(),
        messages: vec![Message {
            role: MessageRole::User,
            content: vec![olp_engine::domain::canonical::requests::ContentPart::Text {
                text: "say hello".to_owned(),
            }],
            name: None,
            tool_call_id: None,
            tool_calls: Vec::new(),
        }],
        parameters: GenerationParameters {
            max_output_tokens: Some(8),
            stream: mode == TransportMode::Streaming,
            ..GenerationParameters::default()
        },
        tools: Vec::new(),
        tool_choice: None,
        response_format: None,
        extensions: SourceExtensions::new(surface, BTreeMap::new()),
    });
    provider_request(kind, surface, mode, operation, 2_000)
}

pub(super) fn provider_request(
    kind: ProviderKind,
    surface: Surface,
    mode: TransportMode,
    operation: Operation,
    timeout_ms: u64,
) -> ProviderRequest {
    ProviderRequest {
        metadata: RequestMetadata {
            request_id: request_id(),
            operation: operation.kind(),
            surface,
            mode,
        },
        attempt: AttemptPlan {
            generation_id: RuntimeGenerationId::new(),
            route_id: RouteId::new(),
            target_id: TargetId::new(),
            routing_id: TargetId::new(),
            provider_id: ProviderId::new(),
            provider_revision_id: None,
            provider_kind: kind,
            upstream_model: MODEL.to_owned(),
            timeout: DurationMs::new(timeout_ms),
            priority: 0,
        },
        operation: Arc::new(operation),
        media: None,
        max_inline_media_bytes: 1024 * 1024,
        propagate_trace_context: false,
    }
}

pub(super) async fn collect_events(output: ProviderOutput) -> Result<Vec<Event>, TransportError> {
    let ProviderOutput::Events(mut stream) = output else {
        panic!("expected canonical event stream");
    };
    let mut events = Vec::new();
    while let Some(event) = stream.next().await {
        events.push(event?);
    }
    Ok(events)
}

pub(super) fn event_frame(event_type: &str, payload: &str) -> Vec<u8> {
    let message = EventMessage::new(payload.as_bytes().to_vec())
        .add_header(Header::new(
            ":message-type",
            HeaderValue::String("event".into()),
        ))
        .add_header(Header::new(
            ":event-type",
            HeaderValue::String(event_type.to_owned().into()),
        ))
        .add_header(Header::new(
            ":content-type",
            HeaderValue::String("application/json".into()),
        ));
    let mut encoded = Vec::new();
    write_message_to(&message, &mut encoded).unwrap();
    encoded
}

/// Bedrock has no native gateway surface — Converse is not one — so the
/// harness drives its connector with OpenAI-surface canonical requests, which
/// is why the domain's `None` collapses to [`Surface::OpenAi`] here.
pub(super) fn native_surface(kind: ProviderKind) -> Surface {
    kind.native_surface().unwrap_or(Surface::OpenAi)
}

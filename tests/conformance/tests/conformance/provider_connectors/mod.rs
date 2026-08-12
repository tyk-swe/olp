mod compatibility;
mod matrix;

use std::{collections::BTreeMap, sync::Arc, time::Duration};

use aws_smithy_eventstream::frame::write_message_to;
use aws_smithy_types::event_stream::{Header, HeaderValue, Message as EventMessage};
use bytes::Bytes;
use futures::StreamExt as _;
use olp_engine::domain::{
    AttemptFailureClass, AttemptPlan, CanonicalEvent, CanonicalEventKind, CanonicalResult,
    ContentPart, DurationMs, GenerationParameters, GenerationRequest, MediaArtifact, MediaHandle,
    MediaSource, MediaSpool, MediaSpoolError, MediaUpload, Message, MessageRole, OpenedMedia,
    Operation, OperationKind, ProviderId, ProviderKind, ProviderOutput, ProviderRequest,
    ProviderTransport, RequestId, RequestMetadata, ResponseFormat, RouteId, RouteSlug,
    RuntimeGenerationId, SourceExtensions, Surface, TargetId, ToolChoice, ToolDefinition,
    TranscriptionRequest, TransportError, TransportMode, TransportPhase, Usage,
    validate_event_sequence,
};
use olp_engine::protocols::sse::DEFAULT_MAX_EVENT_BYTES;
use olp_engine::providers::{
    CompatibleCapability,
    test_support::{API_KEY, BEDROCK_ACCESS_KEY, BEDROCK_SECRET_KEY, VERTEX_TOKEN, local_provider},
};
use serde_json::{Value, json};
use tokio::{
    io::{AsyncReadExt as _, AsyncWriteExt as _},
    net::{TcpListener, TcpStream},
    sync::oneshot,
    task::JoinHandle,
};

const MODEL: &str = "conformance-model";
const REQUEST_ID: &str = "01989dc0-2c00-7000-8000-000000000001";

#[derive(Debug)]
struct CapturedRequest {
    head: String,
    body: Vec<u8>,
}

impl CapturedRequest {
    fn path(&self) -> &str {
        self.head
            .lines()
            .next()
            .and_then(|line| line.split_ascii_whitespace().nth(1))
            .unwrap()
    }

    fn header(&self, expected: &str) -> Option<&str> {
        self.head.lines().skip(1).find_map(|line| {
            let (name, value) = line.split_once(':')?;
            name.eq_ignore_ascii_case(expected).then(|| value.trim())
        })
    }

    fn body_text(&self) -> &str {
        std::str::from_utf8(&self.body).unwrap()
    }
}

fn http_response(status: &str, content_type: &str, body: impl AsRef<[u8]>) -> Vec<u8> {
    http_response_with_headers(status, content_type, &[], body)
}

fn http_stream_response(content_type: &str, body: impl AsRef<[u8]>) -> Vec<u8> {
    let mut response = format!(
        "HTTP/1.1 200 OK\r\nContent-Type: {content_type}\r\nConnection: keep-alive\r\n\r\n"
    )
    .into_bytes();
    response.extend_from_slice(body.as_ref());
    response
}

fn http_response_with_headers(
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

async fn spawn_server(response: Vec<u8>) -> (String, JoinHandle<CapturedRequest>) {
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

async fn spawn_certification_emulator(
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

async fn spawn_stalling_server() -> (
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

async fn spawn_streaming_handoff_server(
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

struct RawRequest {
    head: Vec<u8>,
    body: Vec<u8>,
}

struct InlineMediaSpool {
    filename: &'static str,
    content_type: &'static str,
    data: &'static [u8],
}

impl MediaSpool for InlineMediaSpool {
    fn put<'a>(
        &'a self,
        _upload: MediaUpload,
    ) -> olp_engine::domain::BoxFuture<'a, Result<MediaArtifact, MediaSpoolError>> {
        Box::pin(async { Err(MediaSpoolError::Unavailable) })
    }

    fn open<'a>(
        &'a self,
        handle: &'a MediaHandle,
    ) -> olp_engine::domain::BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
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
    ) -> olp_engine::domain::BoxFuture<'a, Result<(), MediaSpoolError>> {
        Box::pin(async { Err(MediaSpoolError::Unavailable) })
    }
}

async fn read_request(socket: &mut TcpStream) -> RawRequest {
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

async fn transport_at(
    kind: ProviderKind,
    response: Vec<u8>,
) -> (Arc<dyn ProviderTransport>, JoinHandle<CapturedRequest>) {
    let (origin, server) = spawn_server(response).await;
    let provider = local_provider(kind, &origin).await.unwrap();
    (provider.into_transport(), server)
}

fn request_id() -> RequestId {
    REQUEST_ID.parse().unwrap()
}

fn generation_request(
    kind: ProviderKind,
    surface: Surface,
    mode: TransportMode,
) -> ProviderRequest {
    let operation = Operation::Generation(GenerationRequest {
        route: RouteSlug::parse("conformance").unwrap(),
        messages: vec![Message {
            role: MessageRole::User,
            content: vec![olp_engine::domain::ContentPart::Text {
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

fn provider_request(
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
            provider_id: ProviderId::new(),
            provider_kind: kind,
            upstream_model: MODEL.to_owned(),
            timeout: DurationMs::new(timeout_ms),
            priority: 0,
        },
        operation,
        media: None,
    }
}

async fn collect_events(output: ProviderOutput) -> Result<Vec<CanonicalEvent>, TransportError> {
    let ProviderOutput::Events(mut stream) = output else {
        panic!("expected canonical event stream");
    };
    let mut events = Vec::new();
    while let Some(event) = stream.next().await {
        events.push(event?);
    }
    Ok(events)
}

fn event_frame(event_type: &str, payload: &str) -> Vec<u8> {
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

fn native_surface(kind: ProviderKind) -> Surface {
    match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
            Surface::OpenAi
        }
        ProviderKind::Anthropic => Surface::Anthropic,
        ProviderKind::Gemini | ProviderKind::VertexAi => Surface::Gemini,
        ProviderKind::Bedrock => Surface::OpenAi,
    }
}

fn unary_response(kind: ProviderKind) -> Vec<u8> {
    let body = match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
            json!({
                "id": "provider-response-id",
                "object": "chat.completion",
                "created": 1,
                "model": MODEL,
                "choices": [{
                    "index": 0,
                    "message": {"role": "assistant", "content": "hello back"},
                    "finish_reason": "stop"
                }],
                "usage": {
                    "prompt_tokens": 5,
                    "completion_tokens": 2,
                    "total_tokens": 7,
                    "prompt_tokens_details": {"cached_tokens": 3}
                }
            })
        }
        ProviderKind::Anthropic => json!({
            "id": "provider-response-id",
            "type": "message",
            "role": "assistant",
            "content": [{"type": "text", "text": "hello back"}],
            "model": MODEL,
            "stop_reason": "end_turn",
            "stop_sequence": null,
            "usage": {
                "input_tokens": 2,
                "output_tokens": 2,
                "cache_read_input_tokens": 3
            }
        }),
        ProviderKind::Gemini | ProviderKind::VertexAi => json!({
            "candidates": [{
                "content": {"role": "model", "parts": [{"text": "hello back"}]},
                "finishReason": "STOP",
                "index": 0
            }],
            "usageMetadata": {
                "promptTokenCount": 5,
                "candidatesTokenCount": 2,
                "totalTokenCount": 7,
                "cachedContentTokenCount": 3
            },
            "modelVersion": MODEL,
            "responseId": "provider-response-id"
        }),
        ProviderKind::Bedrock => json!({
            "output": {"message": {"role": "assistant", "content": [{"text": "hello back"}]}},
            "stopReason": "end_turn",
            "usage": {"inputTokens": 5, "outputTokens": 2, "totalTokens": 7},
            "metrics": {"latencyMs": 1}
        }),
    };
    http_response(
        "200 OK",
        "application/json",
        serde_json::to_vec(&body).unwrap(),
    )
}

fn streaming_response(kind: ProviderKind) -> Vec<u8> {
    match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
            let body = concat!(
                "data: {\"id\":\"provider-response-id\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"conformance-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello back\"},\"finish_reason\":null}]}\n\n",
                "data: {\"id\":\"provider-response-id\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"conformance-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\n",
                "data: [DONE]\n\n"
            );
            http_response("200 OK", "text/event-stream", body)
        }
        ProviderKind::Anthropic => {
            let body = concat!(
                "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"provider-response-id\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"conformance-model\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":2,\"output_tokens\":0,\"cache_read_input_tokens\":3}}}\n\n",
                "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
                "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello back\"}}\n\n",
                "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
                "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n",
                "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
            );
            http_response("200 OK", "text/event-stream", body)
        }
        ProviderKind::Gemini | ProviderKind::VertexAi => {
            let body = concat!(
                "data: {\"responseId\":\"provider-response-id\",\"modelVersion\":\"conformance-model\",\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hello back\"}]},\"index\":0}]}\n\n",
                "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[]},\"finishReason\":\"STOP\",\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":2,\"totalTokenCount\":7,\"cachedContentTokenCount\":3}}\n\n"
            );
            http_response("200 OK", "text/event-stream", body)
        }
        ProviderKind::Bedrock => {
            let mut body = Vec::new();
            for (event, payload) in [
                ("messageStart", r#"{"role":"assistant"}"#),
                (
                    "contentBlockDelta",
                    r#"{"delta":{"text":"hello back"},"contentBlockIndex":0}"#,
                ),
                ("contentBlockStop", r#"{"contentBlockIndex":0}"#),
                ("messageStop", r#"{"stopReason":"end_turn"}"#),
                (
                    "metadata",
                    r#"{"usage":{"inputTokens":5,"outputTokens":2,"totalTokens":7},"metrics":{"latencyMs":1}}"#,
                ),
            ] {
                body.extend(event_frame(event, payload));
            }
            http_response("200 OK", "application/vnd.amazon.eventstream", body)
        }
    }
}

fn token_count_response(kind: ProviderKind) -> Vec<u8> {
    let body = match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
            r#"{"object":"response.input_tokens","input_tokens":7}"#
        }
        ProviderKind::Anthropic => r#"{"input_tokens":7}"#,
        ProviderKind::Gemini | ProviderKind::VertexAi => {
            r#"{"totalTokens":7,"cachedContentTokenCount":2}"#
        }
        ProviderKind::Bedrock => r#"{"inputTokens":7}"#,
    };
    http_response("200 OK", "application/json", body)
}

fn certification_response(kind: ProviderKind, request: &CapturedRequest) -> Vec<u8> {
    let path = request.path();
    if path.ends_with("/models") {
        return http_response(
            "200 OK",
            "application/json",
            json!({
                "object": "list",
                "data": [{
                    "id": MODEL,
                    "object": "model",
                    "created": 1,
                    "owned_by": "openai"
                }]
            })
            .to_string(),
        );
    }
    if path.contains("/chat/completions") {
        return if request.body_text().contains("\"stream\":true") {
            streaming_response(kind)
        } else {
            unary_response(kind)
        };
    }
    if path.contains("/responses") && !path.contains("/responses/input_tokens") {
        if request.body_text().contains("\"stream\":true") {
            return http_response(
                "200 OK",
                "text/event-stream",
                concat!(
                    "event: response.created\n",
                    "data: {\"type\":\"response.created\",\"response\":{\"id\":\"provider-response-id\",\"model\":\"conformance-model\"}}\n\n",
                    "event: response.output_item.added\n",
                    "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\"}}\n\n",
                    "event: response.output_text.delta\n",
                    "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"OK\"}\n\n",
                    "event: response.output_item.done\n",
                    "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"status\":\"completed\"}}\n\n",
                    "event: response.completed\n",
                    "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"status\":\"completed\"}],\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n"
                ),
            );
        }
        return http_response(
            "200 OK",
            "application/json",
            json!({
                "id": "provider-response-id",
                "object": "response",
                "created_at": 1,
                "status": "completed",
                "model": MODEL,
                "output": [{
                    "id": "message-id",
                    "type": "message",
                    "role": "assistant",
                    "status": "completed",
                    "content": [{"type": "output_text", "text": "OK", "annotations": []}]
                }],
                "usage": {"input_tokens": 3, "output_tokens": 1, "total_tokens": 4}
            })
            .to_string(),
        );
    }
    if path.contains("/embeddings") {
        return http_response(
            "200 OK",
            "application/json",
            json!({
                "object": "list",
                "model": MODEL,
                "data": [{"object": "embedding", "index": 0, "embedding": [0.25]}],
                "usage": {"prompt_tokens": 1, "total_tokens": 1}
            })
            .to_string(),
        );
    }
    if path.contains("/moderations") {
        return http_response(
            "200 OK",
            "application/json",
            json!({
                "id": "moderation-id",
                "model": MODEL,
                "results": [{
                    "flagged": false,
                    "categories": {"violence": false},
                    "category_scores": {"violence": 0.0}
                }]
            })
            .to_string(),
        );
    }
    if path.contains("input_tokens")
        || path.contains("count_tokens")
        || path.contains("countTokens")
        || path.contains("count-tokens")
    {
        return token_count_response(kind);
    }
    if path.contains("streamGenerateContent")
        || path.contains("converse-stream")
        || (path.contains("/messages") && request.body_text().contains("\"stream\":true"))
    {
        return streaming_response(kind);
    }
    if path.contains("generateContent") || path.contains("/converse") || path.contains("/messages")
    {
        return unary_response(kind);
    }
    panic!("unhandled certification request for {kind:?}: {path}");
}

#[tokio::test]
async fn all_connectors_execute_unary_generation_with_reviewed_endpoint_and_auth() {
    use matrix::{Disposition, row_for};

    for kind in ProviderKind::ALL {
        let (transport, server) = transport_at(kind, unary_response(kind)).await;
        let events = collect_events(
            transport
                .execute(generation_request(
                    kind,
                    native_surface(kind),
                    TransportMode::Unary,
                ))
                .await
                .unwrap(),
        )
        .await
        .unwrap();
        validate_event_sequence(&events).unwrap();
        assert!(
            events.iter().any(|event| matches!(
                &event.kind,
                CanonicalEventKind::TextDelta { text, .. } if text == "hello back"
            )),
            "{kind:?}"
        );
        assert_usage(kind, &events);
        assert_response_id(kind, &events);

        let captured = server.await.unwrap();
        assert_endpoint_and_auth(kind, TransportMode::Unary, &captured);
        assert_eq!(
            captured.header("x-request-id"),
            match row_for(kind).request_ids {
                Disposition::SharedContract => Some(REQUEST_ID),
                Disposition::Inapplicable(_) => None,
            },
            "{kind:?} request ID placement"
        );
    }
}

fn assert_usage(kind: ProviderKind, events: &[CanonicalEvent]) {
    use matrix::{Disposition, row_for};

    let usage = events.iter().find_map(|event| match event.kind {
        CanonicalEventKind::Usage { usage } => Some(usage),
        _ => None,
    });
    let cached_input_tokens = match row_for(kind).cached_usage {
        Disposition::SharedContract => Some(3),
        Disposition::Inapplicable(_) => None,
    };
    assert_eq!(
        usage,
        Some(Usage {
            input_tokens: 5,
            output_tokens: 2,
            total_tokens: 7,
            cached_input_tokens,
            reasoning_tokens: None,
        }),
        "{kind:?} usage"
    );
}

fn assert_response_id(kind: ProviderKind, events: &[CanonicalEvent]) {
    use matrix::{Disposition, row_for};

    let id = events.iter().find_map(|event| match &event.kind {
        CanonicalEventKind::ResponseStart { response_id, .. } => response_id.as_deref(),
        _ => None,
    });
    assert_eq!(
        id,
        match row_for(kind).request_ids {
            Disposition::SharedContract => Some("provider-response-id"),
            Disposition::Inapplicable(_) => None,
        },
        "{kind:?} provider response ID"
    );
}

fn assert_endpoint_and_auth(kind: ProviderKind, mode: TransportMode, request: &CapturedRequest) {
    let streaming = mode == TransportMode::Streaming;
    let (path_fragment, auth_name) = match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible => {
            ("/v1/chat/completions", "authorization")
        }
        ProviderKind::Anthropic => ("/v1/messages", "x-api-key"),
        ProviderKind::Gemini if streaming => (
            "/v1beta/models/conformance-model:streamGenerateContent?alt=sse",
            "x-goog-api-key",
        ),
        ProviderKind::Gemini => (
            "/v1beta/models/conformance-model:generateContent",
            "x-goog-api-key",
        ),
        ProviderKind::VertexAi if streaming => (
            "/v1/projects/conformance-project/locations/us-central1/publishers/google/models/conformance-model:streamGenerateContent?alt=sse",
            "authorization",
        ),
        ProviderKind::VertexAi => (
            "/v1/projects/conformance-project/locations/us-central1/publishers/google/models/conformance-model:generateContent",
            "authorization",
        ),
        ProviderKind::Bedrock => (
            if streaming {
                "/model/conformance-model/converse-stream"
            } else {
                "/model/conformance-model/converse"
            },
            "authorization",
        ),
        ProviderKind::AzureOpenAi => (
            "/openai/deployments/conformance-deployment/chat/completions?api-version=2024-10-21",
            "api-key",
        ),
    };
    assert_eq!(request.path(), path_fragment, "{kind:?} endpoint");
    let auth = request
        .header(auth_name)
        .unwrap_or_else(|| panic!("{kind:?} missing {auth_name} header: {}", request.head));
    match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible => {
            assert_eq!(auth, format!("Bearer {API_KEY}"), "{kind:?}");
        }
        ProviderKind::VertexAi => {
            assert_eq!(auth, format!("Bearer {VERTEX_TOKEN}"), "{kind:?}");
        }
        ProviderKind::Bedrock => {
            assert!(
                auth.contains(BEDROCK_ACCESS_KEY),
                "{kind:?} authentication placement: {}",
                request.head
            );
        }
        ProviderKind::Anthropic | ProviderKind::Gemini | ProviderKind::AzureOpenAi => {
            assert_eq!(auth, API_KEY, "{kind:?}");
        }
    }
    assert!(!request.head.contains(BEDROCK_SECRET_KEY));
    if kind == ProviderKind::Gemini {
        assert!(
            !request.path().contains("key="),
            "Gemini key must stay out of URL"
        );
    }
}

#[tokio::test]
async fn all_connectors_stream_valid_terminal_sequences_and_usage() {
    for kind in ProviderKind::ALL {
        let (transport, server) = transport_at(kind, streaming_response(kind)).await;
        let events = collect_events(
            transport
                .execute(generation_request(
                    kind,
                    native_surface(kind),
                    TransportMode::Streaming,
                ))
                .await
                .unwrap(),
        )
        .await
        .unwrap();
        validate_event_sequence(&events).unwrap();
        assert!(
            matches!(
                events.last().map(|event| &event.kind),
                Some(CanonicalEventKind::Done)
            ),
            "{kind:?}"
        );
        assert!(
            events.iter().any(|event| matches!(
                &event.kind,
                CanonicalEventKind::TextDelta { text, .. } if text == "hello back"
            )),
            "{kind:?}"
        );
        assert_usage(kind, &events);
        assert_response_id(kind, &events);
        let captured = server.await.unwrap();
        assert_endpoint_and_auth(kind, TransportMode::Streaming, &captured);
        match kind {
            ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
                assert!(captured.body_text().contains("\"include_usage\":true"));
            }
            _ => {}
        }
    }
}

fn tool_response(kind: ProviderKind) -> Vec<u8> {
    let body = match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
            json!({
                "id": "tool-response",
                "object": "chat.completion",
                "created": 1,
                "model": MODEL,
                "choices": [{
                    "index": 0,
                    "message": {
                        "role": "assistant",
                        "content": null,
                        "tool_calls": [{
                            "id": "call-1",
                            "type": "function",
                            "function": {"name": "weather", "arguments": "{\"city\":\"Paris\"}"}
                        }]
                    },
                    "finish_reason": "tool_calls"
                }],
                "usage": {"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3}
            })
        }
        ProviderKind::Anthropic => json!({
            "id": "tool-response",
            "type": "message",
            "role": "assistant",
            "content": [{
                "type": "tool_use",
                "id": "call-1",
                "name": "weather",
                "input": {"city": "Paris"}
            }],
            "model": MODEL,
            "stop_reason": "tool_use",
            "stop_sequence": null,
            "usage": {"input_tokens": 2, "output_tokens": 1}
        }),
        ProviderKind::Gemini | ProviderKind::VertexAi => json!({
            "candidates": [{
                "content": {"role": "model", "parts": [{
                    "functionCall": {"name": "weather", "args": {"city": "Paris"}}
                }]},
                "finishReason": "STOP",
                "index": 0
            }],
            "usageMetadata": {"promptTokenCount": 2, "candidatesTokenCount": 1, "totalTokenCount": 3},
            "modelVersion": MODEL,
            "responseId": "tool-response"
        }),
        ProviderKind::Bedrock => json!({
            "output": {"message": {"role": "assistant", "content": [{
                "toolUse": {"toolUseId": "call-1", "name": "weather", "input": {"city": "Paris"}}
            }]}},
            "stopReason": "tool_use",
            "usage": {"inputTokens": 2, "outputTokens": 1, "totalTokens": 3},
            "metrics": {"latencyMs": 1}
        }),
    };
    http_response(
        "200 OK",
        "application/json",
        serde_json::to_vec(&body).unwrap(),
    )
}

fn tool_request(kind: ProviderKind) -> ProviderRequest {
    let surface = native_surface(kind);
    let mut request = generation_request(kind, surface, TransportMode::Unary);
    let Operation::Generation(generation) = &mut request.operation else {
        unreachable!()
    };
    generation.tools = vec![ToolDefinition {
        name: "weather".to_owned(),
        description: Some("Look up weather".to_owned()),
        input_schema: json!({
            "type": "object",
            "properties": {"city": {"type": "string"}},
            "required": ["city"]
        }),
    }];
    generation.tool_choice = Some(ToolChoice::Auto);
    request
}

#[tokio::test]
async fn all_connectors_round_trip_advertised_tools() {
    for kind in ProviderKind::ALL {
        let (transport, server) = transport_at(kind, tool_response(kind)).await;
        let events = collect_events(transport.execute(tool_request(kind)).await.unwrap())
            .await
            .unwrap();
        validate_event_sequence(&events).unwrap();
        assert!(
            events.iter().any(|event| matches!(
                &event.kind,
                CanonicalEventKind::ToolCallDelta {
                    name: Some(name),
                    arguments_delta,
                    ..
                } if name == "weather" && arguments_delta.contains("Paris")
            )),
            "{kind:?}: {events:?}"
        );
        if !matches!(kind, ProviderKind::Gemini | ProviderKind::VertexAi) {
            assert!(
                events.iter().any(|event| matches!(
                    &event.kind,
                    CanonicalEventKind::ToolCallDelta { id: Some(id), .. } if id == "call-1"
                )),
                "{kind:?}: provider tool ID"
            );
        }
        let captured = server.await.unwrap();
        assert!(captured.body_text().contains("weather"), "{kind:?}");
        assert!(captured.body_text().contains("city"), "{kind:?}");
    }
}

#[tokio::test]
async fn structured_output_is_exercised_or_rejected_exactly_as_reviewed() {
    use matrix::{Disposition, ROWS};

    const SCHEMA_ONLY_PROPERTY: &str = "schema_only_conformance_sentinel";
    for row in ROWS {
        let kind = row.kind;
        let mut request = generation_request(kind, native_surface(kind), TransportMode::Unary);
        let Operation::Generation(generation) = &mut request.operation else {
            unreachable!()
        };
        generation.response_format = Some(ResponseFormat::JsonSchema {
            name: "conformance_schema".to_owned(),
            description: Some("A bounded answer".to_owned()),
            schema: json!({
                "type": "object",
                "properties": {SCHEMA_ONLY_PROPERTY: {"type": "string"}},
                "required": [SCHEMA_ONLY_PROPERTY],
                "additionalProperties": false
            }),
            strict: Some(true),
        });

        match row.structured_output {
            Disposition::SharedContract => {
                let (transport, server) = transport_at(kind, unary_response(kind)).await;
                let events = collect_events(transport.execute(request).await.unwrap())
                    .await
                    .unwrap();
                validate_event_sequence(&events).unwrap();
                let captured = server.await.unwrap();
                let body: Value = serde_json::from_slice(&captured.body).unwrap();
                if matches!(kind, ProviderKind::Gemini | ProviderKind::VertexAi) {
                    assert_eq!(
                        body["generationConfig"]["responseMimeType"], "application/json",
                        "{kind:?}"
                    );
                    assert_eq!(
                        body["generationConfig"]["responseSchema"]["properties"]
                            [SCHEMA_ONLY_PROPERTY]["type"],
                        "string",
                        "{kind:?}"
                    );
                } else {
                    assert_eq!(body["response_format"]["type"], "json_schema", "{kind:?}");
                    assert_eq!(
                        body["response_format"]["json_schema"]["strict"], true,
                        "{kind:?}"
                    );
                    assert_eq!(
                        body["response_format"]["json_schema"]["schema"]["properties"]
                            [SCHEMA_ONLY_PROPERTY]["type"],
                        "string",
                        "{kind:?}"
                    );
                }
            }
            Disposition::Inapplicable(_) => {
                let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
                let origin = format!("http://{}", listener.local_addr().unwrap());
                let provider = local_provider(kind, &origin).await.unwrap();
                let error = provider
                    .into_transport()
                    .execute(request)
                    .await
                    .expect_err("reviewed inapplicable format must fail closed");
                assert_eq!(error.class, AttemptFailureClass::Protocol, "{kind:?}");
                assert_eq!(error.phase, TransportPhase::Connect, "{kind:?}");
                drop(listener);
            }
        }
    }
}

fn classified_error_response(kind: ProviderKind, status: &str, secret: &str) -> Vec<u8> {
    let error_type = match status.as_bytes().first() {
        Some(b'4') if status.starts_with("429") => "ThrottlingException",
        Some(b'4') => "ValidationException",
        _ => "ServiceUnavailableException",
    };
    let body = json!({"__type": error_type, "message": format!("reflected {secret}")});
    let headers = [("x-amzn-errortype", error_type), ("retry-after", "7")];
    if kind == ProviderKind::Bedrock {
        http_response_with_headers(status, "application/json", &headers, body.to_string())
    } else {
        http_response_with_headers(
            status,
            "application/json",
            &[("retry-after", "7")],
            body.to_string(),
        )
    }
}

#[tokio::test]
async fn all_connectors_redact_secrets_and_classify_retryability() {
    let cases = [
        (
            "429 Too Many Requests",
            AttemptFailureClass::RateLimit,
            true,
        ),
        (
            "503 Service Unavailable",
            AttemptFailureClass::UpstreamServer,
            true,
        ),
        (
            "400 Bad Request",
            AttemptFailureClass::UpstreamClient,
            false,
        ),
    ];
    for kind in ProviderKind::ALL {
        let reflected_secret = match kind {
            ProviderKind::VertexAi => VERTEX_TOKEN,
            ProviderKind::Bedrock => BEDROCK_SECRET_KEY,
            _ => API_KEY,
        };
        for (status, class, retryable) in cases {
            let response = classified_error_response(kind, status, reflected_secret);
            let (transport, server) = transport_at(kind, response).await;
            let error = transport
                .execute(generation_request(
                    kind,
                    native_surface(kind),
                    TransportMode::Unary,
                ))
                .await
                .expect_err("error status must fail");
            assert_eq!(error.class, class, "{kind:?} {status}");
            assert_eq!(error.allows_failover(), retryable, "{kind:?} {status}");
            assert_eq!(
                error.retry_after,
                match matrix::row_for(kind).retry_after {
                    matrix::Disposition::SharedContract => Some(Duration::from_secs(7)),
                    matrix::Disposition::Inapplicable(_) => None,
                },
                "{kind:?} {status} Retry-After propagation"
            );
            for secret in [
                API_KEY,
                VERTEX_TOKEN,
                BEDROCK_ACCESS_KEY,
                BEDROCK_SECRET_KEY,
            ] {
                assert!(
                    !error.message.contains(secret),
                    "{kind:?} leaked secret in error"
                );
                assert!(
                    !format!("{error:?}").contains(secret),
                    "{kind:?} leaked secret in Debug"
                );
            }
            assert!(format!("{error:?}").contains("[REDACTED]"));
            server.await.unwrap();
        }
    }
}

#[tokio::test]
async fn all_connectors_propagate_attempt_deadlines() {
    for kind in ProviderKind::ALL {
        let (origin, accepted, connection) = spawn_stalling_server().await;
        let provider = local_provider(kind, &origin).await.unwrap();
        let mut request = generation_request(kind, native_surface(kind), TransportMode::Unary);
        request.attempt.timeout = DurationMs::new(250);
        let transport = provider.into_transport();
        let execute = tokio::spawn(async move { transport.execute(request).await });
        tokio::time::timeout(Duration::from_secs(1), accepted)
            .await
            .unwrap_or_else(|_| panic!("{kind:?}: connector request was not sent"))
            .expect("connector request was not sent");
        let error = tokio::time::timeout(Duration::from_secs(1), execute)
            .await
            .unwrap_or_else(|_| panic!("{kind:?}: connector ignored the attempt deadline"))
            .unwrap()
            .expect_err("stalled provider must hit the attempt deadline");
        assert_eq!(
            error.class,
            AttemptFailureClass::Timeout,
            "{kind:?}: {error:?}"
        );
        let expected_phase = if kind == ProviderKind::Bedrock {
            TransportPhase::Body
        } else {
            TransportPhase::FirstByte
        };
        assert_eq!(error.phase, expected_phase, "{kind:?}: {error:?}");
        connection.abort();
    }
}

#[tokio::test]
async fn dropping_execute_cancels_every_in_flight_connector_request() {
    for kind in ProviderKind::ALL {
        let (origin, accepted, connection) = spawn_stalling_server().await;
        let provider = local_provider(kind, &origin).await.unwrap();
        let transport = provider.into_transport();
        let request = generation_request(kind, native_surface(kind), TransportMode::Unary);
        let execute = tokio::spawn(async move { transport.execute(request).await });
        accepted.await.expect("connector request was not sent");
        execute.abort();
        let _ = execute.await;
        let closed = tokio::time::timeout(Duration::from_secs(1), connection)
            .await
            .expect("cancelled request must close its socket")
            .unwrap()
            .unwrap();
        assert_eq!(closed, 0, "{kind:?}: no bytes after request body");
    }
}

#[tokio::test]
async fn dropping_stream_after_handoff_closes_every_connector_socket() {
    for kind in ProviderKind::ALL {
        let (origin, connection) =
            spawn_streaming_handoff_server(stream_handoff_response(kind)).await;
        let provider = local_provider(kind, &origin).await.unwrap();
        let output = provider
            .into_transport()
            .execute(generation_request(
                kind,
                native_surface(kind),
                TransportMode::Streaming,
            ))
            .await
            .unwrap();
        let ProviderOutput::Events(events) = output else {
            panic!("{kind:?} did not return a stream");
        };
        drop(events);

        let (captured, bytes_after_request) =
            tokio::time::timeout(Duration::from_secs(1), connection)
                .await
                .expect("dropping a handed-off stream must close its socket")
                .unwrap();
        assert_endpoint_and_auth(kind, TransportMode::Streaming, &captured);
        assert_eq!(
            bytes_after_request, 0,
            "{kind:?}: no bytes after request body"
        );
    }
}

fn stream_handoff_response(kind: ProviderKind) -> Vec<u8> {
    match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
            http_stream_response(
                "text/event-stream",
                format!(
                    "data: {{\"id\":\"handoff\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"{MODEL}\",\"choices\":[{{\"index\":0,\"delta\":{{\"role\":\"assistant\",\"content\":\"partial\"}},\"finish_reason\":null}}]}}\n\n"
                ),
            )
        }
        ProviderKind::Anthropic => http_stream_response(
            "text/event-stream",
            format!(
                "event: message_start\ndata: {{\"type\":\"message_start\",\"message\":{{\"id\":\"handoff\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"{MODEL}\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{{\"input_tokens\":1,\"output_tokens\":0}}}}}}\n\n"
            ),
        ),
        ProviderKind::Gemini | ProviderKind::VertexAi => http_stream_response(
            "text/event-stream",
            r#"data: {"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]},"index":0}]}

"#,
        ),
        ProviderKind::Bedrock => http_stream_response(
            "application/vnd.amazon.eventstream",
            event_frame("messageStart", &json!({"role": "assistant"}).to_string()),
        ),
    }
}

fn truncated_stream_response(kind: ProviderKind) -> Vec<u8> {
    match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
            http_response(
                "200 OK",
                "text/event-stream",
                format!(
                    "data: {{\"id\":\"truncated\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"{MODEL}\",\"choices\":[{{\"index\":0,\"delta\":{{\"role\":\"assistant\",\"content\":\"partial\"}},\"finish_reason\":null}}]}}\n\n"
                ),
            )
        }
        ProviderKind::Anthropic => http_response(
            "200 OK",
            "text/event-stream",
            format!(
                "event: message_start\ndata: {{\"type\":\"message_start\",\"message\":{{\"id\":\"truncated\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"{MODEL}\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{{\"input_tokens\":1,\"output_tokens\":0}}}}}}\n\n"
            ),
        ),
        ProviderKind::Gemini | ProviderKind::VertexAi => http_response(
            "200 OK",
            "text/event-stream",
            r#"data: {"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]},"index":0}]}

"#,
        ),
        ProviderKind::Bedrock => http_response(
            "200 OK",
            "application/vnd.amazon.eventstream",
            event_frame("messageStart", &json!({"role": "assistant"}).to_string()),
        ),
    }
}

fn invalid_ordered_stream_response(kind: ProviderKind) -> Vec<u8> {
    match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
            let finished = format!(
                "data: {{\"id\":\"invalid-order\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"{MODEL}\",\"choices\":[{{\"index\":0,\"delta\":{{}},\"finish_reason\":\"stop\"}}]}}\n\n"
            );
            let late_data = format!(
                "data: {{\"id\":\"invalid-order\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"{MODEL}\",\"choices\":[{{\"index\":0,\"delta\":{{\"content\":\"late\"}},\"finish_reason\":null}}]}}\n\n"
            );
            http_response(
                "200 OK",
                "text/event-stream",
                [finished, late_data].concat(),
            )
        }
        ProviderKind::Anthropic => http_response(
            "200 OK",
            "text/event-stream",
            format!(
                "event: message_start\ndata: {{\"type\":\"message_start\",\"message\":{{\"id\":\"invalid-order\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"{MODEL}\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{{\"input_tokens\":1,\"output_tokens\":0}}}}}}\n\n\
                 event: message_delta\ndata: {{\"type\":\"message_delta\",\"delta\":{{\"stop_reason\":\"end_turn\",\"stop_sequence\":null}},\"usage\":{{\"output_tokens\":1}}}}\n\n\
                 event: content_block_delta\ndata: {{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{{\"type\":\"text_delta\",\"text\":\"late\"}}}}\n\n"
            ),
        ),
        ProviderKind::Gemini | ProviderKind::VertexAi => http_response(
            "200 OK",
            "text/event-stream",
            concat!(
                "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[]},\"finishReason\":\"STOP\",\"index\":0}]}\n\n",
                "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"late\"}]},\"index\":0}]}\n\n"
            ),
        ),
        ProviderKind::Bedrock => http_response(
            "200 OK",
            "application/vnd.amazon.eventstream",
            event_frame(
                "contentBlockDelta",
                r#"{"delta":{"text":"late"},"contentBlockIndex":0}"#,
            ),
        ),
    }
}

#[tokio::test]
async fn all_streaming_connectors_reject_truncated_event_sequences() {
    for kind in ProviderKind::ALL {
        let (transport, server) = transport_at(kind, truncated_stream_response(kind)).await;
        let output = transport
            .execute(generation_request(
                kind,
                native_surface(kind),
                TransportMode::Streaming,
            ))
            .await
            .unwrap();
        let error = collect_events(output)
            .await
            .expect_err("stream without a vendor terminal event must fail");
        assert_eq!(
            error.class,
            AttemptFailureClass::Protocol,
            "{kind:?}: {error:?}"
        );
        assert!(
            error.response_committed,
            "{kind:?}: partial output commits response"
        );
        server.await.unwrap();
    }
}

#[tokio::test]
async fn all_streaming_connectors_reject_misordered_event_sequences() {
    for kind in ProviderKind::ALL {
        let (transport, server) = transport_at(kind, invalid_ordered_stream_response(kind)).await;
        let output = transport
            .execute(generation_request(
                kind,
                native_surface(kind),
                TransportMode::Streaming,
            ))
            .await
            .unwrap();
        let error = collect_events(output)
            .await
            .expect_err("misordered vendor stream must fail");
        assert_eq!(
            error.class,
            AttemptFailureClass::Protocol,
            "{kind:?}: {error:?}"
        );
        assert_eq!(error.phase, TransportPhase::Body, "{kind:?}: {error:?}");
        server.await.unwrap();
    }
}

fn invalid_body_response(case: &str) -> Vec<u8> {
    match case {
        "empty" => http_response("200 OK", "application/json", []),
        "malformed" => http_response("200 OK", "application/json", b"{"),
        "truncated" => {
            b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 20\r\nConnection: close\r\n\r\n{}".to_vec()
        }
        _ => unreachable!(),
    }
}

#[tokio::test]
async fn all_unary_connectors_fail_closed_on_empty_malformed_and_truncated_bodies() {
    for kind in ProviderKind::ALL {
        for case in ["empty", "malformed", "truncated"] {
            let (transport, server) = transport_at(kind, invalid_body_response(case)).await;
            let error = transport
                .execute(generation_request(
                    kind,
                    native_surface(kind),
                    TransportMode::Unary,
                ))
                .await
                .expect_err("invalid body must fail");
            if case == "truncated" {
                assert!(
                    matches!(
                        error.class,
                        AttemptFailureClass::Protocol | AttemptFailureClass::Connect
                    ),
                    "{kind:?} {case}: {error:?}"
                );
            } else {
                assert_eq!(
                    error.class,
                    AttemptFailureClass::Protocol,
                    "{kind:?} {case}: {error:?}"
                );
            }
            assert!(!error.response_committed, "{kind:?} {case}");
            server.await.unwrap();
        }
    }
}

#[tokio::test]
async fn all_generation_connectors_hydrate_bounded_inline_media() {
    use matrix::{Disposition, ROWS};

    for row in ROWS {
        let kind = row.kind;
        if let Disposition::Inapplicable(_) = row.media {
            continue;
        }
        let openai_like = matches!(
            kind,
            ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi
        );
        let response = if openai_like {
            http_response("200 OK", "application/json", r#"{"text":"hello back"}"#)
        } else {
            unary_response(kind)
        };
        let (transport, server) = transport_at(kind, response).await;
        let mut request = generation_request(kind, native_surface(kind), TransportMode::Unary);
        if openai_like {
            request.metadata.operation = OperationKind::Transcription;
            request.operation = Operation::Transcription(TranscriptionRequest {
                route: RouteSlug::parse("transcription").unwrap(),
                audio: MediaHandle::new("conformance-audio"),
                language: Some("en".to_owned()),
                prompt: None,
                stream: false,
                extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
            });
            request.media = Some(Arc::new(InlineMediaSpool {
                filename: "sample.wav",
                content_type: "audio/wav",
                data: b"wave-data",
            }));
            let output = transport.execute(request).await.unwrap();
            assert!(matches!(
                output,
                ProviderOutput::Result(result)
                    if matches!(*result, CanonicalResult::Transcription(_))
            ));
        } else {
            let Operation::Generation(generation) = &mut request.operation else {
                unreachable!()
            };
            generation.messages[0].content.push(ContentPart::Image {
                source: MediaSource::Handle(MediaHandle::new("conformance-image")),
                detail: None,
            });
            let mime_path = match kind {
                ProviderKind::Anthropic => "/messages/0/content/1/source/media_type",
                ProviderKind::Gemini | ProviderKind::VertexAi => {
                    "/contents/0/parts/1/inlineData/mimeType"
                }
                _ => unreachable!(),
            };
            generation
                .extensions
                .values
                .insert(mime_path.to_owned(), json!("image/png"));
            request.media = Some(Arc::new(InlineMediaSpool {
                filename: "pixel.png",
                content_type: "image/png",
                data: b"hi",
            }));
            let events = collect_events(transport.execute(request).await.unwrap())
                .await
                .unwrap();
            validate_event_sequence(&events).unwrap();
        }
        let captured = server.await.unwrap();
        if openai_like {
            assert!(captured.body_text().contains("wave-data"), "{kind:?}");
            assert!(captured.head.contains("multipart/form-data"), "{kind:?}");
        } else {
            assert!(captured.body_text().contains("aGk="), "{kind:?}");
            assert!(captured.body_text().contains("image/png"), "{kind:?}");
        }
    }
}

#[tokio::test]
async fn bounded_http_connectors_reject_oversized_unary_responses() {
    use matrix::{Disposition, ROWS};

    const OVERSIZED: usize = 16 * 1024 * 1024 + 1;
    for row in ROWS {
        let kind = row.kind;
        if let Disposition::Inapplicable(_) = row.oversized_responses {
            continue;
        }
        let response = http_response("200 OK", "application/json", vec![b'x'; OVERSIZED]);
        let (transport, server) = transport_at(kind, response).await;
        let error = transport
            .execute(generation_request(
                kind,
                native_surface(kind),
                TransportMode::Unary,
            ))
            .await
            .expect_err("oversized response must fail before decoding");
        assert_eq!(error.phase, TransportPhase::Body, "{kind:?}: {error:?}");
        assert_eq!(
            error.class,
            AttemptFailureClass::Protocol,
            "{kind:?}: {error:?}"
        );
        assert!(!error.response_committed, "{kind:?}");
        server.await.unwrap();
    }
}

fn oversized_stream_response() -> Vec<u8> {
    let event = format!("data: {}\n\n", "x".repeat(DEFAULT_MAX_EVENT_BYTES + 1));
    http_response("200 OK", "text/event-stream", event)
}

#[tokio::test]
async fn bounded_http_connectors_reject_oversized_streaming_events() {
    use matrix::{Disposition, ROWS};

    for row in ROWS {
        let kind = row.kind;
        if let Disposition::Inapplicable(_) = row.oversized_responses {
            continue;
        }
        let (transport, server) = transport_at(kind, oversized_stream_response()).await;
        let output = transport
            .execute(generation_request(
                kind,
                native_surface(kind),
                TransportMode::Streaming,
            ))
            .await
            .unwrap();
        let error = collect_events(output)
            .await
            .expect_err("oversized streaming event must fail before decoding");
        assert_eq!(error.phase, TransportPhase::Body, "{kind:?}: {error:?}");
        assert_eq!(
            error.class,
            AttemptFailureClass::Protocol,
            "{kind:?}: {error:?}"
        );
        assert!(
            error.message.contains("SSE event exceeds"),
            "{kind:?}: {error:?}"
        );
        assert!(!error.response_committed, "{kind:?}");
        server.await.unwrap();
    }
}

#[tokio::test]
async fn every_reviewed_capability_tuple_executes_its_certification_contract() {
    for kind in ProviderKind::ALL {
        for (operation, surface, mode) in matrix::expected_certifiable_capabilities(kind) {
            let capability = CompatibleCapability {
                operation,
                surface,
                mode,
            };
            let (origin, shutdown, server) = spawn_certification_emulator(kind).await;
            let provider = local_provider(kind, &origin)
                .await
                .expect("assemble local certification connector");
            let upstream_model = if kind == ProviderKind::AzureOpenAi {
                "conformance-deployment"
            } else {
                MODEL
            };
            provider
                .facade()
                .certify_capability(upstream_model, capability)
                .await
                .unwrap_or_else(|error| {
                    panic!(
                        "{kind:?} {operation:?} {surface:?} {mode:?} certification failed: {error}"
                    )
                });
            shutdown.send(()).unwrap();
            let captured = server.await.unwrap();
            assert!(
                !captured.is_empty(),
                "{kind:?} {operation:?} {surface:?} {mode:?} made no certification probe"
            );
        }
    }
}

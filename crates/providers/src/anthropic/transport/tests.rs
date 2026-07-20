use std::{collections::BTreeMap, sync::Arc};

use olp_domain::{
    AttemptPlan, DurationMs, GenerationParameters, GenerationRequest, Message as CoreMessage,
    MessageRole, OperationKind, ProviderId, RequestId, RequestMetadata, RouteId, RouteSlug,
    RuntimeGenerationId, SourceExtensions, TargetId, TokenCountRequest,
};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
    sync::oneshot,
};

use super::*;
use crate::anthropic::ConnectorTimeouts;

struct MockResponse {
    chunks: Vec<(Duration, Vec<u8>)>,
}

struct InlineSpool;

impl MediaSpool for InlineSpool {
    fn put<'a>(
        &'a self,
        _upload: olp_domain::MediaUpload,
    ) -> olp_domain::BoxFuture<'a, Result<olp_domain::MediaArtifact, olp_domain::MediaSpoolError>>
    {
        Box::pin(async { Err(olp_domain::MediaSpoolError::Unavailable) })
    }

    fn open<'a>(
        &'a self,
        handle: &'a olp_domain::MediaHandle,
    ) -> olp_domain::BoxFuture<'a, Result<olp_domain::OpenedMedia, olp_domain::MediaSpoolError>>
    {
        let handle = handle.clone();
        Box::pin(async move {
            Ok(olp_domain::OpenedMedia {
                artifact: olp_domain::MediaArtifact {
                    handle,
                    content_type: Some("image/png".into()),
                    content_length: Some(2),
                },
                filename: "inline.png".into(),
                bytes: Box::pin(stream::once(async { Ok(Bytes::from_static(b"hi")) })),
            })
        })
    }

    fn remove<'a>(
        &'a self,
        _handle: &'a olp_domain::MediaHandle,
    ) -> olp_domain::BoxFuture<'a, Result<(), olp_domain::MediaSpoolError>> {
        Box::pin(async { Ok(()) })
    }
}

#[tokio::test]
async fn same_protocol_base64_image_handle_is_rehydrated() {
    let handle = olp_domain::MediaHandle::new("inline");
    let mut messages = vec![Message {
        role: Role::User,
        content: MessageContent::Blocks(vec![ContentBlock::Image(ImageBlock {
            kind: "image".into(),
            source: AnthropicMediaSource {
                kind: "base64".into(),
                media_type: Some("image/png".into()),
                data: Some(olp_domain::inline_media_marker(&handle)),
                url: None,
                extra: BTreeMap::new(),
            },
            extra: BTreeMap::new(),
        })]),
        extra: BTreeMap::new(),
    }];
    let spool: Arc<dyn MediaSpool> = Arc::new(InlineSpool);
    hydrate_anthropic_messages(&mut messages, Some(&spool))
        .await
        .unwrap();
    let MessageContent::Blocks(blocks) = &messages[0].content else {
        panic!("expected blocks")
    };
    let ContentBlock::Image(image) = &blocks[0] else {
        panic!("expected image")
    };
    assert_eq!(image.source.data.as_deref(), Some("aGk="));
}

async fn spawn_mock(response: MockResponse) -> (String, oneshot::Receiver<Vec<u8>>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (sender, receiver) = oneshot::channel();
    tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let request = read_request(&mut socket).await;
        let _ = sender.send(request);
        for (delay, chunk) in response.chunks {
            tokio::time::sleep(delay).await;
            if socket.write_all(&chunk).await.is_err() {
                return;
            }
            let _ = socket.flush().await;
        }
    });
    (format!("http://{address}/v1/"), receiver)
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

fn find_bytes(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    haystack
        .windows(needle.len())
        .position(|part| part == needle)
}

fn attempt(
    operation: OperationKind,
    mode: TransportMode,
    operation_value: Operation,
) -> ProviderRequest {
    ProviderRequest {
        metadata: RequestMetadata {
            request_id: RequestId::new(),
            operation,
            surface: Surface::Anthropic,
            mode,
        },
        attempt: AttemptPlan {
            generation_id: RuntimeGenerationId::new(),
            route_id: RouteId::new(),
            target_id: TargetId::new(),
            provider_id: ProviderId::new(),
            provider_kind: ProviderKind::Anthropic,
            provider_model: "claude-sonnet-4-5".into(),
            timeout: DurationMs::new(2_000),
            priority: 0,
        },
        operation: operation_value,
        media: None,
    }
}

fn generation(streaming: bool) -> ProviderRequest {
    attempt(
        OperationKind::Generation,
        if streaming {
            TransportMode::Streaming
        } else {
            TransportMode::Unary
        },
        Operation::Generation(GenerationRequest {
            route: RouteSlug::parse("default").unwrap(),
            messages: vec![CoreMessage {
                role: MessageRole::User,
                content: vec![ContentPart::Text {
                    text: "hello".into(),
                }],
                name: None,
                tool_call_id: None,
                tool_calls: Vec::new(),
            }],
            parameters: GenerationParameters {
                max_output_tokens: Some(32),
                stream: streaming,
                ..GenerationParameters::default()
            },
            tools: Vec::new(),
            tool_choice: None,
            response_format: None,
            extensions: SourceExtensions::new(Surface::Anthropic, BTreeMap::new()),
        }),
    )
}

fn count() -> ProviderRequest {
    attempt(
        OperationKind::TokenCount,
        TransportMode::Unary,
        Operation::TokenCount(TokenCountRequest {
            route: RouteSlug::parse("default").unwrap(),
            input: vec![ContentPart::Text {
                text: "hello".into(),
            }],
            extensions: SourceExtensions::default(),
        }),
    )
}

#[test]
fn preserved_count_tokens_body_is_forwarded_exactly_with_late_bound_model() {
    let mut request = count();
    let Operation::TokenCount(count) = &mut request.operation else {
        unreachable!()
    };
    count.extensions = SourceExtensions::new(
        Surface::Anthropic,
        BTreeMap::from([(
            ANTHROPIC_COUNT_REQUEST_EXTENSION.into(),
            serde_json::json!({
                "model": "public-route",
                "system": "keep system",
                "messages": [{"role":"user","content":"hello"}],
                "tools": [{"name":"lookup","input_schema":{"type":"object"}}],
                "vendor": true
            }),
        )]),
    );
    let wire = encode_count_tokens(count, "claude-private").unwrap();
    let wire = serde_json::to_value(wire).unwrap();
    assert_eq!(wire["model"], "claude-private");
    assert_eq!(wire["system"], "keep system");
    assert_eq!(wire["tools"][0]["name"], "lookup");
    assert_eq!(wire["vendor"], true);
}

fn connector(base_url: &str) -> AnthropicConnector {
    AnthropicConnector::new(
        ConnectorConfig::for_local_test(base_url, ConnectorTimeouts::default()),
        AnthropicApiKey::new("upstream-secret").unwrap(),
    )
}

fn response(content_type: &str, body: &[u8]) -> Vec<u8> {
    let headers = format!(
        "HTTP/1.1 200 OK\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    );
    [headers.as_bytes(), body].concat()
}

#[tokio::test]
async fn model_discovery_uses_anthropic_pagination_contract() {
    let body = br#"{"data":[{"id":"claude-test","display_name":"Claude Test"}],"has_more":false,"last_id":"claude-test"}"#;
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, response("application/json", body))],
    })
    .await;
    let models = connector(&base_url).discover_models().await.unwrap();
    assert_eq!(models[0].display_name, "Claude Test");
    let request = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(request.starts_with("GET /v1/models?limit=100 "));
    assert!(request.contains("x-api-key: upstream-secret"));
}

async fn collect(connector: &AnthropicConnector, request: ProviderRequest) -> Vec<CanonicalEvent> {
    let ProviderOutput::Events(mut stream) = connector.execute(request).await.unwrap() else {
        panic!("Anthropic connector returned a unary result for an event operation");
    };
    let mut events = Vec::new();
    while let Some(event) = stream.next().await {
        events.push(event.unwrap());
    }
    events
}

#[tokio::test]
async fn executes_unary_messages_with_late_bound_headers() {
    let body = serde_json::to_vec(&serde_json::json!({
        "id":"msg_1","type":"message","role":"assistant",
        "content":[{"type":"text","text":"hello back"}],
        "model":"claude-sonnet-4-5","stop_reason":"end_turn","stop_sequence":null,
        "usage":{"input_tokens":2,"output_tokens":2}
    }))
    .unwrap();
    let (base, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, response("application/json", &body))],
    })
    .await;
    let events = collect(&connector(&base), generation(false)).await;
    assert!(events.iter().any(|event| matches!(&event.kind, CanonicalEventKind::TextDelta { text, .. } if text == "hello back")));
    assert!(matches!(
        events.last().map(|event| &event.kind),
        Some(CanonicalEventKind::Done)
    ));
    let request = String::from_utf8(captured.await.unwrap())
        .unwrap()
        .to_ascii_lowercase();
    assert!(request.starts_with("post /v1/messages "));
    assert!(request.contains("x-api-key: upstream-secret"));
    assert!(request.contains("anthropic-version: 2023-06-01"));
    assert!(request.contains("\"model\":\"claude-sonnet-4-5\""));
}

#[tokio::test]
async fn decodes_fragmented_stream_and_token_count() {
    let sse = concat!(
        "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n",
        "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
        "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"snow ☃\"}}\n\n",
        "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
        "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n",
        "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
    ).as_bytes().to_vec();
    let split = find_bytes(&sse, "☃".as_bytes()).unwrap() + 1;
    let headers =
        b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n";
    let (base, _) = spawn_mock(MockResponse {
        chunks: vec![
            (Duration::ZERO, [headers.as_slice(), &sse[..split]].concat()),
            (Duration::from_millis(2), sse[split..].to_vec()),
        ],
    })
    .await;
    let events = collect(&connector(&base), generation(true)).await;
    assert!(events.iter().any(|event| matches!(&event.kind, CanonicalEventKind::TextDelta { text, .. } if text == "snow ☃")));

    let count_body = br#"{"input_tokens":7}"#;
    let (base, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, response("application/json", count_body))],
    })
    .await;
    let output = connector(&base).execute(count()).await.unwrap();
    assert!(matches!(
        output,
        ProviderOutput::Result(result)
            if matches!(*result, CanonicalResult::TokenCount(TokenCountResult { input_tokens: 7, .. }))
    ));
    let request = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(request.starts_with("POST /v1/messages/count_tokens "));
}

#[tokio::test]
async fn redirects_are_not_followed_and_errors_redact_credentials() {
    let redirect = b"HTTP/1.1 302 Found\r\nLocation: http://169.254.169.254/latest\r\nContent-Length: 0\r\nConnection: close\r\n\r\n";
    let (base, _) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, redirect.to_vec())],
    })
    .await;
    let error = connector(&base)
        .execute(generation(false))
        .await
        .err()
        .unwrap();
    assert_eq!(error.class, AttemptFailureClass::UpstreamClient);

    let message = safe_upstream_error_message(
        StatusCode::BAD_REQUEST,
        br#"{"error":{"message":"bad upstream-secret","private":"do-not-echo"}}"#,
        "upstream-secret",
    );
    assert!(message.contains("[REDACTED]"));
    assert!(!message.contains("upstream-secret"));
    assert!(!message.contains("do-not-echo"));
}

#[tokio::test]
#[ignore = "requires OLP_LIVE_ANTHROPIC_API_KEY"]
async fn live_provider_discovers_anthropic_models() {
    let key = std::env::var("OLP_LIVE_ANTHROPIC_API_KEY")
        .expect("set OLP_LIVE_ANTHROPIC_API_KEY for the ignored live test");
    let connector = AnthropicConnector::new(
        ConnectorConfig::default(),
        AnthropicApiKey::new(key).expect("live Anthropic key must be representable"),
    );
    assert!(!connector.discover_models().await.unwrap().is_empty());
}

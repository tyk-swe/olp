use std::{collections::BTreeMap, sync::Arc, time::Duration};

use super::*;
use crate::domain::{
    canonical::{
        events::{Event, Kind},
        identity::{OperationKind, RequestMetadata, Surface, TransportMode},
        requests::{
            ContentPart, GenerationParameters, GenerationRequest, Message as CoreMessage,
            MessageRole, Operation, SourceExtensions, TokenCountRequest,
        },
        results::{CanonicalResult, TokenCountResult},
    },
    ids::{DurationMs, ProviderId, RequestId, RouteId, RouteSlug, RuntimeGenerationId, TargetId},
    ports::{AttemptFailureClass, MediaSpool, ProviderOutput, ProviderRequest, ProviderTransport},
    routing::{provider::ProviderKind, selection::AttemptPlan},
};
use crate::protocols::anthropic::{
    count::ANTHROPIC_COUNT_REQUEST_EXTENSION,
    dto::{
        ContentBlock, ImageBlock, MediaSource as AnthropicMediaSource, Message, MessageContent,
        Role,
    },
};
use crate::providers::anthropic::transport::media::hydrate_anthropic_messages;
use crate::providers::mock_server::{
    MockResponse, find_bytes, response, spawn_mock as spawn_http_mock,
};
use crate::providers::{
    anthropic::{ApiKey, ConnectorConfig, transport::operations::Connector},
    connector::Timeouts,
};
use bytes::Bytes;
use futures::{StreamExt, stream};
use http::StatusCode;

struct InlineSpool;

impl MediaSpool for InlineSpool {
    fn put<'a>(
        &'a self,
        _upload: crate::domain::ports::MediaUpload,
    ) -> crate::domain::ports::BoxFuture<
        'a,
        Result<
            crate::domain::canonical::results::MediaArtifact,
            crate::domain::ports::MediaSpoolError,
        >,
    > {
        Box::pin(async { Err(crate::domain::ports::MediaSpoolError::Unavailable) })
    }

    fn open<'a>(
        &'a self,
        handle: &'a crate::domain::canonical::requests::MediaHandle,
    ) -> crate::domain::ports::BoxFuture<
        'a,
        Result<crate::domain::ports::OpenedMedia, crate::domain::ports::MediaSpoolError>,
    > {
        let handle = handle.clone();
        Box::pin(async move {
            Ok(crate::domain::ports::OpenedMedia {
                artifact: crate::domain::canonical::results::MediaArtifact {
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
        _handle: &'a crate::domain::canonical::requests::MediaHandle,
    ) -> crate::domain::ports::BoxFuture<'a, Result<(), crate::domain::ports::MediaSpoolError>>
    {
        Box::pin(async { Ok(()) })
    }
}

#[tokio::test]
async fn same_protocol_base64_image_handle_is_rehydrated() {
    let handle = crate::domain::canonical::requests::MediaHandle::new("inline");
    let mut messages = vec![Message {
        role: Role::User,
        content: MessageContent::Blocks(vec![ContentBlock::Image(ImageBlock {
            kind: "image".into(),
            source: AnthropicMediaSource {
                kind: "base64".into(),
                media_type: Some("image/png".into()),
                data: Some(crate::domain::canonical::requests::inline_media_marker(
                    &handle,
                )),
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

async fn spawn_mock(response: MockResponse) -> (String, tokio::sync::oneshot::Receiver<Vec<u8>>) {
    spawn_http_mock("/v1/", response).await
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
            routing_id: TargetId::new(),
            provider_id: ProviderId::new(),
            provider_kind: ProviderKind::Anthropic,
            upstream_model: "claude-sonnet-4-5".into(),
            timeout: DurationMs::new(2_000),
            priority: 0,
        },
        operation: Arc::new(operation_value),
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
    let Operation::TokenCount(count) = Arc::make_mut(&mut request.operation) else {
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

fn connector(base_url: &str) -> Connector {
    Connector::new(
        ConnectorConfig::for_local_test(base_url, Timeouts::default()),
        ApiKey::new("upstream-secret").unwrap(),
    )
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

async fn collect(connector: &Connector, request: ProviderRequest) -> Vec<Event> {
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
    assert!(
        events.iter().any(
            |event| matches!(&event.kind, Kind::TextDelta { text, .. } if text == "hello back")
        )
    );
    assert!(matches!(
        events.last().map(|event| &event.kind),
        Some(Kind::Done)
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
    assert!(
        events
            .iter()
            .any(|event| matches!(&event.kind, Kind::TextDelta { text, .. } if text == "snow ☃"))
    );

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
    let connector = Connector::new(
        ConnectorConfig::default(),
        ApiKey::new(key).expect("live Anthropic key must be representable"),
    );
    assert!(!connector.discover_models().await.unwrap().is_empty());
}

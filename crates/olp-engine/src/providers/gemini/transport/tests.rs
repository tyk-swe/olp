use std::{collections::BTreeMap, sync::Arc, time::Duration};

use super::*;
use crate::domain::{
    AttemptFailureClass, AttemptPlan, CanonicalEvent, CanonicalEventKind, CanonicalResult,
    ContentPart, DurationMs, GenerationParameters, GenerationRequest, MediaSpool, Message,
    MessageRole, Operation, OperationKind, ProviderId, ProviderKind, ProviderOutput,
    ProviderRequest, ProviderTransport, RequestId, RequestMetadata, RouteId, RouteSlug,
    RuntimeGenerationId, SourceExtensions, Surface, TargetId, TokenCountRequest, TransportMode,
};
use crate::protocols::gemini::{Content, GEMINI_COUNT_REQUEST_EXTENSION, Part};
use crate::providers::gemini::transport::media::hydrate_gemini_contents;
use crate::providers::gemini::{ConnectorConfig, ConnectorTimeouts, GeminiApiKey};
use crate::providers::mock_server::{
    MockResponse, find_bytes, response, spawn_mock as spawn_http_mock,
};
use bytes::Bytes;
use futures::{StreamExt, stream};
use http::StatusCode;

struct InlineSpool;

impl MediaSpool for InlineSpool {
    fn put<'a>(
        &'a self,
        _upload: crate::domain::MediaUpload,
    ) -> crate::domain::BoxFuture<
        'a,
        Result<crate::domain::MediaArtifact, crate::domain::MediaSpoolError>,
    > {
        Box::pin(async { Err(crate::domain::MediaSpoolError::Unavailable) })
    }

    fn open<'a>(
        &'a self,
        handle: &'a crate::domain::MediaHandle,
    ) -> crate::domain::BoxFuture<
        'a,
        Result<crate::domain::OpenedMedia, crate::domain::MediaSpoolError>,
    > {
        let handle = handle.clone();
        Box::pin(async move {
            Ok(crate::domain::OpenedMedia {
                artifact: crate::domain::MediaArtifact {
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
        _handle: &'a crate::domain::MediaHandle,
    ) -> crate::domain::BoxFuture<'a, Result<(), crate::domain::MediaSpoolError>> {
        Box::pin(async { Ok(()) })
    }
}

#[tokio::test]
async fn same_protocol_inline_data_handle_is_rehydrated() {
    let handle = crate::domain::MediaHandle::new("inline");
    let mut contents = vec![Content {
        role: Some("user".into()),
        parts: vec![Part::InlineData(crate::protocols::gemini::InlineDataPart {
            inline_data: crate::protocols::gemini::Blob {
                mime_type: "image/png".into(),
                data: crate::domain::inline_media_marker(&handle),
                extra: BTreeMap::new(),
            },
            extra: BTreeMap::new(),
        })],
        extra: BTreeMap::new(),
    }];
    let spool: Arc<dyn MediaSpool> = Arc::new(InlineSpool);
    hydrate_gemini_contents(&mut contents, Some(&spool))
        .await
        .unwrap();
    let Part::InlineData(part) = &contents[0].parts[0] else {
        panic!("expected inline data")
    };
    assert_eq!(part.inline_data.data, "aGk=");
}

async fn spawn_mock(response: MockResponse) -> (String, tokio::sync::oneshot::Receiver<Vec<u8>>) {
    spawn_http_mock("/v1beta/", response).await
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
            surface: Surface::Gemini,
            mode,
        },
        attempt: AttemptPlan {
            generation_id: RuntimeGenerationId::new(),
            route_id: RouteId::new(),
            target_id: TargetId::new(),
            provider_id: ProviderId::new(),
            provider_kind: ProviderKind::Gemini,
            upstream_model: "gemini-2.5-flash".into(),
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
            messages: vec![Message {
                role: MessageRole::User,
                content: vec![ContentPart::Text {
                    text: "hello".into(),
                }],
                name: None,
                tool_call_id: None,
                tool_calls: Vec::new(),
            }],
            parameters: GenerationParameters {
                stream: streaming,
                ..GenerationParameters::default()
            },
            tools: Vec::new(),
            tool_choice: None,
            response_format: None,
            extensions: SourceExtensions::new(Surface::Gemini, BTreeMap::new()),
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
fn preserved_count_tokens_body_keeps_nested_semantics_and_rebinds_model() {
    let mut request = count();
    let Operation::TokenCount(count) = &mut request.operation else {
        unreachable!()
    };
    count.extensions = SourceExtensions::new(
        Surface::Gemini,
        BTreeMap::from([(
            GEMINI_COUNT_REQUEST_EXTENSION.into(),
            serde_json::json!({
                "generateContentRequest": {
                    "model": "models/public-route",
                    "contents": [{"role":"user","parts":[{"text":"hello"}]}],
                    "safetySettings": [{"category":"HARM_CATEGORY_HATE_SPEECH","threshold":"BLOCK_NONE"}]
                },
                "vendorOption": true
            }),
        )]),
    );
    let wire = encode_count_tokens(count, "models/gemini-private").unwrap();
    let wire = serde_json::to_value(wire).unwrap();
    assert_eq!(
        wire["generateContentRequest"]["model"],
        "models/gemini-private"
    );
    assert!(wire["generateContentRequest"]["safetySettings"].is_array());
    assert_eq!(wire["vendorOption"], true);
}

fn connector(base_url: &str) -> GeminiConnector {
    GeminiConnector::new(
        ConnectorConfig::for_local_test(base_url, ConnectorTimeouts::default()),
        GeminiApiKey::new("upstream-secret").unwrap(),
    )
}

#[tokio::test]
async fn model_discovery_uses_gemini_pagination_contract() {
    let body = br#"{"models":[{"name":"models/gemini-test","displayName":"Gemini Test"}]}"#;
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, response("application/json", body))],
    })
    .await;
    let models = connector(&base_url).discover_models().await.unwrap();
    assert_eq!(models[0].display_name, "Gemini Test");
    let request = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(request.starts_with("GET /v1beta/models?pageSize=1000 "));
    assert!(request.contains("x-goog-api-key: upstream-secret"));
}

async fn collect(connector: &GeminiConnector, request: ProviderRequest) -> Vec<CanonicalEvent> {
    let ProviderOutput::Events(mut stream) = connector.execute(request).await.unwrap() else {
        panic!("Gemini connector returned a unary result for an event operation");
    };
    let mut events = Vec::new();
    while let Some(event) = stream.next().await {
        events.push(event.unwrap());
    }
    events
}

#[tokio::test]
async fn executes_unary_generation_with_header_auth_and_model_path() {
    let body = serde_json::to_vec(&serde_json::json!({
        "candidates":[{"content":{"role":"model","parts":[{"text":"hello back"}]},"finishReason":"STOP","index":0}],
        "usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":2,"totalTokenCount":4},
        "modelVersion":"gemini-2.5-flash","responseId":"response-1"
    })).unwrap();
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
    assert!(request.starts_with("post /v1beta/models/gemini-2.5-flash:generatecontent "));
    assert!(request.contains("x-goog-api-key: upstream-secret"));
    assert!(!request.contains("?key="));
}

#[tokio::test]
async fn decodes_fragmented_sse_and_count_tokens() {
    let sse = concat!(
        "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"snow ☃\"}]},\"index\":0}]}\n\n",
        "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[]},\"finishReason\":\"STOP\",\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":2,\"totalTokenCount\":4}}\n\n"
    ).as_bytes().to_vec();
    let split = find_bytes(&sse, "☃".as_bytes()).unwrap() + 1;
    let headers = b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream; charset=utf-8\r\nConnection: close\r\n\r\n";
    let (base, captured) = spawn_mock(MockResponse {
        chunks: vec![
            (Duration::ZERO, [headers.as_slice(), &sse[..split]].concat()),
            (Duration::from_millis(2), sse[split..].to_vec()),
        ],
    })
    .await;
    let events = collect(&connector(&base), generation(true)).await;
    assert!(events.iter().any(|event| matches!(&event.kind, CanonicalEventKind::TextDelta { text, .. } if text == "snow ☃")));
    let request = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(
        request.starts_with("POST /v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse ")
    );

    let (base, captured) = spawn_mock(MockResponse {
        chunks: vec![(
            Duration::ZERO,
            response(
                "application/json",
                br#"{"totalTokens":7,"cachedContentTokenCount":2}"#,
            ),
        )],
    })
    .await;
    let ProviderOutput::Result(result) = connector(&base).execute(count()).await.unwrap() else {
        panic!("Gemini countTokens must return a typed result")
    };
    let CanonicalResult::TokenCount(result) = *result else {
        panic!("Gemini countTokens returned the wrong result type")
    };
    assert_eq!(result.input_tokens, 7);
    assert_eq!(
        result.extensions.values["/cachedContentTokenCount"],
        serde_json::json!(2)
    );
    let request = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(request.starts_with("POST /v1beta/models/gemini-2.5-flash:countTokens "));
}

#[tokio::test]
async fn redirects_are_not_followed_and_error_messages_redact_keys() {
    let redirect = b"HTTP/1.1 307 Temporary Redirect\r\nLocation: http://169.254.169.254/latest\r\nContent-Length: 0\r\nConnection: close\r\n\r\n";
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
        br#"{"error":{"message":"bad upstream-secret","details":"do-not-echo"}}"#,
        "upstream-secret",
    );
    assert!(message.contains("[REDACTED]"));
    assert!(!message.contains("upstream-secret"));
    assert!(!message.contains("do-not-echo"));
}

#[tokio::test]
#[ignore = "requires OLP_LIVE_GEMINI_API_KEY"]
async fn live_provider_discovers_gemini_models() {
    let key = std::env::var("OLP_LIVE_GEMINI_API_KEY")
        .expect("set OLP_LIVE_GEMINI_API_KEY for the ignored live test");
    let connector = GeminiConnector::new(
        ConnectorConfig::default(),
        GeminiApiKey::new(key).expect("live Gemini key must be representable"),
    );
    assert!(!connector.discover_models().await.unwrap().is_empty());
}

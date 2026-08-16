use std::{collections::BTreeMap, sync::Arc, time::Duration};

use super::*;
use crate::domain::{
    canonical::{
        events::{Event, Kind},
        identity::{OperationKind, RequestMetadata, Surface, TransportMode},
        requests::{
            ContentPart, GenerationParameters, GenerationRequest, MediaHandle, MediaSource,
            Message, MessageRole, ModerationRequest, Operation, SourceExtensions,
            TokenCountRequest,
        },
        results::CanonicalResult,
    },
    ids::{DurationMs, ProviderId, RequestId, RouteId, RouteSlug, RuntimeGenerationId, TargetId},
    ports::{AttemptFailureClass, MediaSpool, ProviderOutput, ProviderRequest, ProviderTransport},
    routing::{provider::ProviderKind, selection::AttemptPlan},
};
use crate::protocols::gemini::{
    count::GEMINI_COUNT_REQUEST_EXTENSION,
    dto::{Blob, Content, InlineDataPart, Part},
};
use crate::providers::gemini::transport::media::hydrate_gemini_contents;
use crate::providers::mock_server::{
    MockResponse, find_bytes, response, spawn_mock as spawn_http_mock, status_response,
};
use crate::providers::{
    connector::Timeouts,
    gemini::{
        ApiKey, ConnectorConfig,
        transport::operations::{Connector, validate_operation},
    },
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
async fn same_protocol_inline_data_handle_is_rehydrated() {
    let handle = crate::domain::canonical::requests::MediaHandle::new("inline");
    let mut contents = vec![Content {
        role: Some("user".into()),
        parts: vec![Part::InlineData(InlineDataPart {
            inline_data: Blob {
                mime_type: "image/png".into(),
                data: crate::domain::canonical::requests::inline_media_marker(&handle),
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

fn count_request(input: Vec<ContentPart>, extensions: SourceExtensions) -> TokenCountRequest {
    TokenCountRequest {
        route: RouteSlug::parse("default").unwrap(),
        input,
        extensions,
    }
}

fn extensions(
    surface: Surface,
    values: impl IntoIterator<Item = (&'static str, serde_json::Value)>,
) -> SourceExtensions {
    SourceExtensions::new(
        surface,
        values
            .into_iter()
            .map(|(path, value)| (path.to_owned(), value))
            .collect(),
    )
}

fn image(source: MediaSource, detail: Option<&str>) -> ContentPart {
    ContentPart::Image {
        source,
        detail: detail.map(str::to_owned),
    }
}

#[test]
fn operation_validation_and_count_reconstruction_fail_closed() {
    let generation = generation(false).operation;
    validate_operation(&generation, "gemini-private").unwrap();
    let count = count().operation;
    validate_operation(&count, "gemini-private").unwrap();
    let unsupported = Operation::Moderation(ModerationRequest {
        route: RouteSlug::parse("default").unwrap(),
        input: vec![ContentPart::Text { text: "x".into() }],
        extensions: SourceExtensions::default(),
    });
    assert_eq!(
        validate_operation(&unsupported, "gemini-private")
            .unwrap_err()
            .class,
        AttemptFailureClass::Protocol
    );

    let uri = MediaSource::Uri("gs://bucket/image.png".into());
    let wire = encode_count_tokens(
        &count_request(
            vec![
                ContentPart::Text {
                    text: "hello".into(),
                },
                image(uri.clone(), None),
            ],
            extensions(
                Surface::Gemini,
                [(
                    "/contents/0/parts/1/fileData/mimeType",
                    serde_json::json!("image/png"),
                )],
            ),
        ),
        "gemini-private",
    )
    .unwrap();
    assert!(matches!(wire.contents[0].parts[0], Part::Text(_)));
    assert!(matches!(wire.contents[0].parts[1], Part::FileData(_)));

    let invalid = [
        count_request(Vec::new(), SourceExtensions::default()),
        count_request(
            vec![image(uri.clone(), Some("high"))],
            extensions(
                Surface::Gemini,
                [(
                    "/contents/0/parts/0/fileData/mimeType",
                    serde_json::json!("image/png"),
                )],
            ),
        ),
        count_request(
            vec![image(MediaSource::Handle(MediaHandle::new("image")), None)],
            extensions(
                Surface::Gemini,
                [(
                    "/contents/0/parts/0/fileData/mimeType",
                    serde_json::json!("image/png"),
                )],
            ),
        ),
        count_request(vec![image(uri, None)], SourceExtensions::default()),
        count_request(
            vec![ContentPart::InputAudio {
                media: MediaHandle::new("audio"),
                format: "wav".into(),
            }],
            SourceExtensions::default(),
        ),
        count_request(
            vec![ContentPart::Refusal { text: "no".into() }],
            SourceExtensions::default(),
        ),
        count_request(
            vec![ContentPart::Text { text: "x".into() }],
            extensions(Surface::Gemini, [("/unconsumed", serde_json::json!(true))]),
        ),
        count_request(
            vec![ContentPart::Text { text: "x".into() }],
            extensions(Surface::OpenAi, [("/foreign", serde_json::json!(true))]),
        ),
        count_request(
            vec![ContentPart::Text { text: "x".into() }],
            extensions(
                Surface::Gemini,
                [(GEMINI_COUNT_REQUEST_EXTENSION, serde_json::json!({}))],
            ),
        ),
        count_request(
            vec![ContentPart::Text { text: "x".into() }],
            extensions(
                Surface::Gemini,
                [
                    (
                        GEMINI_COUNT_REQUEST_EXTENSION,
                        serde_json::json!({
                            "generateContentRequest": {
                                "model": "models/public-route",
                                "contents": [{"role":"user","parts":[{"text":"x"}]}]
                            }
                        }),
                    ),
                    ("/extra", serde_json::json!(true)),
                ],
            ),
        ),
    ];
    for request in invalid {
        assert_eq!(
            encode_count_tokens(&request, "gemini-private")
                .unwrap_err()
                .class,
            AttemptFailureClass::Protocol
        );
    }
}

#[tokio::test]
async fn request_envelope_and_transport_mode_mismatches_stop_before_network() {
    let transport = connector("http://127.0.0.1:1/v1beta/");
    let mut cases = Vec::new();

    let mut metadata_mismatch = generation(false);
    metadata_mismatch.metadata.operation = OperationKind::TokenCount;
    cases.push(metadata_mismatch);

    let mut provider_mismatch = generation(false);
    provider_mismatch.attempt.provider_kind = ProviderKind::OpenAi;
    cases.push(provider_mismatch);

    let mut asynchronous = generation(false);
    asynchronous.metadata.mode = TransportMode::Async;
    cases.push(asynchronous);

    let mut stream_mismatch = generation(false);
    let Operation::Generation(generation) = &mut stream_mismatch.operation else {
        unreachable!()
    };
    generation.parameters.stream = true;
    cases.push(stream_mismatch);

    let mut streaming_count = count();
    streaming_count.metadata.mode = TransportMode::Streaming;
    cases.push(streaming_count);

    for request in cases {
        let error = transport.execute(request).await.err().unwrap();
        assert_eq!(error.class, AttemptFailureClass::Protocol);
        assert!(!error.message.is_empty());
    }
}

fn connector(base_url: &str) -> Connector {
    Connector::new(
        ConnectorConfig::for_local_test(base_url, Timeouts::default()),
        ApiKey::new("upstream-secret").unwrap(),
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

#[tokio::test]
async fn discovery_and_probe_reject_malformed_provider_contracts() {
    for (body, expected) in [
        (b"not json".as_slice(), "not valid JSON"),
        (br#"{"other":[]}"#.as_slice(), "omitted models"),
        (br#"{"models":[{"name":""}]}"#.as_slice(), "invalid name"),
    ] {
        let (base, _) =
            spawn_mock(MockResponse::immediate(response("application/json", body))).await;
        let error = connector(&base).discover_models().await.unwrap_err();
        assert_eq!(error.class, AttemptFailureClass::Protocol);
        assert!(error.message.contains(expected));
    }

    let (base, captured) = spawn_mock(MockResponse::immediate(response(
        "application/json",
        br#"{"totalTokens":1}"#,
    )))
    .await;
    connector(&base)
        .probe_model("models/gemini-test")
        .await
        .unwrap();
    let request = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(request.starts_with("POST /v1beta/models/gemini-test:countTokens "));
    assert!(request.contains("\"health\""));

    for body in [br#"{"totalTokens":0}"#.as_slice(), b"not json".as_slice()] {
        let (base, _) =
            spawn_mock(MockResponse::immediate(response("application/json", body))).await;
        let error = connector(&base)
            .probe_model("gemini-test")
            .await
            .unwrap_err();
        assert_eq!(error.class, AttemptFailureClass::Protocol);
    }
}

#[tokio::test]
async fn upstream_statuses_and_unary_body_contracts_are_classified() {
    for (status, class) in [
        ("408 Request Timeout", AttemptFailureClass::Timeout),
        ("429 Too Many Requests", AttemptFailureClass::RateLimit),
        (
            "503 Service Unavailable",
            AttemptFailureClass::UpstreamServer,
        ),
        ("400 Bad Request", AttemptFailureClass::UpstreamClient),
    ] {
        let (base, _) = spawn_mock(MockResponse::immediate(status_response(
            status,
            "application/json",
            br#"{"error":{"message":"provider rejected request"}}"#,
        )))
        .await;
        let error = connector(&base)
            .execute(generation(false))
            .await
            .err()
            .unwrap();
        assert_eq!(error.class, class, "status {status}");
        assert!(!error.response_committed);
    }

    for (request, content_type, body) in [
        (generation(false), "text/plain", b"{}".as_slice()),
        (
            generation(false),
            "application/json",
            b"not json".as_slice(),
        ),
        (count(), "application/json", b"not json".as_slice()),
    ] {
        let (base, _) = spawn_mock(MockResponse::immediate(response(content_type, body))).await;
        let error = connector(&base).execute(request).await.err().unwrap();
        assert_eq!(error.class, AttemptFailureClass::Protocol);
    }
}

async fn collect(connector: &Connector, request: ProviderRequest) -> Vec<Event> {
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
    assert!(
        events
            .iter()
            .any(|event| matches!(&event.kind, Kind::TextDelta { text, .. } if text == "snow ☃"))
    );
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
    let connector = Connector::new(
        ConnectorConfig::default(),
        ApiKey::new(key).expect("live Gemini key must be representable"),
    );
    assert!(!connector.discover_models().await.unwrap().is_empty());
}

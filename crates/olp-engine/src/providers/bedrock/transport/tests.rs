use std::time::Duration;

use crate::domain::{
    canonical::{
        identity::{OperationKind, RequestMetadata, Surface},
        requests::{
            GenerationParameters, GenerationRequest, Message, SourceExtensions, TokenCountRequest,
        },
    },
    ids::{DurationMs, ProviderId, RequestId, RouteId, RouteSlug, RuntimeGenerationId, TargetId},
    routing::selection::AttemptPlan,
};
use aws_smithy_eventstream::frame::write_message_to;
use aws_smithy_types::event_stream::{Header, HeaderValue, Message as EventMessage};
use futures::StreamExt;
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::TcpListener,
};

use super::*;
use crate::providers::{
    bedrock::{Credentials, StaticCredentials},
    connector::Timeouts,
};

fn provider_request() -> ProviderRequest {
    ProviderRequest {
        metadata: RequestMetadata {
            request_id: RequestId::new(),
            operation: OperationKind::Generation,
            surface: Surface::OpenAi,
            mode: TransportMode::Unary,
        },
        attempt: AttemptPlan {
            generation_id: RuntimeGenerationId::new(),
            route_id: RouteId::new(),
            target_id: TargetId::new(),
            routing_id: TargetId::new(),
            provider_id: ProviderId::new(),
            provider_kind: ProviderKind::Bedrock,
            upstream_model: "anthropic.claude-test-v1:0".to_owned(),
            timeout: DurationMs::new(2_000),
            priority: 0,
        },
        operation: Operation::Generation(GenerationRequest {
            route: RouteSlug::parse("chat").unwrap(),
            messages: vec![Message {
                role: MessageRole::User,
                content: vec![crate::domain::canonical::requests::ContentPart::Text {
                    text: "hello".to_owned(),
                }],
                name: None,
                tool_call_id: None,
                tool_calls: vec![],
            }],
            parameters: GenerationParameters::default(),
            tools: vec![],
            tool_choice: None,
            response_format: None,
            extensions: SourceExtensions::default(),
        }),
        media: None,
    }
}

fn streaming_request() -> ProviderRequest {
    let mut request = provider_request();
    request.metadata.mode = TransportMode::Streaming;
    let Operation::Generation(generation) = &mut request.operation else {
        unreachable!();
    };
    generation.parameters.stream = true;
    request
}

fn token_count_request() -> ProviderRequest {
    let mut request = provider_request();
    request.metadata.operation = OperationKind::TokenCount;
    request.operation = Operation::TokenCount(TokenCountRequest {
        route: RouteSlug::parse("chat").unwrap(),
        input: vec![crate::domain::canonical::requests::ContentPart::Text {
            text: "count this".to_owned(),
        }],
        extensions: SourceExtensions::default(),
    });
    request
}

#[test]
fn connector_and_model_validation_is_explicit() {
    let request = provider_request();
    assert!(validate_request(&request).is_ok());
    for model in [
        "us.anthropic.claude-3-7-sonnet-20250219-v1:0",
        "arn:aws:bedrock:us-east-1:123456789012:inference-profile/example",
    ] {
        assert!(validate_model_id(model).is_ok(), "{model}");
    }
    for model in ["", " bad", "bad model", "bad\nmodel"] {
        assert!(validate_model_id(model).is_err(), "{model:?}");
    }
    assert!(validate_model_id(&"x".repeat(2_049)).is_err());

    let cases: [fn(&mut ProviderRequest); 5] = [
        |r| r.attempt.provider_kind = ProviderKind::OpenAi,
        |r| r.metadata.operation = OperationKind::TokenCount,
        |r| r.metadata.mode = TransportMode::Streaming,
        |r| {
            r.metadata.mode = TransportMode::Async;
            let Operation::Generation(generation) = &mut r.operation else {
                unreachable!()
            };
            generation.parameters.stream = true;
        },
        |r| {
            *r = token_count_request();
            r.metadata.mode = TransportMode::Streaming;
        },
    ];
    for mutate in cases {
        let mut invalid = provider_request();
        mutate(&mut invalid);
        assert!(validate_request(&invalid).is_err());
    }
}

#[test]
fn service_error_taxonomy_is_retry_aware() {
    for (code, expected) in [
        ("ThrottlingException", AttemptFailureClass::RateLimit),
        (
            "ServiceQuotaExceededException",
            AttemptFailureClass::RateLimit,
        ),
        ("ModelTimeoutException", AttemptFailureClass::Timeout),
        ("AccessDeniedException", AttemptFailureClass::UpstreamClient),
        (
            "UnrecognizedClientException",
            AttemptFailureClass::UpstreamClient,
        ),
        (
            "InvalidSignatureException",
            AttemptFailureClass::UpstreamClient,
        ),
        ("ExpiredTokenException", AttemptFailureClass::UpstreamClient),
        ("ValidationException", AttemptFailureClass::UpstreamClient),
        (
            "ResourceNotFoundException",
            AttemptFailureClass::UpstreamClient,
        ),
        ("ConflictException", AttemptFailureClass::UpstreamClient),
        (
            "ServiceUnavailableException",
            AttemptFailureClass::UpstreamServer,
        ),
    ] {
        assert_eq!(classify_service_code(Some(code)), expected, "{code}");
    }
    let uncoded = classify_service_code(None);
    assert_eq!(uncoded, AttemptFailureClass::UpstreamServer);
    assert!(
        TransportError {
            upstream: Default::default(),
            phase: TransportPhase::FirstByte,
            class: uncoded,
            response_committed: false,
            message: String::new(),
        }
        .allows_failover()
    );
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

async fn serve_once(
    body: Vec<u8>,
    content_type: &'static str,
) -> (String, tokio::task::JoinHandle<String>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let mut request = Vec::new();
        let mut buffer = [0_u8; 4_096];
        let header_end = loop {
            let read = socket.read(&mut buffer).await.unwrap();
            assert_ne!(read, 0);
            request.extend_from_slice(&buffer[..read]);
            if let Some(position) = request.windows(4).position(|window| window == b"\r\n\r\n") {
                break position + 4;
            }
        };
        let headers = String::from_utf8_lossy(&request[..header_end]);
        let content_length = headers
            .lines()
            .find_map(|line| {
                line.to_ascii_lowercase()
                    .strip_prefix("content-length:")
                    .map(str::trim)
                    .and_then(|value| value.parse::<usize>().ok())
            })
            .unwrap_or(0);
        while request.len() < header_end + content_length {
            let read = socket.read(&mut buffer).await.unwrap();
            assert_ne!(read, 0);
            request.extend_from_slice(&buffer[..read]);
        }
        let response_headers = format!(
            "HTTP/1.1 200 OK\r\ncontent-type: {content_type}\r\ncontent-length: {}\r\nconnection: close\r\n\r\n",
            body.len()
        );
        socket.write_all(response_headers.as_bytes()).await.unwrap();
        socket.write_all(&body).await.unwrap();
        socket.shutdown().await.unwrap();
        String::from_utf8_lossy(&request).into_owned()
    });
    (format!("http://{address}"), task)
}

async fn mock_connector(endpoint: &str) -> Connector {
    let config = ConnectorConfig::new("us-east-1")
        .unwrap()
        .with_timeouts(Timeouts {
            connect: Duration::from_secs(1),
            first_byte: Duration::from_secs(1),
            idle: Duration::from_secs(1),
        })
        .unwrap()
        .with_endpoint_url(endpoint)
        .unwrap();
    let credentials = StaticCredentials::from_json(
        br#"{"access_key_id":"AKIAEXAMPLEVALUE","secret_access_key":"secret-secret-secret"}"#,
    )
    .unwrap();
    Connector::new(config, Credentials::Static(credentials)).await
}

#[tokio::test]
async fn official_sdk_decodes_local_converse_event_stream_and_signs_request() {
    let mut frames = Vec::new();
    for (kind, payload) in [
        ("messageStart", r#"{"role":"assistant"}"#),
        (
            "contentBlockDelta",
            r#"{"delta":{"text":"hello"},"contentBlockIndex":0}"#,
        ),
        ("contentBlockStop", r#"{"contentBlockIndex":0}"#),
        (
            "contentBlockStart",
            r#"{"start":{"toolUse":{"toolUseId":"call-1","name":"weather"}},"contentBlockIndex":1}"#,
        ),
        (
            "contentBlockDelta",
            r#"{"delta":{"toolUse":{"input":"{\"city\":\"Paris\"}"}},"contentBlockIndex":1}"#,
        ),
        ("contentBlockStop", r#"{"contentBlockIndex":1}"#),
        ("messageStop", r#"{"stopReason":"tool_use"}"#),
        (
            "metadata",
            r#"{"usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2},"metrics":{"latencyMs":1}}"#,
        ),
    ] {
        frames.extend(event_frame(kind, payload));
    }
    let (endpoint, server) = serve_once(frames, "application/vnd.amazon.eventstream").await;
    let connector = mock_connector(&endpoint).await;
    let ProviderOutput::Events(events) = connector.execute(streaming_request()).await.unwrap()
    else {
        panic!("expected event stream");
    };
    let events: Vec<_> = events
        .collect::<Vec<_>>()
        .await
        .into_iter()
        .collect::<Result<_, _>>()
        .unwrap();
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        Kind::TextDelta { text, .. } if text == "hello"
    )));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        Kind::ToolCallDelta {
            id: Some(id),
            name: Some(name),
            ..
        } if id == "call-1" && name == "weather"
    )));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        Kind::ToolCallDelta {
            arguments_delta,
            ..
        } if arguments_delta == "{\"city\":\"Paris\"}"
    )));
    let tool_indexes = events
        .iter()
        .filter_map(|event| match event.kind {
            Kind::ToolCallDelta { tool_index, .. } => Some(tool_index),
            _ => None,
        })
        .collect::<Vec<_>>();
    assert_eq!(tool_indexes, vec![0, 0]);
    assert!(
        events
            .iter()
            .any(|event| matches!(event.kind, Kind::Usage { .. }))
    );
    assert!(matches!(events.last().unwrap().kind, Kind::Done));
    let request = server.await.unwrap().to_ascii_lowercase();
    assert!(request.starts_with("post /model/anthropic.claude-test-v1%3a0/converse-stream"));
    assert!(request.contains("authorization: aws4-hmac-sha256"));
    assert!(!request.contains("secret-secret-secret"));
}

#[tokio::test]
async fn official_control_sdk_discovers_connector_specific_model_ids() {
    let body = br#"{"modelSummaries":[{"modelArn":"arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-test","modelId":"anthropic.claude-test-v1:0","modelName":"Claude Test","providerName":"Anthropic","inputModalities":["TEXT"],"outputModalities":["TEXT"],"responseStreamingSupported":true,"inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"ACTIVE"}},{"modelArn":"arn:aws:bedrock:us-east-1::foundation-model/stability.image-test","modelId":"stability.image-test-v1:0","modelName":"Image Test","providerName":"Stability AI","inputModalities":["TEXT"],"outputModalities":["IMAGE"],"responseStreamingSupported":false,"inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"ACTIVE"}}]}"#.to_vec();
    let (endpoint, server) = serve_once(body, "application/json").await;
    let connector = mock_connector(&endpoint).await;
    let models = connector.discover_models().await.unwrap();
    assert_eq!(models.len(), 1);
    assert_eq!(models[0].id, "anthropic.claude-test-v1:0");
    assert_eq!(models[0].display_name, "Claude Test");
    let request = server.await.unwrap().to_ascii_lowercase();
    assert!(request.starts_with("get /foundation-models?byoutputmodality=text"));
    assert!(request.contains("authorization: aws4-hmac-sha256"));
}

#[tokio::test]
async fn official_runtime_sdk_returns_typed_token_count() {
    let (endpoint, server) = serve_once(br#"{"inputTokens":7}"#.to_vec(), "application/json").await;
    let connector = mock_connector(&endpoint).await;
    let ProviderOutput::Result(result) = connector.execute(token_count_request()).await.unwrap()
    else {
        panic!("expected typed token-count result");
    };
    let CanonicalResult::TokenCount(result) = *result else {
        panic!("expected canonical token-count result");
    };
    assert_eq!(result.input_tokens, 7);
    let request = server.await.unwrap().to_ascii_lowercase();
    assert!(request.starts_with("post /model/anthropic.claude-test-v1%3a0/count-tokens"));
    assert!(request.contains("authorization: aws4-hmac-sha256"));
}

#[tokio::test]
async fn official_runtime_sdk_maps_malformed_success_body_as_protocol() {
    let (endpoint, server) = serve_once(b"{".to_vec(), "application/json").await;
    let connector = mock_connector(&endpoint).await;
    let error = connector.execute(provider_request()).await.unwrap_err();
    assert_eq!(error.class, AttemptFailureClass::Protocol);
    assert_eq!(error.phase, TransportPhase::Body);
    assert!(!error.response_committed);
    let request = server.await.unwrap().to_ascii_lowercase();
    assert!(request.starts_with("post /model/anthropic.claude-test-v1%3a0/converse"));
}

#[tokio::test]
async fn official_sdk_performs_no_hidden_retry() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let endpoint = format!("http://{}", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        let mut accepted = 0_u32;
        loop {
            let Ok(Ok((mut socket, _))) =
                timeout(Duration::from_millis(150), listener.accept()).await
            else {
                break;
            };
            accepted = accepted.saturating_add(1);
            let mut request = [0_u8; 8_192];
            let _ = socket.read(&mut request).await.unwrap();
            let body = br#"{"message":"temporarily unavailable"}"#;
            let response = format!(
                "HTTP/1.1 503 Service Unavailable\r\ncontent-type: application/json\r\nx-amzn-errortype: ServiceUnavailableException\r\ncontent-length: {}\r\nconnection: close\r\n\r\n",
                body.len()
            );
            socket.write_all(response.as_bytes()).await.unwrap();
            socket.write_all(body).await.unwrap();
            socket.shutdown().await.unwrap();
        }
        accepted
    });
    let connector = mock_connector(&endpoint).await;
    let error = connector.execute(provider_request()).await.unwrap_err();
    assert_eq!(error.class, AttemptFailureClass::UpstreamServer);
    assert_eq!(server.await.unwrap(), 1);
}

#[tokio::test]
async fn event_stream_idle_deadline_is_enforced_after_commit() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let endpoint = format!("http://{}", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let mut request = [0_u8; 8_192];
        let _ = socket.read(&mut request).await.unwrap();
        let frame = event_frame("messageStart", r#"{"role":"assistant"}"#);
        let content_length = frame.len() + 1_000_000;
        socket
            .write_all(format!(
                "HTTP/1.1 200 OK\r\ncontent-type: application/vnd.amazon.eventstream\r\ncontent-length: {content_length}\r\nconnection: close\r\n\r\n"
            ).as_bytes())
            .await
            .unwrap();
        socket.write_all(&frame).await.unwrap();
        tokio::time::sleep(Duration::from_millis(250)).await;
    });
    let config = ConnectorConfig::new("us-east-1")
        .unwrap()
        .with_timeouts(Timeouts {
            connect: Duration::from_secs(1),
            first_byte: Duration::from_secs(1),
            idle: Duration::from_millis(25),
        })
        .unwrap()
        .with_endpoint_url(&endpoint)
        .unwrap();
    let credentials = StaticCredentials::from_json(
        br#"{"access_key_id":"AKIAEXAMPLEVALUE","secret_access_key":"secret-secret-secret"}"#,
    )
    .unwrap();
    let connector = Connector::new(config, Credentials::Static(credentials)).await;
    let ProviderOutput::Events(mut events) = connector.execute(streaming_request()).await.unwrap()
    else {
        panic!("expected event stream");
    };
    assert!(events.next().await.unwrap().is_ok());
    assert!(events.next().await.unwrap().is_ok());
    let error = events.next().await.unwrap().unwrap_err();
    assert_eq!(error.class, AttemptFailureClass::Timeout);
    assert!(error.response_committed);
    drop(events);
    server.await.unwrap();
}

#[tokio::test]
async fn malformed_event_stream_sequences_fail_closed_after_commit() {
    let cases = [
        (
            "role",
            vec![event_frame("messageStart", r#"{"role":"user"}"#)],
        ),
        (
            "content before message",
            vec![event_frame(
                "contentBlockDelta",
                r#"{"delta":{"text":"early"},"contentBlockIndex":0}"#,
            )],
        ),
        (
            "missing stop",
            vec![event_frame("messageStart", r#"{"role":"assistant"}"#)],
        ),
        (
            "duplicate start",
            vec![
                event_frame("messageStart", r#"{"role":"assistant"}"#),
                event_frame("messageStart", r#"{"role":"assistant"}"#),
            ],
        ),
        (
            "tool delta before start",
            vec![
                event_frame("messageStart", r#"{"role":"assistant"}"#),
                event_frame(
                    "contentBlockDelta",
                    r#"{"delta":{"toolUse":{"input":"{}"}},"contentBlockIndex":0}"#,
                ),
            ],
        ),
        (
            "negative index",
            vec![
                event_frame("messageStart", r#"{"role":"assistant"}"#),
                event_frame("contentBlockStop", r#"{"contentBlockIndex":-1}"#),
            ],
        ),
    ];
    for (name, frames) in cases {
        let (endpoint, server) = serve_once(
            frames.into_iter().flatten().collect(),
            "application/vnd.amazon.eventstream",
        )
        .await;
        let connector = mock_connector(&endpoint).await;
        let ProviderOutput::Events(events) = connector.execute(streaming_request()).await.unwrap()
        else {
            panic!("expected event stream");
        };
        let items = events.collect::<Vec<_>>().await;
        let error = items
            .into_iter()
            .find_map(Result::err)
            .unwrap_or_else(|| panic!("{name}"));
        assert_eq!(error.phase, TransportPhase::Body, "{name}");
        assert_eq!(error.class, AttemptFailureClass::Protocol, "{name}");
        assert!(error.response_committed, "{name}");
        server.await.unwrap();
    }
}

#[tokio::test]
#[ignore = "requires OLP_BEDROCK_LIVE_REGION and an AWS default credential chain"]
async fn live_provider_discovers_models_with_default_chain() {
    let region = std::env::var("OLP_BEDROCK_LIVE_REGION")
        .expect("set OLP_BEDROCK_LIVE_REGION for the ignored live test");
    let connector = Connector::new(
        ConnectorConfig::new(region).unwrap(),
        Credentials::DefaultChain,
    )
    .await;
    assert!(!connector.discover_models().await.unwrap().is_empty());
}

#[tokio::test]
#[ignore = "requires OLP_BEDROCK_LIVE_REGION, OLP_BEDROCK_LIVE_MODEL, and AWS credentials"]
async fn live_provider_runs_converse_with_default_chain() {
    let region = std::env::var("OLP_BEDROCK_LIVE_REGION")
        .expect("set OLP_BEDROCK_LIVE_REGION for the ignored live test");
    let model = std::env::var("OLP_BEDROCK_LIVE_MODEL")
        .expect("set OLP_BEDROCK_LIVE_MODEL for the ignored live test");
    let connector = Connector::new(
        ConnectorConfig::new(region).unwrap(),
        Credentials::DefaultChain,
    )
    .await;
    let mut request = provider_request();
    request.attempt.upstream_model = model;
    let ProviderOutput::Events(events) = connector.execute(request).await.unwrap() else {
        panic!("expected generation events");
    };
    let events = events.collect::<Vec<_>>().await;
    assert!(events.iter().all(Result::is_ok));
}

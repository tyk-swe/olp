use std::{collections::BTreeMap, time::Duration};

use crate::openai::ConnectorTimeouts;
use futures::StreamExt;
use olp_domain::{
    AttemptPlan, ContentPart, DurationMs, EmbeddingInput, EmbeddingsRequest, GenerationParameters,
    GenerationRequest, Message, MessageRole, Operation, ProviderId, ProviderKind, ProviderOutput,
    ProviderRequest, RequestId, RequestMetadata, RouteId, RouteSlug, RuntimeGenerationId,
    SourceExtensions, Surface, TargetId, TransportMode,
};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
    sync::oneshot,
};

use super::*;

async fn spawn_server(response: Vec<u8>) -> (String, oneshot::Receiver<Vec<u8>>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (sender, receiver) = oneshot::channel();
    tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let request = read_request(&mut socket).await;
        let _ = sender.send(request);
        let _ = socket.write_all(&response).await;
        let _ = socket.flush().await;
    });
    (format!("http://{address}"), receiver)
}

async fn spawn_response_sequence(
    responses: Vec<Vec<u8>>,
) -> (String, oneshot::Receiver<Vec<Vec<u8>>>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (sender, receiver) = oneshot::channel();
    tokio::spawn(async move {
        let mut requests = Vec::with_capacity(responses.len());
        for response in responses {
            let (mut socket, _) = listener.accept().await.unwrap();
            requests.push(read_request(&mut socket).await);
            let _ = socket.write_all(&response).await;
            let _ = socket.flush().await;
        }
        let _ = sender.send(requests);
    });
    (format!("http://{address}"), receiver)
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
            && let Some(end) = request.windows(4).position(|part| part == b"\r\n\r\n")
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

fn response(content_type: &str, body: &str) -> Vec<u8> {
    format!(
        "HTTP/1.1 200 OK\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    )
    .into_bytes()
}

fn error_response(status: &str) -> Vec<u8> {
    let body = r#"{"error":{"message":"deployment or API version rejected"}}"#;
    format!(
        "HTTP/1.1 {status}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    )
    .into_bytes()
}

fn envelope(operation: Operation, mode: TransportMode) -> ProviderRequest {
    ProviderRequest {
        metadata: RequestMetadata {
            request_id: RequestId::new(),
            operation: operation.kind(),
            surface: Surface::OpenAi,
            mode,
        },
        attempt: AttemptPlan {
            generation_id: RuntimeGenerationId::new(),
            route_id: RouteId::new(),
            target_id: TargetId::new(),
            provider_id: ProviderId::new(),
            provider_kind: ProviderKind::AzureOpenAi,
            upstream_model: "gpt-4o".to_owned(),
            timeout: DurationMs::new(2_000),
            priority: 0,
        },
        operation,
        media: None,
    }
}

fn connector(origin: &str) -> AzureOpenAiConnector {
    AzureOpenAiConnector::new(
        ConnectorConfig::for_local_test(
            origin,
            "team-chat",
            "2024-10-21",
            ConnectorTimeouts {
                connect: Duration::from_secs(1),
                first_byte: Duration::from_secs(1),
                idle: Duration::from_secs(1),
            },
        ),
        AzureOpenAiApiKey::new("azure-secret").unwrap(),
    )
}

#[tokio::test]
async fn streams_chat_on_deployment_path_with_api_key_and_version() {
    let body = "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n";
    let (origin, captured) = spawn_server(response("text/event-stream", body)).await;
    let operation = Operation::Generation(GenerationRequest {
        route: RouteSlug::parse("chat").unwrap(),
        messages: vec![Message {
            role: MessageRole::User,
            content: vec![ContentPart::Text {
                text: "hello".to_owned(),
            }],
            name: None,
            tool_call_id: None,
            tool_calls: Vec::new(),
        }],
        parameters: GenerationParameters {
            stream: true,
            ..GenerationParameters::default()
        },
        tools: Vec::new(),
        tool_choice: None,
        response_format: None,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    });
    let ProviderOutput::Events(mut events) = connector(&origin)
        .execute(envelope(operation, TransportMode::Streaming))
        .await
        .unwrap()
    else {
        panic!("expected streaming events")
    };
    while let Some(event) = events.next().await {
        event.unwrap();
    }
    let request = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(request.starts_with(
        "POST /openai/deployments/team-chat/chat/completions?api-version=2024-10-21 HTTP/1.1"
    ));
    assert!(request.contains("api-key: azure-secret\r\n"));
    assert!(!request.to_ascii_lowercase().contains("authorization:"));
}

#[tokio::test]
async fn returns_typed_embedding_result_on_azure_path() {
    let body = r#"{"object":"list","data":[{"object":"embedding","embedding":[0.25,-0.5],"index":0}],"model":"embedding-model","usage":{"prompt_tokens":2,"total_tokens":2}}"#;
    let (origin, captured) = spawn_server(response("application/json", body)).await;
    let operation = Operation::Embeddings(EmbeddingsRequest {
        route: RouteSlug::parse("embedding").unwrap(),
        input: vec![EmbeddingInput::Text("hello".to_owned())],
        dimensions: None,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    });
    let ProviderOutput::Result(result) = connector(&origin)
        .execute(envelope(operation, TransportMode::Unary))
        .await
        .unwrap()
    else {
        panic!("expected typed result")
    };
    assert!(matches!(
        *result,
        olp_domain::CanonicalResult::Embeddings(_)
    ));
    let request = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(request.starts_with(
        "POST /openai/deployments/team-chat/embeddings?api-version=2024-10-21 HTTP/1.1"
    ));
}

#[tokio::test]
async fn probes_exact_deployment_path_version_and_auth() {
    let body = r#"{"id":"chatcmpl-probe","object":"chat.completion","created":1,"model":"ignored","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}"#;
    let (origin, captured) = spawn_server(response("application/json", body)).await;
    let models = connector(&origin).discover_models().await.unwrap();
    assert_eq!(models[0].id, "team-chat");
    let request = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(request.starts_with(
        "POST /openai/deployments/team-chat/chat/completions?api-version=2024-10-21 HTTP/1.1"
    ));
    assert!(request.contains("api-key: azure-secret\r\n"));
    assert!(!request.to_ascii_lowercase().contains("authorization:"));
    assert!(request.contains("\"max_completion_tokens\":1"));
}

#[tokio::test]
async fn embedding_only_deployment_passes_the_bounded_fallback_probe() {
    let embeddings = r#"{"object":"list","data":[{"object":"embedding","embedding":[0.25],"index":0}],"model":"embedding-model","usage":{"prompt_tokens":2,"total_tokens":2}}"#;
    let (origin, captured) = spawn_response_sequence(vec![
        error_response("404 Not Found"),
        response("application/json", embeddings),
    ])
    .await;
    let models = connector(&origin).discover_models().await.unwrap();
    assert_eq!(models[0].id, "team-chat");
    let requests = captured.await.unwrap();
    let chat = String::from_utf8(requests[0].clone()).unwrap();
    let embeddings = String::from_utf8(requests[1].clone()).unwrap();
    assert!(chat.starts_with(
        "POST /openai/deployments/team-chat/chat/completions?api-version=2024-10-21 HTTP/1.1"
    ));
    assert!(embeddings.starts_with(
        "POST /openai/deployments/team-chat/embeddings?api-version=2024-10-21 HTTP/1.1"
    ));
    assert!(requests.iter().all(|request| {
        request
            .windows(21)
            .any(|part| part == b"api-key: azure-secret")
    }));
}

#[tokio::test]
async fn rejected_deployment_or_api_version_never_becomes_probe_evidence() {
    let (origin, captured) = spawn_response_sequence(vec![
        error_response("404 Not Found"),
        error_response("400 Bad Request"),
    ])
    .await;
    let error = connector(&origin).discover_models().await.unwrap_err();
    assert!(matches!(
        error.class,
        AttemptFailureClass::UpstreamClient | AttemptFailureClass::Protocol
    ));
    let requests = captured.await.unwrap();
    assert_eq!(requests.len(), 2);
    assert!(requests.iter().all(|request| {
        String::from_utf8_lossy(request).contains("/openai/deployments/team-chat/")
            && String::from_utf8_lossy(request).contains("api-version=2024-10-21")
    }));
}

#[tokio::test]
async fn cross_origin_chat_tuple_is_certified_without_claiming_responses() {
    let body = r#"{"id":"chatcmpl-probe","object":"chat.completion","created":1,"model":"ignored","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}"#;
    let (origin, captured) = spawn_server(response("application/json", body)).await;
    connector(&origin)
        .certify_deployment_capability(
            "team-chat",
            CompatibleCapability {
                operation: OperationKind::Generation,
                surface: Surface::Anthropic,
                mode: TransportMode::Unary,
            },
        )
        .await
        .unwrap();
    let request = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(request.contains("/chat/completions?api-version=2024-10-21"));
    assert!(!request.contains("/responses?"));

    let error = connector("http://127.0.0.1:1")
        .certify_deployment_capability(
            "team-chat",
            CompatibleCapability {
                operation: OperationKind::ImageGeneration,
                surface: Surface::OpenAi,
                mode: TransportMode::Streaming,
            },
        )
        .await
        .unwrap_err();
    assert_eq!(error, CompatibleCapabilityCertificationError::Unsupported);
}

#[test]
fn rejects_unsafe_configuration_and_redacts_key() {
    assert!(
        ConnectorConfig::new("http://resource.openai.azure.com", "chat", "2024-10-21").is_err()
    );
    assert!(
        ConnectorConfig::new(
            "https://resource.openai.azure.com/path",
            "chat",
            "2024-10-21"
        )
        .is_err()
    );
    assert!(
        ConnectorConfig::new("https://resource.openai.azure.com", "../chat", "2024-10-21").is_err()
    );
    assert!(ConnectorConfig::new("https://resource.openai.azure.com", "chat", "latest").is_err());
    let key = AzureOpenAiApiKey::new("do-not-print").unwrap();
    assert!(!format!("{key:?}").contains("do-not-print"));
}

#[tokio::test]
#[ignore = "requires OLP_AZURE_OPENAI_LIVE_ENDPOINT, deployment, version, and key"]
async fn live_provider_azure_chat_smoke() {
    let endpoint = std::env::var("OLP_AZURE_OPENAI_LIVE_ENDPOINT").unwrap();
    let deployment = std::env::var("OLP_AZURE_OPENAI_LIVE_DEPLOYMENT").unwrap();
    let api_version = std::env::var("OLP_AZURE_OPENAI_LIVE_API_VERSION").unwrap();
    let key = std::env::var("OLP_AZURE_OPENAI_LIVE_API_KEY").unwrap();
    let connector = AzureOpenAiConnector::new(
        ConnectorConfig::new(&endpoint, deployment, api_version).unwrap(),
        AzureOpenAiApiKey::new(key).unwrap(),
    );
    let operation = Operation::Generation(GenerationRequest {
        route: RouteSlug::parse("chat").unwrap(),
        messages: vec![Message {
            role: MessageRole::User,
            content: vec![ContentPart::Text {
                text: "Reply with OK.".to_owned(),
            }],
            name: None,
            tool_call_id: None,
            tool_calls: Vec::new(),
        }],
        parameters: GenerationParameters::default(),
        tools: Vec::new(),
        tool_choice: None,
        response_format: None,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    });
    let ProviderOutput::Events(mut events) = connector
        .execute(envelope(operation, TransportMode::Unary))
        .await
        .unwrap()
    else {
        panic!("expected generation events")
    };
    assert!(events.next().await.is_some());
}

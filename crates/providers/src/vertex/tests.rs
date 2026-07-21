use std::{collections::BTreeMap, sync::Arc};

use crate::gemini::{BearerTokenError, BearerTokenProvider, ConnectorTimeouts, SecretBearerToken};
use futures::StreamExt;
use olp_domain::{
    AttemptPlan, ContentPart, DurationMs, GenerationParameters, GenerationRequest, Message,
    MessageRole, Operation, OperationKind, ProviderId, ProviderKind, ProviderOutput, RequestId,
    RequestMetadata, RouteId, RouteSlug, RuntimeGenerationId, SourceExtensions, Surface, TargetId,
    TransportMode,
};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
    sync::oneshot,
};

use super::*;

#[derive(Debug)]
struct StaticToken;

impl BearerTokenProvider for StaticToken {
    fn token<'a>(
        &'a self,
    ) -> olp_domain::BoxFuture<'a, Result<SecretBearerToken, BearerTokenError>> {
        Box::pin(async { SecretBearerToken::new("vertex-access-token") })
    }
}

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

fn http_response(content_type: &str, body: &str) -> Vec<u8> {
    format!(
        "HTTP/1.1 200 OK\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    )
    .into_bytes()
}

fn streaming_request() -> ProviderRequest {
    ProviderRequest {
        metadata: RequestMetadata {
            request_id: RequestId::new(),
            operation: OperationKind::Generation,
            surface: Surface::Gemini,
            mode: TransportMode::Streaming,
        },
        attempt: AttemptPlan {
            generation_id: RuntimeGenerationId::new(),
            route_id: RouteId::new(),
            target_id: TargetId::new(),
            provider_id: ProviderId::new(),
            provider_kind: ProviderKind::VertexAi,
            upstream_model: "gemini-2.5-flash".to_owned(),
            timeout: DurationMs::new(2_000),
            priority: 0,
        },
        operation: Operation::Generation(GenerationRequest {
            route: RouteSlug::parse("default").unwrap(),
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
            extensions: SourceExtensions::new(Surface::Gemini, BTreeMap::new()),
        }),
        media: None,
    }
}

#[tokio::test]
async fn streams_from_regional_publisher_path_with_oauth() {
    let event = "data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hello\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n";
    let (origin, captured) = spawn_server(http_response("text/event-stream", event)).await;
    let base =
        format!("{origin}/v1/projects/test-project/locations/us-central1/publishers/google/");
    let config = ConnectorConfig::for_local_test(
        "test-project",
        "us-central1",
        "gemini-2.5-flash",
        &base,
        ConnectorTimeouts::default(),
    );
    let connector = VertexConnector::with_token_provider(config, Arc::new(StaticToken));
    let ProviderOutput::Events(mut events) = connector.execute(streaming_request()).await.unwrap()
    else {
        panic!("generation must stream events")
    };
    let mut count = 0;
    while let Some(event) = events.next().await {
        event.unwrap();
        count += 1;
    }
    assert!(count >= 2);
    let request = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(request.starts_with("POST /v1/projects/test-project/locations/us-central1/publishers/google/models/gemini-2.5-flash:streamGenerateContent?alt=sse HTTP/1.1"));
    assert!(request.contains("authorization: Bearer vertex-access-token\r\n"));
    assert!(!request.to_ascii_lowercase().contains("proxy-authorization"));
}

#[tokio::test]
async fn service_account_uses_hardened_token_exchange_and_cached_token() {
    let token_body = r#"{"access_token":"oauth-secret","token_type":"Bearer","expires_in":3600}"#;
    let (token_origin, token_request) =
        spawn_server(http_response("application/json", token_body)).await;
    let count_body = r#"{"totalTokens":1}"#;
    let (provider_origin, provider_request) =
        spawn_server(http_response("application/json", count_body)).await;
    let credential = serde_json::json!({
        "type": "service_account",
        "project_id": "test-project",
        "private_key_id": "test-key",
        "private_key": include_str!("testdata/private_key.pem"),
        "client_email": "test@test-project.iam.gserviceaccount.com",
        "token_uri": format!("{token_origin}/token")
    });
    let provider =
        oauth::ServiceAccountTokenProvider::from_json_for_test(&credential.to_string()).unwrap();
    let base = format!(
        "{provider_origin}/v1/projects/test-project/locations/us-central1/publishers/google/"
    );
    let config = ConnectorConfig::for_local_test(
        "test-project",
        "us-central1",
        "gemini-2.5-flash",
        &base,
        ConnectorTimeouts::default(),
    );
    let connector = VertexConnector::with_token_provider(config, Arc::new(provider));
    assert_eq!(connector.discover_models().await.unwrap().len(), 1);

    let token_request = String::from_utf8(token_request.await.unwrap()).unwrap();
    assert!(token_request.starts_with("POST /token HTTP/1.1"));
    assert!(
        token_request.contains("grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Ajwt-bearer")
    );
    assert!(token_request.contains("assertion="));
    assert!(!token_request.contains("BEGIN+PRIVATE+KEY"));
    let provider_request = String::from_utf8(provider_request.await.unwrap()).unwrap();
    assert!(provider_request.contains("authorization: Bearer oauth-secret\r\n"));
    assert!(provider_request.contains("models/gemini-2.5-flash:countTokens"));
}

#[test]
fn validates_cloud_context_and_redacts_credentials() {
    assert!(ConnectorConfig::new("../project", "us-central1", "model").is_err());
    assert!(ConnectorConfig::new("project", "us/central1", "model").is_err());
    assert!(ConnectorConfig::new("project", "us-central1", "../model").is_err());
    assert!(matches!(
        VertexConnector::with_service_account_json(
            ConnectorConfig::new("project", "us-central1", "model").unwrap(),
            r#"{"type":"service_account"}"#,
        ),
        Err(ConnectorBuildError::ServiceAccount(_))
    ));
    let token = SecretBearerToken::new("do-not-print").unwrap();
    assert!(!format!("{token:?}").contains("do-not-print"));
}

#[tokio::test]
#[ignore = "requires OLP_VERTEX_LIVE_PROJECT, OLP_VERTEX_LIVE_LOCATION, OLP_VERTEX_LIVE_MODEL and ADC"]
async fn live_vertex_adc_smoke() {
    let project = std::env::var("OLP_VERTEX_LIVE_PROJECT").unwrap();
    let location = std::env::var("OLP_VERTEX_LIVE_LOCATION").unwrap();
    let model = std::env::var("OLP_VERTEX_LIVE_MODEL").unwrap();
    let connector = VertexConnector::with_application_default(
        ConnectorConfig::new(project, location, model).unwrap(),
    )
    .unwrap();
    assert_eq!(connector.discover_models().await.unwrap().len(), 1);
}

#[tokio::test]
#[ignore = "requires OLP_VERTEX_LIVE_CREDENTIALS, OLP_VERTEX_LIVE_LOCATION, and OLP_VERTEX_LIVE_MODEL"]
async fn live_provider_vertex_service_account_smoke() {
    let credential = std::env::var("OLP_VERTEX_LIVE_CREDENTIALS")
        .expect("set OLP_VERTEX_LIVE_CREDENTIALS for the ignored live test");
    let project = serde_json::from_str::<serde_json::Value>(&credential)
        .ok()
        .and_then(|value| value["project_id"].as_str().map(str::to_owned))
        .expect("Vertex service-account JSON must contain project_id");
    let location = std::env::var("OLP_VERTEX_LIVE_LOCATION")
        .expect("set OLP_VERTEX_LIVE_LOCATION for the ignored live test");
    let model = std::env::var("OLP_VERTEX_LIVE_MODEL")
        .expect("set OLP_VERTEX_LIVE_MODEL for the ignored live test");
    let connector = VertexConnector::with_service_account_json(
        ConnectorConfig::new(project, location, model).unwrap(),
        &credential,
    )
    .unwrap();
    assert_eq!(connector.discover_models().await.unwrap().len(), 1);
}

use std::collections::BTreeMap;
use std::sync::Arc;

use crate::ids::DurationMs;
use crate::ids::ProviderId;
use crate::ids::RequestId;
use crate::ids::RouteId;
use crate::ids::RouteSlug;
use crate::ids::RuntimeGenerationId;
use crate::ids::TargetId;
use crate::inference::transport::ProviderOutput;
use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::RequestMetadata;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::identity::TransportMode;
use crate::protocols::canonical::requests::ContentPart;
use crate::protocols::canonical::requests::GenerationParameters;
use crate::protocols::canonical::requests::GenerationRequest;
use crate::protocols::canonical::requests::Message;
use crate::protocols::canonical::requests::MessageRole;
use crate::protocols::canonical::requests::Operation;
use crate::protocols::canonical::requests::SourceExtensions;
use crate::providers::connector::Timeouts;
use crate::providers::gemini::BearerTokenError;
use crate::providers::gemini::BearerTokenProvider;
use crate::providers::gemini::SecretBearerToken;
use crate::providers::mock_server::MockResponse;
use crate::providers::mock_server::response as http_response;
use crate::providers::mock_server::spawn_mock;
use crate::providers::runtime_model::ProviderKind;
use crate::providers::vertex::*;
use crate::routes::selection::AttemptPlan;
use futures::StreamExt;

#[derive(Debug)]
struct StaticToken;

impl BearerTokenProvider for StaticToken {
    fn token<'a>(
        &'a self,
    ) -> crate::inference::transport::BoxFuture<'a, Result<SecretBearerToken, BearerTokenError>>
    {
        Box::pin(async { SecretBearerToken::new("vertex-access-token") })
    }
}

async fn spawn_server(response: Vec<u8>) -> (String, tokio::sync::oneshot::Receiver<Vec<u8>>) {
    spawn_mock("", MockResponse::immediate(response)).await
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
            connection_limits: None,
            credential_limits: None,
            attempt_limit: None,
            routing_policy: None,
            credential_slot_id: None,
            credential_version_id: None,
            pricing_revision_id: None,
            generation_id: RuntimeGenerationId::new(),
            route_id: RouteId::new(),
            target_id: TargetId::new(),
            routing_id: TargetId::new(),
            provider_id: ProviderId::new(),
            provider_revision_id: uuid::Uuid::now_v7(),
            provider_kind: ProviderKind::VertexAi,
            upstream_model: "gemini-2.5-flash".to_owned(),
            timeout: DurationMs::new(2_000),
            priority: 0,
        },
        operation: Arc::new(Operation::Generation(GenerationRequest {
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
        })),
        media: None,
        max_inline_media_bytes: 1024 * 1024,
        propagate_trace_context: false,
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
        Timeouts::default(),
    );
    let connector = Connector::with_token_provider(config, Arc::new(StaticToken));
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
#[ignore = "requires PostgreSQL via make integration"]
async fn default_slot_probe_uses_the_requested_model_instead_of_the_original_seed() {
    let db = crate::test_support::TestDb::create_migrated("vertex_probe_model").await;
    let pool = db.pool(2).await;
    let actor = uuid::Uuid::now_v7();
    let provider = uuid::Uuid::now_v7();
    let version = uuid::Uuid::now_v7();
    sqlx::query("INSERT INTO users(id,email,display_name,role) VALUES($1,'vertex-probe@test.example','Owner','owner')").bind(actor).execute(&pool).await.unwrap();
    sqlx::query("INSERT INTO providers(id,name,kind,auth_mode,cloud_project,cloud_region,etag,created_by) VALUES($1,'vertex','vertex_ai','service_account','test-project','us-central1',$2,$3)").bind(provider).bind(uuid::Uuid::now_v7()).bind(actor).execute(&pool).await.unwrap();
    sqlx::query("INSERT INTO provider_credential_slots(id,provider_id,name,is_default,allowed_models) VALUES($1,$1,'Default',true,ARRAY['model-b'])").bind(provider).execute(&pool).await.unwrap();
    for (id, model) in [
        (uuid::Uuid::from_u128(1), "model-a"),
        (uuid::Uuid::from_u128(2), "model-b"),
    ] {
        sqlx::query("INSERT INTO provider_models(id,provider_id,upstream_model,display_name,enabled) VALUES($1,$2,$3,$3,true)").bind(id).bind(provider).bind(model).execute(&pool).await.unwrap();
    }
    let credential = serde_json::json!({
        "type":"service_account", "project_id":"test-project", "private_key_id":"test-key",
        "private_key":include_str!("../../../tests/provider-data/vertex/test_only_private_key.pem"),
        "client_email":"test@test-project.iam.gserviceaccount.com", "token_uri":"https://oauth2.googleapis.com/token"
    });
    let master = Arc::new(crate::crypto::envelope::MasterKey::new(1, [64; 32]));
    let encrypted = master
        .seal(
            credential.to_string().as_bytes(),
            &crate::crypto::aad::credential(provider, version, 1),
        )
        .unwrap();
    sqlx::query("INSERT INTO provider_credential_versions(id,provider_id,slot_id,version,ciphertext,nonce,master_key_version,created_by) VALUES($1,$2,$2,1,$3,$4,1,$5)").bind(version).bind(provider).bind(encrypted.ciphertext).bind(encrypted.nonce.to_vec()).bind(actor).execute(&pool).await.unwrap();
    sqlx::query("UPDATE providers SET active_credential_version_id=$2 WHERE id=$1")
        .bind(provider)
        .bind(version)
        .execute(&pool)
        .await
        .unwrap();
    assert_eq!(
        crate::providers::repository::get_provider(&pool, provider)
            .await
            .unwrap()
            .configuration
            .probe_model
            .as_deref(),
        Some("model-a")
    );
    let mut state = crate::http::control::state::ManagementState::new(
        crate::process::mode::ApiMode::Control,
        Some(pool),
        Arc::new(crate::runtime::manager::Manager::empty()),
        "https://olp.test",
        "console",
    );
    state.master_key = Some(master);
    let built =
        crate::providers::connect::provider_connector_for_model(&state, provider, Some("model-b"))
            .await
            .unwrap();
    let crate::providers::connectors::ProviderConnector::Vertex(built) = built else {
        panic!("Expected Vertex connector")
    };
    assert_eq!(built.config.probe_model, "model-b");
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
        "private_key": include_str!("../../../tests/provider-data/vertex/test_only_private_key.pem"),
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
        Timeouts::default(),
    );
    let connector = Connector::with_token_provider(config, Arc::new(provider));
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
        Connector::with_service_account_json(
            ConnectorConfig::new("project", "us-central1", "model").unwrap(),
            r#"{"type":"service_account"}"#,
        ),
        Err(ConnectorBuildError::ServiceAccount(_))
    ));
    let token = SecretBearerToken::new("do-not-print").unwrap();
    assert!(!format!("{token:?}").contains("do-not-print"));
}

async fn rejected_service_account(status: &str, code: &str) -> Connector {
    let body = serde_json::json!({"error":code,"error_description":"private account detail"});
    let (origin, _) = spawn_server(crate::providers::mock_server::status_response(
        status,
        "application/json",
        body.to_string(),
    ))
    .await;
    let credential = serde_json::json!({
        "type":"service_account", "private_key_id":"test-key",
        "private_key":include_str!("../../../tests/provider-data/vertex/test_only_private_key.pem"),
        "client_email":"test@test-project.iam.gserviceaccount.com",
        "token_uri":format!("{origin}/token")
    });
    let token =
        oauth::ServiceAccountTokenProvider::from_json_for_test(&credential.to_string()).unwrap();
    Connector::with_token_provider(
        ConnectorConfig::for_local_test(
            "test-project",
            "us-central1",
            "gemini-2.5-flash",
            &format!("{origin}/v1/projects/test-project/locations/us-central1/publishers/google/"),
            Timeouts::default(),
        ),
        Arc::new(token),
    )
}

#[tokio::test]
async fn oauth_credential_rejections_allow_slot_failover_without_opening_the_endpoint() {
    use crate::inference::{circuit::Breaker, transport::AttemptFailureClass};
    for (status, code, rejected) in [
        ("400 Bad Request", "invalid_grant", true),
        ("400 Bad Request", "disabled_client", true),
        ("401 Unauthorized", "invalid_client", true),
        ("403 Forbidden", "access_denied", true),
        ("400 Bad Request", "invalid_request", false),
        ("503 Service Unavailable", "temporarily_unavailable", false),
    ] {
        let request = streaming_request();
        let target = request.attempt.routing_id;
        let error = rejected_service_account(status, code)
            .await
            .execute(request)
            .await
            .unwrap_err();
        assert!(!error.message.contains("private account detail"));
        if rejected {
            assert_eq!(error.class, AttemptFailureClass::UpstreamClient);
            assert_eq!(error.upstream.status, Some(401));
            assert!(error.allows_failover());
            let breaker = Breaker::default();
            for _ in 0..5 {
                breaker.record_failure(target, error.class);
            }
            assert!(breaker.is_selectable(target));
        } else {
            assert_eq!(error.class, AttemptFailureClass::Connect);
            assert_eq!(error.upstream.status, None);
        }
    }
}

#[tokio::test]
#[ignore = "requires Valkey via make integration"]
async fn rejected_oauth_credential_cools_only_its_pinned_slot() {
    use crate::limits::{admission::ReloadableLimiter, distributed::DistributedLimiter};
    use crate::providers::pool_transport::{PoolTransport, clear_cooldowns, is_cooling};
    let limiter = ReloadableLimiter::default();
    limiter.install(
        DistributedLimiter::connect(
            &std::env::var("OLP_VALKEY_URL").unwrap(),
            format!("olp_test_oauth_{}", uuid::Uuid::now_v7().simple()),
        )
        .await
        .unwrap(),
    );
    let mut request = streaming_request();
    let provider_id = request.attempt.provider_id.as_uuid();
    let slot = uuid::Uuid::now_v7();
    let version = uuid::Uuid::now_v7();
    request.attempt.credential_slot_id = Some(slot);
    request.attempt.credential_version_id = Some(version);
    let transport = PoolTransport {
        transports: BTreeMap::from([(
            Some(version),
            Arc::new(rejected_service_account("400 Bad Request", "invalid_grant").await)
                as Arc<dyn ProviderTransport>,
        )]),
        default: None,
        slots: Vec::new(),
        options: Default::default(),
        limiter: limiter.clone(),
    };
    assert_eq!(
        transport
            .execute(request.clone())
            .await
            .unwrap_err()
            .upstream
            .status,
        Some(401)
    );
    let backend = limiter.current().unwrap();
    assert_eq!(
        is_cooling(backend.as_ref(), provider_id, slot, Some(version)).await,
        Some(true)
    );
    assert_eq!(
        is_cooling(
            backend.as_ref(),
            provider_id,
            uuid::Uuid::now_v7(),
            Some(uuid::Uuid::now_v7())
        )
        .await,
        Some(false)
    );
    assert_eq!(
        transport.execute(request).await.unwrap_err().message,
        "provider_credential_cooling_down"
    );
    clear_cooldowns(backend.as_ref(), provider_id, slot, Some(version)).await;
}

#[test]
fn builds_regional_and_multi_region_endpoints() {
    for (location, host) in [
        ("us-central1", "us-central1-aiplatform.googleapis.com"),
        ("us", "aiplatform.us.rep.googleapis.com"),
        ("eu", "aiplatform.eu.rep.googleapis.com"),
        ("global", "aiplatform.googleapis.com"),
    ] {
        let url = regional_base_url("test-project", location).unwrap();
        assert_eq!(url.host_str(), Some(host));
        assert_eq!(
            url.path(),
            format!("/v1/projects/test-project/locations/{location}/publishers/google/")
        );
    }
}

#[tokio::test]
#[ignore = "requires OLP_VERTEX_LIVE_PROJECT, OLP_VERTEX_LIVE_LOCATION, OLP_VERTEX_LIVE_MODEL and ADC"]
async fn live_provider_vertex_adc_smoke() {
    let project = std::env::var("OLP_VERTEX_LIVE_PROJECT").unwrap();
    let location = std::env::var("OLP_VERTEX_LIVE_LOCATION").unwrap();
    let model = std::env::var("OLP_VERTEX_LIVE_MODEL").unwrap();
    let connector = Connector::with_application_default(
        ConnectorConfig::new(project, location, model).unwrap(),
    )
    .unwrap();
    assert_eq!(connector.discover_models().await.unwrap().len(), 1);
}

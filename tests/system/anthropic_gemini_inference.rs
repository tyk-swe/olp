use std::collections::BTreeMap;
use std::collections::BTreeSet;
use std::num::NonZeroU16;
use std::num::NonZeroU32;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::atomic::AtomicBool;
use std::sync::atomic::AtomicUsize;
use std::sync::atomic::Ordering;
use std::time::Duration;

use axum::body::Body;
use axum::http::Request;
use axum::http::StatusCode;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use chrono::Utc;
use futures::Stream;
use futures::stream;
use http_body_util::BodyExt;
use olp::access::policy::ApiKey;
use olp::access::policy::ApiKeyDigest;
use olp::access::policy::ApiKeyLimits;
use olp::access::policy::ApiKeyScope;
use olp::access::policy::ApiKeyStatus;
use olp::crypto::key_material::AuthHmacKey;
use olp::http::router::gateway_router_for_test;
use olp::ids::ApiKeyId;
use olp::ids::ApiKeyLookupId;
use olp::ids::DurationMs;
use olp::ids::ProviderId;
use olp::ids::RouteId;
use olp::ids::RouteSlug;
use olp::ids::RuntimeGenerationId;
use olp::ids::TargetId;
use olp::inference::http::state::GatewayState;
use olp::inference::transport::AttemptFailureClass;
use olp::inference::transport::BoxFuture;
use olp::inference::transport::ProviderEventStream;
use olp::inference::transport::ProviderOutput;
use olp::inference::transport::ProviderRequest;
use olp::inference::transport::ProviderTransport;
use olp::inference::transport::TransportError;
use olp::inference::transport::TransportPhase;
use olp::process::mode::ApiMode;
use olp::process::state::ProcessComposition;
use olp::protocols::canonical::events::Event;
use olp::protocols::canonical::events::FinishReason;
use olp::protocols::canonical::events::Kind;
use olp::protocols::canonical::events::Usage;
use olp::protocols::canonical::identity::OperationKind;
use olp::protocols::canonical::identity::Surface;
use olp::protocols::canonical::identity::TransportMode;
use olp::protocols::canonical::requests::MessageRole;
use olp::protocols::canonical::requests::SourceExtensions;
use olp::protocols::canonical::results::CanonicalResult;
use olp::protocols::canonical::results::TokenCountResult;
use olp::providers::runtime_model::Capability;
use olp::providers::runtime_model::Provider;
use olp::providers::runtime_model::ProviderKind;
use olp::routes::model::Route;
use olp::routes::model::Target;
use olp::runtime::manager::Manager;
use olp::runtime::snapshot::RuntimeGeneration;
use olp::runtime::snapshot::Snapshot;
use serde_json::Value;
use serde_json::json;
use tower::ServiceExt;

#[derive(Clone, Debug)]
struct RecordedCall {
    provider_id: ProviderId,
    surface: Surface,
    operation: OperationKind,
    mode: TransportMode,
    route: String,
}

struct MockTransport {
    provider_id: ProviderId,
    native_surface: Surface,
    text: &'static str,
    calls: Arc<Mutex<Vec<RecordedCall>>>,
}

impl ProviderTransport for MockTransport {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        let call = RecordedCall {
            provider_id: self.provider_id,
            surface: request.metadata.surface,
            operation: request.metadata.operation,
            mode: request.metadata.mode,
            route: request
                .operation
                .route()
                .map(ToString::to_string)
                .unwrap_or_default(),
        };
        self.calls.lock().unwrap().push(call);
        let surface = self.native_surface;
        let text = self.text;
        let upstream_model = request.attempt.upstream_model.clone();
        Box::pin(async move {
            if request.metadata.operation == OperationKind::TokenCount {
                return Ok(ProviderOutput::Result(Box::new(
                    CanonicalResult::TokenCount(TokenCountResult {
                        input_tokens: 13,
                        extensions: SourceExtensions::new(surface, BTreeMap::new()),
                    }),
                )));
            }
            let events = generation_events(text, &upstream_model);
            Ok(ProviderOutput::Events(Box::pin(stream::iter(
                events.into_iter().map(Ok),
            ))))
        })
    }
}

fn generation_events(text: &str, upstream_model: &str) -> Vec<Event> {
    vec![
        Event::new(
            0,
            Kind::ResponseStart {
                response_id: Some("provider-response".into()),
                provider_model: Some(upstream_model.into()),
            },
        ),
        Event::new(
            1,
            Kind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            },
        ),
        Event::new(
            2,
            Kind::TextDelta {
                output_index: 0,
                text: text.into(),
            },
        ),
        Event::new(
            3,
            Kind::Usage {
                usage: Usage {
                    input_tokens: 3,
                    output_tokens: 2,
                    total_tokens: 5,
                    cached_input_tokens: None,
                    reasoning_tokens: None,
                },
            },
        ),
        Event::new(
            4,
            Kind::Finish {
                output_index: 0,
                reason: FinishReason::Stop,
            },
        ),
        Event::new(5, Kind::Done),
    ]
}

struct TestGateway {
    state: GatewayState,
    key: String,
    calls: Arc<Mutex<Vec<RecordedCall>>>,
    anthropic_provider: ProviderId,
    gemini_provider: ProviderId,
}

fn test_gateway() -> TestGateway {
    let auth_hmac_key = Arc::new(AuthHmacKey::new([41; 32]));
    let material = auth_hmac_key.generate_api_key();
    let key = material.expose_once().to_owned();
    let lookup = ApiKeyLookupId::parse(material.lookup_id.clone()).unwrap();
    let anthropic_provider = ProviderId::new();
    let gemini_provider = ProviderId::new();
    let anthropic_model = "claude-private";
    let gemini_model = "gemini-private";
    let operations = BTreeSet::from([OperationKind::Generation, OperationKind::TokenCount]);
    let capabilities = |model: &str, surface: Surface| {
        BTreeSet::from([
            Capability::new(
                model,
                OperationKind::Generation,
                surface,
                TransportMode::Unary,
            ),
            Capability::new(
                model,
                OperationKind::Generation,
                surface,
                TransportMode::Streaming,
            ),
            Capability::new(
                model,
                OperationKind::TokenCount,
                surface,
                TransportMode::Unary,
            ),
        ])
    };
    let cross_slug = RouteSlug::parse("team-default").unwrap();
    let cross_route = Route {
        id: RouteId::new(),
        routing_id: RouteId::new(),
        slug: cross_slug.clone(),
        operations: operations.clone(),
        overall_timeout: DurationMs::new(5_000),
        max_attempts: NonZeroU16::new(2).unwrap(),
        targets: vec![
            Target {
                id: TargetId::new(),
                routing_id: TargetId::new(),
                provider_id: anthropic_provider,
                upstream_model: anthropic_model.into(),
                priority: 0,
                weight: NonZeroU32::new(1).unwrap(),
                timeout: DurationMs::new(4_000),
            },
            Target {
                id: TargetId::new(),
                routing_id: TargetId::new(),
                provider_id: gemini_provider,
                upstream_model: gemini_model.into(),
                priority: 0,
                weight: NonZeroU32::new(1).unwrap(),
                timeout: DurationMs::new(4_000),
            },
        ],
    };
    let snapshot = Snapshot {
        routing: Default::default(),
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::new(),
            ordinal: 9,
            activated_at: Utc::now(),
        },
        providers: BTreeMap::from([
            (
                anthropic_provider,
                Provider {
                    id: anthropic_provider,
                    revision_id: uuid::Uuid::now_v7(),
                    name: "anthropic".into(),
                    kind: ProviderKind::Anthropic,
                    enabled: true,
                    active_credential: None,
                    capabilities: capabilities(anthropic_model, Surface::Anthropic),
                },
            ),
            (
                gemini_provider,
                Provider {
                    id: gemini_provider,
                    revision_id: uuid::Uuid::now_v7(),
                    name: "gemini".into(),
                    kind: ProviderKind::Gemini,
                    enabled: true,
                    active_credential: None,
                    capabilities: capabilities(gemini_model, Surface::Gemini),
                },
            ),
        ]),
        routes: BTreeMap::from([(cross_slug, cross_route)]),
        api_keys: BTreeMap::from([(
            lookup.clone(),
            ApiKey {
                routing_policy: Default::default(),
                id: ApiKeyId::new(),
                lookup_id: lookup,
                digest: ApiKeyDigest::new(material.digest),
                status: ApiKeyStatus::Active,
                expires_at: None,
                scopes: BTreeSet::from([ApiKeyScope::Inference, ApiKeyScope::ModelsRead]),
                allowed_routes: BTreeSet::new(),
                limits: ApiKeyLimits::default(),
            },
        )]),
    };
    let calls = Arc::new(Mutex::new(Vec::new()));
    let transports: BTreeMap<ProviderId, Arc<dyn ProviderTransport>> = BTreeMap::from([
        (
            anthropic_provider,
            Arc::new(MockTransport {
                provider_id: anthropic_provider,
                native_surface: Surface::Anthropic,
                text: "anthropic answer",
                calls: calls.clone(),
            }) as Arc<dyn ProviderTransport>,
        ),
        (
            gemini_provider,
            Arc::new(MockTransport {
                provider_id: gemini_provider,
                native_surface: Surface::Gemini,
                text: "gemini answer",
                calls: calls.clone(),
            }) as Arc<dyn ProviderTransport>,
        ),
    ]);
    let runtime = Arc::new(Manager::empty());
    runtime.install(snapshot, transports).unwrap();
    let mut state = ProcessComposition::new(
        ApiMode::Gateway,
        sqlx::postgres::PgPoolOptions::new()
            .max_connections(1)
            .acquire_timeout(Duration::from_millis(10))
            .connect_lazy("postgres://olp:olp@127.0.0.1/olp")
            .unwrap(),
        runtime,
        "https://olp.test",
        "console",
    );
    state.auth_hmac_key = auth_hmac_key;
    let state = state.mode_dependencies().gateway().unwrap();
    TestGateway {
        state,
        key,
        calls,
        anthropic_provider,
        gemini_provider,
    }
}

async fn body_json(response: axum::response::Response) -> Value {
    let bytes = response.into_body().collect().await.unwrap().to_bytes();
    serde_json::from_slice(&bytes).unwrap()
}

fn post_json(path: &str, header: (&str, &str), body: Value) -> Request<Body> {
    Request::post(path)
        .header("content-type", "application/json")
        .header(header.0, header.1)
        .body(Body::from(body.to_string()))
        .unwrap()
}

mod native_surfaces;
mod semantics;
mod streaming;

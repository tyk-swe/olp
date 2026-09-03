use std::{
    collections::{BTreeMap, BTreeSet},
    num::{NonZeroU16, NonZeroU32},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    time::Duration,
};

use axum::{
    body::Body,
    http::{Request, StatusCode},
};
use base64::{Engine as _, engine::general_purpose::STANDARD};
use chrono::Utc;
use futures::{Stream, stream};
use http_body_util::BodyExt;
use olp::{
    bootstrap::{
        mode_dependencies::GatewayState,
        state::{ApiMode, ProcessComposition},
    },
    public_http::router::gateway_router_for_test,
};
use olp_db::{security::key_material::AuthHmacKey, store::Store};
use olp_engine::domain::{
    auth::{ApiKey, ApiKeyDigest, ApiKeyLimits, ApiKeyScope, ApiKeyStatus},
    canonical::{
        events::{Event, FinishReason, Kind, Usage},
        identity::{OperationKind, Surface, TransportMode},
        requests::{MessageRole, SourceExtensions},
        results::{CanonicalResult, TokenCountResult},
    },
    ids::{
        ApiKeyId, ApiKeyLookupId, DurationMs, ProviderId, RouteId, RouteSlug, RuntimeGenerationId,
        TargetId,
    },
    ports::{
        AttemptFailureClass, BoxFuture, ProviderEventStream, ProviderOutput, ProviderRequest,
        ProviderTransport, TransportError, TransportPhase,
    },
    routing::{
        provider::{Capability, Provider, ProviderKind},
        route::{Route, Target},
        snapshot::{RuntimeGeneration, Snapshot},
    },
};
use olp_engine::inference::runtime::Manager;
use serde_json::{Value, json};
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
        routing_id: None,
        slug: cross_slug.clone(),
        operations: operations.clone(),
        overall_timeout: DurationMs::new(5_000),
        max_attempts: NonZeroU16::new(2).unwrap(),
        targets: vec![
            Target {
                id: TargetId::new(),
                routing_id: None,
                provider_id: anthropic_provider,
                upstream_model: anthropic_model.into(),
                priority: 0,
                weight: NonZeroU32::new(1).unwrap(),
                timeout: DurationMs::new(4_000),
            },
            Target {
                id: TargetId::new(),
                routing_id: None,
                provider_id: gemini_provider,
                upstream_model: gemini_model.into(),
                priority: 0,
                weight: NonZeroU32::new(1).unwrap(),
                timeout: DurationMs::new(4_000),
            },
        ],
    };
    let snapshot = Snapshot {
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
                    revision_id: None,
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
                    revision_id: None,
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
        Store::from_pool(
            sqlx::postgres::PgPoolOptions::new()
                .max_connections(1)
                .acquire_timeout(Duration::from_millis(10))
                .connect_lazy("postgres://olp:olp@127.0.0.1/olp")
                .unwrap(),
        ),
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

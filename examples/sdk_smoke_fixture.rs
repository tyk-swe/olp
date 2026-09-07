//! Build tooling, not a usage example: an in-memory OpenLLMProxy fixture
//! server built by `tests/sdk-smoke/run.sh` (via `cargo build --example
//! sdk_smoke_fixture --features test-util`) so the official
//! OpenAI/Anthropic/Gemini SDKs can be
//! exercised without PostgreSQL, Valkey, or live providers.

use std::collections::BTreeMap;
use std::collections::BTreeSet;
use std::env;
use std::num::NonZeroU16;
use std::num::NonZeroU32;
use std::sync::Arc;

use chrono::Utc;
use futures::stream;
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
use olp::inference::transport::AttemptFailureClass;
use olp::inference::transport::BoxFuture;
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
use serde::Serialize;
use sqlx::postgres::PgPoolOptions;

const ROUTE_SLUG: &str = "sdk-smoke-route";
const UPSTREAM_MODEL: &str = "private-sdk-fixture-model";

struct StaticCanonicalTransport;

impl ProviderTransport for StaticCanonicalTransport {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        Box::pin(async move {
            if request.operation.route().map(RouteSlug::as_str) != Some(ROUTE_SLUG)
                || !matches!(
                    request.metadata.operation,
                    OperationKind::Generation | OperationKind::TokenCount
                )
            {
                return Err(TransportError {
                    upstream: Default::default(),
                    phase: TransportPhase::Body,
                    class: AttemptFailureClass::Protocol,
                    response_committed: false,
                    message: "SDK smoke fixture received an unexpected canonical operation"
                        .to_owned(),
                });
            }

            if request.metadata.operation == OperationKind::TokenCount {
                return Ok(ProviderOutput::Result(Box::new(
                    CanonicalResult::TokenCount(TokenCountResult {
                        input_tokens: 13,
                        extensions: Default::default(),
                    }),
                )));
            }

            let surface = match request.metadata.surface {
                Surface::OpenAi => "openai",
                Surface::Anthropic => "anthropic",
                Surface::Gemini => "gemini",
            };
            let text = format!("official {surface} sdk reached {ROUTE_SLUG}");
            let events = generation_events(&text, &request.attempt.upstream_model);
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
                response_id: Some("sdk-smoke-response".to_owned()),
                provider_model: Some(upstream_model.to_owned()),
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
                text: text.to_owned(),
            },
        ),
        Event::new(
            3,
            Kind::Usage {
                usage: Usage {
                    input_tokens: 4,
                    output_tokens: 6,
                    total_tokens: 10,
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

fn fixture_api_key(lookup_id: ApiKeyLookupId, digest: ApiKeyDigest) -> ApiKey {
    ApiKey {
        id: ApiKeyId::new(),
        lookup_id,
        digest,
        status: ApiKeyStatus::Active,
        expires_at: None,
        scopes: BTreeSet::from([ApiKeyScope::Inference, ApiKeyScope::ModelsRead]),
        allowed_routes: BTreeSet::new(),
        limits: ApiKeyLimits::default(),
    }
}

#[derive(Serialize)]
struct FixtureMetadata<'a> {
    origin: &'a str,
    api_key: &'a str,
    conflict_api_key: &'a str,
    route_slug: &'a str,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let metadata_path = env::var("OLP_SDK_SMOKE_METADATA")
        .map_err(|_| "OLP_SDK_SMOKE_METADATA must name a private output file")?;
    let address = env::var("OLP_SDK_SMOKE_ADDR").unwrap_or_else(|_| "127.0.0.1:0".to_owned());

    let auth_hmac_key = Arc::new(AuthHmacKey::new([73; 32]));
    let key_material = auth_hmac_key.generate_api_key();
    let plaintext_key = key_material.expose_once().to_owned();
    let lookup_id = ApiKeyLookupId::parse(key_material.lookup_id.clone())?;
    let conflict_key_material = auth_hmac_key.generate_api_key();
    let conflict_plaintext_key = conflict_key_material.expose_once().to_owned();
    let conflict_lookup_id = ApiKeyLookupId::parse(conflict_key_material.lookup_id.clone())?;
    let provider_id = ProviderId::new();
    let route_slug = RouteSlug::parse(ROUTE_SLUG)?;

    let snapshot = fixture_snapshot(
        provider_id,
        route_slug,
        BTreeMap::from([
            (
                lookup_id.clone(),
                fixture_api_key(lookup_id, ApiKeyDigest::new(key_material.digest)),
            ),
            (
                conflict_lookup_id.clone(),
                fixture_api_key(
                    conflict_lookup_id,
                    ApiKeyDigest::new(conflict_key_material.digest),
                ),
            ),
        ]),
    );
    let runtime = Arc::new(Manager::empty());
    runtime.install(
        snapshot,
        BTreeMap::from([(
            provider_id,
            Arc::new(StaticCanonicalTransport) as Arc<dyn ProviderTransport>,
        )]),
    )?;

    let listener = tokio::net::TcpListener::bind(&address).await?;
    let local_address = listener.local_addr()?;
    let origin = format!("http://{local_address}");
    // The SDK fixture exercises no persistence path, but the production
    // gateway surface still has a mandatory storage capability. A lazy pool
    // supplies that typed capability without adding a database service to this
    // protocol-only fixture.
    let pool = PgPoolOptions::new().connect_lazy("postgres://olp:olp@127.0.0.1/olp-sdk-smoke")?;
    let mut state = ProcessComposition::new(ApiMode::Gateway, pool, runtime, &origin, "console");
    state.auth_hmac_key = auth_hmac_key;
    let gateway_state = state.mode_dependencies().gateway().ok_or_else(|| {
        std::io::Error::other("gateway mode did not produce gateway dependencies")
    })?;

    tokio::fs::write(
        &metadata_path,
        serde_json::to_vec(&FixtureMetadata {
            origin: &origin,
            api_key: &plaintext_key,
            conflict_api_key: &conflict_plaintext_key,
            route_slug: ROUTE_SLUG,
        })?,
    )
    .await?;
    eprintln!("SDK smoke fixture listening on {origin}");

    axum::serve(listener, gateway_router_for_test(gateway_state)).await?;
    Ok(())
}

fn fixture_snapshot(
    provider_id: ProviderId,
    route_slug: RouteSlug,
    api_keys: BTreeMap<ApiKeyLookupId, ApiKey>,
) -> Snapshot {
    let mut capabilities = BTreeSet::new();
    for surface in [Surface::OpenAi, Surface::Anthropic, Surface::Gemini] {
        for mode in [TransportMode::Unary, TransportMode::Streaming] {
            capabilities.insert(Capability::new(
                UPSTREAM_MODEL,
                OperationKind::Generation,
                surface,
                mode,
            ));
        }
    }
    capabilities.insert(Capability::new(
        UPSTREAM_MODEL,
        OperationKind::TokenCount,
        Surface::Anthropic,
        TransportMode::Unary,
    ));

    let route = Route {
        id: RouteId::new(),
        routing_id: RouteId::new(),
        slug: route_slug.clone(),
        operations: BTreeSet::from([OperationKind::Generation, OperationKind::TokenCount]),
        overall_timeout: DurationMs::new(5_000),
        max_attempts: NonZeroU16::new(1).expect("one is nonzero"),
        targets: vec![Target {
            id: TargetId::new(),
            routing_id: TargetId::new(),
            provider_id,
            upstream_model: UPSTREAM_MODEL.to_owned(),
            priority: 0,
            weight: NonZeroU32::new(1).expect("one is nonzero"),
            timeout: DurationMs::new(4_000),
        }],
    };
    Snapshot {
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::new(),
            ordinal: 1,
            activated_at: Utc::now(),
        },
        providers: BTreeMap::from([(
            provider_id,
            Provider {
                id: provider_id,
                revision_id: uuid::Uuid::now_v7(),
                name: "sdk-smoke-static-provider".to_owned(),
                kind: ProviderKind::OpenAi,
                enabled: true,
                active_credential: None,
                capabilities,
            },
        )]),
        routes: BTreeMap::from([(route_slug, route)]),
        api_keys,
    }
}

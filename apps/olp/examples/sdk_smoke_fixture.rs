//! Build tooling, not a usage example: an in-memory OpenLLMProxy fixture
//! server built by `tests/sdk-smoke/run.sh` (via `cargo build --example
//! sdk_smoke_fixture --features test-util`) so the official
//! OpenAI/Anthropic/Gemini SDKs can be
//! exercised without PostgreSQL, Valkey, or live providers.

use std::{collections::BTreeMap, env, sync::Arc};

use futures::stream;
use olp::{
    bootstrap::state::{ApiMode, ProcessComposition},
    public_http::router::gateway_router_for_test,
};
use olp_db::{security::key_material::AuthHmacKey, store::Store};
use olp_engine::domain::{
    auth::{ApiKeyDigest, ApiKeyScope},
    canonical::{
        events::{Event, FinishReason, Kind, Usage},
        identity::{OperationKind, Surface, TransportMode},
        requests::MessageRole,
    },
    ids::{ApiKeyLookupId, ProviderId, RouteSlug},
    ports::{
        AttemptFailureClass, BoxFuture, ProviderOutput, ProviderRequest, ProviderTransport,
        TransportError, TransportPhase,
    },
    routing::{
        fixtures,
        provider::{Capability, ProviderKind},
    },
};
use olp_engine::inference::runtime::Manager;
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
            if request.metadata.operation != OperationKind::Generation
                || request.operation.route().map(RouteSlug::as_str) != Some(ROUTE_SLUG)
            {
                return Err(TransportError {
                    phase: TransportPhase::Body,
                    class: AttemptFailureClass::Protocol,
                    response_committed: false,
                    message: "SDK smoke fixture received an unexpected canonical operation"
                        .to_owned(),
                });
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
    let capabilities = [Surface::OpenAi, Surface::Anthropic, Surface::Gemini]
        .into_iter()
        .flat_map(|surface| {
            [TransportMode::Unary, TransportMode::Streaming].map(|mode| {
                Capability::new(UPSTREAM_MODEL, OperationKind::Generation, surface, mode)
            })
        });
    let mut provider = fixtures::provider(provider_id, ProviderKind::OpenAi, capabilities);
    provider.active_credential = None;
    let snapshot = fixtures::snapshot(1)
        .with_provider(provider)
        .with_route(fixtures::route(
            ROUTE_SLUG,
            [OperationKind::Generation],
            vec![fixtures::target(provider_id, UPSTREAM_MODEL)],
        ))
        .with_api_key(fixtures::api_key(
            lookup_id,
            ApiKeyDigest::new(key_material.digest),
            [ApiKeyScope::Inference, ApiKeyScope::ModelsRead],
        ))
        .with_api_key(fixtures::api_key(
            conflict_lookup_id,
            ApiKeyDigest::new(conflict_key_material.digest),
            [ApiKeyScope::Inference, ApiKeyScope::ModelsRead],
        ));
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
    let store = Store::from_pool(
        PgPoolOptions::new().connect_lazy("postgres://olp:olp@127.0.0.1/olp-sdk-smoke")?,
    );
    let mut state =
        ProcessComposition::new(ApiMode::Gateway, Some(store), runtime, &origin, "console");
    state.auth_hmac_key = Some(auth_hmac_key);
    let gateway_state = state.mode_dependencies()?.gateway.ok_or_else(|| {
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

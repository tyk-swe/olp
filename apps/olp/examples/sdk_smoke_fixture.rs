use std::{
    collections::{BTreeMap, BTreeSet},
    env,
    num::{NonZeroU16, NonZeroU32},
    sync::Arc,
};

use chrono::Utc;
use futures::stream;
use olp::{ApiMode, ApiState, RuntimeManager, public_router};
use olp_domain::{
    ApiKey, ApiKeyDigest, ApiKeyId, ApiKeyLimits, ApiKeyLookupId, ApiKeyScope, ApiKeyStatus,
    AttemptFailureClass, BoxFuture, CanonicalEvent, CanonicalEventKind, Capability, DurationMs,
    FinishReason, MessageRole, OperationKind, Provider, ProviderId, ProviderKind, ProviderOutput,
    ProviderRequest, ProviderTransport, Route, RouteId, RouteSlug, RuntimeGeneration,
    RuntimeGenerationId, RuntimeSnapshot, Surface, Target, TargetId, TransportError, TransportMode,
    TransportPhase, Usage,
};
use olp_storage::{AuthHmacKey, PgStore};
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

fn generation_events(text: &str, upstream_model: &str) -> Vec<CanonicalEvent> {
    vec![
        CanonicalEvent::new(
            0,
            CanonicalEventKind::ResponseStart {
                response_id: Some("sdk-smoke-response".to_owned()),
                provider_model: Some(upstream_model.to_owned()),
            },
        ),
        CanonicalEvent::new(
            1,
            CanonicalEventKind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            },
        ),
        CanonicalEvent::new(
            2,
            CanonicalEventKind::TextDelta {
                output_index: 0,
                text: text.to_owned(),
            },
        ),
        CanonicalEvent::new(
            3,
            CanonicalEventKind::Usage {
                usage: Usage {
                    input_tokens: 4,
                    output_tokens: 6,
                    total_tokens: 10,
                    cached_input_tokens: None,
                    reasoning_tokens: None,
                },
            },
        ),
        CanonicalEvent::new(
            4,
            CanonicalEventKind::Finish {
                output_index: 0,
                reason: FinishReason::Stop,
            },
        ),
        CanonicalEvent::new(5, CanonicalEventKind::Done),
    ]
}

#[derive(Serialize)]
struct FixtureMetadata<'a> {
    origin: &'a str,
    api_key: &'a str,
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
    let provider_id = ProviderId::new();
    let route_slug = RouteSlug::parse(ROUTE_SLUG)?;

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

    let route = Route {
        id: RouteId::new(),
        routing_id: None,
        slug: route_slug.clone(),
        operations: BTreeSet::from([OperationKind::Generation]),
        overall_timeout: DurationMs::new(5_000),
        max_attempts: NonZeroU16::new(1).expect("one is nonzero"),
        targets: vec![Target {
            id: TargetId::new(),
            routing_id: None,
            provider_id,
            upstream_model: UPSTREAM_MODEL.to_owned(),
            priority: 0,
            weight: NonZeroU32::new(1).expect("one is nonzero"),
            timeout: DurationMs::new(4_000),
        }],
    };
    let snapshot = RuntimeSnapshot {
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::new(),
            ordinal: 1,
            activated_at: Utc::now(),
        },
        providers: BTreeMap::from([(
            provider_id,
            Provider {
                id: provider_id,
                name: "sdk-smoke-static-provider".to_owned(),
                kind: ProviderKind::OpenAi,
                enabled: true,
                active_credential: None,
                capabilities,
            },
        )]),
        routes: BTreeMap::from([(route_slug, route)]),
        api_keys: BTreeMap::from([(
            lookup_id.clone(),
            ApiKey {
                id: ApiKeyId::new(),
                lookup_id,
                digest: ApiKeyDigest::new(key_material.digest),
                status: ApiKeyStatus::Active,
                expires_at: None,
                scopes: BTreeSet::from([ApiKeyScope::Inference, ApiKeyScope::ModelsRead]),
                allowed_routes: BTreeSet::new(),
                limits: ApiKeyLimits::default(),
            },
        )]),
    };
    let runtime = Arc::new(RuntimeManager::empty());
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
    let store = PgStore::from_pool(
        PgPoolOptions::new().connect_lazy("postgres://olp:olp@127.0.0.1/olp-sdk-smoke")?,
    );
    let mut state = ApiState::new(ApiMode::Gateway, Some(store), runtime, &origin, "console");
    state.auth_hmac_key = Some(auth_hmac_key);
    let gateway_state = state.mode_dependencies()?.gateway().ok_or_else(|| {
        std::io::Error::other("gateway mode did not produce gateway dependencies")
    })?;

    tokio::fs::write(
        &metadata_path,
        serde_json::to_vec(&FixtureMetadata {
            origin: &origin,
            api_key: &plaintext_key,
            route_slug: ROUTE_SLUG,
        })?,
    )
    .await?;
    eprintln!("SDK smoke fixture listening on {origin}");

    axum::serve(listener, public_router(gateway_state)).await?;
    Ok(())
}

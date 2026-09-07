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

use crate::access::policy::ApiKey;
use crate::access::policy::ApiKeyDigest;
use crate::access::policy::ApiKeyLimits;
use crate::access::policy::ApiKeyScope;
use crate::access::policy::ApiKeyStatus;
use crate::crypto::key_material::AuthHmacKey;
use crate::ids::ApiKeyId;
use crate::ids::ApiKeyLookupId;
use crate::ids::CredentialVersionId;
use crate::ids::DurationMs;
use crate::ids::ProviderId;
use crate::ids::RouteId;
use crate::ids::RouteSlug;
use crate::ids::RuntimeGenerationId;
use crate::ids::TargetId;
use crate::inference::execution::RequiredTarget;
use crate::inference::transport::BoxFuture;
use crate::inference::transport::MediaSpool;
use crate::inference::transport::ProviderEventStream;
use crate::inference::transport::ProviderOutput;
use crate::inference::transport::ProviderRequest;
use crate::inference::transport::ProviderTransport;
use crate::inference::transport::TransportError;
use crate::limits::admission::reserve;
use crate::protocols::canonical::events::Error;
use crate::protocols::canonical::events::ErrorClass;
use crate::protocols::canonical::events::Event;
use crate::protocols::canonical::events::FinishReason;
use crate::protocols::canonical::events::Kind;
use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::identity::TransportMode;
use crate::protocols::canonical::requests::MediaHandle;
use crate::protocols::canonical::requests::MessageRole;
use crate::protocols::canonical::requests::Operation;
use crate::protocols::canonical::requests::SourceExtensions;
use crate::protocols::canonical::results::CanonicalResult;
use crate::protocols::openai::chat::CompletionRequest;
use crate::protocols::openai::chat::decode;
use crate::protocols::openai::responses::token_count::ResponseInputTokensRequest;
use crate::protocols::openai::responses::token_count::decode_response_input_tokens;
use crate::providers::runtime_model::Capability;
use crate::providers::runtime_model::Provider;
use crate::providers::runtime_model::ProviderKind;
use crate::routes::model::Route;
use crate::routes::model::Target;
use crate::runtime::manager::Manager;
use crate::runtime::snapshot::RuntimeGeneration;
use crate::runtime::snapshot::Snapshot;
use axum::body::Body;
use axum::body::Bytes;
use axum::http::Request;
use axum::http::StatusCode;
use axum::http::header;
use axum::response::Response;
use chrono::Utc;
use futures::StreamExt;
use futures::stream;
use http_body_util::BodyExt;
use serde_json::Value;
use serde_json::json;
use tower::ServiceExt;

use crate::http::request_admission::multipart::MultipartRequestAdmission;
use crate::inference::http::error::InferenceError;
use crate::inference::http::execution::execute_event_operation;
use crate::inference::http::execution::execute_routed_result;
use crate::inference::http::multipart::MultipartFormData;
use crate::inference::http::*;

#[derive(Clone)]
struct StaticTransport {
    events: Vec<Event>,
}

#[derive(Clone)]
struct FiniteStaticTransport {
    events: Vec<Event>,
}

#[derive(Clone)]
struct StaticResultTransport {
    result: CanonicalResult,
}

impl ProviderTransport for StaticResultTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        let result = self.result.clone();
        Box::pin(async move { Ok(ProviderOutput::Result(Box::new(result))) })
    }
}

impl ProviderTransport for StaticTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        let events = self.events.clone();
        Box::pin(async move {
            let events = stream::iter(events.into_iter().map(Ok)).chain(stream::pending());
            Ok(ProviderOutput::Events(
                Box::pin(events) as ProviderEventStream
            ))
        })
    }
}

impl ProviderTransport for FiniteStaticTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        let events = self.events.clone();
        Box::pin(async move {
            Ok(ProviderOutput::Events(Box::pin(stream::iter(
                events.into_iter().map(Ok),
            ))))
        })
    }
}

fn test_state(streaming: bool) -> (GatewayState, String) {
    let auth_hmac_key = Arc::new(AuthHmacKey::new([7; 32]));
    let material = auth_hmac_key.generate_api_key();
    let plaintext = material.expose_once().to_owned();
    let lookup = ApiKeyLookupId::parse(material.lookup_id.clone()).unwrap();
    let route_slug = RouteSlug::parse("default").unwrap();
    let provider_id = ProviderId::new();
    let mode = if streaming {
        TransportMode::Streaming
    } else {
        TransportMode::Unary
    };
    let provider = Provider {
        id: provider_id,
        revision_id: uuid::Uuid::now_v7(),
        name: "mock-openai".to_owned(),
        kind: ProviderKind::OpenAi,
        enabled: true,
        active_credential: Some(CredentialVersionId::new()),
        capabilities: BTreeSet::from([Capability::new(
            "upstream-model",
            OperationKind::Generation,
            Surface::OpenAi,
            mode,
        )]),
    };
    let route = Route {
        id: RouteId::new(),
        routing_id: RouteId::new(),
        slug: route_slug.clone(),
        operations: BTreeSet::from([OperationKind::Generation]),
        overall_timeout: DurationMs::new(5_000),
        max_attempts: NonZeroU16::new(1).unwrap(),
        targets: vec![Target {
            id: TargetId::new(),
            routing_id: TargetId::new(),
            provider_id,
            upstream_model: "upstream-model".to_owned(),
            priority: 0,
            weight: NonZeroU32::new(1).unwrap(),
            timeout: DurationMs::new(4_000),
        }],
    };
    let snapshot = Snapshot {
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::new(),
            ordinal: 1,
            activated_at: Utc::now(),
        },
        providers: BTreeMap::from([(provider_id, provider)]),
        routes: BTreeMap::from([(route_slug, route)]),
        api_keys: BTreeMap::from([(
            lookup.clone(),
            ApiKey {
                id: ApiKeyId::new(),
                lookup_id: lookup,
                digest: ApiKeyDigest::new(material.digest),
                status: ApiKeyStatus::Active,
                expires_at: None,
                scopes: BTreeSet::from([ApiKeyScope::Inference]),
                allowed_routes: BTreeSet::new(),
                limits: ApiKeyLimits::default(),
            },
        )]),
    };
    let runtime = Arc::new(Manager::empty());
    let transport: Arc<dyn ProviderTransport> = Arc::new(StaticTransport {
        events: vec![
            Event::new(
                0,
                Kind::ResponseStart {
                    response_id: Some("chatcmpl-upstream".to_owned()),
                    provider_model: Some("upstream-model".to_owned()),
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
                    text: "hello from OLP".to_owned(),
                },
            ),
            Event::new(
                3,
                Kind::Finish {
                    output_index: 0,
                    reason: FinishReason::Stop,
                },
            ),
            Event::new(4, Kind::Done),
        ],
    });
    runtime
        .install(snapshot, BTreeMap::from([(provider_id, transport)]))
        .unwrap();
    let mut state = GatewayState::new(
        crate::process::mode::ApiMode::Gateway,
        None,
        runtime,
        "https://olp.test",
        "console",
    );
    state.replace_auth_hmac_key_for_test(auth_hmac_key);
    (state, plaintext)
}

fn test_admission(
    state: &GatewayState,
    surface: Surface,
) -> crate::http::request_admission::HttpRequestAdmission {
    crate::http::request_admission::HttpRequestAdmission::for_test(
        test_principal(state, surface),
        None,
        None,
    )
}

fn test_principal(
    state: &GatewayState,
    surface: Surface,
) -> crate::inference::principal::Principal {
    let runtime = state.runtime().pin();
    let (lookup_id, _) = runtime.api_keys.iter().next().unwrap();
    crate::inference::principal::Principal::new(
        Arc::clone(&runtime),
        lookup_id.clone(),
        surface,
        Some(crate::access::policy::GatewayCapability::Inference),
    )
}

fn reinstall_api_keys(state: &GatewayState, api_keys: BTreeMap<ApiKeyLookupId, ApiKey>) {
    let pinned = state.runtime().pin();
    let snapshot = Snapshot {
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::new(),
            ordinal: pinned.generation.ordinal + 1,
            activated_at: Utc::now(),
        },
        providers: pinned.providers.clone(),
        routes: pinned.routes.clone(),
        api_keys,
    };
    let transports = pinned
        .providers
        .keys()
        .map(|provider_id| (*provider_id, pinned.transport(*provider_id).unwrap()))
        .collect();
    state.runtime().install(snapshot, transports).unwrap();
}

fn install_result(state: &GatewayState, operation: OperationKind, result: CanonicalResult) {
    let pinned = state.runtime().pin();
    let provider_id = *pinned.providers.keys().next().unwrap();
    let mut providers = pinned.providers.clone();
    providers.get_mut(&provider_id).unwrap().capabilities = BTreeSet::from([Capability::new(
        "upstream-model",
        operation,
        Surface::OpenAi,
        TransportMode::Unary,
    )]);
    let mut routes = pinned.routes.clone();
    let route = routes
        .get_mut(&RouteSlug::parse("default").unwrap())
        .unwrap();
    route.operations = BTreeSet::from([operation]);
    let snapshot = Snapshot {
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::new(),
            ordinal: pinned.generation.ordinal + 1,
            activated_at: Utc::now(),
        },
        providers,
        routes,
        api_keys: pinned.api_keys.clone(),
    };
    let transport: Arc<dyn ProviderTransport> = Arc::new(StaticResultTransport { result });
    state
        .runtime()
        .install(snapshot, BTreeMap::from([(provider_id, transport)]))
        .unwrap();
}

fn install_transport(state: &GatewayState, transport: Arc<dyn ProviderTransport>) {
    let pinned = state.runtime().pin();
    let provider_id = *pinned.providers.keys().next().unwrap();
    let snapshot = Snapshot {
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::new(),
            ordinal: pinned.generation.ordinal + 1,
            activated_at: Utc::now(),
        },
        providers: pinned.providers.clone(),
        routes: pinned.routes.clone(),
        api_keys: pinned.api_keys.clone(),
    };
    state
        .runtime()
        .install(snapshot, BTreeMap::from([(provider_id, transport)]))
        .unwrap();
}

fn generation_stream_events(text: &str) -> Vec<Event> {
    vec![
        Event::new(
            0,
            Kind::ResponseStart {
                response_id: Some("response-upstream".into()),
                provider_model: Some("upstream-model".into()),
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
            Kind::Finish {
                output_index: 0,
                reason: FinishReason::Stop,
            },
        ),
        Event::new(
            4,
            Kind::Usage {
                usage: crate::protocols::canonical::events::Usage {
                    input_tokens: 7,
                    output_tokens: 3,
                    total_tokens: 10,
                    cached_input_tokens: Some(2),
                    reasoning_tokens: Some(1),
                },
            },
        ),
        Event::new(5, Kind::Done),
    ]
}

async fn post_json(state: &GatewayState, key: &str, path: &str, body: &'static str) -> Response {
    crate::http::router::gateway_router_for_test(state.clone())
        .oneshot(
            Request::post(path)
                .header(header::AUTHORIZATION, format!("Bearer {key}"))
                .header(header::CONTENT_TYPE, "application/json")
                .body(Body::from(body))
                .unwrap(),
        )
        .await
        .unwrap()
}

async fn post_multipart(state: &GatewayState, key: &str, path: &str, body: String) -> Response {
    crate::http::router::gateway_router_for_test(state.clone())
        .oneshot(
            Request::post(path)
                .header(header::AUTHORIZATION, format!("Bearer {key}"))
                .header(
                    header::CONTENT_TYPE,
                    "multipart/form-data; boundary=olp-test-boundary",
                )
                .body(Body::from(body))
                .unwrap(),
        )
        .await
        .unwrap()
}

async fn response_text(response: Response) -> String {
    String::from_utf8(
        response
            .into_body()
            .collect()
            .await
            .unwrap()
            .to_bytes()
            .to_vec(),
    )
    .unwrap()
}

fn multipart(fields: &[(&str, &str)], file_name: &str, bytes: &str) -> String {
    let mut body = String::new();
    for (name, value) in fields {
        body.push_str(&format!(
            "--olp-test-boundary\r\nContent-Disposition: form-data; name=\"{name}\"\r\n\r\n{value}\r\n"
        ));
    }
    body.push_str(&format!(
        "--olp-test-boundary\r\nContent-Disposition: form-data; name=\"{file_name}\"; filename=\"fixture.bin\"\r\nContent-Type: application/octet-stream\r\n\r\n{bytes}\r\n--olp-test-boundary--\r\n"
    ));
    body
}

pub mod cancellation;

pub mod failover;

pub mod media;

pub mod streaming;

pub mod unary;

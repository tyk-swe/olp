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
    body::{Body, Bytes},
    http::{Request, StatusCode, header},
    response::Response,
};
use futures::{StreamExt, stream};
use http_body_util::BodyExt;
use olp_db::security::key_material::AuthHmacKey;
use olp_engine::domain::{
    auth::{ApiKey, ApiKeyDigest, ApiKeyScope},
    canonical::{
        events::{Error, ErrorClass, Event, EventSequenceValidator, FinishReason, Kind},
        identity::{OperationKind, RequestMetadata, Surface, TransportMode},
        requests::{
            ImageGenerationRequest, ImageOperation, MediaHandle, MessageRole, Operation,
            SourceExtensions,
        },
        results::CanonicalResult,
    },
    ids::{ApiKeyLookupId, DurationMs, ProviderId, RequestId, RouteSlug, TargetId},
    ports::{
        AttemptFailureClass, BoxFuture, MediaSpool, ProviderEventStream, ProviderOutput,
        ProviderRequest, ProviderTransport, TransportError,
    },
    routing::{fixtures, provider::Capability, provider::ProviderKind, selection::select_attempts},
};
use olp_engine::inference::{
    circuit::Breaker,
    execution::RequiredTarget,
    failover::{
        Context, EventStream, circuit_accounted_event_stream, execute,
        reclassify_ambiguous_transport_failure, validated_event_stream,
    },
    limits::reserve,
    runtime::{Bundle, Manager},
};
use olp_engine::protocols::openai::{
    chat::{CompletionRequest, decode},
    responses::token_count::{ResponseInputTokensRequest, decode_response_input_tokens},
};
use serde_json::{Value, json};
use tower::ServiceExt;

use super::*;
use super::{
    error::InferenceError,
    execution::{
        execute_event_operation, execute_event_operation_without_admission,
        execute_routed_result_without_admission,
    },
    multipart::MultipartFormData,
};
use crate::public_http::request_admission::multipart::MultipartRequestAdmission;

mod cancellation;
mod failover;
mod media;
mod streaming;
mod unary;

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
    let provider_id = ProviderId::new();
    let mode = if streaming {
        TransportMode::Streaming
    } else {
        TransportMode::Unary
    };
    let snapshot = fixtures::snapshot(1)
        .with_provider(fixtures::provider(
            provider_id,
            ProviderKind::OpenAi,
            fixtures::capabilities(
                "upstream-model",
                Surface::OpenAi,
                [(OperationKind::Generation, mode)],
            ),
        ))
        .with_route(fixtures::route(
            "default",
            [OperationKind::Generation],
            vec![fixtures::target(provider_id, "upstream-model")],
        ))
        .with_api_key(fixtures::api_key(
            lookup,
            ApiKeyDigest::new(material.digest),
            [ApiKeyScope::Inference],
        ));
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
        crate::bootstrap::state::ApiMode::Gateway,
        None,
        runtime,
        "https://olp.test",
        "console",
    );
    state.replace_auth_hmac_key_for_test(auth_hmac_key);
    (state, plaintext)
}

fn test_principal(
    state: &GatewayState,
    surface: Surface,
) -> olp_engine::inference::principal::Principal {
    let runtime = state.runtime().pin();
    let (lookup_id, _) = runtime.api_keys.iter().next().unwrap();
    olp_engine::inference::principal::Principal::new(
        Arc::clone(&runtime),
        lookup_id.clone(),
        surface,
        Some(olp_engine::domain::auth::GatewayCapability::Inference),
    )
}

fn reinstall_api_keys(state: &GatewayState, api_keys: BTreeMap<ApiKeyLookupId, ApiKey>) {
    let pinned = state.runtime().pin();
    let mut snapshot = fixtures::next_generation(&pinned);
    snapshot.api_keys = api_keys;
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
    let mut snapshot = fixtures::next_generation(&pinned);
    snapshot.providers = providers;
    snapshot.routes = routes;
    let transport: Arc<dyn ProviderTransport> = Arc::new(StaticResultTransport { result });
    state
        .runtime()
        .install(snapshot, BTreeMap::from([(provider_id, transport)]))
        .unwrap();
}

fn install_transport(state: &GatewayState, transport: Arc<dyn ProviderTransport>) {
    let pinned = state.runtime().pin();
    let provider_id = *pinned.providers.keys().next().unwrap();
    let snapshot = fixtures::next_generation(&pinned);
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
                usage: olp_engine::domain::canonical::events::Usage {
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
    crate::public_http::router::gateway_router_for_test(state.clone())
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
    crate::public_http::router::gateway_router_for_test(state.clone())
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

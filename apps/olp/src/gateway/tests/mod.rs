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
use chrono::Utc;
use futures::{StreamExt, stream};
use http_body_util::BodyExt;
use olp_domain::{
    ApiKey, ApiKeyDigest, ApiKeyId, ApiKeyLimits, ApiKeyLookupId, ApiKeyScope, ApiKeyStatus,
    AttemptFailureClass, BoxFuture, CanonicalError, CanonicalEvent, CanonicalEventKind,
    CanonicalResult, Capability, CredentialVersionId, DurationMs, ErrorClass,
    EventSequenceValidator, FinishReason, ImageGenerationRequest, ImageOperation, MediaHandle,
    MediaSpool, MessageRole, Operation, OperationKind, Provider, ProviderEventStream, ProviderId,
    ProviderKind, ProviderOutput, ProviderRequest, ProviderTransport, RequestId, RequestMetadata,
    Route, RouteId, RouteSlug, RuntimeGeneration, RuntimeGenerationId, RuntimeSnapshot,
    SourceExtensions, Surface, Target, TargetId, TransportError, TransportMode, select_attempts,
};
use olp_protocols::openai::{
    ChatCompletionRequest, ResponseInputTokensRequest, decode_chat_completion,
    decode_response_input_tokens,
};
use olp_storage::AuthHmacKey;
use serde_json::{Value, json};
use tower::ServiceExt;

use super::*;
use super::{
    execution::{
        RequiredTarget, execute_event_operation_for_surface_inner,
        execute_routed_result_for_surface_inner,
    },
    failover::{
        EventStream, FailoverContext, circuit_accounted_event_stream, execute_with_failover,
        validated_event_stream,
    },
    limits::reserve_limits,
    media_jobs::{media_job_state, valid_upstream_media_job_id},
    multipart::MultipartFormData,
};
use crate::MultipartRequestAdmission;

mod cancellation;
mod failover;
mod media;
mod streaming;
mod unary;

#[derive(Clone)]
struct StaticTransport {
    events: Vec<CanonicalEvent>,
}

#[derive(Clone)]
struct FiniteStaticTransport {
    events: Vec<CanonicalEvent>,
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
        routing_id: None,
        slug: route_slug.clone(),
        operations: BTreeSet::from([OperationKind::Generation]),
        overall_timeout: DurationMs::new(5_000),
        max_attempts: NonZeroU16::new(1).unwrap(),
        targets: vec![Target {
            id: TargetId::new(),
            routing_id: None,
            provider_id,
            upstream_model: "upstream-model".to_owned(),
            priority: 0,
            weight: NonZeroU32::new(1).unwrap(),
            timeout: DurationMs::new(4_000),
        }],
    };
    let snapshot = RuntimeSnapshot {
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
    let runtime = Arc::new(crate::RuntimeManager::empty());
    let transport: Arc<dyn ProviderTransport> = Arc::new(StaticTransport {
        events: vec![
            CanonicalEvent::new(
                0,
                CanonicalEventKind::ResponseStart {
                    response_id: Some("chatcmpl-upstream".to_owned()),
                    provider_model: Some("upstream-model".to_owned()),
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
                    text: "hello from OLP".to_owned(),
                },
            ),
            CanonicalEvent::new(
                3,
                CanonicalEventKind::Finish {
                    output_index: 0,
                    reason: FinishReason::Stop,
                },
            ),
            CanonicalEvent::new(4, CanonicalEventKind::Done),
        ],
    });
    runtime
        .install(snapshot, BTreeMap::from([(provider_id, transport)]))
        .unwrap();
    let mut state = GatewayState::new(
        crate::ApiMode::Gateway,
        None,
        runtime,
        "https://olp.test",
        "console",
    );
    state.auth_hmac_key = auth_hmac_key;
    (state, plaintext)
}

fn test_principal(state: &GatewayState, surface: Surface) -> crate::InferencePrincipal {
    let runtime = state.runtime.pin();
    let (lookup_id, _) = runtime.api_keys.iter().next().unwrap();
    crate::InferencePrincipal::for_test(Arc::clone(&runtime), lookup_id.clone(), surface)
}

fn reinstall_api_keys(state: &GatewayState, api_keys: BTreeMap<ApiKeyLookupId, ApiKey>) {
    let pinned = state.runtime.pin();
    let snapshot = RuntimeSnapshot {
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
    state.runtime.install(snapshot, transports).unwrap();
}

fn install_result(state: &GatewayState, operation: OperationKind, result: CanonicalResult) {
    let pinned = state.runtime.pin();
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
    let snapshot = RuntimeSnapshot {
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
        .runtime
        .install(snapshot, BTreeMap::from([(provider_id, transport)]))
        .unwrap();
}

fn install_transport(state: &GatewayState, transport: Arc<dyn ProviderTransport>) {
    let pinned = state.runtime.pin();
    let provider_id = *pinned.providers.keys().next().unwrap();
    let snapshot = RuntimeSnapshot {
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
        .runtime
        .install(snapshot, BTreeMap::from([(provider_id, transport)]))
        .unwrap();
}

fn generation_stream_events(text: &str) -> Vec<CanonicalEvent> {
    vec![
        CanonicalEvent::new(
            0,
            CanonicalEventKind::ResponseStart {
                response_id: Some("response-upstream".into()),
                provider_model: Some("upstream-model".into()),
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
            CanonicalEventKind::Finish {
                output_index: 0,
                reason: FinishReason::Stop,
            },
        ),
        CanonicalEvent::new(
            4,
            CanonicalEventKind::Usage {
                usage: olp_domain::Usage {
                    input_tokens: 7,
                    output_tokens: 3,
                    total_tokens: 10,
                    cached_input_tokens: Some(2),
                    reasoning_tokens: Some(1),
                },
            },
        ),
        CanonicalEvent::new(5, CanonicalEventKind::Done),
    ]
}

async fn post_json(state: &GatewayState, key: &str, path: &str, body: &'static str) -> Response {
    crate::router::gateway_router_for_test(state.clone())
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
    crate::router::gateway_router_for_test(state.clone())
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

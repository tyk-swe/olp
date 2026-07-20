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
use olp::{ApiMode, ApiState, RuntimeManager, public_router};
use olp_domain::{
    ApiKey, ApiKeyDigest, ApiKeyId, ApiKeyLimits, ApiKeyLookupId, ApiKeyScope, ApiKeyStatus,
    AttemptFailureClass, BoxFuture, CanonicalEvent, CanonicalEventKind, CanonicalResult,
    Capability, DurationMs, FinishReason, MessageRole, OperationKind, Provider,
    ProviderEventStream, ProviderId, ProviderKind, ProviderOutput, ProviderRequest,
    ProviderTransport, Route, RouteId, RouteSlug, RuntimeGeneration, RuntimeGenerationId,
    RuntimeSnapshot, SourceExtensions, Surface, Target, TargetId, TokenCountResult, TransportError,
    TransportMode, TransportPhase, Usage,
};
use olp_storage::AuthHmacKey;
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
        let provider_model = request.attempt.provider_model.clone();
        Box::pin(async move {
            if request.metadata.operation == OperationKind::TokenCount {
                return Ok(ProviderOutput::Result(Box::new(
                    CanonicalResult::TokenCount(TokenCountResult {
                        input_tokens: 13,
                        extensions: SourceExtensions::new(surface, BTreeMap::new()),
                    }),
                )));
            }
            let events = generation_events(text, &provider_model);
            Ok(ProviderOutput::Events(Box::pin(stream::iter(
                events.into_iter().map(Ok),
            ))))
        })
    }
}

fn generation_events(text: &str, provider_model: &str) -> Vec<CanonicalEvent> {
    vec![
        CanonicalEvent::new(
            0,
            CanonicalEventKind::ResponseStart {
                response_id: Some("provider-response".into()),
                provider_model: Some(provider_model.into()),
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
                text: text.into(),
            },
        ),
        CanonicalEvent::new(
            3,
            CanonicalEventKind::Usage {
                usage: Usage {
                    input_tokens: 3,
                    output_tokens: 2,
                    total_tokens: 5,
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

struct TestGateway {
    state: ApiState,
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
                provider_model: anthropic_model.into(),
                priority: 0,
                weight: NonZeroU32::new(1).unwrap(),
                timeout: DurationMs::new(4_000),
            },
            Target {
                id: TargetId::new(),
                routing_id: None,
                provider_id: gemini_provider,
                provider_model: gemini_model.into(),
                priority: 0,
                weight: NonZeroU32::new(1).unwrap(),
                timeout: DurationMs::new(4_000),
            },
        ],
    };
    let snapshot = RuntimeSnapshot {
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
    let runtime = Arc::new(RuntimeManager::empty());
    runtime.install(snapshot, transports).unwrap();
    let mut state = ApiState::new(
        ApiMode::Gateway,
        None,
        runtime,
        "https://olp.test",
        "console",
    );
    state.auth_hmac_key = Some(auth_hmac_key);
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

#[tokio::test]
async fn anthropic_unary_count_models_and_native_errors_use_the_shared_pipeline() {
    let fixture = test_gateway();
    let app = public_router(fixture.state.clone());
    let response = app
        .clone()
        .oneshot(post_json(
            "/anthropic/v1/messages",
            ("x-api-key", &fixture.key),
            json!({
                "model": "team-default",
                "max_tokens": 32,
                "messages": [{"role": "user", "content": "hello"}]
            }),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let body = body_json(response).await;
    assert_eq!(body["model"], "team-default");
    assert_eq!(body["content"][0]["text"], "anthropic answer");
    assert_eq!(body["type"], "message");

    let response = app
        .clone()
        .oneshot(post_json(
            "/anthropic/v1/messages",
            ("x-api-key", &fixture.key),
            json!({
                "model": "team-default",
                "max_tokens": 32,
                "stream": true,
                "messages": [{"role": "user", "content": "hello"}]
            }),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let wire = String::from_utf8(
        response
            .into_body()
            .collect()
            .await
            .unwrap()
            .to_bytes()
            .to_vec(),
    )
    .unwrap();
    assert!(wire.contains("event: message_start"));
    assert!(wire.contains("event: content_block_delta"));
    assert!(wire.contains("anthropic answer"));
    assert!(wire.contains("event: message_stop"));

    let response = app
        .clone()
        .oneshot(post_json(
            "/anthropic/v1/messages/count_tokens",
            ("x-api-key", &fixture.key),
            json!({
                "model": "team-default",
                "system": "count all semantics",
                "messages": [{"role": "user", "content": "hello"}],
                "tools": [{"name": "lookup", "input_schema": {"type": "object"}}]
            }),
        ))
        .await
        .unwrap();
    assert_eq!(body_json(response).await["input_tokens"], 13);

    let response = app
        .clone()
        .oneshot(
            Request::get("/anthropic/v1/models/team-default")
                .header("x-api-key", &fixture.key)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    let body = body_json(response).await;
    assert_eq!(body["id"], "team-default");
    assert_eq!(body["type"], "model");

    let response = app
        .clone()
        .oneshot(
            Request::get("/anthropic/v1/models?limit=1")
                .header("x-api-key", &fixture.key)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    let body = body_json(response).await;
    assert_eq!(body["data"][0]["id"], "team-default");
    assert_eq!(body["has_more"], false);

    let stale = app
        .clone()
        .oneshot(
            Request::get("/anthropic/v1/models?after_id=removed-route")
                .header("x-api-key", &fixture.key)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(stale.status(), StatusCode::BAD_REQUEST);

    let response = app
        .oneshot(post_json(
            "/anthropic/v1/messages",
            ("x-api-key", "bad-key"),
            json!({"model":"team-default","max_tokens":1,"messages":[{"role":"user","content":"x"}]}),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
    let body = body_json(response).await;
    assert_eq!(body["type"], "error");
    assert_eq!(body["error"]["type"], "authentication_error");

    let calls = fixture.calls.lock().unwrap();
    assert!(calls.iter().any(|call| {
        call.provider_id == fixture.anthropic_provider
            && call.surface == Surface::Anthropic
            && call.operation == OperationKind::Generation
            && call.route == "team-default"
    }));
    assert!(!calls.iter().any(|call| {
        call.provider_id == fixture.gemini_provider && call.surface == Surface::Anthropic
    }));
}

#[tokio::test]
async fn both_gemini_versions_support_unary_sdk_sse_count_and_models() {
    let fixture = test_gateway();
    let app = public_router(fixture.state.clone());
    for version in ["v1", "v1beta"] {
        let response = app
            .clone()
            .oneshot(post_json(
                &format!("/gemini/{version}/models/team-default:generateContent"),
                ("x-goog-api-key", &fixture.key),
                json!({"contents":[{"role":"user","parts":[{"text":"hello"}]}]}),
            ))
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::OK);
        let body = body_json(response).await;
        assert_eq!(body["modelVersion"], "team-default");
        assert_eq!(
            body["candidates"][0]["content"]["parts"][0]["text"],
            "gemini answer"
        );
    }

    let response = app
        .clone()
        .oneshot(post_json(
            "/gemini/v1beta/models/team-default:streamGenerateContent?alt=sse",
            ("x-goog-api-key", &fixture.key),
            json!({"contents":[{"role":"user","parts":[{"text":"hello"}]}]}),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    assert_eq!(
        response.headers()["content-type"],
        "text/event-stream; charset=utf-8"
    );
    let bytes = response.into_body().collect().await.unwrap().to_bytes();
    let wire = String::from_utf8(bytes.to_vec()).unwrap();
    assert!(wire.contains("data: "));
    assert!(wire.contains("gemini answer"));
    assert!(wire.contains("\"modelVersion\":\"team-default\""));

    let response = app
        .clone()
        .oneshot(post_json(
            "/gemini/v1/models/team-default:countTokens",
            ("x-goog-api-key", &fixture.key),
            json!({"contents":[{"role":"user","parts":[{"text":"hello"}]}]}),
        ))
        .await
        .unwrap();
    assert_eq!(body_json(response).await["totalTokens"], 13);

    let response = app
        .clone()
        .oneshot(
            Request::get("/gemini/v1/models?pageSize=1")
                .header("x-goog-api-key", &fixture.key)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    let body = body_json(response).await;
    assert_eq!(body["models"][0]["name"], "models/team-default");
    assert!(
        body["models"][0]["supportedGenerationMethods"]
            .as_array()
            .unwrap()
            .iter()
            .any(|method| method == "generateContent")
    );

    let stale = app
        .clone()
        .oneshot(
            Request::get("/gemini/v1/models?pageToken=b2xwLXYxOmdvbmU")
                .header("x-goog-api-key", &fixture.key)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(stale.status(), StatusCode::BAD_REQUEST);

    let response = app
        .clone()
        .oneshot(
            Request::get("/gemini/v1beta/models/team-default")
                .header("x-goog-api-key", &fixture.key)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    let body = body_json(response).await;
    assert_eq!(body["name"], "models/team-default");
    assert_eq!(body["baseModelId"], "team-default");

    let response = app
        .oneshot(post_json(
            "/gemini/v1/models/provider/model:generateContent",
            ("x-goog-api-key", &fixture.key),
            json!({"contents":[{"parts":[{"text":"x"}]}]}),
        ))
        .await
        .unwrap();
    assert!(matches!(
        response.status(),
        StatusCode::BAD_REQUEST | StatusCode::NOT_FOUND
    ));

    let calls = fixture.calls.lock().unwrap();
    assert!(calls.iter().any(|call| {
        call.provider_id == fixture.gemini_provider
            && call.surface == Surface::Gemini
            && call.mode == TransportMode::Streaming
    }));
    assert!(!calls.iter().any(|call| {
        call.provider_id == fixture.anthropic_provider && call.surface == Surface::Gemini
    }));
}

#[tokio::test]
async fn inline_media_is_admitted_for_same_protocol_and_rejected_when_malformed_or_oversized() {
    let fixture = test_gateway();
    let app = public_router(fixture.state.clone());
    let response = app
        .clone()
        .oneshot(post_json(
            "/anthropic/v1/messages",
            ("x-api-key", &fixture.key),
            json!({
                "model":"team-default","max_tokens":8,
                "messages":[{"role":"user","content":[{"type":"image","source":{
                    "type":"base64","media_type":"image/png","data":"aGk="
                }}]}]
            }),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);

    let response = app
        .clone()
        .oneshot(post_json(
            "/gemini/v1beta/models/team-default:generateContent",
            ("x-goog-api-key", &fixture.key),
            json!({"contents":[{"role":"user","parts":[{"inlineData":{
                "mimeType":"image/png","data":"aGk="
            }}]}]}),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let successful_calls = fixture.calls.lock().unwrap().len();

    for data in [
        "%%%".to_owned(),
        STANDARD.encode(vec![0_u8; 1024 * 1024 + 1]),
    ] {
        let response = app
            .clone()
            .oneshot(post_json(
                "/anthropic/v1/messages",
                ("x-api-key", &fixture.key),
                json!({
                    "model":"team-default","max_tokens":8,
                    "messages":[{"role":"user","content":[{"type":"image","source":{
                        "type":"base64","media_type":"image/png","data":data
                    }}]}]
                }),
            ))
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    }
    assert_eq!(fixture.calls.lock().unwrap().len(), successful_calls);
}

#[tokio::test]
async fn certified_cross_protocol_tuple_is_runtime_reachable_without_semantic_loss() {
    let mut fixture = test_gateway();
    let pinned = fixture.state.runtime.pin();
    let mut snapshot = RuntimeSnapshot {
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::new(),
            ordinal: pinned.generation.ordinal + 1,
            activated_at: Utc::now(),
        },
        providers: pinned.providers.clone(),
        routes: pinned.routes.clone(),
        api_keys: pinned.api_keys.clone(),
    };
    snapshot
        .providers
        .retain(|provider_id, _| *provider_id == fixture.anthropic_provider);
    let provider = snapshot
        .providers
        .get_mut(&fixture.anthropic_provider)
        .unwrap();
    // This is the exact cross-origin tuple admitted by native certification:
    // an OpenAI client surface translated to Anthropic generation transport.
    provider.capabilities = BTreeSet::from([Capability::new(
        "claude-private",
        OperationKind::Generation,
        Surface::OpenAi,
        TransportMode::Unary,
    )]);
    let route = snapshot.routes.values_mut().next().unwrap();
    route.operations = BTreeSet::from([OperationKind::Generation]);
    route.max_attempts = NonZeroU16::new(1).unwrap();
    route
        .targets
        .retain(|target| target.provider_id == fixture.anthropic_provider);
    let runtime = Arc::new(RuntimeManager::empty());
    runtime
        .install(
            snapshot,
            BTreeMap::from([(
                fixture.anthropic_provider,
                Arc::new(MockTransport {
                    provider_id: fixture.anthropic_provider,
                    native_surface: Surface::Anthropic,
                    text: "cross-protocol answer",
                    calls: fixture.calls.clone(),
                }) as Arc<dyn ProviderTransport>,
            )]),
        )
        .unwrap();
    fixture.state.runtime = runtime;
    let app = public_router(fixture.state.clone());

    let response = app
        .clone()
        .oneshot(post_json(
            "/openai/v1/chat/completions",
            ("authorization", &format!("Bearer {}", fixture.key)),
            json!({
                "model": "team-default",
                "max_tokens": 32,
                "messages": [{"role":"user","content":"hello"}],
                "tools": [{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]
            }),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    assert_eq!(
        body_json(response).await["choices"][0]["message"]["content"],
        "cross-protocol answer"
    );

    // The internal Responses endpoint hint is removed before the Anthropic
    // encoder; it is not treated as client semantics or forwarded upstream.
    let response = app
        .clone()
        .oneshot(post_json(
            "/openai/v1/responses",
            ("authorization", &format!("Bearer {}", fixture.key)),
            json!({"model":"team-default","input":"hello","max_output_tokens":32}),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    assert_eq!(body_json(response).await["object"], "response");
    let successful_calls = fixture.calls.lock().unwrap().len();

    for body in [
        json!({
            "model":"team-default",
            "max_tokens":32,
            "messages":[{"role":"user","content":"hello"}],
            "response_format":{"type":"json_object"}
        }),
        json!({
            "model":"team-default",
            "max_tokens":32,
            "messages":[{"role":"user","content":"hello"}],
            "reasoning":{"effort":"high"}
        }),
        json!({
            "model":"team-default",
            "max_tokens":32,
            "messages":[{"role":"user","content":"hello"}],
            "citations":[{"url":"https://example.test"}]
        }),
        json!({
            "model":"team-default",
            "max_tokens":32,
            "messages":[{"role":"user","content":"hello"}],
            "safety":{"threshold":"strict"}
        }),
        json!({
            "model":"team-default",
            "max_tokens":32,
            "messages":[{"role":"user","content":[{"type":"refusal","refusal":"source-only media result"}]}]
        }),
        json!({
            "model":"team-default",
            "max_tokens":32,
            "messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{
                "data":"aGk=","format":"wav"
            }}]}]
        }),
    ] {
        let response = app
            .clone()
            .oneshot(post_json(
                "/openai/v1/chat/completions",
                ("authorization", &format!("Bearer {}", fixture.key)),
                body,
            ))
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
        let body = body_json(response).await;
        assert_eq!(body["error"]["type"], "invalid_request_error");
    }
    assert_eq!(fixture.calls.lock().unwrap().len(), successful_calls);
}

struct PostCommitFailureTransport {
    calls: Arc<AtomicUsize>,
}

impl ProviderTransport for PostCommitFailureTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        Box::pin(async {
            let events = vec![
                Ok(CanonicalEvent::new(
                    0,
                    CanonicalEventKind::ResponseStart {
                        response_id: Some("committed".into()),
                        provider_model: Some("primary".into()),
                    },
                )),
                Err(TransportError {
                    phase: TransportPhase::Body,
                    class: AttemptFailureClass::UpstreamServer,
                    response_committed: true,
                    message: "failed after commit".into(),
                }),
            ];
            Ok(ProviderOutput::Events(Box::pin(stream::iter(events))))
        })
    }
}

struct NeverCalledTransport(Arc<AtomicUsize>);

impl ProviderTransport for NeverCalledTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        self.0.fetch_add(1, Ordering::SeqCst);
        Box::pin(async {
            Err(TransportError {
                phase: TransportPhase::Connect,
                class: AttemptFailureClass::Connect,
                response_committed: false,
                message: "secondary invoked".into(),
            })
        })
    }
}

#[tokio::test]
async fn streaming_never_fails_over_after_the_first_canonical_event() {
    let mut fixture = test_gateway();
    let runtime = Arc::new(RuntimeManager::empty());
    let snapshot = fixture.state.runtime.pin();
    let mut snapshot = RuntimeSnapshot {
        generation: snapshot.generation.clone(),
        providers: snapshot.providers.clone(),
        routes: snapshot.routes.clone(),
        api_keys: snapshot.api_keys.clone(),
    };
    let provider_ids = snapshot.providers.keys().copied().collect::<Vec<_>>();
    for provider in snapshot.providers.values_mut() {
        provider.kind = ProviderKind::Anthropic;
        provider.capabilities = BTreeSet::from([Capability::new(
            if provider.id == provider_ids[0] {
                "claude-private"
            } else {
                "gemini-private"
            },
            OperationKind::Generation,
            Surface::Anthropic,
            TransportMode::Streaming,
        )]);
    }
    let route = snapshot.routes.values_mut().next().unwrap();
    route.operations = BTreeSet::from([OperationKind::Generation]);
    route.targets[0].priority = 0;
    route.targets[1].priority = 1;
    let primary_calls = Arc::new(AtomicUsize::new(0));
    let secondary_calls = Arc::new(AtomicUsize::new(0));
    let transports: BTreeMap<ProviderId, Arc<dyn ProviderTransport>> = BTreeMap::from([
        (
            route.targets[0].provider_id,
            Arc::new(PostCommitFailureTransport {
                calls: primary_calls.clone(),
            }) as Arc<dyn ProviderTransport>,
        ),
        (
            route.targets[1].provider_id,
            Arc::new(NeverCalledTransport(secondary_calls.clone())) as Arc<dyn ProviderTransport>,
        ),
    ]);
    runtime.install(snapshot, transports).unwrap();
    fixture.state.runtime = runtime;
    let response = public_router(fixture.state)
        .oneshot(post_json(
            "/anthropic/v1/messages",
            ("x-api-key", &fixture.key),
            json!({"model":"team-default","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hello"}]}),
        ))
        .await
        .unwrap();
    let wire = String::from_utf8(
        response
            .into_body()
            .collect()
            .await
            .unwrap()
            .to_bytes()
            .to_vec(),
    )
    .unwrap();
    assert!(wire.contains("event: error"));
    assert_eq!(primary_calls.load(Ordering::SeqCst), 1);
    assert_eq!(secondary_calls.load(Ordering::SeqCst), 0);
}

struct DropAwareStream {
    first: Option<CanonicalEvent>,
    dropped: Arc<AtomicBool>,
}

impl Stream for DropAwareStream {
    type Item = Result<CanonicalEvent, TransportError>;

    fn poll_next(
        mut self: std::pin::Pin<&mut Self>,
        _context: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        if let Some(first) = self.first.take() {
            std::task::Poll::Ready(Some(Ok(first)))
        } else {
            std::task::Poll::Pending
        }
    }
}

impl Drop for DropAwareStream {
    fn drop(&mut self) {
        self.dropped.store(true, Ordering::SeqCst);
    }
}

struct DropAwareTransport(Arc<AtomicBool>);

impl ProviderTransport for DropAwareTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        let dropped = self.0.clone();
        Box::pin(async move {
            Ok(ProviderOutput::Events(Box::pin(DropAwareStream {
                first: Some(CanonicalEvent::new(
                    0,
                    CanonicalEventKind::ResponseStart {
                        response_id: Some("cancel".into()),
                        provider_model: Some("private".into()),
                    },
                )),
                dropped,
            })
                as ProviderEventStream))
        })
    }
}

#[tokio::test]
async fn client_disconnect_drops_the_upstream_stream() {
    let mut fixture = test_gateway();
    let runtime = Arc::new(RuntimeManager::empty());
    let snapshot = fixture.state.runtime.pin();
    let mut snapshot = RuntimeSnapshot {
        generation: snapshot.generation.clone(),
        providers: snapshot.providers.clone(),
        routes: snapshot.routes.clone(),
        api_keys: snapshot.api_keys.clone(),
    };
    let route = snapshot.routes.values_mut().next().unwrap();
    route.operations = BTreeSet::from([OperationKind::Generation]);
    route.max_attempts = NonZeroU16::new(1).unwrap();
    route.targets.truncate(1);
    let provider_id = route.targets[0].provider_id;
    snapshot.providers.retain(|id, _| *id == provider_id);
    snapshot
        .providers
        .get_mut(&provider_id)
        .unwrap()
        .capabilities = BTreeSet::from([Capability::new(
        route.targets[0].provider_model.clone(),
        OperationKind::Generation,
        Surface::Anthropic,
        TransportMode::Streaming,
    )]);
    let dropped = Arc::new(AtomicBool::new(false));
    runtime
        .install(
            snapshot,
            BTreeMap::from([(
                provider_id,
                Arc::new(DropAwareTransport(dropped.clone())) as Arc<dyn ProviderTransport>,
            )]),
        )
        .unwrap();
    fixture.state.runtime = runtime;
    let response = public_router(fixture.state)
        .oneshot(post_json(
            "/anthropic/v1/messages",
            ("x-api-key", &fixture.key),
            json!({"model":"team-default","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hello"}]}),
        ))
        .await
        .unwrap();
    drop(response);
    tokio::time::timeout(Duration::from_secs(1), async {
        while !dropped.load(Ordering::SeqCst) {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
}

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
    EventSequenceValidator, FinishReason, MediaHandle, MediaSpool, MessageRole, Operation,
    OperationKind, Provider, ProviderEventStream, ProviderId, ProviderKind, ProviderOutput,
    ProviderRequest, ProviderTransport, Route, RouteId, RouteSlug, RuntimeGeneration,
    RuntimeGenerationId, RuntimeSnapshot, Surface, Target, TargetId, TransportError, TransportMode,
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
    failover::{EventStream, circuit_accounted_event_stream, validated_event_stream},
    limits::reserve_limits,
    media_jobs::{media_job_state, valid_upstream_media_job_id},
    multipart::MultipartFormData,
};
use crate::MultipartRequestAdmission;

#[test]
fn inference_error_debug_redacts_client_message() {
    let error =
        InferenceError::bad_gateway("provider_protocol_error", "sensitive upstream response");
    let debug = format!("{error:?}");

    assert!(!debug.contains("sensitive upstream response"));
    assert!(debug.contains("[REDACTED]"));
}

#[test]
fn unknown_upstream_video_status_fails_closed() {
    let error = media_job_state(&olp_domain::VideoStatus::Other("mystery".to_owned()))
        .expect_err("unknown upstream status must not become a local terminal state");
    assert_eq!(error.status, StatusCode::BAD_GATEWAY);
    assert_eq!(error.code, "provider_protocol_error");
}

#[test]
fn upstream_media_identity_is_bounded_before_durable_attachment() {
    assert!(valid_upstream_media_job_id("video_123"));
    assert!(!valid_upstream_media_job_id(""));
    assert!(!valid_upstream_media_job_id(" video_123"));
    assert!(!valid_upstream_media_job_id("video\n123"));
    assert!(!valid_upstream_media_job_id(&"x".repeat(1_025)));
}

#[tokio::test]
async fn canonical_event_stream_wrapper_rejects_gaps_and_missing_done() {
    let first = CanonicalEvent::new(
        0,
        CanonicalEventKind::ResponseStart {
            response_id: None,
            provider_model: None,
        },
    );
    let mut validator = EventSequenceValidator::new();
    validator.push(&first).unwrap();
    let events: EventStream = Box::pin(stream::iter([Ok(CanonicalEvent::new(
        2,
        CanonicalEventKind::Done,
    ))]));
    let error = match validated_event_stream(events, validator).next().await {
        Some(Err(error)) => error,
        _ => panic!("sequence gap must become a protocol error"),
    };
    assert_eq!(error.class, AttemptFailureClass::Protocol);
    assert!(error.response_committed);
    assert!(
        error
            .message
            .contains("expected canonical event sequence 1")
    );

    let mut validator = EventSequenceValidator::new();
    validator.push(&first).unwrap();
    let events: EventStream = Box::pin(stream::empty());
    let error = match validated_event_stream(events, validator).next().await {
        Some(Err(error)) => error,
        _ => panic!("missing done must become a protocol error"),
    };
    assert_eq!(error.class, AttemptFailureClass::Protocol);
    assert!(error.message.contains("ended before done"));

    let mut validator = EventSequenceValidator::new();
    validator.push(&first).unwrap();
    let events: EventStream = Box::pin(stream::iter([Ok(CanonicalEvent::new(
        1,
        CanonicalEventKind::Done,
    ))]));
    let mut events = validated_event_stream(events, validator);
    assert!(matches!(
        events.next().await,
        Some(Ok(CanonicalEvent {
            kind: CanonicalEventKind::Done,
            ..
        }))
    ));
    assert!(events.next().await.is_none());
}

#[tokio::test]
async fn committed_stream_failures_trip_circuit_only_after_terminal_accounting() {
    let circuits = crate::circuit::CircuitBreaker::default();
    let target = TargetId::new();
    let first = CanonicalEvent::new(
        0,
        CanonicalEventKind::ResponseStart {
            response_id: None,
            provider_model: None,
        },
    );

    for _ in 0..5 {
        assert!(circuits.try_acquire(target));
        let mut validator = EventSequenceValidator::new();
        validator.push(&first).unwrap();
        let provider: EventStream = Box::pin(stream::iter([Err(TransportError {
            phase: olp_domain::TransportPhase::Body,
            class: AttemptFailureClass::UpstreamServer,
            response_committed: false,
            message: "stream failed after its first event".to_owned(),
        })]));
        let mut events = circuit_accounted_event_stream(
            validated_event_stream(provider, validator),
            circuits.clone(),
            target,
            false,
        );
        let error = events.next().await.unwrap().unwrap_err();
        assert!(error.response_committed);
    }
    assert!(!circuits.is_selectable(target));

    let recovered_target = TargetId::new();
    circuits.record_failure(recovered_target, AttemptFailureClass::UpstreamServer);
    let mut validator = EventSequenceValidator::new();
    validator.push(&first).unwrap();
    let provider: EventStream = Box::pin(stream::iter([Ok(CanonicalEvent::new(
        1,
        CanonicalEventKind::Done,
    ))]));
    let mut events = circuit_accounted_event_stream(
        validated_event_stream(provider, validator),
        circuits.clone(),
        recovered_target,
        false,
    );
    assert!(matches!(
        events.next().await,
        Some(Ok(CanonicalEvent {
            kind: CanonicalEventKind::Done,
            ..
        }))
    ));
    for _ in 0..4 {
        circuits.record_failure(recovered_target, AttemptFailureClass::UpstreamServer);
    }
    assert!(circuits.is_selectable(recovered_target));
}

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

struct PendingTransport;

#[derive(Default)]
struct CountingAdmissionSpool {
    puts: AtomicUsize,
}

impl olp_domain::MediaSpool for CountingAdmissionSpool {
    fn put<'a>(
        &'a self,
        _upload: olp_domain::MediaUpload,
    ) -> BoxFuture<'a, Result<olp_domain::MediaArtifact, olp_domain::MediaSpoolError>> {
        self.puts.fetch_add(1, Ordering::SeqCst);
        Box::pin(async { Err(olp_domain::MediaSpoolError::Unavailable) })
    }

    fn open<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> BoxFuture<'a, Result<olp_domain::OpenedMedia, olp_domain::MediaSpoolError>> {
        Box::pin(async { Err(olp_domain::MediaSpoolError::NotFound) })
    }

    fn remove<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> BoxFuture<'a, Result<(), olp_domain::MediaSpoolError>> {
        Box::pin(async { Ok(()) })
    }
}

struct RecordingSpool {
    inner: Arc<dyn MediaSpool>,
    handles: Mutex<Vec<MediaHandle>>,
}

impl RecordingSpool {
    fn new(inner: Arc<dyn MediaSpool>) -> Arc<Self> {
        Arc::new(Self {
            inner,
            handles: Mutex::new(Vec::new()),
        })
    }

    fn handles(&self) -> Vec<MediaHandle> {
        self.handles.lock().unwrap().clone()
    }
}

impl MediaSpool for RecordingSpool {
    fn capacity_bytes(&self) -> Option<u64> {
        self.inner.capacity_bytes()
    }

    fn put<'a>(
        &'a self,
        upload: olp_domain::MediaUpload,
    ) -> BoxFuture<'a, Result<olp_domain::MediaArtifact, olp_domain::MediaSpoolError>> {
        Box::pin(async move {
            let artifact = self.inner.put(upload).await?;
            self.handles.lock().unwrap().push(artifact.handle.clone());
            Ok(artifact)
        })
    }

    fn open<'a>(
        &'a self,
        handle: &'a MediaHandle,
    ) -> BoxFuture<'a, Result<olp_domain::OpenedMedia, olp_domain::MediaSpoolError>> {
        self.inner.open(handle)
    }

    fn remove<'a>(
        &'a self,
        handle: &'a MediaHandle,
    ) -> BoxFuture<'a, Result<(), olp_domain::MediaSpoolError>> {
        self.inner.remove(handle)
    }
}

struct CapturingPendingTransport {
    captured: tokio::sync::mpsc::UnboundedSender<MediaHandle>,
}

impl ProviderTransport for PendingTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        Box::pin(std::future::pending())
    }
}

impl ProviderTransport for CapturingPendingTransport {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        if let Operation::TokenCount(operation) = &request.operation
            && let Some(handle) = operation.input.iter().find_map(|part| match part {
                olp_domain::ContentPart::InputAudio { media, .. }
                | olp_domain::ContentPart::InputFile { media, .. } => Some(media.clone()),
                _ => None,
            })
        {
            let _ = self.captured.send(handle);
        }
        Box::pin(std::future::pending())
    }
}

#[tokio::test]
async fn invalid_keys_cannot_spool_responses_media_before_authentication() {
    let (mut state, _) = test_state(false);
    let (_, invalid_key) = test_state(false);
    let spool = Arc::new(CountingAdmissionSpool::default());
    state.media_spool = spool.clone();

    for (path, body) in [
        (
            "/openai/v1/responses",
            r#"{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_audio","input_audio":{"data":"YXVkaW8=","format":"wav"}}]}]}"#,
        ),
        (
            "/openai/v1/responses/input_tokens",
            r#"{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_audio","input_audio":{"data":"YXVkaW8=","format":"wav"}}]}]}"#,
        ),
    ] {
        let response = post_json(&state, &invalid_key, path, body).await;
        assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
    }
    assert_eq!(spool.puts.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn responses_scope_authorization_precedes_json_errors_and_media_staging() {
    let (mut state, key) = test_state(false);
    let spool = Arc::new(CountingAdmissionSpool::default());
    state.media_spool = spool.clone();
    replace_api_key_scopes(&state, BTreeSet::from([ApiKeyScope::ModelsRead]));

    for path in ["/openai/v1/responses", "/openai/v1/responses/input_tokens"] {
        let response = post_json(&state, &key, path, "{").await;
        assert_eq!(
            response.status(),
            StatusCode::FORBIDDEN,
            "operation authorization must precede malformed JSON for {path}"
        );

        let response = post_json(
            &state,
            &key,
            path,
            r#"{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_audio","input_audio":{"data":"YXVkaW8=","format":"wav"}}]}]}"#,
        )
        .await;
        assert_eq!(
            response.status(),
            StatusCode::FORBIDDEN,
            "operation authorization must precede inline-media admission for {path}"
        );
        assert_eq!(spool.puts.load(Ordering::SeqCst), 0, "{path}");
    }
}

#[tokio::test]
async fn restricted_multipart_key_rejects_file_before_model_without_spooling() {
    let (mut state, key) = test_state(false);
    let spool = Arc::new(CountingAdmissionSpool::default());
    state.media_spool = spool.clone();
    restrict_api_key_to_route(&state, RouteSlug::parse("default").unwrap());
    let body = concat!(
        "--olp-test-boundary\r\n",
        "Content-Disposition: form-data; name=\"image\"; filename=\"fixture.png\"\r\n",
        "Content-Type: image/png\r\n\r\n",
        "file-before-model\r\n",
        "--olp-test-boundary\r\n",
        "Content-Disposition: form-data; name=\"model\"\r\n\r\n",
        "default\r\n",
        "--olp-test-boundary--\r\n"
    )
    .to_owned();
    let response = post_multipart(&state, &key, "/openai/v1/images/edits", body).await;
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    assert_eq!(spool.puts.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn multipart_route_header_mismatch_cleans_the_staged_file() {
    let (mut state, key) = test_state(false);
    let recording = RecordingSpool::new(
        crate::media_spool::FileMediaSpool::create().unwrap() as Arc<dyn MediaSpool>
    );
    state.media_spool = recording.clone();
    restrict_api_key_to_route(&state, RouteSlug::parse("default").unwrap());
    let body = concat!(
        "--olp-test-boundary\r\n",
        "Content-Disposition: form-data; name=\"image\"; filename=\"fixture.png\"\r\n",
        "Content-Type: image/png\r\n\r\n",
        "staged-image\r\n",
        "--olp-test-boundary\r\n",
        "Content-Disposition: form-data; name=\"model\"\r\n\r\n",
        "other\r\n",
        "--olp-test-boundary\r\n",
        "Content-Disposition: form-data; name=\"prompt\"\r\n\r\n",
        "route mismatch\r\n",
        "--olp-test-boundary--\r\n"
    );
    let response = crate::public_router(state.clone())
        .oneshot(
            Request::post("/openai/v1/images/edits")
                .header(header::AUTHORIZATION, format!("Bearer {key}"))
                .header("x-olp-route", "default")
                .header(
                    header::CONTENT_TYPE,
                    "multipart/form-data; boundary=olp-test-boundary",
                )
                .body(Body::from(body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    let handles = recording.handles();
    assert_eq!(handles.len(), 1);
    assert!(matches!(
        recording.open(&handles[0]).await,
        Err(olp_domain::MediaSpoolError::NotFound)
    ));
}

#[tokio::test]
async fn cancelling_response_input_tokens_handler_cleans_admitted_media() {
    let (state, key) = test_state(false);
    install_result(
        &state,
        OperationKind::TokenCount,
        CanonicalResult::TokenCount(olp_domain::TokenCountResult {
            input_tokens: 1,
            extensions: olp_domain::SourceExtensions::default(),
        }),
    );
    let (captured, mut handles) = tokio::sync::mpsc::unbounded_channel();
    install_transport(&state, Arc::new(CapturingPendingTransport { captured }));
    let state_for_task = state.clone();
    let task = tokio::spawn(async move {
        post_json(
            &state_for_task,
            &key,
            "/openai/v1/responses/input_tokens",
            r#"{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_audio","input_audio":{"data":"YXVkaW8=","format":"wav"}}]}]}"#,
        )
        .await
    });
    let handle = tokio::time::timeout(Duration::from_secs(1), handles.recv())
        .await
        .expect("token-count request must reach its transport")
        .expect("transport must capture the admitted handle");
    assert!(state.media_spool.open(&handle).await.is_ok());

    task.abort();
    let _ = task.await;
    tokio::time::timeout(Duration::from_secs(1), async {
        loop {
            match state.media_spool.open(&handle).await {
                Err(olp_domain::MediaSpoolError::NotFound) => break,
                Ok(_) => tokio::task::yield_now().await,
                Err(error) => panic!("unexpected media cleanup error: {error}"),
            }
        }
    })
    .await
    .expect("handler cancellation must schedule admitted-media cleanup");
}

#[tokio::test]
async fn dropping_blocked_upstream_request_cleans_owned_media_handles() {
    let (state, _) = test_state(false);
    let artifact = state
        .media_spool
        .put(olp_domain::MediaUpload {
            filename: "inline.wav".into(),
            content_type: Some("audio/wav".into()),
            maximum_length: 16,
            bytes: Box::pin(stream::once(async { Ok(Bytes::from_static(b"audio")) })),
        })
        .await
        .unwrap();
    install_transport(&state, Arc::new(PendingTransport));
    let request: ChatCompletionRequest = serde_json::from_value(json!({
        "model":"default","messages":[{"role":"user","content":"hello"}]
    }))
    .unwrap();
    let mut operation = decode_chat_completion(request).unwrap();
    let Operation::Generation(generation) = &mut operation else {
        unreachable!()
    };
    generation.messages[0].content = vec![olp_domain::ContentPart::InputAudio {
        media: artifact.handle.clone(),
        format: "wav".into(),
    }];

    let principal = test_principal(&state, Surface::OpenAi);
    let state_for_task = state.clone();
    let task = tokio::spawn(async move {
        execute_event_operation_for_surface(
            &state_for_task,
            &principal,
            operation,
            TransportMode::Unary,
        )
        .await
    });
    tokio::time::sleep(Duration::from_millis(20)).await;
    task.abort();
    let _ = task.await;

    tokio::time::timeout(Duration::from_secs(1), async {
        loop {
            match state.media_spool.open(&artifact.handle).await {
                Err(olp_domain::MediaSpoolError::NotFound) => break,
                Ok(_) => tokio::task::yield_now().await,
                Err(error) => panic!("unexpected media cleanup error: {error}"),
            }
        }
    })
    .await
    .expect("request media guard must schedule cleanup when its future is dropped");
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

struct DropAwareStream {
    first: Option<CanonicalEvent>,
    dropped: Arc<AtomicBool>,
}

impl futures::Stream for DropAwareStream {
    type Item = Result<CanonicalEvent, TransportError>;

    fn poll_next(
        mut self: std::pin::Pin<&mut Self>,
        _context: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        self.first.take().map_or(std::task::Poll::Pending, |event| {
            std::task::Poll::Ready(Some(Ok(event)))
        })
    }
}

impl Drop for DropAwareStream {
    fn drop(&mut self) {
        self.dropped.store(true, Ordering::Release);
    }
}

#[derive(Clone)]
struct DropAwareTransport {
    dropped: Arc<AtomicBool>,
}

impl ProviderTransport for DropAwareTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        let dropped = self.dropped.clone();
        Box::pin(async move {
            Ok(ProviderOutput::Events(Box::pin(DropAwareStream {
                first: Some(CanonicalEvent::new(
                    0,
                    CanonicalEventKind::TextDelta {
                        output_index: 0,
                        text: "first".into(),
                    },
                )),
                dropped,
            })))
        })
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

fn test_state(streaming: bool) -> (ApiState, String) {
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
    let mut state = ApiState::new(
        crate::ApiMode::Gateway,
        None,
        runtime,
        "https://olp.test",
        "console",
    );
    state.auth_hmac_key = Some(auth_hmac_key);
    (state, plaintext)
}

fn test_principal(state: &ApiState, surface: Surface) -> crate::InferencePrincipal {
    let runtime = state.runtime.pin();
    let (lookup_id, _) = runtime.api_keys.iter().next().unwrap();
    crate::InferencePrincipal::for_test(Arc::clone(&runtime), lookup_id.clone(), surface)
}

fn reinstall_api_keys(state: &ApiState, api_keys: BTreeMap<ApiKeyLookupId, ApiKey>) {
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

fn install_hard_limits(state: &ApiState) {
    let pinned = state.runtime.pin();
    let mut api_keys = pinned.api_keys.clone();
    api_keys
        .values_mut()
        .next()
        .unwrap()
        .limits
        .requests_per_minute = NonZeroU32::new(10);
    api_keys.values_mut().next().unwrap().limits.concurrency = NonZeroU32::new(2);
    reinstall_api_keys(state, api_keys);
}

fn restrict_api_key_to_route(state: &ApiState, route: RouteSlug) {
    let pinned = state.runtime.pin();
    let mut api_keys = pinned.api_keys.clone();
    api_keys.values_mut().next().unwrap().allowed_routes = BTreeSet::from([route]);
    reinstall_api_keys(state, api_keys);
}

fn replace_api_key_scopes(state: &ApiState, scopes: BTreeSet<ApiKeyScope>) {
    let pinned = state.runtime.pin();
    let mut api_keys = pinned.api_keys.clone();
    api_keys.values_mut().next().unwrap().scopes = scopes;
    reinstall_api_keys(state, api_keys);
}

fn install_result(state: &ApiState, operation: OperationKind, result: CanonicalResult) {
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

fn install_transport(state: &ApiState, transport: Arc<dyn ProviderTransport>) {
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

fn install_event_stream(
    state: &ApiState,
    operation: OperationKind,
    events: Vec<CanonicalEvent>,
    finite: bool,
) {
    let pinned = state.runtime.pin();
    let provider_id = *pinned.providers.keys().next().unwrap();
    let mut providers = pinned.providers.clone();
    providers.get_mut(&provider_id).unwrap().capabilities = BTreeSet::from([Capability::new(
        "upstream-model",
        operation,
        Surface::OpenAi,
        TransportMode::Streaming,
    )]);
    let mut routes = pinned.routes.clone();
    routes
        .get_mut(&RouteSlug::parse("default").unwrap())
        .unwrap()
        .operations = BTreeSet::from([operation]);
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
    let transport: Arc<dyn ProviderTransport> = if finite {
        Arc::new(FiniteStaticTransport { events })
    } else {
        Arc::new(StaticTransport { events })
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

fn raw_media_event(sequence: u64, event: &str, data: Value) -> CanonicalEvent {
    CanonicalEvent::new(
        sequence,
        CanonicalEventKind::SourceExtension {
            extensions: olp_domain::SourceExtensions::new(
                Surface::OpenAi,
                BTreeMap::from([
                    ("/__olp/raw_sse/event".into(), Value::String(event.into())),
                    ("/__olp/raw_sse/data".into(), data),
                ]),
            ),
        },
    )
}

#[tokio::test]
async fn unary_openai_route_authenticates_routes_and_encodes() {
    let (mut state, key) = test_state(false);
    let (emitter, mut request_metadata) = olp_storage::RequestMetadataEmitter::bounded(4);
    state.request_metadata = Some(emitter);
    let response = tokio::time::timeout(
        Duration::from_millis(250),
        crate::public_router(state).oneshot(
            Request::post("/openai/v1/chat/completions")
                .header(header::AUTHORIZATION, format!("Bearer {key}"))
                .header(header::CONTENT_TYPE, "application/json")
                .body(Body::from(
                    r#"{"model":"default","messages":[{"role":"user","content":"hi"}]}"#,
                ))
                .unwrap(),
        ),
    )
    .await
    .expect("canonical Done must stop polling a provider that holds the stream open")
    .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let body = response.into_body().collect().await.unwrap().to_bytes();
    let value: Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(value["model"], "default");
    assert_eq!(value["choices"][0]["message"]["content"], "hello from OLP");
    let event = request_metadata.recv_next().await.unwrap();
    assert_eq!(event.status_code, Some(200));
    assert_eq!(event.attempts.len(), 1);
    assert!(event.committed);
    assert!(!event.usage_complete, "missing provider usage is explicit");
}

#[tokio::test]
async fn openai_json_audio_and_responses_pdf_reach_same_protocol_transport() {
    let (state, key) = test_state(false);
    let app = crate::public_router(state);
    let response = app
        .clone()
        .oneshot(
            Request::post("/openai/v1/chat/completions")
                .header(header::AUTHORIZATION, format!("Bearer {key}"))
                .header(header::CONTENT_TYPE, "application/json")
                .body(Body::from(
                    r#"{"model":"default","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"aGk=","format":"wav"}}]}]}"#,
                ))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);

    let response = app
        .oneshot(
            Request::post("/openai/v1/responses")
                .header(header::AUTHORIZATION, format!("Bearer {key}"))
                .header(header::CONTENT_TYPE, "application/json")
                .body(Body::from(
                    r#"{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_file","filename":"brief.pdf","file_data":"data:application/pdf;base64,aGk="}]}]}"#,
                ))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
}

#[tokio::test]
async fn direct_executor_reserves_hard_limits_before_route_selection() {
    let (state, _) = test_state(false);
    install_hard_limits(&state);
    let request: ChatCompletionRequest = serde_json::from_value(json!({
        "model": "route-does-not-exist",
        "messages": [{"role": "user", "content": "hello"}]
    }))
    .unwrap();
    let operation = decode_chat_completion(request).unwrap();
    let principal = test_principal(&state, Surface::OpenAi);
    let error = match execute_event_operation_for_surface_inner(
        &state,
        &principal,
        operation,
        TransportMode::Unary,
    )
    .await
    {
        Ok(_) => panic!("missing limiter must fail closed before route selection"),
        Err(error) => error,
    };
    assert_eq!(error.status, StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(error.code, "distributed_limits_unavailable");
}

#[tokio::test]
async fn required_target_unavailability_is_normalized_by_shared_execution_kernel() {
    let (state, _) = test_state(false);
    install_result(
        &state,
        OperationKind::TokenCount,
        CanonicalResult::TokenCount(olp_domain::TokenCountResult {
            input_tokens: 1,
            extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        }),
    );
    let request: ResponseInputTokensRequest = serde_json::from_value(json!({
        "model": "default",
        "input": "hello"
    }))
    .unwrap();
    let operation = decode_response_input_tokens(request).unwrap();
    let principal = test_principal(&state, Surface::OpenAi);

    let error = match execute_routed_result_for_surface_inner(
        &state,
        &principal,
        operation,
        TransportMode::Unary,
        Some(RequiredTarget {
            provider_id: uuid::Uuid::now_v7(),
            upstream_model: "unavailable-model".to_owned(),
        }),
    )
    .await
    {
        Ok(_) => panic!("a missing pinned target must not fall back to another target"),
        Err(error) => error,
    };

    assert_eq!(error.status, StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(error.code, "media_job_target_unavailable");
}

#[tokio::test]
async fn http_pre_reservation_marker_reuses_the_full_reservation() {
    let (state, key) = test_state(false);
    install_hard_limits(&state);
    let snapshot = state.runtime.pin();
    let api_key = snapshot.api_keys.values().next().unwrap();
    let lookup = state
        .auth_hmac_key
        .as_ref()
        .unwrap()
        .lookup_id(&key)
        .unwrap();
    let operation = decode_chat_completion(
        serde_json::from_value(json!({
            "model": "default",
            "messages": [{"role": "user", "content": "hello"}]
        }))
        .unwrap(),
    )
    .unwrap();
    let lease = crate::HTTP_INFERENCE_LIMITS_RESERVED
        .scope(
            10_000,
            reserve_limits(&state, api_key, &operation, lookup, Duration::from_secs(30)),
        )
        .await
        .expect("the canonical executor must reuse the HTTP reservation");
    assert!(lease.is_none());
}

#[tokio::test]
async fn http_request_above_baseline_requires_token_delta_reservation() {
    let (state, key) = test_state(false);
    let pinned = state.runtime.pin();
    let mut api_keys = pinned.api_keys.clone();
    api_keys
        .values_mut()
        .next()
        .unwrap()
        .limits
        .tokens_per_minute = std::num::NonZeroU64::new(2_200);
    reinstall_api_keys(&state, api_keys);
    let snapshot = state.runtime.pin();
    let api_key = snapshot.api_keys.values().next().unwrap();
    let lookup = state
        .auth_hmac_key
        .as_ref()
        .unwrap()
        .lookup_id(&key)
        .unwrap();
    let operation = Operation::Images(olp_domain::ImageOperation::Edit(
        olp_domain::ImageEditRequest {
            route: RouteSlug::parse("default").unwrap(),
            images: vec![MediaHandle::new("bounded-image")],
            mask: None,
            prompt: "x".repeat(8_500),
            stream: false,
            extensions: olp_domain::SourceExtensions::default(),
        },
    ));
    let error = crate::HTTP_INFERENCE_LIMITS_RESERVED
        .scope(
            2_000,
            reserve_limits(&state, api_key, &operation, lookup, Duration::from_secs(30)),
        )
        .await
        .expect_err("missing delta limiter must fail closed above the HTTP baseline");
    assert_eq!(error.status, StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(error.code, "distributed_limits_unavailable");
}

#[tokio::test]
async fn invalid_proxy_key_gets_native_openai_error() {
    let (state, _) = test_state(false);
    let response = crate::public_router(state)
        .oneshot(
            Request::post("/openai/v1/chat/completions")
                .header(header::AUTHORIZATION, "Bearer olp_v2_deadbeef0000_bad")
                .header(header::CONTENT_TYPE, "application/json")
                .body(Body::from(r#"{"model":"default","messages":[]}"#))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
    let body = response.into_body().collect().await.unwrap().to_bytes();
    let value: Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(value["error"]["type"], "authentication_error");
}

#[tokio::test]
async fn responses_surface_encodes_responses_object_not_chat_object() {
    let (state, key) = test_state(false);
    let response = crate::public_router(state)
        .oneshot(
            Request::post("/openai/v1/responses")
                .header(header::AUTHORIZATION, format!("Bearer {key}"))
                .header(header::CONTENT_TYPE, "application/json")
                .body(Body::from(r#"{"model":"default","input":"hello"}"#))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let body = response.into_body().collect().await.unwrap().to_bytes();
    let value: Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(value["object"], "response");
    assert_eq!(value["model"], "default");
    assert_eq!(value["output"][0]["content"][0]["text"], "hello from OLP");
}

#[tokio::test]
async fn embeddings_surface_routes_and_encodes_typed_result() {
    let (state, key) = test_state(false);
    install_result(
        &state,
        OperationKind::Embeddings,
        CanonicalResult::Embeddings(olp_domain::EmbeddingsResult {
            model: Some("upstream-model".into()),
            data: vec![olp_domain::EmbeddingVector {
                index: 0,
                values: vec![0.25, -0.5],
            }],
            usage: Some(olp_domain::Usage {
                input_tokens: 1,
                output_tokens: 0,
                total_tokens: 1,
                cached_input_tokens: None,
                reasoning_tokens: None,
            }),
            extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        }),
    );
    let response = crate::public_router(state)
        .oneshot(
            Request::post("/openai/v1/embeddings")
                .header(header::AUTHORIZATION, format!("Bearer {key}"))
                .header(header::CONTENT_TYPE, "application/json")
                .body(Body::from(r#"{"model":"default","input":"hello"}"#))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let body = response.into_body().collect().await.unwrap().to_bytes();
    let value: Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(value["object"], "list");
    assert_eq!(value["model"], "default");
    assert_eq!(value["data"][0]["embedding"][0], 0.25);
}

async fn post_json(state: &ApiState, key: &str, path: &str, body: &'static str) -> Response {
    crate::public_router(state.clone())
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

async fn post_multipart(state: &ApiState, key: &str, path: &str, body: String) -> Response {
    crate::public_router(state.clone())
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

#[tokio::test]
async fn chat_and_responses_stream_through_the_real_router_with_native_usage() {
    let (mut state, key) = test_state(true);
    let (emitter, mut request_metadata) = olp_storage::RequestMetadataEmitter::bounded(8);
    state.request_metadata = Some(emitter);

    install_event_stream(
        &state,
        OperationKind::Generation,
        generation_stream_events("chat stream"),
        false,
    );
    let response = post_json(
        &state,
        &key,
        "/openai/v1/chat/completions",
        r#"{"model":"default","messages":[{"role":"user","content":"hi"}],"stream":true}"#,
    )
    .await;
    assert_eq!(response.status(), StatusCode::OK);
    assert_eq!(
        response.headers()[header::CONTENT_TYPE],
        "text/event-stream; charset=utf-8"
    );
    let body = response_text(response).await;
    assert!(body.contains("\"object\":\"chat.completion.chunk\""));
    assert!(body.contains("chat stream"));
    assert!(body.contains("\"prompt_tokens\":7"));
    assert!(body.ends_with("data: [DONE]\n\n"));
    let event = request_metadata.recv_next().await.unwrap();
    assert_eq!(event.operation, OperationKind::Generation);
    assert_eq!(event.input_tokens, Some(7));
    assert_eq!(event.output_tokens, Some(3));
    assert!(event.usage_complete);

    install_event_stream(
        &state,
        OperationKind::Generation,
        generation_stream_events("responses stream"),
        false,
    );
    let response = post_json(
        &state,
        &key,
        "/openai/v1/responses",
        r#"{"model":"default","input":"hi","stream":true}"#,
    )
    .await;
    assert_eq!(response.status(), StatusCode::OK);
    let body = response_text(response).await;
    assert!(body.contains("event: response.created"));
    assert!(body.contains("event: response.output_text.delta"));
    assert!(body.contains("responses stream"));
    assert!(body.contains("event: response.completed"));
    assert!(!body.contains("chat.completion.chunk"));
    let event = request_metadata.recv_next().await.unwrap();
    assert_eq!(event.operation, OperationKind::Generation);
    assert_eq!(event.input_tokens, Some(7));
    assert!(event.usage_complete);
}

#[tokio::test]
async fn canonical_stream_error_is_not_persisted_as_success() {
    let (mut state, key) = test_state(true);
    let (emitter, mut request_metadata) = olp_storage::RequestMetadataEmitter::bounded(2);
    state.request_metadata = Some(emitter);
    install_event_stream(
        &state,
        OperationKind::Generation,
        vec![
            CanonicalEvent::new(
                0,
                CanonicalEventKind::Error {
                    error: CanonicalError {
                        class: ErrorClass::RateLimit,
                        message: "provider throttled the request".to_owned(),
                        provider_code: Some("rate_limit".to_owned()),
                        retryable: true,
                    },
                },
            ),
            CanonicalEvent::new(1, CanonicalEventKind::Done),
        ],
        true,
    );

    let response = post_json(
        &state,
        &key,
        "/openai/v1/responses",
        r#"{"model":"default","input":"hi","stream":true}"#,
    )
    .await;
    assert_eq!(response.status(), StatusCode::OK);
    let body = response_text(response).await;
    assert!(
        body.contains("error") || body.contains("failed"),
        "stream body was {body:?}"
    );
    let event = request_metadata.recv_next().await.unwrap();
    assert_eq!(event.status_code, Some(429));
    assert_ne!(event.error_class.as_deref(), None);
    assert!(event.committed);
}

#[tokio::test]
async fn incompatible_unary_result_is_finalized_as_protocol_failure() {
    let (mut state, key) = test_state(false);
    let (emitter, mut request_metadata) = olp_storage::RequestMetadataEmitter::bounded(2);
    state.request_metadata = Some(emitter);
    install_result(
        &state,
        OperationKind::TokenCount,
        CanonicalResult::ModelList(olp_domain::ModelListResult {
            models: Vec::new(),
            extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        }),
    );

    let response = post_json(
        &state,
        &key,
        "/openai/v1/responses/input_tokens",
        r#"{"model":"default","input":"hello"}"#,
    )
    .await;
    assert_eq!(response.status(), StatusCode::BAD_GATEWAY);
    let event = request_metadata.recv_next().await.unwrap();
    assert_eq!(event.status_code, Some(502));
    assert_eq!(
        event.error_class.as_deref(),
        Some("provider_protocol_error")
    );
    assert_eq!(event.attempts.len(), 1);
    assert!(event.committed);
}

#[tokio::test]
async fn real_router_generation_streams_report_truncation_in_native_envelopes() {
    let (state, key) = test_state(true);
    let truncated = generation_stream_events("partial")
        .into_iter()
        .take(3)
        .collect::<Vec<_>>();
    install_event_stream(&state, OperationKind::Generation, truncated.clone(), true);
    let response = post_json(
        &state,
        &key,
        "/openai/v1/chat/completions",
        r#"{"model":"default","messages":[{"role":"user","content":"hi"}],"stream":true}"#,
    )
    .await;
    let body = response_text(response).await;
    assert!(body.contains("provider_protocol_error"));
    assert!(body.ends_with("data: [DONE]\n\n"));

    install_event_stream(&state, OperationKind::Generation, truncated, true);
    let response = post_json(
        &state,
        &key,
        "/openai/v1/responses",
        r#"{"model":"default","input":"hi","stream":true}"#,
    )
    .await;
    let body = response_text(response).await;
    assert!(body.contains("event: error"));
    assert!(body.contains("\"type\":\"error\""));
    assert!(body.contains("provider_protocol_error"));
    assert!(!body.contains("event: response.completed"));

    install_event_stream(
        &state,
        OperationKind::ImageGeneration,
        vec![raw_media_event(
            0,
            "image_generation.partial_image",
            json!({"type":"image_generation.partial_image","partial_image_index":0,"b64_json":"YQ=="}),
        )],
        true,
    );
    let response = post_json(
        &state,
        &key,
        "/openai/v1/images/generations",
        r#"{"model":"default","prompt":"fox","stream":true}"#,
    )
    .await;
    let body = response_text(response).await;
    assert!(body.contains("event: image_generation.partial_image"));
    assert!(body.contains("provider_protocol_error"));
}

#[tokio::test]
async fn image_speech_and_transcription_stream_native_sse_and_usage_through_router() {
    let (mut state, key) = test_state(true);
    let (emitter, mut request_metadata) = olp_storage::RequestMetadataEmitter::bounded(8);
    state.request_metadata = Some(emitter);

    install_event_stream(
        &state,
        OperationKind::ImageGeneration,
        vec![
            raw_media_event(
                0,
                "image_generation.partial_image",
                json!({"type":"image_generation.partial_image","partial_image_index":0,"b64_json":"YQ=="}),
            ),
            raw_media_event(
                1,
                "image_generation.completed",
                json!({"type":"image_generation.completed","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}),
            ),
            CanonicalEvent::new(2, CanonicalEventKind::Done),
        ],
        false,
    );
    let response = post_json(
        &state,
        &key,
        "/openai/v1/images/generations",
        r#"{"model":"default","prompt":"fox","stream":true}"#,
    )
    .await;
    let body = response_text(response).await;
    assert!(body.contains("event: image_generation.partial_image"));
    assert!(body.contains("event: image_generation.completed"));
    let event = request_metadata.recv_next().await.unwrap();
    assert_eq!(event.operation, OperationKind::ImageGeneration);
    assert_eq!(event.input_tokens, Some(4));
    assert!(event.usage_complete);

    install_event_stream(
        &state,
        OperationKind::Speech,
        vec![
            raw_media_event(
                0,
                "speech.audio.delta",
                json!({"type":"speech.audio.delta","audio":"bXAz"}),
            ),
            raw_media_event(
                1,
                "speech.audio.done",
                json!({"type":"speech.audio.done","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}),
            ),
            CanonicalEvent::new(2, CanonicalEventKind::Done),
        ],
        false,
    );
    let response = post_json(
        &state,
        &key,
        "/openai/v1/audio/speech",
        r#"{"model":"default","input":"hello","voice":"coral","stream_format":"sse"}"#,
    )
    .await;
    let body = response_text(response).await;
    assert!(body.contains("event: speech.audio.delta"));
    assert!(body.contains("\"audio\":\"bXAz\""));
    assert!(body.contains("event: speech.audio.done"));
    let event = request_metadata.recv_next().await.unwrap();
    assert_eq!(event.operation, OperationKind::Speech);
    assert_eq!(event.input_tokens, Some(2));
    assert!(event.usage_complete);

    install_event_stream(
        &state,
        OperationKind::Transcription,
        vec![
            raw_media_event(
                0,
                "transcript.text.delta",
                json!({"type":"transcript.text.delta","delta":"hello"}),
            ),
            raw_media_event(
                1,
                "transcript.text.done",
                json!({"type":"transcript.text.done","text":"hello","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}),
            ),
            CanonicalEvent::new(2, CanonicalEventKind::Done),
        ],
        false,
    );
    let response = post_multipart(
        &state,
        &key,
        "/openai/v1/audio/transcriptions",
        multipart(
            &[("model", "default"), ("stream", "true")],
            "file",
            "wave-bytes",
        ),
    )
    .await;
    let body = response_text(response).await;
    assert!(body.contains("event: transcript.text.delta"));
    assert!(body.contains("event: transcript.text.done"));
    let event = request_metadata.recv_next().await.unwrap();
    assert_eq!(event.operation, OperationKind::Transcription);
    assert_eq!(event.input_tokens, Some(3));
    assert!(event.usage_complete);
}

#[tokio::test]
async fn selected_openai_unary_surfaces_route_and_encode_native_results() {
    let (state, key) = test_state(false);

    install_result(
        &state,
        OperationKind::TokenCount,
        CanonicalResult::TokenCount(olp_domain::TokenCountResult {
            input_tokens: 9,
            extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        }),
    );
    let response = post_json(
        &state,
        &key,
        "/openai/v1/responses/input_tokens",
        r#"{"model":"default","input":"hello"}"#,
    )
    .await;
    assert_eq!(response.status(), StatusCode::OK);
    let body: Value =
        serde_json::from_slice(&response.into_body().collect().await.unwrap().to_bytes()).unwrap();
    assert_eq!(body["input_tokens"], 9);

    install_result(
        &state,
        OperationKind::Moderation,
        CanonicalResult::Moderation(olp_domain::ModerationResult {
            id: Some("modr-upstream".to_owned()),
            model: Some("omni-moderation-latest".to_owned()),
            results: vec![olp_domain::ModerationItem {
                flagged: true,
                categories: BTreeMap::from([("violence".to_owned(), true)]),
                category_scores: BTreeMap::from([("violence".to_owned(), 0.9)]),
            }],
            extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        }),
    );
    let response = post_json(
        &state,
        &key,
        "/openai/v1/moderations",
        r#"{"model":"default","input":"hello"}"#,
    )
    .await;
    assert_eq!(response.status(), StatusCode::OK);
    let body: Value =
        serde_json::from_slice(&response.into_body().collect().await.unwrap().to_bytes()).unwrap();
    assert_eq!(body["model"], "default");
    assert_eq!(body["results"][0]["flagged"], true);

    let image_result = || {
        CanonicalResult::Images(olp_domain::ImagesResult {
            created_at: Some(1_800_000_000),
            images: vec![olp_domain::ImageArtifact {
                source: olp_domain::MediaSource::Uri("https://images.example/result.png".into()),
                revised_prompt: Some("revised".into()),
            }],
            usage: None,
            extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        })
    };
    install_result(&state, OperationKind::ImageGeneration, image_result());
    let response = post_json(
        &state,
        &key,
        "/openai/v1/images/generations",
        r#"{"model":"default","prompt":"cobalt fox"}"#,
    )
    .await;
    assert_eq!(response.status(), StatusCode::OK);
    let body: Value =
        serde_json::from_slice(&response.into_body().collect().await.unwrap().to_bytes()).unwrap();
    assert_eq!(body["data"][0]["url"], "https://images.example/result.png");

    install_result(&state, OperationKind::ImageEdit, image_result());
    let response = post_multipart(
        &state,
        &key,
        "/openai/v1/images/edits",
        multipart(
            &[("model", "default"), ("prompt", "edit this")],
            "image",
            "image-bytes",
        ),
    )
    .await;
    assert_eq!(response.status(), StatusCode::OK);

    install_result(&state, OperationKind::ImageVariation, image_result());
    let response = post_multipart(
        &state,
        &key,
        "/openai/v1/images/variations",
        multipart(&[("model", "default")], "image", "image-bytes"),
    )
    .await;
    assert_eq!(response.status(), StatusCode::OK);

    install_result(
        &state,
        OperationKind::Transcription,
        CanonicalResult::Transcription(olp_domain::TranscriptionResult {
            text: "transcribed".to_owned(),
            language: Some("en".to_owned()),
            duration_seconds: Some(1.0),
            segments: Vec::new(),
            extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        }),
    );
    let response = post_multipart(
        &state,
        &key,
        "/openai/v1/audio/transcriptions",
        multipart(&[("model", "default")], "file", "audio-bytes"),
    )
    .await;
    assert_eq!(response.status(), StatusCode::OK);
    let body: Value =
        serde_json::from_slice(&response.into_body().collect().await.unwrap().to_bytes()).unwrap();
    assert_eq!(body["text"], "transcribed");
}

#[tokio::test]
async fn speech_surface_streams_bounded_spooled_result() {
    let (state, key) = test_state(false);
    let artifact = state
        .media_spool
        .put(olp_domain::MediaUpload {
            filename: "speech.mp3".into(),
            content_type: Some("audio/mpeg".into()),
            maximum_length: 32,
            bytes: Box::pin(stream::once(async {
                Ok(Bytes::from_static(b"audio-result"))
            })),
        })
        .await
        .unwrap();
    install_result(
        &state,
        OperationKind::Speech,
        CanonicalResult::Speech(olp_domain::SpeechResult {
            audio: artifact,
            extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        }),
    );
    let response = post_json(
        &state,
        &key,
        "/openai/v1/audio/speech",
        r#"{"model":"default","input":"hello","voice":"coral"}"#,
    )
    .await;
    assert_eq!(response.status(), StatusCode::OK);
    assert_eq!(response.headers()[header::CONTENT_TYPE], "audio/mpeg");
    assert_eq!(
        response.into_body().collect().await.unwrap().to_bytes(),
        Bytes::from_static(b"audio-result")
    );
}

#[tokio::test]
async fn malformed_multipart_is_rejected_before_routing() {
    let (state, key) = test_state(false);
    let response = crate::public_router(state)
        .oneshot(
            Request::post("/openai/v1/images/edits")
                .header(header::AUTHORIZATION, format!("Bearer {key}"))
                .header(header::CONTENT_TYPE, "multipart/form-data")
                .body(Body::from("not-a-multipart-body"))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
}

#[tokio::test]
async fn failed_multipart_validation_removes_staged_files() {
    let spool = crate::media_spool::FileMediaSpool::create().unwrap();
    let artifact = spool
        .put(olp_domain::MediaUpload {
            filename: "upload.png".to_owned(),
            content_type: Some("image/png".to_owned()),
            maximum_length: 16,
            bytes: Box::pin(stream::once(async { Ok(Bytes::from_static(b"staged")) })),
        })
        .await
        .unwrap();
    let mut form = MultipartFormData::new(spool.clone(), MultipartRequestAdmission::unrestricted());
    form.cleanup_handles.push(artifact.handle.clone());
    drop(form);

    tokio::time::timeout(Duration::from_secs(1), async {
        loop {
            match spool.open(&artifact.handle).await {
                Err(olp_domain::MediaSpoolError::NotFound) => break,
                Ok(_) => tokio::task::yield_now().await,
                Err(error) => panic!("unexpected spool cleanup error: {error}"),
            }
        }
    })
    .await
    .expect("multipart error cleanup must remove the staged file promptly");
}

#[tokio::test]
async fn dropping_client_stream_drops_upstream_within_one_second() {
    let (state, key) = test_state(true);
    let dropped = Arc::new(AtomicBool::new(false));
    install_transport(
        &state,
        Arc::new(DropAwareTransport {
            dropped: dropped.clone(),
        }),
    );
    let response = crate::public_router(state)
        .oneshot(
            Request::post("/openai/v1/chat/completions")
                .header(header::AUTHORIZATION, format!("Bearer {key}"))
                .header(header::CONTENT_TYPE, "application/json")
                .body(Body::from(
                    r#"{"model":"default","messages":[{"role":"user","content":"hi"}],"stream":true}"#,
                ))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    drop(response);
    tokio::time::timeout(Duration::from_secs(1), async {
        while !dropped.load(Ordering::Acquire) {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("client cancellation must promptly drop the upstream stream");
}

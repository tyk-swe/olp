use crate::ids::*;
use crate::inference::transport::{
    AttemptFailureClass, ProviderOutput, ProviderRequest, ProviderTransport,
};
use crate::protocols::canonical::{
    events::Kind,
    identity::{RequestMetadata, Surface, TransportMode},
    requests::{EmbeddingInput, EmbeddingsRequest, Operation, SourceExtensions},
    results::CanonicalResult,
};
use crate::providers::{
    configuration::ProviderConfiguration,
    connector::ResponseLimits,
    connectors::configuration::Credential,
    mock_server::{MockResponse, response, spawn_mock, status_response},
    runtime_model::ProviderKind,
    types::ProviderAuthMode,
};
use crate::routes::selection::AttemptPlan;
use futures::StreamExt;
use std::sync::Arc;

fn request(operation: Operation, mode: TransportMode) -> ProviderRequest {
    ProviderRequest {
        metadata: RequestMetadata {
            request_id: RequestId::new(),
            operation: operation.kind(),
            surface: Surface::OpenAi,
            mode,
        },
        attempt: AttemptPlan {
            connection_limits: None,
            credential_limits: None,
            attempt_limit: None,
            routing_policy: None,
            credential_slot_id: None,
            credential_version_id: None,
            pricing_revision_id: None,
            generation_id: RuntimeGenerationId::new(),
            route_id: RouteId::new(),
            target_id: TargetId::new(),
            routing_id: TargetId::new(),
            provider_id: ProviderId::new(),
            provider_revision_id: uuid::Uuid::now_v7(),
            provider_kind: ProviderKind::OpenAiCompatible,
            upstream_model: "profile-model".into(),
            timeout: DurationMs::new(2000),
            priority: 0,
        },
        operation: Arc::new(operation),
        media: None,
        max_inline_media_bytes: 1024,
        propagate_trace_context: false,
    }
}
async fn connector(
    vendor: &str,
    endpoint: String,
    auth: ProviderAuthMode,
) -> Arc<dyn ProviderTransport> {
    let mut config = ProviderConfiguration::new(ProviderKind::OpenAiCompatible);
    config.endpoint = Some(endpoint);
    config.auth_mode = auth;
    config.options.vendor_id = Some(vendor.into());
    let credential = match auth {
        ProviderAuthMode::None => Credential::None,
        ProviderAuthMode::Headers => {
            config
                .options
                .credential_headers
                .insert("x-service-key".into());
            Credential::ApiKey(zeroize::Zeroizing::new(
                r#"{"x-service-key":"header-secret"}"#.into(),
            ))
        }
        _ => Credential::ApiKey(zeroize::Zeroizing::new("profile-secret".into())),
    };
    crate::providers::connectors::transport(
        config,
        credential,
        &crate::net::egress::EgressPolicy::unsafe_test_targets(),
        ResponseLimits::default(),
    )
    .await
    .unwrap()
}
fn generation(mode: TransportMode) -> Operation {
    Operation::Generation(
        crate::providers::openai::certification::probe_generation_request(
            mode,
            SourceExtensions::default(),
        ),
    )
}

async fn wait_for_rate_window() {
    let mut connection = redis::Client::open(std::env::var("OLP_VALKEY_URL").unwrap())
        .unwrap()
        .get_multiplexed_async_connection()
        .await
        .unwrap();
    let (seconds, microseconds): (u64, u64) = redis::cmd("TIME")
        .query_async(&mut connection)
        .await
        .unwrap();
    let remaining_ms = 60_000 - (seconds % 60) * 1_000 - microseconds / 1_000;
    if remaining_ms < 2_000 {
        tokio::time::sleep(std::time::Duration::from_millis(remaining_ms + 20)).await;
    }
}
fn json_body() -> Vec<u8> {
    serde_json::to_vec(&serde_json::json!({"id":"profile-response","object":"chat.completion","created":1,"model":"profile-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}})).unwrap()
}
fn streaming_body() -> Vec<u8> {
    let first = serde_json::json!({"id":"profile-response","object":"chat.completion.chunk","created":1,"model":"profile-model","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]});
    let last = serde_json::json!({"id":"profile-response","object":"chat.completion.chunk","created":1,"model":"profile-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}});
    format!("data: {first}\n\ndata: {last}\n\ndata: [DONE]\n\n").into_bytes()
}

#[tokio::test]
async fn generation_profiles_preserve_usage_streams_and_token_limits() {
    for vendor in [
        "deepseek",
        "fireworks",
        "deepinfra",
        "huggingface",
        "perplexity",
        "cohere",
    ] {
        for mode in [TransportMode::Unary, TransportMode::Streaming] {
            let (content_type, body) = if mode == TransportMode::Streaming {
                ("text/event-stream", streaming_body())
            } else {
                ("application/json", json_body())
            };
            let (endpoint, captured) =
                spawn_mock("/v1", MockResponse::immediate(response(content_type, body))).await;
            let output = connector(vendor, endpoint, ProviderAuthMode::ApiKey)
                .await
                .execute(request(generation(mode), mode))
                .await
                .unwrap();
            let ProviderOutput::Events(mut events) = output else {
                panic!("expected generation events");
            };
            let mut text = String::new();
            let mut tokens = 0;
            let mut done = false;
            while let Some(event) = events.next().await {
                match event.unwrap().kind {
                    Kind::TextDelta { text: delta, .. } => text.push_str(&delta),
                    Kind::Usage { usage } => tokens = usage.total_tokens,
                    Kind::Done => done = true,
                    _ => {}
                }
            }
            assert_eq!(text, "Hello", "{vendor} {mode}");
            assert_eq!(tokens, 6);
            assert!(done);
            let captured = String::from_utf8(captured.await.unwrap()).unwrap();
            assert!(captured.starts_with("POST /v1/chat/completions "));
            assert!(
                captured
                    .to_lowercase()
                    .contains("authorization: bearer profile-secret")
            );
            let wire: serde_json::Value =
                serde_json::from_str(captured.split_once("\r\n\r\n").unwrap().1).unwrap();
            assert_eq!(wire["max_tokens"], 1, "{vendor}");
            assert!(wire.get("max_completion_tokens").is_none());
        }
    }
}

#[tokio::test]
async fn cohere_accepts_anthropic_single_candidates_without_sending_n() {
    for mode in [TransportMode::Unary, TransportMode::Streaming] {
        let source = serde_json::json!({"model":"profile-model","max_tokens":64,
            "messages":[{"role":"user","content":"hello"}],
            "stream":mode == TransportMode::Streaming});
        let operation = crate::protocols::anthropic::translate::decode::request(
            serde_json::from_value(source).unwrap(),
        )
        .unwrap();
        let (content_type, body) = if mode == TransportMode::Streaming {
            ("text/event-stream", streaming_body())
        } else {
            ("application/json", json_body())
        };
        let (endpoint, captured) =
            spawn_mock("/v1", MockResponse::immediate(response(content_type, body))).await;
        let transport = connector("cohere", endpoint, ProviderAuthMode::ApiKey).await;
        let mut input = request(operation.clone(), mode);
        input.metadata.surface = Surface::Anthropic;
        let ProviderOutput::Events(mut events) = transport.execute(input).await.unwrap() else {
            panic!("Expected generation events");
        };
        let mut done = false;
        while let Some(event) = events.next().await {
            done |= matches!(event.unwrap().kind, Kind::Done);
        }
        assert!(done);
        let captured = String::from_utf8(captured.await.unwrap()).unwrap();
        let wire: serde_json::Value =
            serde_json::from_str(captured.split_once("\r\n\r\n").unwrap().1).unwrap();
        assert!(wire.get("n").is_none());
        assert_eq!(wire["max_tokens"], 64);

        let explicit = crate::protocols::openai::chat::decode::chat_completion(
            serde_json::from_value(serde_json::json!({"model":"profile-model","n":1,
                "messages":[{"role":"user","content":"hello"}],
                "stream":mode == TransportMode::Streaming}))
            .unwrap(),
        )
        .unwrap();
        let Operation::Generation(mut parallel) = operation else {
            unreachable!()
        };
        parallel.parameters.parallel_tool_calls = Some(false);
        for unsupported in [explicit, Operation::Generation(parallel)] {
            let error = transport
                .execute(request(unsupported, mode))
                .await
                .err()
                .unwrap();
            assert_eq!(error.class, AttemptFailureClass::Protocol);
            assert!(error.message.contains("Cohere does not support"));
        }
    }
}

#[tokio::test]
async fn voyage_and_cohere_embeddings_keep_dimensions_and_usage_contracts() {
    for vendor in ["voyage", "cohere"] {
        let usage = if vendor == "voyage" {
            serde_json::json!({"total_tokens":5})
        } else {
            serde_json::json!({"prompt_tokens":5,"total_tokens":5})
        };
        let vector = vec![0.25; 512];
        let body=serde_json::to_vec(&serde_json::json!({"object":"list","model":"profile-model","data":[{"object":"embedding","index":0,"embedding":vector}],"usage":usage})).unwrap();
        let (endpoint, captured) = spawn_mock(
            "/v1",
            MockResponse::immediate(response("application/json", body)),
        )
        .await;
        let operation = Operation::Embeddings(EmbeddingsRequest {
            route: RouteSlug::parse("embeddings").unwrap(),
            input: vec![EmbeddingInput::Text("hello".into())],
            dimensions: (vendor == "voyage").then_some(512),
            extensions: SourceExtensions::default(),
        });
        let output = connector(vendor, endpoint, ProviderAuthMode::ApiKey)
            .await
            .execute(request(operation, TransportMode::Unary))
            .await
            .unwrap();
        let ProviderOutput::Result(result) = output else {
            panic!("expected embeddings");
        };
        let CanonicalResult::Embeddings(embeddings) = *result else {
            panic!("expected embeddings result");
        };
        assert_eq!(embeddings.data[0].values.len(), 512);
        assert_eq!(embeddings.usage.unwrap().input_tokens, 5);
        let captured = String::from_utf8(captured.await.unwrap()).unwrap();
        let wire: serde_json::Value =
            serde_json::from_str(captured.split_once("\r\n\r\n").unwrap().1).unwrap();
        if vendor == "voyage" {
            assert_eq!(wire["output_dimension"], 512);
            assert_eq!(wire["truncation"], false);
            assert!(wire.get("encoding_format").is_none());
        }
    }
}

#[tokio::test]
async fn custom_authentication_redacts_errors_inside_successful_native_streams() {
    for (kind, surface, wire) in [
        (
            ProviderKind::OpenAiCompatible,
            Surface::OpenAi,
            "data: {\"error\":{\"type\":\"rate_limit_error\",\"message\":\"reflected header-secret\",\"code\":\"header-secret\"}}\n\n",
        ),
        (
            ProviderKind::Anthropic,
            Surface::Anthropic,
            "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"reflected header-secret\",\"details\":\"header-secret\"}}\n\n",
        ),
        (
            ProviderKind::Gemini,
            Surface::Gemini,
            "data: {\"error\":{\"code\":429,\"status\":\"RESOURCE_EXHAUSTED\",\"message\":\"reflected header-secret\"}}\n\n",
        ),
    ] {
        let (endpoint, captured) = spawn_mock(
            "/",
            MockResponse::immediate(response("text/event-stream", wire)),
        )
        .await;
        let mut config = ProviderConfiguration::new(kind);
        config.endpoint = Some(endpoint);
        config.auth_mode = ProviderAuthMode::Headers;
        config
            .options
            .credential_headers
            .insert("x-service-key".into());
        let transport = crate::providers::connectors::transport(
            config,
            Credential::ApiKey(zeroize::Zeroizing::new(
                r#"{"x-service-key":"header-secret"}"#.into(),
            )),
            &crate::net::egress::EgressPolicy::unsafe_test_targets(),
            ResponseLimits::default(),
        )
        .await
        .unwrap();
        let mut request = request(
            generation(TransportMode::Streaming),
            TransportMode::Streaming,
        );
        request.attempt.provider_kind = kind;
        request.metadata.surface = surface;
        let ProviderOutput::Events(mut events) = transport.execute(request).await.unwrap() else {
            panic!("Expected stream")
        };
        let mut found = false;
        while let Some(event) = events.next().await {
            let event = event.unwrap();
            assert!(
                !serde_json::to_string(&event)
                    .unwrap()
                    .contains("header-secret"),
                "{kind}"
            );
            if let Kind::Error { error } = event.kind {
                found = true;
                assert_eq!(
                    error.class,
                    crate::protocols::canonical::events::ErrorClass::RateLimit
                );
                assert!(error.retryable);
                assert_eq!(error.message, "Provider request failed");
                assert_eq!(error.provider_code, None);
            }
        }
        assert!(found, "{kind}");
        assert!(
            String::from_utf8(captured.await.unwrap())
                .unwrap()
                .contains("x-service-key: header-secret")
        );
    }
}

#[tokio::test]
async fn compatible_private_authentication_is_explicit_and_secret_errors_are_redacted() {
    for auth in [ProviderAuthMode::None, ProviderAuthMode::Headers] {
        let (endpoint, captured) = spawn_mock(
            "/v1",
            MockResponse::immediate(response("application/json", json_body())),
        )
        .await;
        let output = connector("deepseek", endpoint, auth)
            .await
            .execute(request(
                generation(TransportMode::Unary),
                TransportMode::Unary,
            ))
            .await
            .unwrap();
        if let ProviderOutput::Events(mut events) = output {
            while let Some(event) = events.next().await {
                event.unwrap();
            }
        }
        let captured = String::from_utf8(captured.await.unwrap())
            .unwrap()
            .to_lowercase();
        assert!(!captured.contains("authorization:"));
        assert!(!captured.contains("olp-custom-authentication"));
        assert_eq!(
            captured.contains("x-service-key: header-secret"),
            auth == ProviderAuthMode::Headers
        );
    }
    let (endpoint, _) = spawn_mock(
        "/v1",
        MockResponse::immediate(status_response(
            "401 Unauthorized",
            "application/json",
            r#"{"error":{"message":"reflected header-secret"}}"#,
        )),
    )
    .await;
    let error = connector("deepseek", endpoint, ProviderAuthMode::Headers)
        .await
        .execute(request(
            generation(TransportMode::Unary),
            TransportMode::Unary,
        ))
        .await
        .err()
        .unwrap();
    assert_eq!(error.class, AttemptFailureClass::UpstreamClient);
    assert_eq!(error.upstream.status, Some(401));
    assert!(!error.message.contains("header-secret"));
}

#[tokio::test]
async fn malformed_and_rate_limited_vendor_responses_keep_failure_classification() {
    for (status, body, expected) in [
        ("200 OK", "not-json", AttemptFailureClass::Protocol),
        (
            "429 Too Many Requests",
            r#"{"error":{"message":"busy"}}"#,
            AttemptFailureClass::RateLimit,
        ),
    ] {
        let (endpoint, _) = spawn_mock(
            "/v1",
            MockResponse::immediate(status_response(status, "application/json", body)),
        )
        .await;
        let error = connector("perplexity", endpoint, ProviderAuthMode::ApiKey)
            .await
            .execute(request(
                generation(TransportMode::Unary),
                TransportMode::Unary,
            ))
            .await
            .err()
            .unwrap();
        assert_eq!(error.class, expected);
    }
}

#[tokio::test]
async fn pooled_generation_normalizes_usage_before_settling_both_token_quotas() {
    use crate::inference::transport::BoxFuture;
    use crate::limits::admission::{
        LimitBackend, LimitError, LimitLease, LimitRequest, ReloadableLimiter,
    };
    use tokio::sync::mpsc;
    struct Lease {
        scope: String,
        settled: mpsc::UnboundedSender<(String, i64)>,
    }
    impl LimitLease for Lease {
        fn refund(&self) -> BoxFuture<'_, Result<(), LimitError>> {
            Box::pin(async { panic!("Dispatched requests must not refund admission") })
        }
        fn reconcile(&self, tokens: i64) -> BoxFuture<'_, Result<(), LimitError>> {
            Box::pin(async move {
                self.settled.send((self.scope.clone(), tokens)).unwrap();
                Ok(())
            })
        }
        fn release(&self) -> BoxFuture<'_, Result<(), LimitError>> {
            Box::pin(async { Ok(()) })
        }
    }
    struct Backend(mpsc::UnboundedSender<(String, i64)>);
    impl LimitBackend for Backend {
        fn reserve<'a>(
            &'a self,
            request: LimitRequest<'a>,
        ) -> BoxFuture<'a, Result<Arc<dyn LimitLease>, LimitError>> {
            Box::pin(async move {
                Ok(Arc::new(Lease {
                    scope: request.lookup_id.into(),
                    settled: self.0.clone(),
                }) as Arc<dyn LimitLease>)
            })
        }
        fn ping(&self) -> BoxFuture<'_, Result<(), LimitError>> {
            Box::pin(async { Ok(()) })
        }
    }
    for mode in [TransportMode::Unary, TransportMode::Streaming] {
        // total_tokens is optional on compatible responses. Completion tokens
        // include reasoning, which the canonical decoder makes disjoint.
        let usage = serde_json::json!({"prompt_tokens":4,"completion_tokens":5,
            "completion_tokens_details":{"reasoning_tokens":3}});
        let (content_type, body) = if mode == TransportMode::Unary {
            let mut body: serde_json::Value = serde_json::from_slice(&json_body()).unwrap();
            body["usage"] = usage;
            ("application/json", serde_json::to_vec(&body).unwrap())
        } else {
            let chunk = serde_json::json!({"id":"profile-response",
                "object":"chat.completion.chunk","created":1,"model":"profile-model",
                "choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},
                    "finish_reason":"stop"}],"usage":usage});
            (
                "text/event-stream",
                format!("data: {chunk}\n\ndata: [DONE]\n\n").into_bytes(),
            )
        };
        let (endpoint, _) =
            spawn_mock("/v1", MockResponse::immediate(response(content_type, body))).await;
        let (settled, mut received) = mpsc::unbounded_channel();
        let limiter = ReloadableLimiter::default();
        limiter.install(Backend(settled));
        let slot = crate::providers::pool::CredentialSlot {
            id: uuid::Uuid::now_v7(),
            tokens_per_minute: Some(1000),
            ..Default::default()
        };
        let mut request = request(generation(mode), mode);
        request.attempt.credential_slot_id = Some(slot.id);
        let expected_scopes = std::collections::BTreeSet::from([
            crate::providers::pool_transport::connection_lookup(
                request.attempt.provider_id.as_uuid(),
            ),
            crate::providers::pool_transport::slot_lookup(slot.id),
        ]);
        let transport = crate::providers::pool_transport::PoolTransport {
            transports: std::collections::BTreeMap::from([(
                None,
                connector("deepseek", endpoint, ProviderAuthMode::ApiKey).await,
            )]),
            default: None,
            slots: vec![slot],
            options: crate::providers::options::ConnectionOptions {
                limits: Some(crate::providers::options::ConnectionLimits {
                    tokens_per_minute: Some(1000),
                    ..Default::default()
                }),
                ..Default::default()
            },
            limiter,
        };
        let ProviderOutput::Events(mut events) = transport.execute(request).await.unwrap() else {
            panic!("Generation must return events");
        };
        while let Some(event) = events.next().await {
            event.unwrap();
        }
        let mut scopes = std::collections::BTreeSet::new();
        for _ in 0..2 {
            let (scope, tokens) =
                tokio::time::timeout(std::time::Duration::from_secs(1), received.recv())
                    .await
                    .unwrap()
                    .unwrap();
            assert_eq!(tokens, 9, "{mode}");
            scopes.insert(scope);
        }
        assert_eq!(scopes, expected_scopes);
    }
}

#[tokio::test]
async fn cancellation_between_quota_reservations_refunds_before_dispatch() {
    use crate::inference::transport::{BoxFuture, TransportError};
    use crate::limits::admission::{
        LimitBackend, LimitError, LimitLease, LimitRequest, ReloadableLimiter,
    };
    use std::sync::atomic::{AtomicUsize, Ordering};
    use tokio::sync::Notify;
    struct Lease {
        refunds: AtomicUsize,
        releases: AtomicUsize,
        released: Notify,
    }
    impl LimitLease for Lease {
        fn refund(&self) -> BoxFuture<'_, Result<(), LimitError>> {
            self.refunds.fetch_add(1, Ordering::SeqCst);
            Box::pin(async { Ok(()) })
        }
        fn reconcile(&self, _: i64) -> BoxFuture<'_, Result<(), LimitError>> {
            Box::pin(async { Ok(()) })
        }
        fn release(&self) -> BoxFuture<'_, Result<(), LimitError>> {
            self.releases.fetch_add(1, Ordering::SeqCst);
            self.released.notify_one();
            Box::pin(async { Ok(()) })
        }
    }
    struct Backend {
        calls: Arc<AtomicUsize>,
        entered: Arc<Notify>,
        lease: Arc<Lease>,
    }
    impl LimitBackend for Backend {
        fn reserve<'a>(
            &'a self,
            _: LimitRequest<'a>,
        ) -> BoxFuture<'a, Result<Arc<dyn LimitLease>, LimitError>> {
            let ordinal = self.calls.fetch_add(1, Ordering::SeqCst);
            Box::pin(async move {
                if ordinal == 0 {
                    Ok(self.lease.clone() as Arc<dyn LimitLease>)
                } else {
                    self.entered.notify_one();
                    futures::future::pending().await
                }
            })
        }
        fn ping(&self) -> BoxFuture<'_, Result<(), LimitError>> {
            Box::pin(async { Ok(()) })
        }
    }
    struct Upstream(Arc<AtomicUsize>);
    impl ProviderTransport for Upstream {
        fn execute(
            &self,
            _: ProviderRequest,
        ) -> BoxFuture<'_, Result<ProviderOutput, TransportError>> {
            self.0.fetch_add(1, Ordering::SeqCst);
            Box::pin(async { panic!("Admission must finish before dispatch") })
        }
    }
    let calls = Arc::new(AtomicUsize::new(0));
    let upstream_calls = Arc::new(AtomicUsize::new(0));
    let entered = Arc::new(Notify::new());
    let lease = Arc::new(Lease {
        refunds: AtomicUsize::new(0),
        releases: AtomicUsize::new(0),
        released: Notify::new(),
    });
    let limiter = ReloadableLimiter::default();
    limiter.install(Backend {
        calls: calls.clone(),
        entered: entered.clone(),
        lease: lease.clone(),
    });
    let slot = crate::providers::pool::CredentialSlot {
        id: uuid::Uuid::now_v7(),
        name: "slot".into(),
        requests_per_minute: Some(1),
        ..Default::default()
    };
    let mut request = request(generation(TransportMode::Unary), TransportMode::Unary);
    request.attempt.credential_slot_id = Some(slot.id);
    let transport = crate::providers::pool_transport::PoolTransport {
        transports: std::collections::BTreeMap::from([(
            None,
            Arc::new(Upstream(upstream_calls.clone())) as Arc<dyn ProviderTransport>,
        )]),
        default: None,
        slots: vec![slot],
        limiter,
        options: crate::providers::options::ConnectionOptions {
            limits: Some(crate::providers::options::ConnectionLimits {
                requests_per_minute: Some(1),
                tokens_per_minute: Some(10_000),
                max_concurrency: Some(1),
            }),
            ..Default::default()
        },
    };
    let task = tokio::spawn(async move { transport.execute(request).await });
    tokio::time::timeout(std::time::Duration::from_secs(1), entered.notified())
        .await
        .unwrap();
    task.abort();
    assert!(task.await.unwrap_err().is_cancelled());
    tokio::time::timeout(std::time::Duration::from_secs(1), lease.released.notified())
        .await
        .unwrap();
    assert_eq!(calls.load(Ordering::SeqCst), 2);
    assert_eq!(upstream_calls.load(Ordering::SeqCst), 0);
    assert_eq!(lease.refunds.load(Ordering::SeqCst), 1);
    assert_eq!(lease.releases.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn cancelling_a_pooled_stream_releases_both_concurrency_scopes() {
    use crate::inference::transport::{BoxFuture, TransportError};
    use crate::limits::admission::{
        LimitBackend, LimitError, LimitLease, LimitRequest, ReloadableLimiter,
    };
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
    struct Lease {
        active: Arc<AtomicUsize>,
        released: AtomicBool,
    }
    impl LimitLease for Lease {
        fn refund(&self) -> BoxFuture<'_, Result<(), LimitError>> {
            Box::pin(async { panic!("This fixture does not refund provider admission") })
        }

        fn reconcile(&self, _: i64) -> BoxFuture<'_, Result<(), LimitError>> {
            Box::pin(async { Ok(()) })
        }
        fn release(&self) -> BoxFuture<'_, Result<(), LimitError>> {
            Box::pin(async move {
                if !self.released.swap(true, Ordering::SeqCst) {
                    self.active.fetch_sub(1, Ordering::SeqCst);
                }
                Ok(())
            })
        }
    }
    struct Backend(Arc<AtomicUsize>);
    impl LimitBackend for Backend {
        fn reserve<'a>(
            &'a self,
            _: LimitRequest<'a>,
        ) -> BoxFuture<'a, Result<Arc<dyn LimitLease>, LimitError>> {
            Box::pin(async move {
                self.0.fetch_add(1, Ordering::SeqCst);
                Ok(Arc::new(Lease {
                    active: self.0.clone(),
                    released: AtomicBool::new(false),
                }) as Arc<dyn LimitLease>)
            })
        }
        fn ping(&self) -> BoxFuture<'_, Result<(), LimitError>> {
            Box::pin(async { Ok(()) })
        }
    }
    struct Pending;
    impl ProviderTransport for Pending {
        fn execute(
            &self,
            _: ProviderRequest,
        ) -> BoxFuture<'_, Result<ProviderOutput, TransportError>> {
            Box::pin(async { Ok(ProviderOutput::Events(Box::pin(futures::stream::pending()))) })
        }
    }
    let active = Arc::new(AtomicUsize::new(0));
    let limiter = ReloadableLimiter::default();
    limiter.install(Backend(active.clone()));
    let slot = uuid::Uuid::now_v7();
    let version = uuid::Uuid::now_v7();
    let transport = crate::providers::pool_transport::PoolTransport {
        transports: std::collections::BTreeMap::from([(
            Some(version),
            Arc::new(Pending) as Arc<dyn ProviderTransport>,
        )]),
        default: Some(version),
        slots: vec![crate::providers::pool::CredentialSlot {
            id: slot,
            name: "stream".into(),
            credential_version_id: Some(version),
            max_concurrency: Some(1),
            ..Default::default()
        }],
        options: crate::providers::options::ConnectionOptions {
            limits: Some(crate::providers::options::ConnectionLimits {
                max_concurrency: Some(1),
                ..Default::default()
            }),
            ..Default::default()
        },
        limiter,
    };
    let mut request = request(
        generation(TransportMode::Streaming),
        TransportMode::Streaming,
    );
    request.attempt.credential_slot_id = Some(slot);
    request.attempt.credential_version_id = Some(version);
    let output = transport.execute(request).await.unwrap();
    assert_eq!(active.load(Ordering::SeqCst), 2);
    drop(output);
    tokio::time::timeout(std::time::Duration::from_secs(1), async {
        while active.load(Ordering::SeqCst) != 0 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
}

#[test]
fn responses_specific_semantics_are_not_forwarded_as_ignored_chat_fields() {
    let options = crate::providers::options::ConnectionOptions {
        vendor_id: Some("huggingface".into()),
        ..Default::default()
    };
    let mut operation = generation(TransportMode::Unary);
    if let Operation::Generation(request) = &mut operation {
        request.extensions.values.insert(
            crate::protocols::canonical::requests::OPENAI_ENDPOINT_EXTENSION.into(),
            "responses".into(),
        );
        request
            .extensions
            .values
            .insert("/previous_response_id".into(), "prior-response".into());
    }
    assert!(crate::providers::profiles::validate(&operation, &options).is_err());
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn current_attempt_quotas_override_unlimited_retained_transports() {
    use crate::inference::transport::{BoxFuture, TransportError};
    use crate::limits::admission::{LimitBackend, ReloadableLimiter};
    use crate::limits::distributed::DistributedLimiter;
    use crate::providers::options::ConnectionLimits;
    use std::sync::atomic::{AtomicUsize, Ordering};
    struct Upstream(Arc<AtomicUsize>);
    impl ProviderTransport for Upstream {
        fn execute(
            &self,
            _: ProviderRequest,
        ) -> BoxFuture<'_, Result<ProviderOutput, TransportError>> {
            self.0.fetch_add(1, Ordering::SeqCst);
            Box::pin(async { Ok(ProviderOutput::Events(Box::pin(futures::stream::empty()))) })
        }
    }
    for credential_scope in [false, true] {
        wait_for_rate_window().await;
        let backend = DistributedLimiter::connect(
            &std::env::var("OLP_VALKEY_URL").unwrap(),
            format!("olp:test:current_quota:{}", uuid::Uuid::now_v7()),
        )
        .await
        .unwrap();
        let limiter = ReloadableLimiter::default();
        limiter.install(backend.clone());
        let calls = Arc::new(AtomicUsize::new(0));
        let slot = crate::providers::pool::CredentialSlot {
            id: uuid::Uuid::now_v7(),
            name: "retained".into(),
            ..Default::default()
        };
        let transport = crate::providers::pool_transport::PoolTransport {
            transports: std::collections::BTreeMap::from([(
                None,
                Arc::new(Upstream(calls.clone())) as Arc<dyn ProviderTransport>,
            )]),
            default: None,
            slots: vec![slot.clone()],
            options: Default::default(),
            limiter,
        };
        let mut first = request(generation(TransportMode::Unary), TransportMode::Unary);
        first.attempt.credential_slot_id = Some(slot.id);
        let limits = ConnectionLimits {
            requests_per_minute: Some(1),
            ..Default::default()
        };
        if credential_scope {
            first.attempt.credential_limits = Some(limits);
        } else {
            first.attempt.connection_limits = Some(limits);
        }
        let mut second = request(generation(TransportMode::Unary), TransportMode::Unary);
        second.attempt = first.attempt.clone();
        let provider = first.attempt.provider_id;
        assert!(transport.execute(first).await.is_ok());
        assert!(transport.execute(second).await.is_err());
        assert_eq!(calls.load(Ordering::SeqCst), 1);
        let (id, prefix) = if credential_scope {
            (slot.id, "ps")
        } else {
            (provider.as_uuid(), "pc")
        };
        let usage =
            LimitBackend::provider_usage(&backend, id, &format!("{prefix}_{}", id.simple()))
                .await
                .unwrap()
                .unwrap();
        assert_eq!(usage.requests_this_minute, 1);
    }
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn rejected_slot_admission_refunds_connection_capacity_before_fallback() {
    wait_for_rate_window().await;
    use crate::inference::transport::{BoxFuture, TransportError};
    use crate::limits::admission::{LimitBackend, LimitRequest, ReloadableLimiter};
    use crate::limits::distributed::DistributedLimiter;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::time::Duration;
    struct Upstream(Arc<AtomicUsize>);
    impl ProviderTransport for Upstream {
        fn execute(
            &self,
            _: ProviderRequest,
        ) -> BoxFuture<'_, Result<ProviderOutput, TransportError>> {
            self.0.fetch_add(1, Ordering::SeqCst);
            Box::pin(async { Ok(ProviderOutput::Events(Box::pin(futures::stream::empty()))) })
        }
    }
    let backend = DistributedLimiter::connect(
        &std::env::var("OLP_VALKEY_URL").unwrap(),
        format!("olp:test:pool_refund:{}", uuid::Uuid::now_v7()),
    )
    .await
    .unwrap();
    let limiter = ReloadableLimiter::default();
    limiter.install(backend.clone());
    let calls = Arc::new(AtomicUsize::new(0));
    let exhausted = crate::providers::pool::CredentialSlot {
        id: uuid::Uuid::now_v7(),
        name: "exhausted".into(),
        requests_per_minute: Some(1),
        ..Default::default()
    };
    let available = crate::providers::pool::CredentialSlot {
        id: uuid::Uuid::now_v7(),
        name: "available".into(),
        requests_per_minute: Some(1),
        ..Default::default()
    };
    let lookup = crate::providers::pool_transport::slot_lookup(exhausted.id);
    let prior = backend
        .reserve(LimitRequest {
            api_key_id: exhausted.id,
            lookup_id: &lookup,
            requests_per_minute: Some(1),
            tokens_per_minute: None,
            max_concurrency: None,
            daily_cost_limit: None,
            monthly_cost_limit: None,
            requested_tokens: 1,
            lease_ttl: Duration::from_secs(10),
        })
        .await
        .unwrap();
    backend.release(&prior).await.unwrap();
    let transport = crate::providers::pool_transport::PoolTransport {
        transports: std::collections::BTreeMap::from([(
            None,
            Arc::new(Upstream(calls.clone())) as Arc<dyn ProviderTransport>,
        )]),
        default: None,
        slots: vec![exhausted.clone(), available.clone()],
        limiter,
        options: crate::providers::options::ConnectionOptions {
            limits: Some(crate::providers::options::ConnectionLimits {
                requests_per_minute: Some(1),
                tokens_per_minute: Some(1_000),
                max_concurrency: Some(1),
            }),
            parameter_defaults: std::collections::BTreeMap::from([(
                "max_tokens".into(),
                serde_json::json!(64),
            )]),
            ..Default::default()
        },
    };
    let mut operation = generation(TransportMode::Unary);
    let Operation::Generation(generation) = &mut operation else {
        unreachable!()
    };
    generation.parameters.max_output_tokens = None;
    let mut first = request(operation.clone(), TransportMode::Unary);
    first.attempt.provider_kind = ProviderKind::Anthropic;
    first.attempt.credential_slot_id = Some(exhausted.id);
    let provider = first.attempt.provider_id;
    assert!(transport.execute(first).await.is_err());
    assert_eq!(calls.load(Ordering::SeqCst), 0);
    let usage = LimitBackend::provider_usage(
        &backend,
        provider.as_uuid(),
        &crate::providers::pool_transport::connection_lookup(provider.as_uuid()),
    )
    .await
    .unwrap()
    .unwrap();
    assert_eq!(
        (
            usage.requests_this_minute,
            usage.tokens_this_minute,
            usage.concurrent_requests
        ),
        (0, 0, 0)
    );
    let mut fallback = request(operation, TransportMode::Unary);
    fallback.attempt.provider_kind = ProviderKind::Anthropic;
    fallback.attempt.provider_id = provider;
    fallback.attempt.credential_slot_id = Some(available.id);
    assert!(transport.execute(fallback).await.is_ok());
    assert_eq!(calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn streamed_credential_failures_set_the_same_shared_cooldowns_as_http_errors() {
    use crate::inference::transport::{BoxFuture, TransportError, TransportPhase, UpstreamSignal};
    use crate::limits::admission::{
        LimitBackend, LimitError, LimitLease, LimitRequest, ReloadableLimiter,
    };
    use std::sync::Mutex;
    use std::time::Duration;

    type Writes = Arc<Mutex<Vec<(String, Duration)>>>;
    struct Backend(Writes);
    impl LimitBackend for Backend {
        fn reserve<'a>(
            &'a self,
            _: LimitRequest<'a>,
        ) -> BoxFuture<'a, Result<Arc<dyn LimitLease>, LimitError>> {
            Box::pin(async { panic!("This fixture has no quotas") })
        }
        fn ping(&self) -> BoxFuture<'_, Result<(), LimitError>> {
            Box::pin(async { Ok(()) })
        }
        fn provider_cooldown<'a>(
            &'a self,
            scope: &'a str,
            duration: Option<Duration>,
        ) -> BoxFuture<'a, Result<bool, LimitError>> {
            Box::pin(async move {
                if let Some(duration) = duration {
                    self.0.lock().unwrap().push((scope.into(), duration));
                }
                Ok(false)
            })
        }
    }
    struct FailedStream(u16, bool);
    impl ProviderTransport for FailedStream {
        fn execute(
            &self,
            _: ProviderRequest,
        ) -> BoxFuture<'_, Result<ProviderOutput, TransportError>> {
            let status = self.0;
            let canonical = self.1;
            Box::pin(async move {
                Ok(ProviderOutput::Events(Box::pin(futures::stream::once(
                    async move {
                        if canonical {
                            use crate::protocols::canonical::events::{Error, ErrorClass, Event};
                            return Ok(Event::new(
                                0,
                                Kind::Error {
                                    error: Error {
                                        class: if status == 401 {
                                            ErrorClass::Authentication
                                        } else {
                                            ErrorClass::RateLimit
                                        },
                                        message: "fixture".into(),
                                        provider_code: None,
                                        retryable: true,
                                    },
                                },
                            ));
                        }
                        Err(TransportError {
                            phase: TransportPhase::FirstByte,
                            class: if status == 429 {
                                AttemptFailureClass::RateLimit
                            } else {
                                AttemptFailureClass::UpstreamClient
                            },
                            response_committed: false,
                            message: "fixture".into(),
                            upstream: UpstreamSignal {
                                status: Some(status),
                                retry_after: Some(Duration::from_secs(17)),
                            },
                        })
                    },
                ))))
            })
        }
    }
    for (status, canonical) in [(401, false), (429, false), (401, true), (429, true)] {
        let writes = Writes::default();
        let limiter = ReloadableLimiter::default();
        limiter.install(Backend(writes.clone()));
        let slot = uuid::Uuid::now_v7();
        let version = uuid::Uuid::now_v7();
        let transport = crate::providers::pool_transport::PoolTransport {
            transports: std::collections::BTreeMap::from([(
                Some(version),
                Arc::new(FailedStream(status, canonical)) as Arc<dyn ProviderTransport>,
            )]),
            default: Some(version),
            slots: vec![],
            options: Default::default(),
            limiter,
        };
        let mut request = request(
            generation(TransportMode::Streaming),
            TransportMode::Streaming,
        );
        let provider = request.attempt.provider_id;
        request.attempt.credential_slot_id = Some(slot);
        request.attempt.credential_version_id = Some(version);
        let ProviderOutput::Events(mut events) = transport.execute(request).await.unwrap() else {
            panic!("Expected stream")
        };
        assert_eq!(events.next().await.unwrap().is_ok(), canonical);
        let expected = if status == 401 {
            (format!("{provider}:{version}"), Duration::from_secs(86400))
        } else {
            (
                format!("slot:{slot}"),
                Duration::from_secs(if canonical { 30 } else { 17 }),
            )
        };
        assert_eq!(*writes.lock().unwrap(), vec![expected]);
    }
}

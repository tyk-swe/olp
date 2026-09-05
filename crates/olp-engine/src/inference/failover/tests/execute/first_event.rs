use super::*;
use crate::domain::{
    canonical::{
        events::{Error, ErrorClass},
        requests::{ImageGenerationRequest, ImageOperation},
    },
    ids::DurationMs,
};
use crate::inference::failover::reclassify_ambiguous_transport_failure;
use crate::protocols::openai::chat::{CompletionRequest, decode};
use serde_json::json;

#[derive(Clone)]
struct FirstEventPendingTransport {
    calls: Arc<AtomicUsize>,
}

#[derive(Clone)]
struct CountingFiniteTransport {
    calls: Arc<AtomicUsize>,
    events: Vec<Event>,
}

impl ProviderTransport for FirstEventPendingTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        Box::pin(async {
            Ok(ProviderOutput::Events(
                Box::pin(stream::pending()) as ProviderEventStream
            ))
        })
    }
}

impl ProviderTransport for CountingFiniteTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        let events = self.events.clone();
        Box::pin(async move {
            Ok(ProviderOutput::Events(Box::pin(stream::iter(
                events.into_iter().map(Ok),
            ))))
        })
    }
}

fn install_two_target_streams(
    _operation: OperationKind,
    first: Arc<dyn ProviderTransport>,
    second: Arc<dyn ProviderTransport>,
) -> (
    Arc<Bundle>,
    Vec<crate::domain::routing::selection::AttemptPlan>,
) {
    let manager = Manager::empty();
    let mut providers = BTreeMap::new();
    let mut transports = BTreeMap::new();
    let mut attempts = Vec::new();
    for (transport, timeout) in [(first, 20), (second, 100)] {
        let mut attempt = super::super::plan(TargetId::new());
        attempt.provider_id = ProviderId::new();
        attempt.upstream_model = "upstream-model".to_owned();
        attempt.timeout = DurationMs::new(timeout);
        providers.insert(
            attempt.provider_id,
            Provider {
                id: attempt.provider_id,
                revision_id: None,
                name: "first-event".to_owned(),
                kind: ProviderKind::OpenAi,
                enabled: true,
                active_credential: None,
                capabilities: Default::default(),
            },
        );
        transports.insert(attempt.provider_id, transport);
        attempts.push(attempt);
    }
    manager
        .install(
            Snapshot {
                generation: RuntimeGeneration {
                    id: RuntimeGenerationId::new(),
                    ordinal: 1,
                    activated_at: Utc::now(),
                },
                providers,
                routes: Default::default(),
                api_keys: Default::default(),
            },
            transports,
        )
        .unwrap();
    (manager.pin(), attempts)
}

fn streaming_generation_operation() -> Operation {
    let request: CompletionRequest = serde_json::from_value(json!({
        "model": "default",
        "messages": [{"role": "user", "content": "hello"}],
        "stream": true
    }))
    .unwrap();
    decode::chat_completion(request).unwrap()
}

fn streaming_image_generation_operation() -> Operation {
    Operation::Images(ImageOperation::Generation(ImageGenerationRequest {
        route: RouteSlug::parse("default").unwrap(),
        prompt: "draw a test".to_owned(),
        count: Some(1),
        size: None,
        stream: true,
        extensions: SourceExtensions::default(),
    }))
}

fn streaming_request_metadata(operation: OperationKind) -> RequestMetadata {
    RequestMetadata {
        request_id: RequestId::new(),
        operation,
        surface: Surface::OpenAi,
        mode: TransportMode::Streaming,
    }
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
                usage: crate::domain::canonical::events::Usage {
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
#[tokio::test]
async fn first_event_timeout_obeys_media_ambiguity_policy() {
    let media_first_calls = Arc::new(AtomicUsize::new(0));
    let media_second_calls = Arc::new(AtomicUsize::new(0));
    let (runtime, attempts) = install_two_target_streams(
        OperationKind::ImageGeneration,
        Arc::new(FirstEventPendingTransport {
            calls: media_first_calls.clone(),
        }),
        Arc::new(CountingFiniteTransport {
            calls: media_second_calls.clone(),
            events: Vec::new(),
        }),
    );
    let failure = match execute(
        Context {
            runtime: &runtime,
            overall_timeout: Duration::from_millis(200),
            max_attempts: std::num::NonZeroU16::new(2).unwrap(),
            media_spool: Arc::new(UnavailableSpool),
            max_inline_media_bytes: 1024 * 1024,
            circuits: &Breaker::default(),
            on_attempt_started: None,
            trace: None,
        },
        attempts,
        streaming_request_metadata(OperationKind::ImageGeneration),
        streaming_image_generation_operation(),
    )
    .await
    {
        Ok(_) => panic!("a committed media timeout must be terminal"),
        Err(failure) => failure,
    };
    assert_eq!(failure.error.code(), "ambiguous_upstream_result");
    assert_eq!(failure.attempts.len(), 1);
    assert!(failure.attempts[0].committed);
    assert_eq!(media_first_calls.load(Ordering::SeqCst), 1);
    assert_eq!(media_second_calls.load(Ordering::SeqCst), 0);

    let generation_second_calls = Arc::new(AtomicUsize::new(0));
    let (runtime, attempts) = install_two_target_streams(
        OperationKind::Generation,
        Arc::new(FirstEventPendingTransport {
            calls: Arc::new(AtomicUsize::new(0)),
        }),
        Arc::new(CountingFiniteTransport {
            calls: generation_second_calls.clone(),
            events: generation_stream_events("retried"),
        }),
    );
    let success = match execute(
        Context {
            runtime: &runtime,
            overall_timeout: Duration::from_millis(200),
            max_attempts: std::num::NonZeroU16::new(2).unwrap(),
            media_spool: Arc::new(UnavailableSpool),
            max_inline_media_bytes: 1024 * 1024,
            circuits: &Breaker::default(),
            on_attempt_started: None,
            trace: None,
        },
        attempts,
        streaming_request_metadata(OperationKind::Generation),
        streaming_generation_operation(),
    )
    .await
    {
        Ok(success) => success,
        Err(_) => {
            panic!("generation keeps availability-first failover after a first-event timeout")
        }
    };
    assert_eq!(success.attempts.len(), 2);
    assert_eq!(generation_second_calls.load(Ordering::SeqCst), 1);
}

#[test]
fn post_connect_failure_obeys_media_ambiguity_policy() {
    let failure = TransportError {
        upstream: Default::default(),
        phase: crate::domain::ports::TransportPhase::FirstByte,
        class: AttemptFailureClass::Connect,
        response_committed: false,
        message: "connection closed before response headers".to_owned(),
    };

    let media =
        reclassify_ambiguous_transport_failure(failure.clone(), OperationKind::ImageGeneration);
    assert_eq!(media.class, AttemptFailureClass::Ambiguous);
    assert!(media.response_committed);
    assert!(!media.allows_failover());

    let generation = reclassify_ambiguous_transport_failure(failure, OperationKind::Generation);
    assert_eq!(generation.class, AttemptFailureClass::Connect);
    assert!(!generation.response_committed);
    assert!(generation.allows_failover());

    let connect = reclassify_ambiguous_transport_failure(
        TransportError {
            upstream: Default::default(),
            phase: crate::domain::ports::TransportPhase::Connect,
            class: AttemptFailureClass::Connect,
            response_committed: false,
            message: "connection failed".to_owned(),
        },
        OperationKind::ImageGeneration,
    );
    assert_eq!(connect.class, AttemptFailureClass::Connect);
    assert!(!connect.response_committed);
    assert!(connect.allows_failover());
}

#[tokio::test]
async fn retryable_first_canonical_error_fails_over_before_commit() {
    let second_calls = Arc::new(AtomicUsize::new(0));
    let (runtime, attempts) = install_two_target_streams(
        OperationKind::Generation,
        Arc::new(CountingFiniteTransport {
            calls: Arc::new(AtomicUsize::new(0)),
            events: vec![
                Event::new(
                    0,
                    Kind::Error {
                        error: Error {
                            class: ErrorClass::RateLimit,
                            message: "provider throttled the request".to_owned(),
                            provider_code: Some("rate_limit".to_owned()),
                            retryable: true,
                        },
                    },
                ),
                Event::new(1, Kind::Done),
            ],
        }),
        Arc::new(CountingFiniteTransport {
            calls: second_calls.clone(),
            events: generation_stream_events("recovered"),
        }),
    );
    let success = match execute(
        Context {
            runtime: &runtime,
            overall_timeout: Duration::from_millis(200),
            max_attempts: std::num::NonZeroU16::new(2).unwrap(),
            media_spool: Arc::new(UnavailableSpool),
            max_inline_media_bytes: 1024 * 1024,
            circuits: &Breaker::default(),
            on_attempt_started: None,
            trace: None,
        },
        attempts,
        streaming_request_metadata(OperationKind::Generation),
        streaming_generation_operation(),
    )
    .await
    {
        Ok(success) => success,
        Err(_) => panic!("retryable pre-commit canonical error must use the next target"),
    };
    assert_eq!(success.attempts.len(), 2);
    assert_eq!(
        success.attempts[0].error_class.as_deref(),
        Some("rate_limit")
    );
    assert!(!success.attempts[0].committed);
    assert_eq!(second_calls.load(Ordering::SeqCst), 1);
}

use super::*;

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
    let circuits = CircuitBreaker::default();
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

fn install_event_stream(
    state: &GatewayState,
    operation: OperationKind,
    events: Vec<CanonicalEvent>,
    finite: bool,
) {
    let pinned = state.runtime().pin();
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
        .runtime()
        .install(snapshot, BTreeMap::from([(provider_id, transport)]))
        .unwrap();
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
async fn chat_and_responses_stream_through_the_real_router_with_native_usage() {
    let (mut state, key) = test_state(true);
    let (emitter, mut request_metadata) =
        olp_storage::request_metadata::RequestMetadataEmitter::bounded(8);
    state.replace_request_metadata_for_test(emitter);

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
    let (emitter, mut request_metadata) =
        olp_storage::request_metadata::RequestMetadataEmitter::bounded(2);
    state.replace_request_metadata_for_test(emitter);
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
    let (emitter, mut request_metadata) =
        olp_storage::request_metadata::RequestMetadataEmitter::bounded(8);
    state.replace_request_metadata_for_test(emitter);

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
async fn dropping_client_stream_drops_upstream_within_one_second() {
    let (state, key) = test_state(true);
    let dropped = Arc::new(AtomicBool::new(false));
    install_transport(
        &state,
        Arc::new(DropAwareTransport {
            dropped: dropped.clone(),
        }),
    );
    let response = crate::public_http::router::gateway_router_for_test(state)
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

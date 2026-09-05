use super::*;

struct DropAwareStream {
    first: Option<Event>,
    dropped: Arc<AtomicBool>,
}

impl futures::Stream for DropAwareStream {
    type Item = Result<Event, TransportError>;

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
                first: Some(Event::new(
                    0,
                    Kind::TextDelta {
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
    events: Vec<Event>,
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

fn raw_media_event(sequence: u64, event: &str, data: Value) -> Event {
    Event::new(
        sequence,
        Kind::SourceExtension {
            extensions: olp_engine::domain::canonical::requests::SourceExtensions::new(
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
        olp_engine::inference::request_metadata::Emitter::bounded(8);
    state.replace_request_metadata_for_test(emitter);

    install_event_stream(
        &state,
        OperationKind::Generation,
        generation_stream_events("chat stream"),
        false,
    );
    // Without `stream_options.include_usage` OpenAI sends no usage chunk, and a
    // `chunk.choices[0].delta` loop must not hit an empty-choices chunk.
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
    assert!(!body.contains("\"prompt_tokens\""));
    assert!(!body.contains("\"choices\":[]"));
    assert!(body.ends_with("data: [DONE]\n\n"));
    // Accounting still sees the upstream usage: it is always requested there.
    let event = request_metadata.recv_next().await.unwrap();
    assert_eq!(event.operation, OperationKind::Generation);
    assert_eq!(event.input_tokens, Some(7));
    // The fixture reports 3 output + 1 reasoning token; accounting meters the
    // reasoning-inclusive output count providers bill for.
    assert_eq!(event.output_tokens, Some(4));
    assert!(event.usage_complete);

    install_event_stream(
        &state,
        OperationKind::Generation,
        generation_stream_events("usage stream"),
        false,
    );
    let response = post_json(
        &state,
        &key,
        "/openai/v1/chat/completions",
        r#"{"model":"default","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}"#,
    )
    .await;
    assert_eq!(response.status(), StatusCode::OK);
    let body = response_text(response).await;
    assert!(body.contains("\"prompt_tokens\":7"));
    assert!(body.ends_with("data: [DONE]\n\n"));
    request_metadata.recv_next().await.unwrap();

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

/// A failure the gateway knows about before writing a byte must reach the
/// client as a real HTTP status. Committing `200 OK text/event-stream` and then
/// describing the 429 inside the body defeated every status-driven retry in an
/// SDK, load balancer, or proxy — while the unary path on the identical failure
/// already answered 429.
#[tokio::test]
async fn canonical_stream_error_is_not_persisted_as_success() {
    let (mut state, key) = test_state(true);
    let (emitter, mut request_metadata) =
        olp_engine::inference::request_metadata::Emitter::bounded(2);
    state.replace_request_metadata_for_test(emitter);
    install_event_stream(
        &state,
        OperationKind::Generation,
        vec![
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
        true,
    );

    let response = post_json(
        &state,
        &key,
        "/openai/v1/responses",
        r#"{"model":"default","input":"hi","stream":true}"#,
    )
    .await;
    assert_eq!(response.status(), StatusCode::TOO_MANY_REQUESTS);
    assert_ne!(
        response.headers()[header::CONTENT_TYPE],
        "text/event-stream; charset=utf-8"
    );
    let body = response_text(response).await;
    assert!(
        body.contains("error") || body.contains("failed"),
        "error body was {body:?}"
    );
    let event = request_metadata.recv_next().await.unwrap();
    assert_eq!(event.status_code, Some(429));
    assert_ne!(event.error_class.as_deref(), None);
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
        olp_engine::inference::request_metadata::Emitter::bounded(8);
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
            Event::new(2, Kind::Done),
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
            Event::new(2, Kind::Done),
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
            Event::new(2, Kind::Done),
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

/// A27: real Gemini frames `:streamGenerateContent` as a streamed JSON array
/// unless `alt=sse` is given. Serving `text/event-stream` for the default left
/// an official-SDK client unable to parse the body.
#[tokio::test]
async fn stream_generate_content_requires_alt_sse() {
    let (state, key) = test_state(true);
    install_event_stream(
        &state,
        OperationKind::Generation,
        generation_stream_events("gemini stream"),
        false,
    );

    let gemini = |path: &'static str| {
        let state = state.clone();
        let key = key.clone();
        async move {
            crate::public_http::router::gateway_router_for_test(state)
                .oneshot(
                    Request::post(path)
                        .header("x-goog-api-key", key)
                        .header(header::CONTENT_TYPE, "application/json")
                        .body(Body::from(
                            r#"{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}"#,
                        ))
                        .unwrap(),
                )
                .await
                .unwrap()
        }
    };

    let response = gemini("/gemini/v1beta/models/default:streamGenerateContent").await;
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    assert_ne!(
        response.headers()[header::CONTENT_TYPE],
        "text/event-stream; charset=utf-8"
    );
    assert!(response_text(response).await.contains("alt=sse"));

    // With `alt=sse` the handler proceeds past the framing check and the
    // request is routed like any other.
    let response = gemini("/gemini/v1beta/models/default:streamGenerateContent?alt=sse").await;
    assert!(!response_text(response).await.contains("alt=sse"));
}

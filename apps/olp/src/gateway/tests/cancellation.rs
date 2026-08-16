use super::*;

struct PendingTransport;

struct NotifyingPendingTransport {
    started: Arc<tokio::sync::Notify>,
}

#[derive(Clone)]
struct CollectingPendingTransport {
    reached_pending: Arc<tokio::sync::Notify>,
    events: Vec<Event>,
}

struct BlockingRemoveSpool {
    inner: Arc<dyn MediaSpool>,
    remove_started: Arc<tokio::sync::Notify>,
}

impl MediaSpool for BlockingRemoveSpool {
    fn capacity_bytes(&self) -> Option<u64> {
        self.inner.capacity_bytes()
    }

    fn put<'a>(
        &'a self,
        upload: olp_engine::domain::ports::MediaUpload,
    ) -> BoxFuture<
        'a,
        Result<
            olp_engine::domain::canonical::results::MediaArtifact,
            olp_engine::domain::ports::MediaSpoolError,
        >,
    > {
        self.inner.put(upload)
    }

    fn open<'a>(
        &'a self,
        handle: &'a MediaHandle,
    ) -> BoxFuture<
        'a,
        Result<olp_engine::domain::ports::OpenedMedia, olp_engine::domain::ports::MediaSpoolError>,
    > {
        self.inner.open(handle)
    }

    fn remove<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> BoxFuture<'a, Result<(), olp_engine::domain::ports::MediaSpoolError>> {
        let remove_started = self.remove_started.clone();
        Box::pin(async move {
            remove_started.notify_one();
            std::future::pending().await
        })
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

impl ProviderTransport for NotifyingPendingTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        self.started.notify_one();
        Box::pin(std::future::pending())
    }
}

impl ProviderTransport for CollectingPendingTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        let events = self.events.clone();
        let reached_pending = self.reached_pending.clone();
        Box::pin(async move {
            let pending = stream::once(async move {
                reached_pending.notify_one();
                std::future::pending::<Result<Event, TransportError>>().await
            });
            Ok(ProviderOutput::Events(Box::pin(
                stream::iter(events.into_iter().map(Ok)).chain(pending),
            )))
        })
    }
}

impl ProviderTransport for CapturingPendingTransport {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        if let Operation::TokenCount(operation) = &request.operation
            && let Some(handle) = operation.input.iter().find_map(|part| match part {
                olp_engine::domain::canonical::requests::ContentPart::InputAudio {
                    media, ..
                }
                | olp_engine::domain::canonical::requests::ContentPart::InputFile {
                    media, ..
                } => Some(media.clone()),
                _ => None,
            })
        {
            let _ = self.captured.send(handle);
        }
        Box::pin(std::future::pending())
    }
}

#[tokio::test]
async fn cancelling_response_input_tokens_handler_cleans_admitted_media() {
    let (state, key) = test_state(false);
    install_result(
        &state,
        OperationKind::TokenCount,
        CanonicalResult::TokenCount(olp_engine::domain::canonical::results::TokenCountResult {
            input_tokens: 1,
            extensions: olp_engine::domain::canonical::requests::SourceExtensions::default(),
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
    assert!(state.media_spool().open(&handle).await.is_ok());

    task.abort();
    let _ = task.await;
    tokio::time::timeout(Duration::from_secs(1), async {
        loop {
            match state.media_spool().open(&handle).await {
                Err(olp_engine::domain::ports::MediaSpoolError::NotFound) => break,
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
        .media_spool()
        .put(olp_engine::domain::ports::MediaUpload {
            filename: "inline.wav".into(),
            content_type: Some("audio/wav".into()),
            maximum_length: 16,
            bytes: Box::pin(stream::once(async { Ok(Bytes::from_static(b"audio")) })),
        })
        .await
        .unwrap();
    install_transport(&state, Arc::new(PendingTransport));
    let request: CompletionRequest = serde_json::from_value(json!({
        "model":"default","messages":[{"role":"user","content":"hello"}]
    }))
    .unwrap();
    let mut operation = decode_chat_completion(request).unwrap();
    let Operation::Generation(generation) = &mut operation else {
        unreachable!()
    };
    generation.messages[0].content = vec![
        olp_engine::domain::canonical::requests::ContentPart::InputAudio {
            media: artifact.handle.clone(),
            format: "wav".into(),
        },
    ];

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
            match state.media_spool().open(&artifact.handle).await {
                Err(olp_engine::domain::ports::MediaSpoolError::NotFound) => break,
                Ok(_) => tokio::task::yield_now().await,
                Err(error) => panic!("unexpected media cleanup error: {error}"),
            }
        }
    })
    .await
    .expect("request media guard must schedule cleanup when its future is dropped");
}

#[tokio::test]
async fn cancelling_unary_collection_emits_partial_usage_as_client_cancelled() {
    let (mut state, key) = test_state(false);
    let (emitter, mut request_metadata) =
        olp_engine::inference::request_metadata::Emitter::bounded(2);
    state.replace_request_metadata_for_test(emitter);
    let reached_pending = Arc::new(tokio::sync::Notify::new());
    install_transport(
        &state,
        Arc::new(CollectingPendingTransport {
            reached_pending: reached_pending.clone(),
            events: vec![
                Event::new(
                    0,
                    Kind::ResponseStart {
                        response_id: Some("chatcmpl-cancelled".to_owned()),
                        provider_model: Some("upstream-model".to_owned()),
                    },
                ),
                Event::new(
                    1,
                    Kind::Usage {
                        usage: olp_engine::domain::canonical::events::Usage {
                            input_tokens: 7,
                            output_tokens: 3,
                            total_tokens: 10,
                            cached_input_tokens: Some(2),
                            reasoning_tokens: None,
                        },
                    },
                ),
            ],
        }),
    );
    let state_for_task = state.clone();
    let task = tokio::spawn(async move {
        post_json(
            &state_for_task,
            &key,
            "/openai/v1/chat/completions",
            r#"{"model":"default","messages":[{"role":"user","content":"hi"}]}"#,
        )
        .await
    });
    tokio::time::timeout(Duration::from_secs(1), reached_pending.notified())
        .await
        .expect("unary collection must consume the usage event before stalling");
    task.abort();
    let _ = task.await;

    let event = tokio::time::timeout(Duration::from_secs(1), request_metadata.recv_next())
        .await
        .expect("cancellation must emit request metadata")
        .expect("metadata channel must remain open");
    assert_eq!(event.status_code, None);
    assert_eq!(event.error_class.as_deref(), Some("client_cancelled"));
    assert_eq!(event.input_tokens, Some(7));
    assert_eq!(event.output_tokens, Some(3));
    assert_eq!(event.cached_input_tokens, Some(2));
    assert!(event.usage_complete);
    assert!(event.committed);
}

#[tokio::test]
async fn cancelling_during_transport_wait_records_the_active_attempt() {
    let (mut state, key) = test_state(false);
    let (emitter, mut request_metadata) =
        olp_engine::inference::request_metadata::Emitter::bounded(2);
    state.replace_request_metadata_for_test(emitter);
    let started = Arc::new(tokio::sync::Notify::new());
    install_transport(
        &state,
        Arc::new(NotifyingPendingTransport {
            started: started.clone(),
        }),
    );
    let state_for_task = state.clone();
    let task = tokio::spawn(async move {
        post_json(
            &state_for_task,
            &key,
            "/openai/v1/chat/completions",
            r#"{"model":"default","messages":[{"role":"user","content":"hi"}]}"#,
        )
        .await
    });
    tokio::time::timeout(Duration::from_secs(1), started.notified())
        .await
        .expect("request must start its provider attempt");
    task.abort();
    let _ = task.await;

    let event = tokio::time::timeout(Duration::from_secs(1), request_metadata.recv_next())
        .await
        .expect("cancellation must emit request metadata")
        .expect("metadata channel must remain open");
    assert_eq!(event.status_code, None);
    assert_eq!(event.error_class.as_deref(), Some("client_cancelled"));
    assert_eq!(event.attempts.len(), 1);
    assert_eq!(event.attempts[0].ordinal, 1);
    assert_eq!(event.attempts[0].upstream_model, "upstream-model");
    assert_eq!(event.attempts[0].error_class.as_deref(), Some("cancelled"));
    assert_eq!(event.attempts[0].status_code, None);
    assert!(!event.attempts[0].committed);
}

#[tokio::test]
async fn cancelling_media_cleanup_preserves_the_completed_attempt_outcome() {
    let (mut state, key) = test_state(false);
    let (emitter, mut request_metadata) =
        olp_engine::inference::request_metadata::Emitter::bounded(2);
    state.replace_request_metadata_for_test(emitter);
    let remove_started = Arc::new(tokio::sync::Notify::new());
    state.replace_media_spool_for_test(Arc::new(BlockingRemoveSpool {
        inner: crate::bootstrap::media_spool::FileMediaSpool::create().unwrap(),
        remove_started: remove_started.clone(),
    }));
    install_transport(
        &state,
        Arc::new(FiniteStaticTransport {
            events: generation_stream_events("completed"),
        }),
    );

    let state_for_task = state.clone();
    let task = tokio::spawn(async move {
        post_json(
            &state_for_task,
            &key,
            "/openai/v1/chat/completions",
            r#"{"model":"default","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"aGk=","format":"wav"}}]}]}"#,
        )
        .await
    });
    tokio::time::timeout(Duration::from_secs(1), remove_started.notified())
        .await
        .expect("media cleanup must begin after the provider attempt completes");
    task.abort();
    let _ = task.await;

    let event = tokio::time::timeout(Duration::from_secs(1), request_metadata.recv_next())
        .await
        .expect("cancellation during cleanup must emit request metadata")
        .expect("metadata channel must remain open");
    assert_eq!(event.status_code, None);
    assert_eq!(event.error_class.as_deref(), Some("client_cancelled"));
    assert_eq!(event.attempts.len(), 1);
    assert_eq!(event.attempts[0].status_code, Some(StatusCode::OK.as_u16()));
    assert_eq!(event.attempts[0].error_class, None);
    assert!(event.attempts[0].committed);
}

#[tokio::test]
async fn cancelling_shared_event_collection_preserves_partial_usage() {
    let (mut state, key) = test_state(false);
    let (emitter, mut request_metadata) =
        olp_engine::inference::request_metadata::Emitter::bounded(2);
    state.replace_request_metadata_for_test(emitter);
    let reached_pending = Arc::new(tokio::sync::Notify::new());
    install_transport(
        &state,
        Arc::new(CollectingPendingTransport {
            reached_pending: reached_pending.clone(),
            events: vec![
                Event::new(
                    0,
                    Kind::ResponseStart {
                        response_id: Some("resp_cancelled".to_owned()),
                        provider_model: Some("upstream-model".to_owned()),
                    },
                ),
                Event::new(
                    1,
                    Kind::Usage {
                        usage: olp_engine::domain::canonical::events::Usage {
                            input_tokens: 11,
                            output_tokens: 5,
                            total_tokens: 16,
                            cached_input_tokens: None,
                            reasoning_tokens: None,
                        },
                    },
                ),
            ],
        }),
    );
    let state_for_task = state.clone();
    let task = tokio::spawn(async move {
        post_json(
            &state_for_task,
            &key,
            "/openai/v1/responses",
            r#"{"model":"default","input":"hi"}"#,
        )
        .await
    });
    tokio::time::timeout(Duration::from_secs(1), reached_pending.notified())
        .await
        .expect("shared event collection must consume usage before stalling");
    task.abort();
    let _ = task.await;

    let event = tokio::time::timeout(Duration::from_secs(1), request_metadata.recv_next())
        .await
        .expect("cancellation must emit request metadata")
        .expect("metadata channel must remain open");
    assert_eq!(event.status_code, None);
    assert_eq!(event.error_class.as_deref(), Some("client_cancelled"));
    assert_eq!(event.input_tokens, Some(11));
    assert_eq!(event.output_tokens, Some(5));
    assert!(event.usage_complete);
    assert!(event.committed);
}

#[tokio::test]
async fn dropping_completed_unary_result_records_client_cancellation() {
    let (mut state, _) = test_state(false);
    let (emitter, mut request_metadata) =
        olp_engine::inference::request_metadata::Emitter::bounded(2);
    state.replace_request_metadata_for_test(emitter);
    install_result(
        &state,
        OperationKind::TokenCount,
        CanonicalResult::TokenCount(olp_engine::domain::canonical::results::TokenCountResult {
            input_tokens: 4,
            extensions: SourceExtensions::default(),
        }),
    );
    let request: ResponseInputTokensRequest = serde_json::from_value(json!({
        "model": "default",
        "input": "hello"
    }))
    .unwrap();
    let operation = decode_response_input_tokens(request).unwrap();
    let principal = test_principal(&state, Surface::OpenAi);
    let result = execute_routed_result_for_surface_inner(
        &state,
        &principal,
        operation,
        TransportMode::Unary,
        None,
    )
    .await
    .unwrap();
    drop(result);

    let event = request_metadata.recv_next().await.unwrap();
    assert_eq!(event.status_code, None);
    assert_eq!(event.error_class.as_deref(), Some("client_cancelled"));
    assert_eq!(event.input_tokens, Some(4));
    assert!(event.usage_complete);
}

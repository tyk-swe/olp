use super::*;

#[test]
fn inference_error_debug_redacts_client_message() {
    let error =
        InferenceError::bad_gateway("provider_protocol_error", "sensitive upstream response");
    let debug = format!("{error:?}");

    assert!(!debug.contains("sensitive upstream response"));
    assert!(debug.contains("[REDACTED]"));
}

#[tokio::test]
async fn unary_openai_route_authenticates_routes_and_encodes() {
    let (mut state, key) = test_state(false);
    let (emitter, mut request_metadata) =
        olp_storage::request_metadata::RequestMetadataEmitter::bounded(4);
    state.replace_request_metadata_for_test(emitter);
    let response = tokio::time::timeout(
        Duration::from_millis(250),
        crate::public_http::router::gateway_router_for_test(state).oneshot(
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
    let app = crate::public_http::router::gateway_router_for_test(state);
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
async fn invalid_proxy_key_gets_native_openai_error() {
    let (state, _) = test_state(false);
    let response = crate::public_http::router::gateway_router_for_test(state)
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
    let response = crate::public_http::router::gateway_router_for_test(state)
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
    let response = crate::public_http::router::gateway_router_for_test(state)
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

#[tokio::test]
async fn incompatible_unary_result_is_finalized_as_protocol_failure() {
    let (mut state, key) = test_state(false);
    let (emitter, mut request_metadata) =
        olp_storage::request_metadata::RequestMetadataEmitter::bounded(2);
    state.replace_request_metadata_for_test(emitter);
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
    let (mut state, key) = test_state(false);
    let (emitter, mut request_metadata) =
        olp_storage::request_metadata::RequestMetadataEmitter::bounded(2);
    state.replace_request_metadata_for_test(emitter);
    let artifact = state
        .media_spool()
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
    assert!(
        tokio::time::timeout(Duration::from_millis(10), request_metadata.recv_next())
            .await
            .is_err(),
        "lazy response metadata must not finalize before body delivery"
    );
    assert_eq!(
        response.into_body().collect().await.unwrap().to_bytes(),
        Bytes::from_static(b"audio-result")
    );
    let event = tokio::time::timeout(Duration::from_secs(1), request_metadata.recv_next())
        .await
        .expect("body EOF must finalize request metadata")
        .expect("metadata channel must remain open");
    assert_eq!(event.status_code, Some(StatusCode::OK.as_u16()));
    assert_eq!(event.error_class, None);
}

#[tokio::test]
async fn dropping_lazy_speech_body_records_client_cancellation() {
    let (mut state, key) = test_state(false);
    let (emitter, mut request_metadata) =
        olp_storage::request_metadata::RequestMetadataEmitter::bounded(2);
    state.replace_request_metadata_for_test(emitter);
    let artifact = state
        .media_spool()
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
    drop(response);

    let event = tokio::time::timeout(Duration::from_secs(1), request_metadata.recv_next())
        .await
        .expect("body drop must finalize request metadata")
        .expect("metadata channel must remain open");
    assert_eq!(event.status_code, None);
    assert_eq!(event.error_class.as_deref(), Some("client_cancelled"));
}

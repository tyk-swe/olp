use super::*;

#[tokio::test]
async fn rejects_attempts_for_another_provider_kind_before_transport() {
    let connector = Connector::new(
        ConnectorConfig::default(),
        ApiKey::new("upstream-secret").unwrap(),
    );
    let mut request = fixture_request(false);
    request.attempt.provider_kind = ProviderKind::Anthropic;

    let error = connector.execute(request).await.unwrap_err();
    assert_eq!(error.phase, TransportPhase::Connect);
    assert_eq!(error.class, AttemptFailureClass::Protocol);
}

#[test]
fn rejects_invalid_or_mismatched_modes_before_transport() {
    let mut image = image_request(false);
    image.metadata.mode = TransportMode::Streaming;
    assert!(validate_transport_mode(&image).is_err());

    let mut variation = image_variation_request();
    variation.metadata.mode = TransportMode::Streaming;
    assert!(validate_transport_mode(&variation).is_err());

    let mut video = video_create_request();
    assert!(validate_transport_mode(&video).is_ok());
    video.metadata.mode = TransportMode::Unary;
    assert!(validate_transport_mode(&video).is_err());
}

#[tokio::test]
async fn same_protocol_inline_audio_and_file_handles_are_rehydrated() {
    let handle = MediaHandle::new("inline-media");
    let marker = crate::domain::canonical::requests::inline_media_marker(&handle);
    let spool: Arc<dyn MediaSpool> = Arc::new(FixtureMediaSpool::new(
        "inline.bin",
        "application/octet-stream",
        b"hi",
    ));
    let mut chat: CompletionRequest = serde_json::from_value(serde_json::json!({
        "model":"upstream","messages":[{"role":"user","content":[{
            "type":"input_audio","input_audio":{"data":marker,"format":"wav"}
        }]}]
    }))
    .unwrap();
    hydrate_chat_media(&mut chat, Some(&spool)).await.unwrap();
    let MessageContent::Parts(parts) = chat.messages[0].content.as_ref().unwrap() else {
        panic!("expected content parts")
    };
    let OpenAiContentPart::InputAudio { input_audio, .. } = &parts[0] else {
        panic!("expected input audio")
    };
    assert_eq!(input_audio.data, "aGk=");

    let mut input: ResponseInput = serde_json::from_value(serde_json::json!([{
        "type":"message","role":"user","content":[{
            "type":"input_file","filename":"brief.pdf","file_data":crate::domain::canonical::requests::inline_media_marker(&handle)
        }]
    }]))
    .unwrap();
    hydrate_responses_media(&mut input, Some(&spool))
        .await
        .unwrap();
    let ResponseInput::Items(items) = input else {
        panic!("expected response items")
    };
    assert_eq!(
        items[0]["content"][0]["file_data"],
        "data:application/pdf;base64,aGk="
    );
}

#[tokio::test]
async fn model_discovery_is_credentialed_and_bounded() {
    let body = br#"{"data":[{"id":"gpt-test","object":"model"}]}"#;
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", body))],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());
    let models = connector.discover_models().await.unwrap();
    assert_eq!(models[0].id, "gpt-test");
    let request = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(request.starts_with("GET /v1/models "));
    assert!(request.contains("authorization: Bearer upstream-secret"));
}

#[tokio::test]
async fn executes_unary_chat_with_provider_model_and_late_bound_credential() {
    let body = serde_json::to_vec(&serde_json::json!({
        "id": "chatcmpl-1",
        "object": "chat.completion",
        "created": 1,
        "model": "gpt-4o-mini",
        "choices": [{
            "index": 0,
            "message": {"role": "assistant", "content": "hello back"},
            "finish_reason": "stop"
        }],
        "usage": {"prompt_tokens": 2, "completion_tokens": 2, "total_tokens": 4}
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &body))],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());

    let mut events = execute_events(&connector, fixture_request(false)).await;
    let mut collected = Vec::new();
    while let Some(event) = events.next().await {
        collected.push(event.unwrap());
    }

    assert!(collected.iter().any(|event| matches!(
        &event.kind,
        Kind::TextDelta { text, .. } if text == "hello back"
    )));
    assert!(matches!(
        collected.last().map(|event| &event.kind),
        Some(Kind::Done)
    ));
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(
        captured
            .to_ascii_lowercase()
            .contains("authorization: bearer upstream-secret")
    );
    assert!(captured.contains("\"model\":\"gpt-4o-mini\""));
    assert!(!captured.contains("\"model\":\"default\""));
}

#[tokio::test]
async fn responses_uses_distinct_upstream_endpoint_and_codec() {
    let body = serde_json::to_vec(&serde_json::json!({
        "id": "resp_1",
        "object": "response",
        "created_at": 1,
        "status": "completed",
        "model": "gpt-4o-mini",
        "output": [{
            "id": "msg_1",
            "type": "message",
            "role": "assistant",
            "status": "completed",
            "content": [{"type": "output_text", "text": "responses reply", "annotations": []}]
        }],
        "usage": {"input_tokens": 2, "output_tokens": 2, "total_tokens": 4}
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &body))],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());
    let mut events = execute_events(&connector, responses_request(false)).await;
    let mut text = String::new();
    while let Some(event) = events.next().await {
        if let Kind::TextDelta { text: delta, .. } = event.unwrap().kind {
            text.push_str(&delta);
        }
    }
    assert_eq!(text, "responses reply");
    let request = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(request.starts_with("POST /v1/responses "));
    assert!(!request.starts_with("POST /v1/chat/completions "));
}

#[tokio::test]
async fn responses_input_tokens_forwards_full_stateless_multi_item_body() {
    let body = serde_json::to_vec(&serde_json::json!({
        "object": "response.input_tokens",
        "input_tokens": 19
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &body))],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());
    let ProviderOutput::Result(result) = connector
        .execute(responses_input_tokens_request())
        .await
        .unwrap()
    else {
        panic!("input-token count returned a stream")
    };
    let CanonicalResult::TokenCount(result) = *result else {
        panic!("input-token count returned the wrong result kind")
    };
    assert_eq!(result.input_tokens, 19);
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("POST /v1/responses/input_tokens "));
    assert!(captured.contains("\"model\":\"gpt-count-upstream\""));
    assert!(captured.contains("\"role\":\"developer\""));
    assert!(captured.contains("\"type\":\"function_call_output\""));
    assert!(captured.contains("\"vendor_turn\":true"));
    assert!(captured.contains("\"name\":\"lookup\""));
}

#[tokio::test]
async fn responses_input_tokens_rehydrates_bounded_media_before_transport() {
    let body = serde_json::to_vec(&serde_json::json!({
        "object": "response.input_tokens",
        "input_tokens": 7
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &body))],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());
    let handle = MediaHandle::new("input-tokens-inline-media");
    let marker = crate::domain::canonical::requests::inline_media_marker(&handle);
    let wire: crate::protocols::openai::responses::token_count::ResponseInputTokensRequest =
        serde_json::from_value(serde_json::json!({
            "model":"count-route",
            "input":[{"type":"message","role":"user","content":[
                {"type":"input_audio","input_audio":{"data":marker,"format":"wav"}},
                {"type":"input_file","filename":"brief.pdf",
                 "file_data":crate::domain::canonical::requests::inline_media_marker(&handle)}
            ]}]
        }))
        .unwrap();
    let operation =
        crate::protocols::openai::responses::token_count::decode_response_input_tokens(wire)
            .unwrap();
    let mut request = fixture_request(false);
    request.metadata.operation = OperationKind::TokenCount;
    request.attempt.upstream_model = "gpt-count-upstream".into();
    request.operation = operation;
    request.media = Some(Arc::new(FixtureMediaSpool::new(
        "inline.bin",
        "application/octet-stream",
        b"hi",
    )));

    let ProviderOutput::Result(result) = connector.execute(request).await.unwrap() else {
        panic!("input-token count returned a stream")
    };
    let CanonicalResult::TokenCount(result) = *result else {
        panic!("input-token count returned the wrong result kind")
    };
    assert_eq!(result.input_tokens, 7);
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("POST /v1/responses/input_tokens "));
    assert!(captured.contains("\"data\":\"aGk=\""));
    assert!(captured.contains("\"file_data\":\"data:application/pdf;base64,aGk=\""));
    assert!(!captured.contains("urn:olp:inline-media:"));
}

#[tokio::test]
async fn executes_embeddings_as_a_typed_unary_result() {
    let body = serde_json::to_vec(&serde_json::json!({
        "object": "list",
        "model": "text-embedding-3-small",
        "data": [{"object": "embedding", "index": 0, "embedding": [0.25, -0.5]}],
        "usage": {"prompt_tokens": 1, "total_tokens": 1}
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &body))],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());

    let output = connector.execute(embeddings_request()).await.unwrap();
    let ProviderOutput::Result(result) = output else {
        panic!("connector returned the wrong output kind")
    };
    let CanonicalResult::Embeddings(result) = *result else {
        panic!("connector returned the wrong result kind")
    };
    assert_eq!(result.data[0].values, vec![0.25, -0.5]);
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("POST /v1/embeddings "));
    assert!(captured.contains("\"model\":\"text-embedding-3-small\""));
}

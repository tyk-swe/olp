use olp_domain::{
    CanonicalEventKind, ContentPart, FinishReason, Operation, Surface, validate_event_sequence,
};
use olp_protocols::openai::{
    ChatCompletionRequest, ChatCompletionResponse, OpenAiChatStreamDecoder, OpenAiStreamError,
    decode_chat_completion, decode_chat_completion_response, encode_chat_completion,
};
use serde_json::{Value, json};

#[test]
fn chat_request_translation_preserves_source_scoped_fields_and_semantics() {
    let wire = json!({
        "model": "team-chat",
        "messages": [
            {
                "role": "developer",
                "content": [{"type": "text", "text": "Keep answers short.", "vendor_text_flag": 7}],
                "vendor_message_flag": true
            },
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": "What is shown?", "cache_control": {"type": "ephemeral"}},
                    {"type": "image_url", "image_url": {"url": "https://example.test/image.png", "detail": "low"}}
                ]
            }
        ],
        "max_completion_tokens": 128,
        "temperature": 0.25,
        "stream": true,
        "tools": [{
            "type": "function",
            "function": {
                "name": "lookup",
                "description": "Look up a value",
                "parameters": {"type": "object", "properties": {"id": {"type": "string"}}},
                "strict": true
            }
        }],
        "tool_choice": {"type": "function", "function": {"name": "lookup"}},
        "response_format": {
            "type": "json_schema",
            "json_schema": {
                "name": "answer",
                "schema": {"type": "object"},
                "strict": true,
                "vendor_schema_flag": "retained"
            }
        },
        "service_tier": "priority"
    });
    let dto: ChatCompletionRequest = serde_json::from_value(wire).unwrap();
    let Operation::Generation(canonical) = decode_chat_completion(dto).unwrap() else {
        panic!("chat request translated to the wrong operation");
    };

    assert_eq!(canonical.route.as_str(), "team-chat");
    assert!(canonical.parameters.stream);
    assert_eq!(canonical.parameters.max_output_tokens, Some(128));
    assert_eq!(canonical.messages[1].content.len(), 2);
    assert!(matches!(
        canonical.messages[1].content[0],
        ContentPart::Text { .. }
    ));
    assert_eq!(canonical.tools[0].name, "lookup");
    assert_eq!(canonical.extensions.source, Some(Surface::OpenAi));
    assert_eq!(
        canonical.extensions.values["/service_tier"],
        json!("priority")
    );
    assert_eq!(
        canonical.extensions.values["/messages/0/vendor_message_flag"],
        json!(true)
    );
    assert_eq!(
        canonical.extensions.values["/messages/0/content/0/vendor_text_flag"],
        json!(7)
    );
    assert_eq!(
        canonical.extensions.values["/messages/1/content/0/cache_control"],
        json!({"type": "ephemeral"})
    );
    assert_eq!(
        canonical.extensions.values["/tools/0/function/strict"],
        json!(true)
    );

    let reencoded = encode_chat_completion(&canonical, "gpt-upstream").unwrap();
    let reencoded = serde_json::to_value(reencoded).unwrap();
    assert_eq!(reencoded["model"], "gpt-upstream");
    assert_eq!(reencoded["service_tier"], "priority");
    assert_eq!(reencoded["messages"][0]["vendor_message_flag"], true);
    assert_eq!(
        reencoded["messages"][0]["content"][0]["vendor_text_flag"],
        7
    );
    assert_eq!(
        reencoded["messages"][1]["content"][0]["cache_control"],
        json!({"type": "ephemeral"})
    );
    assert_eq!(reencoded["tools"][0]["function"]["strict"], true);
    assert_eq!(
        reencoded["response_format"]["json_schema"]["vendor_schema_flag"],
        "retained"
    );
}

#[test]
fn chat_request_rejects_ambiguous_or_invalid_parameters() {
    let both_limits = json!({
        "model": "default",
        "messages": [{"role": "user", "content": "hello"}],
        "max_tokens": 10,
        "max_completion_tokens": 20
    });
    let request: ChatCompletionRequest = serde_json::from_value(both_limits).unwrap();
    assert!(decode_chat_completion(request).is_err());

    let invalid_route = json!({
        "model": "provider/model",
        "messages": [{"role": "user", "content": "hello"}]
    });
    let request: ChatCompletionRequest = serde_json::from_value(invalid_route).unwrap();
    assert!(decode_chat_completion(request).is_err());
}

#[test]
fn unary_response_becomes_one_ordered_canonical_event_sequence() {
    let response: ChatCompletionResponse = serde_json::from_value(json!({
        "id": "chatcmpl_1",
        "object": "chat.completion",
        "created": 1800000000,
        "model": "gpt-upstream",
        "system_fingerprint": "fp_123",
        "choices": [{
            "index": 0,
            "message": {"role": "assistant", "content": "hello"},
            "finish_reason": "stop"
        }],
        "usage": {
            "prompt_tokens": 4,
            "completion_tokens": 2,
            "total_tokens": 6,
            "prompt_tokens_details": {"cached_tokens": 1},
            "completion_tokens_details": {"reasoning_tokens": 1}
        }
    }))
    .unwrap();

    let events = decode_chat_completion_response(response).unwrap();
    validate_event_sequence(&events).unwrap();
    assert!(matches!(
        events[0].kind,
        CanonicalEventKind::ResponseStart { .. }
    ));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::TextDelta { text, .. } if text == "hello"
    )));
    assert!(events.iter().any(|event| matches!(
        event.kind,
        CanonicalEventKind::Finish {
            reason: FinishReason::Stop,
            ..
        }
    )));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::SourceExtension { extensions }
            if extensions.values.get("/system_fingerprint") == Some(&Value::String("fp_123".into()))
    )));
    assert!(matches!(
        events.last().unwrap().kind,
        CanonicalEventKind::Done
    ));
}

#[test]
fn fragmented_unicode_sse_and_tool_deltas_decode_without_corruption() {
    let first = json!({
        "id": "chatcmpl_stream",
        "object": "chat.completion.chunk",
        "created": 1800000000,
        "model": "gpt-upstream",
        "choices": [{"index": 0, "delta": {"role": "assistant", "content": "héllø 🌍"}, "finish_reason": null}]
    });
    let second = json!({
        "id": "chatcmpl_stream",
        "object": "chat.completion.chunk",
        "created": 1800000000,
        "model": "gpt-upstream",
        "choices": [{
            "index": 0,
            "delta": {"tool_calls": [{"index": 0, "id": "call_1", "type": "function", "function": {"name": "lookup", "arguments": "{\"id\":1}"}}]},
            "finish_reason": "tool_calls"
        }],
        "usage": {"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8}
    });
    let wire = format!("data: {first}\n\ndata: {second}\n\ndata: [DONE]\n\n");
    let mut decoder = OpenAiChatStreamDecoder::new();
    let mut events = Vec::new();
    for byte in wire.as_bytes() {
        events.extend(decoder.push(std::slice::from_ref(byte)).unwrap());
    }
    events.extend(decoder.finish().unwrap());

    validate_event_sequence(&events).unwrap();
    assert!(decoder.is_done());
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::TextDelta { text, .. } if text == "héllø 🌍"
    )));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::ToolCallDelta { name: Some(name), arguments_delta, .. }
            if name == "lookup" && arguments_delta == "{\"id\":1}"
    )));
    assert!(matches!(
        events.last().unwrap().kind,
        CanonicalEventKind::Done
    ));
}

#[test]
fn stream_eof_without_done_is_an_error() {
    let chunk = json!({
        "id": "chatcmpl_stream",
        "object": "chat.completion.chunk",
        "created": 1800000000,
        "model": "gpt-upstream",
        "choices": [{"index": 0, "delta": {"content": "partial"}, "finish_reason": null}]
    });
    let mut decoder = OpenAiChatStreamDecoder::new();
    decoder
        .push(format!("data: {chunk}\n\n").as_bytes())
        .unwrap();
    assert!(matches!(
        decoder.finish(),
        Err(OpenAiStreamError::UnexpectedEof)
    ));
}

#[test]
fn stream_done_without_a_choice_finish_is_an_error() {
    let chunk = json!({
        "id": "chatcmpl_stream",
        "object": "chat.completion.chunk",
        "created": 1800000000,
        "model": "gpt-upstream",
        "choices": [{"index": 0, "delta": {"content": "partial"}, "finish_reason": null}]
    });
    let mut decoder = OpenAiChatStreamDecoder::new();
    decoder
        .push(format!("data: {chunk}\n\n").as_bytes())
        .unwrap();
    assert!(matches!(
        decoder.push(b"data: [DONE]\n\n"),
        Err(OpenAiStreamError::UnexpectedEof)
    ));
}

#[test]
fn stream_rejects_data_and_duplicate_finish_after_choice_finish() {
    let finished = json!({
        "id": "chatcmpl_stream",
        "object": "chat.completion.chunk",
        "created": 1800000000,
        "model": "gpt-upstream",
        "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]
    });
    let invalid_chunks = [
        json!({
            "id": "chatcmpl_stream",
            "object": "chat.completion.chunk",
            "created": 1800000000,
            "model": "gpt-upstream",
            "choices": [{"index": 0, "delta": {"content": "late"}, "finish_reason": null}]
        }),
        json!({
            "id": "",
            "object": "",
            "created": 0,
            "model": "",
            "choices": [{
                "index": 0,
                "delta": {"content": "late"},
                "finish_reason": null,
                "content_filter_results": {
                    "violence": {"filtered": false, "severity": "safe"}
                },
                "content_filter_offsets": {
                    "check_offset": 4,
                    "start_offset": 0,
                    "end_offset": 4
                }
            }]
        }),
        finished.clone(),
    ];

    for invalid in invalid_chunks {
        let mut decoder = OpenAiChatStreamDecoder::new();
        decoder
            .push(format!("data: {finished}\n\n").as_bytes())
            .unwrap();
        assert!(matches!(
            decoder.push(format!("data: {invalid}\n\n").as_bytes()),
            Err(OpenAiStreamError::DataAfterChoiceFinish(0))
        ));
    }
}

#[test]
fn stream_accepts_extension_only_annotation_after_choice_finish() {
    let finished = json!({
        "id": "chatcmpl_stream",
        "object": "chat.completion.chunk",
        "created": 1800000000,
        "model": "gpt-upstream",
        "choices": [{"index": 0, "delta": {"content": "safe"}, "finish_reason": "stop"}]
    });
    let annotation = json!({
        "id": "",
        "object": "",
        "created": 0,
        "model": "",
        "choices": [{
            "index": 0,
            "finish_reason": null,
            "content_filter_results": {
                "violence": {"filtered": false, "severity": "safe"}
            },
            "content_filter_offsets": {
                "check_offset": 4,
                "start_offset": 0,
                "end_offset": 4
            }
        }],
        "usage": null
    });
    let wire = format!("data: {finished}\n\ndata: {annotation}\n\ndata: [DONE]\n\n");
    let mut decoder = OpenAiChatStreamDecoder::new();
    let mut events = decoder.push(wire.as_bytes()).unwrap();
    events.extend(decoder.finish().unwrap());

    validate_event_sequence(&events).unwrap();
    let finish_position = events
        .iter()
        .position(|event| matches!(event.kind, CanonicalEventKind::Finish { .. }))
        .unwrap();
    let (annotation_position, extensions) = events
        .iter()
        .enumerate()
        .find_map(|(position, event)| match &event.kind {
            CanonicalEventKind::SourceExtension { extensions } => Some((position, extensions)),
            _ => None,
        })
        .unwrap();
    assert!(finish_position < annotation_position);
    assert_eq!(
        extensions.values["/choices/0/content_filter_results"],
        annotation["choices"][0]["content_filter_results"]
    );
    assert_eq!(
        extensions.values["/choices/0/content_filter_offsets"],
        annotation["choices"][0]["content_filter_offsets"]
    );
    assert!(matches!(
        events.last().unwrap().kind,
        CanonicalEventKind::Done
    ));
}

#[test]
fn provider_error_frame_is_terminal_and_canonical() {
    let wire = b"data: {\"error\":{\"message\":\"slow down\",\"type\":\"rate_limit_error\",\"code\":\"rate_limit\"}}\n\n";
    let mut decoder = OpenAiChatStreamDecoder::new();
    let events = decoder.push(wire).unwrap();
    assert!(decoder.is_done());
    assert!(matches!(events[0].kind, CanonicalEventKind::Error { .. }));
    assert!(matches!(events[1].kind, CanonicalEventKind::Done));
}

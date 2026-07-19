use olp_domain::{
    CanonicalEventKind, FinishReason, MessageRole, Operation, Surface, validate_event_sequence,
};
use olp_protocols::anthropic::{
    AnthropicMessagesClientStreamEncoder, AnthropicMessagesStreamDecoder, CountTokensRequest,
    CountTokensResponse, MessagesRequest, MessagesResponse, StreamError,
    decode_count_tokens_request, decode_messages_request, decode_messages_response,
    encode_count_tokens_result, encode_messages_request,
};
use serde_json::{Value, json};

#[test]
fn request_translation_round_trips_tools_results_and_source_extensions() {
    let wire = json!({
        "model": "team-claude",
        "max_tokens": 512,
        "stream": true,
        "system": [{"type": "text", "text": "Be concise", "cache_control": {"type": "ephemeral"}}],
        "messages": [
            {"role": "user", "content": [{"type": "text", "text": "Weather?", "vendor_text": 7}], "vendor_turn": true},
            {"role": "assistant", "content": [
                {"type": "text", "text": "I'll check."},
                {"type": "tool_use", "id": "toolu_1", "name": "weather", "input": {"city": "Paris"}, "eager_input_streaming": true}
            ]},
            {"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": "toolu_1", "content": "sunny", "is_error": false},
                {"type": "tool_result", "tool_use_id": "toolu_2", "content": [{"type": "text", "text": "extra"}], "is_error": true}
            ]}
        ],
        "tools": [
            {"name": "weather", "description": "Weather lookup", "input_schema": {"type": "object"}, "cache_control": {"type": "ephemeral"}},
            {"type": "web_search_20250305", "name": "web_search", "max_uses": 2}
        ],
        "tool_choice": {"type": "any", "disable_parallel_tool_use": true, "vendor_choice": "kept"},
        "metadata": {"user_id": "opaque-user"}
    });
    let dto: MessagesRequest = serde_json::from_value(wire).unwrap();
    let Operation::Generation(canonical) = decode_messages_request(dto).unwrap() else {
        panic!("wrong operation");
    };

    assert_eq!(canonical.route.as_str(), "team-claude");
    assert_eq!(canonical.messages[0].role, MessageRole::System);
    assert_eq!(canonical.messages.len(), 5);
    assert_eq!(canonical.messages[3].role, MessageRole::Tool);
    assert_eq!(
        canonical.messages[3].tool_call_id.as_deref(),
        Some("toolu_1")
    );
    assert_eq!(
        canonical.messages[4].tool_call_id.as_deref(),
        Some("toolu_2")
    );
    assert_eq!(canonical.tools.len(), 1);
    assert_eq!(canonical.parameters.parallel_tool_calls, Some(false));
    assert_eq!(canonical.extensions.source, Some(Surface::Anthropic));
    assert_eq!(
        canonical.extensions.values["/metadata"]["user_id"],
        "opaque-user"
    );
    assert_eq!(
        canonical.extensions.values["/messages/0/content/0/vendor_text"],
        7
    );
    assert_eq!(
        canonical.extensions.values["/messages/2/content/0/is_error"],
        false
    );
    assert_eq!(
        canonical.extensions.values["/messages/3/content/0/is_error"],
        true
    );

    let encoded = encode_messages_request(&canonical, "claude-upstream").unwrap();
    let encoded = serde_json::to_value(encoded).unwrap();
    assert_eq!(encoded["model"], "claude-upstream");
    assert_eq!(encoded["system"][0]["cache_control"]["type"], "ephemeral");
    assert_eq!(encoded["messages"][0]["content"][0]["vendor_text"], 7);
    assert_eq!(encoded["messages"][2]["content"][0]["is_error"], false);
    assert_eq!(encoded["messages"][3]["content"][0]["is_error"], true);
    assert_eq!(encoded["tools"].as_array().unwrap().len(), 2);
    assert_eq!(encoded["tools"][1]["type"], "web_search_20250305");
    assert_eq!(encoded["tool_choice"]["vendor_choice"], "kept");
    assert_eq!(encoded["metadata"]["user_id"], "opaque-user");
}

#[test]
fn inline_media_and_cross_protocol_loss_are_rejected() {
    let inline: MessagesRequest = serde_json::from_value(json!({
        "model": "default",
        "max_tokens": 32,
        "messages": [{"role": "user", "content": [{
            "type": "image",
            "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}
        }]}]
    }))
    .unwrap();
    assert!(decode_messages_request(inline).is_err());

    let request: MessagesRequest = serde_json::from_value(json!({
        "model": "default",
        "max_tokens": 32,
        "messages": [{"role": "user", "content": "hello"}],
        "thinking": {"type": "adaptive"}
    }))
    .unwrap();
    let Operation::Generation(mut canonical) = decode_messages_request(request).unwrap() else {
        unreachable!();
    };
    canonical.extensions.source = Some(Surface::Gemini);
    assert!(encode_messages_request(&canonical, "claude-upstream").is_err());
}

#[test]
fn unary_response_preserves_thinking_and_maps_tools_usage_and_finish() {
    let response: MessagesResponse = serde_json::from_value(json!({
        "id": "msg_1",
        "type": "message",
        "role": "assistant",
        "model": "claude-upstream",
        "content": [
            {"type": "thinking", "thinking": "private reasoning", "signature": "sig"},
            {"type": "text", "text": "Calling a tool", "citations": [{"type": "char_location"}]},
            {"type": "tool_use", "id": "toolu_1", "name": "weather", "input": {"city": "Paris"}}
        ],
        "stop_reason": "tool_use",
        "stop_sequence": null,
        "usage": {"input_tokens": 20, "output_tokens": 8, "cache_read_input_tokens": 4}
    }))
    .unwrap();
    let events = decode_messages_response(response).unwrap();
    validate_event_sequence(&events).unwrap();
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::TextDelta { text, .. } if text == "Calling a tool"
    )));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::ToolCallDelta { name: Some(name), arguments_delta, .. }
            if name == "weather" && arguments_delta.contains("Paris")
    )));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::SourceExtension { extensions }
            if extensions.values.contains_key("/content/0")
                && extensions.values.contains_key("/content/1/citations")
    )));
    assert!(events.iter().any(|event| matches!(
        event.kind,
        CanonicalEventKind::Finish {
            reason: FinishReason::ToolCalls,
            ..
        }
    )));
    assert!(events.iter().any(|event| matches!(
        event.kind,
        CanonicalEventKind::Usage { usage } if usage.input_tokens == 24
            && usage.total_tokens == 32
            && usage.cached_input_tokens == Some(4)
    )));
}

fn sse(event: &str, data: Value) -> String {
    format!("event: {event}\ndata: {data}\n\n")
}

#[test]
fn fragmented_stream_maps_text_thinking_tool_usage_unknown_events_and_done() {
    let mut wire = String::new();
    wire.push_str(&sse(
        "message_start",
        json!({
            "type": "message_start",
            "message": {
                "id": "msg_stream", "type": "message", "role": "assistant", "content": [],
                "model": "claude-upstream", "stop_reason": null, "stop_sequence": null,
                "usage": {"input_tokens": 12, "output_tokens": 1}
            }
        }),
    ));
    wire.push_str(&sse(
        "content_block_start",
        json!({
            "type": "content_block_start", "index": 0,
            "content_block": {"type": "thinking", "thinking": "", "signature": ""}
        }),
    ));
    wire.push_str(&sse(
        "content_block_delta",
        json!({
            "type": "content_block_delta", "index": 0,
            "delta": {"type": "thinking_delta", "thinking": "hidden"}
        }),
    ));
    wire.push_str(&sse(
        "content_block_stop",
        json!({"type": "content_block_stop", "index": 0}),
    ));
    wire.push_str(&sse(
        "future_event",
        json!({"type": "future_event", "payload": 1}),
    ));
    wire.push_str(&sse(
        "content_block_start",
        json!({
            "type": "content_block_start", "index": 1,
            "content_block": {"type": "text", "text": ""}
        }),
    ));
    wire.push_str(&sse(
        "content_block_delta",
        json!({
            "type": "content_block_delta", "index": 1,
            "delta": {"type": "text_delta", "text": "héllo 🌍"}
        }),
    ));
    wire.push_str(&sse(
        "content_block_stop",
        json!({"type": "content_block_stop", "index": 1}),
    ));
    wire.push_str(&sse(
        "content_block_start",
        json!({
            "type": "content_block_start", "index": 2,
            "content_block": {"type": "tool_use", "id": "toolu_1", "name": "weather", "input": {}}
        }),
    ));
    wire.push_str(&sse(
        "content_block_delta",
        json!({
            "type": "content_block_delta", "index": 2,
            "delta": {"type": "input_json_delta", "partial_json": "{\"city\":"}
        }),
    ));
    wire.push_str(&sse(
        "content_block_delta",
        json!({
            "type": "content_block_delta", "index": 2,
            "delta": {"type": "input_json_delta", "partial_json": "\"Paris\"}"}
        }),
    ));
    wire.push_str(&sse(
        "content_block_stop",
        json!({"type": "content_block_stop", "index": 2}),
    ));
    wire.push_str(&sse(
        "message_delta",
        json!({
            "type": "message_delta", "delta": {"stop_reason": "tool_use", "stop_sequence": null},
            "usage": {"output_tokens": 17, "cache_read_input_tokens": 3}
        }),
    ));
    wire.push_str(&sse("message_stop", json!({"type": "message_stop"})));

    let mut decoder = AnthropicMessagesStreamDecoder::new();
    let mut events = Vec::new();
    for byte in wire.as_bytes() {
        events.extend(decoder.push(std::slice::from_ref(byte)).unwrap());
    }
    events.extend(decoder.finish().unwrap());
    validate_event_sequence(&events).unwrap();
    assert!(decoder.is_done());
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::TextDelta { text, .. } if text == "héllo 🌍"
    )));
    let arguments = events
        .iter()
        .filter_map(|event| match &event.kind {
            CanonicalEventKind::ToolCallDelta {
                arguments_delta, ..
            } => Some(arguments_delta.as_str()),
            _ => None,
        })
        .collect::<String>();
    assert_eq!(arguments, "{\"city\":\"Paris\"}");
    assert!(events.iter().any(|event| matches!(
        event.kind,
        CanonicalEventKind::Usage { usage } if usage.input_tokens == 15
            && usage.output_tokens == 17 && usage.cached_input_tokens == Some(3)
    )));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::SourceExtension { extensions }
            if extensions.values.keys().any(|path| path.contains("thinking"))
                || extensions.values.contains_key("/events/future_event")
    )));
    assert!(matches!(
        events.last().unwrap().kind,
        CanonicalEventKind::Done
    ));
}

#[test]
fn stream_errors_are_terminal_and_truncation_is_not_success() {
    let error_wire = sse(
        "error",
        json!({
            "type": "error", "error": {"type": "overloaded_error", "message": "busy"}
        }),
    );
    let mut decoder = AnthropicMessagesStreamDecoder::new();
    let events = decoder.push(error_wire.as_bytes()).unwrap();
    assert!(decoder.is_done());
    assert!(matches!(
        &events[0].kind,
        CanonicalEventKind::Error { error } if error.retryable
    ));
    assert!(matches!(events[1].kind, CanonicalEventKind::Done));

    let start = sse(
        "message_start",
        json!({
            "type": "message_start",
            "message": {
                "id": "msg_truncated", "type": "message", "role": "assistant", "content": [],
                "model": "claude-upstream", "stop_reason": null, "stop_sequence": null,
                "usage": {"input_tokens": 1, "output_tokens": 1}
            }
        }),
    );
    let mut truncated = AnthropicMessagesStreamDecoder::new();
    truncated.push(start.as_bytes()).unwrap();
    assert!(matches!(
        truncated.finish(),
        Err(StreamError::UnexpectedEof)
    ));
}

#[test]
fn count_token_dtos_are_typed_and_bounded_by_the_http_layer() {
    let request: CountTokensRequest = serde_json::from_value(json!({
        "model": "claude-upstream",
        "messages": [{"role": "user", "content": "hello"}],
        "tools": []
    }))
    .unwrap();
    assert_eq!(request.model, "claude-upstream");
    let response: CountTokensResponse = serde_json::from_value(json!({
        "input_tokens": 9,
        "vendor_usage": true
    }))
    .unwrap();
    assert_eq!(response.input_tokens, 9);
    assert_eq!(response.extra["vendor_usage"], Value::Bool(true));
}

#[test]
fn count_tokens_preserves_full_anthropic_semantics_and_encodes_native_result() {
    let request: CountTokensRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "system": [{"type": "text", "text": "system", "cache_control": {"type": "ephemeral"}}],
        "messages": [{"role": "user", "content": "hello"}],
        "tools": [{"name": "lookup", "input_schema": {"type": "object"}}],
        "metadata": {"tenant": "source-only"}
    }))
    .unwrap();
    let Operation::TokenCount(canonical) = decode_count_tokens_request(request).unwrap() else {
        panic!("wrong operation")
    };
    assert_eq!(canonical.route.as_str(), "team-claude");
    assert_eq!(canonical.extensions.source, Some(Surface::Anthropic));
    let preserved =
        &canonical.extensions.values[olp_protocols::anthropic::ANTHROPIC_COUNT_REQUEST_EXTENSION];
    assert_eq!(preserved["metadata"]["tenant"], "source-only");
    assert_eq!(preserved["tools"][0]["name"], "lookup");

    let response = encode_count_tokens_result(&olp_domain::TokenCountResult {
        input_tokens: 17,
        extensions: olp_domain::SourceExtensions::new(
            Surface::Anthropic,
            [("/vendor_usage".into(), Value::Bool(true))].into(),
        ),
    })
    .unwrap();
    assert_eq!(response.input_tokens, 17);
    assert_eq!(response.extra["vendor_usage"], true);
}

#[test]
fn count_tokens_plain_user_text_is_cross_protocol_representable() {
    let request: CountTokensRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "messages": [{"role": "user", "content": "plain text"}]
    }))
    .unwrap();
    let Operation::TokenCount(canonical) = decode_count_tokens_request(request).unwrap() else {
        panic!("wrong operation")
    };
    assert!(canonical.extensions.values.is_empty());
    assert_eq!(canonical.extensions.source, Some(Surface::Anthropic));
    canonical
        .extensions
        .ensure_representable_on(Surface::Gemini)
        .unwrap();
}

#[test]
fn client_stream_encoder_emits_native_anthropic_sse_and_rejects_cross_surface_extensions() {
    let canonical = vec![
        olp_domain::CanonicalEvent::new(
            0,
            CanonicalEventKind::ResponseStart {
                response_id: Some("msg-client".into()),
                provider_model: Some("private".into()),
            },
        ),
        olp_domain::CanonicalEvent::new(
            1,
            CanonicalEventKind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            },
        ),
        olp_domain::CanonicalEvent::new(
            2,
            CanonicalEventKind::Usage {
                usage: olp_domain::Usage {
                    input_tokens: 4,
                    output_tokens: 0,
                    total_tokens: 4,
                    cached_input_tokens: None,
                    reasoning_tokens: None,
                },
            },
        ),
        olp_domain::CanonicalEvent::new(
            3,
            CanonicalEventKind::TextDelta {
                output_index: 0,
                text: "héllo".into(),
            },
        ),
        olp_domain::CanonicalEvent::new(
            4,
            CanonicalEventKind::Usage {
                usage: olp_domain::Usage {
                    input_tokens: 4,
                    output_tokens: 2,
                    total_tokens: 6,
                    cached_input_tokens: None,
                    reasoning_tokens: None,
                },
            },
        ),
        olp_domain::CanonicalEvent::new(
            5,
            CanonicalEventKind::Finish {
                output_index: 0,
                reason: FinishReason::Stop,
            },
        ),
        olp_domain::CanonicalEvent::new(6, CanonicalEventKind::Done),
    ];
    let mut encoder = AnthropicMessagesClientStreamEncoder::new("public-route", "fallback");
    let mut wire = String::new();
    for event in canonical {
        for frame in encoder.push(event).unwrap() {
            wire.push_str(&format!(
                "event: {}\ndata: {}\n\n",
                frame.event.unwrap(),
                frame.data
            ));
        }
    }
    assert!(wire.contains("\"model\":\"public-route\""));
    let mut decoder = AnthropicMessagesStreamDecoder::new();
    let mut decoded = Vec::new();
    for chunk in wire.as_bytes().chunks(2) {
        decoded.extend(decoder.push(chunk).unwrap());
    }
    decoded.extend(decoder.finish().unwrap());
    assert!(decoded.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::TextDelta { text, .. } if text == "héllo"
    )));

    let mut encoder = AnthropicMessagesClientStreamEncoder::new("route", "fallback");
    assert!(
        encoder
            .push(olp_domain::CanonicalEvent::new(
                0,
                CanonicalEventKind::SourceExtension {
                    extensions: olp_domain::SourceExtensions::new(
                        Surface::Gemini,
                        [("/safety".into(), json!({}))].into(),
                    ),
                },
            ))
            .is_err()
    );
}

#[test]
fn native_anthropic_stream_losslessly_preserves_thinking_cache_and_unknown_events() {
    let wire = [
        sse(
            "message_start",
            json!({
                "type": "message_start",
                "message": {
                    "id": "msg_native", "type": "message", "role": "assistant",
                    "content": [], "model": "claude-upstream", "stop_reason": null,
                    "stop_sequence": null,
                    "usage": {"input_tokens": 4, "output_tokens": 0, "cache_creation_input_tokens": 3}
                }
            }),
        ),
        sse(
            "content_block_start",
            json!({
                "type": "content_block_start", "index": 0,
                "content_block": {"type": "thinking", "thinking": "", "signature": "sig"}
            }),
        ),
        sse(
            "content_block_delta",
            json!({
                "type": "content_block_delta", "index": 0,
                "delta": {"type": "thinking_delta", "thinking": "private summary"}
            }),
        ),
        sse(
            "content_block_stop",
            json!({"type": "content_block_stop", "index": 0}),
        ),
        sse(
            "future_event",
            json!({"type": "future_event", "vendor": {"kept": true}}),
        ),
        sse(
            "message_delta",
            json!({
                "type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": null},
                "usage": {"output_tokens": 2, "cache_creation_input_tokens": 3}
            }),
        ),
        sse("message_stop", json!({"type": "message_stop"})),
    ]
    .concat();
    let mut decoder =
        AnthropicMessagesStreamDecoder::with_max_event_bytes_and_raw_passthrough(1024 * 1024, true);
    let mut events = decoder.push(wire.as_bytes()).unwrap();
    events.extend(decoder.finish().unwrap());
    validate_event_sequence(&events).unwrap();

    let mut encoder = AnthropicMessagesClientStreamEncoder::new("public-route", "fallback");
    let frames = events
        .into_iter()
        .flat_map(|event| encoder.push(event).unwrap())
        .collect::<Vec<_>>();
    let encoded = frames
        .iter()
        .map(|frame| frame.data.as_str())
        .collect::<String>();
    assert_eq!(frames.len(), 7);
    assert!(encoded.contains("\"model\":\"public-route\""));
    assert!(encoded.contains("cache_creation_input_tokens"));
    assert!(encoded.contains("thinking_delta"));
    assert!(encoded.contains("future_event"));
    assert!(encoded.contains("\"kept\":true"));
}

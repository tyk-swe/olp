use olp_engine::domain::{
    CanonicalEventKind, FinishReason, MessageRole, Operation, Surface, validate_event_sequence,
};
use olp_engine::protocols::anthropic::{
    AnthropicMessagesClientStreamEncoder, AnthropicMessagesStreamDecoder, ClientStreamEncodeError,
    CountTokensRequest, CountTokensResponse, MessagesRequest, MessagesResponse, StreamError,
    decode_count_tokens_request, decode_messages_request, decode_messages_response,
    encode_count_tokens_result, encode_messages_request, encode_messages_response,
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

#[test]
fn unary_partial_usage_is_attached_to_done_for_accounting() {
    let response: MessagesResponse = serde_json::from_value(json!({
        "id": "msg_partial",
        "type": "message",
        "role": "assistant",
        "model": "claude-upstream",
        "content": [{"type": "text", "text": "ok"}],
        "stop_reason": "end_turn",
        "usage": {
            "input_tokens": 20,
            "cache_creation_input_tokens": 2,
            "cache_read_input_tokens": 4
        }
    }))
    .unwrap();

    let events = decode_messages_response(response).unwrap();
    assert!(
        !events
            .iter()
            .any(|event| matches!(event.kind, CanonicalEventKind::Usage { .. }))
    );
    let observation = events.last().unwrap().usage_observation.unwrap();
    assert_eq!(observation.input_tokens, Some(26));
    assert_eq!(observation.output_tokens, None);
    assert_eq!(observation.total_tokens, None);
    assert_eq!(observation.cached_input_tokens, Some(4));
}

#[test]
fn unary_usage_rejects_derived_counters_outside_the_accounting_range() {
    for usage in [
        json!({"input_tokens": i64::MAX as u64, "output_tokens": 1}),
        json!({
            "input_tokens": i64::MAX as u64,
            "output_tokens": 0,
            "cache_read_input_tokens": 1
        }),
    ] {
        let response: MessagesResponse = serde_json::from_value(json!({
            "id": "msg_usage_range",
            "type": "message",
            "role": "assistant",
            "model": "claude-upstream",
            "content": [],
            "stop_reason": "end_turn",
            "usage": usage
        }))
        .unwrap();
        assert!(decode_messages_response(response).is_err());
    }
}

#[test]
fn unary_response_cache_usage_round_trips_without_double_counting() {
    let response: MessagesResponse = serde_json::from_value(json!({
        "id": "msg_cache",
        "type": "message",
        "role": "assistant",
        "model": "claude-upstream",
        "content": [{"type": "text", "text": "ok"}],
        "stop_reason": "end_turn",
        "usage": {
            "input_tokens": 10,
            "output_tokens": 4,
            "cache_creation_input_tokens": 3,
            "cache_read_input_tokens": 2
        }
    }))
    .unwrap();

    let events = decode_messages_response(response).unwrap();
    let encoded = encode_messages_response(&events, "public-route", "fallback").unwrap();
    assert_eq!(encoded.usage.input_tokens, Some(10));
    assert_eq!(encoded.usage.output_tokens, Some(4));
    assert_eq!(encoded.usage.cache_creation_input_tokens, Some(3));
    assert_eq!(encoded.usage.cache_read_input_tokens, Some(2));
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
fn stream_usage_is_complete_only_on_the_terminal_delta_and_later_deltas_fail() {
    let start = sse(
        "message_start",
        json!({
            "type": "message_start",
            "message": {
                "id": "msg_usage", "type": "message", "role": "assistant", "content": [],
                "model": "claude-upstream", "stop_reason": null, "stop_sequence": null,
                "usage": {
                    "input_tokens": 4,
                    "output_tokens": 0,
                    "cache_creation_input_tokens": 2,
                    "cache_read_input_tokens": 1
                }
            }
        }),
    );
    let terminal_without_final_usage = sse(
        "message_delta",
        json!({
            "type": "message_delta",
            "delta": {"stop_reason": "end_turn", "stop_sequence": null},
            "usage": {}
        }),
    );
    let stop = sse("message_stop", json!({"type": "message_stop"}));

    let mut partial = AnthropicMessagesStreamDecoder::new();
    let mut events = partial
        .push(format!("{start}{terminal_without_final_usage}{stop}").as_bytes())
        .unwrap();
    events.extend(partial.finish().unwrap());
    assert!(
        !events
            .iter()
            .any(|event| matches!(event.kind, CanonicalEventKind::Usage { .. }))
    );
    let observation = events.last().unwrap().usage_observation.unwrap();
    assert_eq!(observation.input_tokens, Some(7));
    assert_eq!(observation.output_tokens, Some(0));
    assert_eq!(observation.total_tokens, None);
    assert_eq!(observation.cached_input_tokens, Some(1));

    let mut complete = AnthropicMessagesStreamDecoder::new();
    complete.push(start.as_bytes()).unwrap();
    let events = complete
        .push(
            sse(
                "message_delta",
                json!({
                    "type": "message_delta",
                    "delta": {"stop_reason": "end_turn", "stop_sequence": null},
                    "usage": {"output_tokens": 3}
                }),
            )
            .as_bytes(),
        )
        .unwrap();
    assert_eq!(
        events
            .iter()
            .filter(|event| matches!(event.kind, CanonicalEventKind::Usage { .. }))
            .count(),
        1
    );
    assert!(events.iter().any(|event| matches!(
        event.kind,
        CanonicalEventKind::Usage { usage }
            if usage.input_tokens == 7 && usage.output_tokens == 3 && usage.total_tokens == 10
    )));
    assert!(matches!(
        complete.push(
            sse(
                "message_delta",
                json!({
                    "type": "message_delta",
                    "delta": {"stop_reason": null, "stop_sequence": null},
                    "usage": {"output_tokens": 4}
                }),
            )
            .as_bytes()
        ),
        Err(StreamError::ContentAfterFinish)
    ));
}

#[test]
fn stream_usage_rejects_counters_outside_the_accounting_range() {
    let mut decoder = AnthropicMessagesStreamDecoder::new();
    let error = decoder
        .push(
            sse(
                "message_start",
                json!({
                    "type": "message_start",
                    "message": {
                        "id": "msg_usage", "type": "message", "role": "assistant",
                        "content": [], "model": "claude-upstream", "stop_reason": null,
                        "stop_sequence": null,
                        "usage": {"input_tokens": (i64::MAX as u64) + 1, "output_tokens": 0}
                    }
                }),
            )
            .as_bytes(),
        )
        .unwrap_err();
    assert!(matches!(error, StreamError::InvalidUsage));
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
    let preserved = &canonical.extensions.values
        [olp_engine::protocols::anthropic::ANTHROPIC_COUNT_REQUEST_EXTENSION];
    assert_eq!(preserved["metadata"]["tenant"], "source-only");
    assert_eq!(preserved["tools"][0]["name"], "lookup");

    let response = encode_count_tokens_result(&olp_engine::domain::TokenCountResult {
        input_tokens: 17,
        extensions: olp_engine::domain::SourceExtensions::new(
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
        olp_engine::domain::CanonicalEvent::new(
            0,
            CanonicalEventKind::ResponseStart {
                response_id: Some("msg-client".into()),
                provider_model: Some("private".into()),
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            1,
            CanonicalEventKind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            2,
            CanonicalEventKind::Usage {
                usage: olp_engine::domain::Usage {
                    input_tokens: 4,
                    output_tokens: 0,
                    total_tokens: 4,
                    cached_input_tokens: None,
                    reasoning_tokens: None,
                },
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            3,
            CanonicalEventKind::TextDelta {
                output_index: 0,
                text: "héllo".into(),
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            4,
            CanonicalEventKind::Usage {
                usage: olp_engine::domain::Usage {
                    input_tokens: 4,
                    output_tokens: 2,
                    total_tokens: 6,
                    cached_input_tokens: None,
                    reasoning_tokens: None,
                },
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            5,
            CanonicalEventKind::Finish {
                output_index: 0,
                reason: FinishReason::Stop,
            },
        ),
        olp_engine::domain::CanonicalEvent::new(6, CanonicalEventKind::Done),
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
            .push(olp_engine::domain::CanonicalEvent::new(
                0,
                CanonicalEventKind::SourceExtension {
                    extensions: olp_engine::domain::SourceExtensions::new(
                        Surface::Gemini,
                        [("/safety".into(), json!({}))].into(),
                    ),
                },
            ))
            .is_err()
    );
}

#[test]
fn client_stream_buffers_finish_for_later_cached_usage() {
    let canonical = vec![
        olp_engine::domain::CanonicalEvent::new(
            0,
            CanonicalEventKind::ResponseStart {
                response_id: Some("msg-client-order".into()),
                provider_model: Some("private".into()),
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            1,
            CanonicalEventKind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            2,
            CanonicalEventKind::TextDelta {
                output_index: 0,
                text: "hello".into(),
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            3,
            CanonicalEventKind::Finish {
                output_index: 0,
                reason: FinishReason::Stop,
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            4,
            CanonicalEventKind::Usage {
                usage: olp_engine::domain::Usage {
                    input_tokens: 10,
                    output_tokens: 2,
                    total_tokens: 12,
                    cached_input_tokens: Some(4),
                    reasoning_tokens: None,
                },
            },
        ),
        olp_engine::domain::CanonicalEvent::new(5, CanonicalEventKind::Done),
    ];
    let mut encoder = AnthropicMessagesClientStreamEncoder::new("public-route", "fallback");
    let mut wire = String::new();
    for (index, event) in canonical.into_iter().enumerate() {
        let frames = encoder.push(event).unwrap();
        if index == 2 || index == 3 {
            assert!(frames.is_empty());
        }
        if index == 4 {
            let message_start = frames
                .iter()
                .find(|frame| frame.event.as_deref() == Some("message_start"))
                .unwrap();
            let start: Value = serde_json::from_str(&message_start.data).unwrap();
            assert_eq!(start["message"]["usage"]["input_tokens"], 6);
            assert_eq!(start["message"]["usage"]["output_tokens"], 0);
            assert_eq!(start["message"]["usage"]["cache_read_input_tokens"], 4);
            let terminal = frames
                .iter()
                .find(|frame| frame.event.as_deref() == Some("message_delta"))
                .unwrap();
            let usage: Value = serde_json::from_str(&terminal.data).unwrap();
            assert_eq!(usage["usage"]["input_tokens"], 6);
            assert_eq!(usage["usage"]["output_tokens"], 2);
            assert_eq!(usage["usage"]["cache_read_input_tokens"], 4);
        }
        for frame in frames {
            wire.push_str(&format!(
                "event: {}\ndata: {}\n\n",
                frame.event.unwrap(),
                frame.data
            ));
        }
    }

    let mut decoder = AnthropicMessagesStreamDecoder::new();
    let mut decoded = decoder.push(wire.as_bytes()).unwrap();
    decoded.extend(decoder.finish().unwrap());
    assert!(decoded.iter().any(|event| matches!(
        event.kind,
        CanonicalEventKind::Usage { usage }
            if usage.input_tokens == 10
                && usage.output_tokens == 2
                && usage.cached_input_tokens == Some(4)
    )));
}

#[test]
fn client_stream_error_drops_buffered_content_and_does_not_emit_message_stop() {
    let mut encoder = AnthropicMessagesClientStreamEncoder::new("route", "fallback");
    for event in [
        olp_engine::domain::CanonicalEvent::new(
            0,
            CanonicalEventKind::ResponseStart {
                response_id: None,
                provider_model: None,
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            1,
            CanonicalEventKind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            2,
            CanonicalEventKind::TextDelta {
                output_index: 0,
                text: "discard me".into(),
            },
        ),
    ] {
        assert!(encoder.push(event).unwrap().is_empty());
    }
    let error_frames = encoder
        .push(olp_engine::domain::CanonicalEvent::new(
            3,
            CanonicalEventKind::Error {
                error: olp_engine::domain::CanonicalError {
                    class: olp_engine::domain::ErrorClass::Upstream,
                    message: "failed".into(),
                    provider_code: None,
                    retryable: false,
                },
            },
        ))
        .unwrap();
    assert_eq!(error_frames.len(), 1);
    assert_eq!(error_frames[0].event.as_deref(), Some("error"));

    let done_frames = encoder
        .push(olp_engine::domain::CanonicalEvent::new(
            4,
            CanonicalEventKind::Done,
        ))
        .unwrap();
    assert!(done_frames.is_empty());

    let mut encoder = AnthropicMessagesClientStreamEncoder::new("route", "fallback");
    for event in [
        olp_engine::domain::CanonicalEvent::new(
            0,
            CanonicalEventKind::ResponseStart {
                response_id: None,
                provider_model: None,
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            1,
            CanonicalEventKind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            2,
            CanonicalEventKind::Usage {
                usage: olp_engine::domain::Usage {
                    input_tokens: 1,
                    output_tokens: 0,
                    total_tokens: 1,
                    cached_input_tokens: None,
                    reasoning_tokens: None,
                },
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            3,
            CanonicalEventKind::TextDelta {
                output_index: 0,
                text: "already emitted".into(),
            },
        ),
    ] {
        encoder.push(event).unwrap();
    }
    assert_eq!(
        encoder
            .push(olp_engine::domain::CanonicalEvent::new(
                4,
                CanonicalEventKind::Error {
                    error: olp_engine::domain::CanonicalError {
                        class: olp_engine::domain::ErrorClass::Upstream,
                        message: "failed".into(),
                        provider_code: None,
                        retryable: false,
                    },
                },
            ))
            .unwrap()[0]
            .event
            .as_deref(),
        Some("error")
    );
    assert!(
        encoder
            .push(olp_engine::domain::CanonicalEvent::new(
                5,
                CanonicalEventKind::Done,
            ))
            .unwrap()
            .is_empty()
    );
}

#[test]
fn client_stream_rejects_successful_done_without_usage() {
    let mut encoder = AnthropicMessagesClientStreamEncoder::new("route", "fallback");
    for event in [
        olp_engine::domain::CanonicalEvent::new(
            0,
            CanonicalEventKind::ResponseStart {
                response_id: None,
                provider_model: None,
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            1,
            CanonicalEventKind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            2,
            CanonicalEventKind::Finish {
                output_index: 0,
                reason: FinishReason::Stop,
            },
        ),
    ] {
        encoder.push(event).unwrap();
    }
    assert!(matches!(
        encoder.push(olp_engine::domain::CanonicalEvent::new(
            3,
            CanonicalEventKind::Done
        )),
        Err(ClientStreamEncodeError::MissingUsage)
    ));
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

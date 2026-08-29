use olp_engine::domain::canonical::{
    events::{FinishReason, Kind, validate_event_sequence},
    identity::Surface,
    requests::{MessageRole, Operation},
};
use olp_engine::protocols::anthropic::{
    client_stream::Encoder,
    count::{decode_count_tokens_request, encode_count_tokens_result},
    dto::{CountTokensRequest, CountTokensResponse, MessagesRequest, MessagesResponse},
    stream::{Decoder, Error as StreamError},
    translate::{
        decode::request as decode_request, encode::request as encode_request,
        response::decode as decode_response,
    },
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
    let Operation::Generation(canonical) = decode_request(dto).unwrap() else {
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

    let encoded = encode_request(&canonical, "claude-upstream").unwrap();
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
    assert!(decode_request(inline).is_err());

    let request: MessagesRequest = serde_json::from_value(json!({
        "model": "default",
        "max_tokens": 32,
        "messages": [{"role": "user", "content": "hello"}],
        "thinking": {"type": "adaptive"}
    }))
    .unwrap();
    let Operation::Generation(mut canonical) = decode_request(request).unwrap() else {
        unreachable!();
    };
    canonical.extensions.source = Some(Surface::Gemini);
    assert!(encode_request(&canonical, "claude-upstream").is_err());
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
    let events = decode_response(response).unwrap();
    validate_event_sequence(&events).unwrap();
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        Kind::TextDelta { text, .. } if text == "Calling a tool"
    )));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        Kind::ToolCallDelta { name: Some(name), arguments_delta, .. }
            if name == "weather" && arguments_delta.contains("Paris")
    )));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        Kind::SourceExtension { extensions }
            if extensions.values.contains_key("/content/0")
                && extensions.values.contains_key("/content/1/citations")
    )));
    assert!(events.iter().any(|event| matches!(
        event.kind,
        Kind::Finish {
            reason: FinishReason::ToolCalls,
            ..
        }
    )));
    assert!(events.iter().any(|event| matches!(
        event.kind,
        Kind::Usage { usage } if usage.input_tokens == 24
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

    let mut decoder = Decoder::new();
    let mut events = Vec::new();
    for byte in wire.as_bytes() {
        events.extend(decoder.push(std::slice::from_ref(byte)).unwrap());
    }
    events.extend(decoder.finish().unwrap());
    validate_event_sequence(&events).unwrap();
    assert!(decoder.is_done());
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        Kind::TextDelta { text, .. } if text == "héllo 🌍"
    )));
    let arguments = events
        .iter()
        .filter_map(|event| match &event.kind {
            Kind::ToolCallDelta {
                arguments_delta, ..
            } => Some(arguments_delta.as_str()),
            _ => None,
        })
        .collect::<String>();
    assert_eq!(arguments, "{\"city\":\"Paris\"}");
    assert!(events.iter().any(|event| matches!(
        event.kind,
        Kind::Usage { usage } if usage.input_tokens == 15
            && usage.output_tokens == 17 && usage.cached_input_tokens == Some(3)
    )));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        Kind::SourceExtension { extensions }
            if extensions.values.keys().any(|path| path.contains("thinking"))
                || extensions.values.contains_key("/events/future_event")
    )));
    assert!(matches!(events.last().unwrap().kind, Kind::Done));
}

#[test]
fn stream_errors_are_terminal_and_truncation_is_not_success() {
    let error_wire = sse(
        "error",
        json!({
            "type": "error", "error": {"type": "overloaded_error", "message": "busy"}
        }),
    );
    let mut decoder = Decoder::new();
    let events = decoder.push(error_wire.as_bytes()).unwrap();
    assert!(decoder.is_done());
    assert!(matches!(
        &events[0].kind,
        Kind::Error { error } if error.retryable
    ));
    assert!(matches!(events[1].kind, Kind::Done));

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
    let mut truncated = Decoder::new();
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
    let preserved = &canonical.extensions.values
        [olp_engine::protocols::anthropic::count::ANTHROPIC_COUNT_REQUEST_EXTENSION];
    assert_eq!(preserved["metadata"]["tenant"], "source-only");
    assert_eq!(preserved["tools"][0]["name"], "lookup");

    let response =
        encode_count_tokens_result(&olp_engine::domain::canonical::results::TokenCountResult {
            input_tokens: 17,
            extensions: olp_engine::domain::canonical::requests::SourceExtensions::new(
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
        olp_engine::domain::canonical::events::Event::new(
            0,
            Kind::ResponseStart {
                response_id: Some("msg-client".into()),
                provider_model: Some("private".into()),
            },
        ),
        olp_engine::domain::canonical::events::Event::new(
            1,
            Kind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            },
        ),
        olp_engine::domain::canonical::events::Event::new(
            2,
            Kind::Usage {
                usage: olp_engine::domain::canonical::events::Usage {
                    input_tokens: 4,
                    output_tokens: 0,
                    total_tokens: 4,
                    cached_input_tokens: None,
                    reasoning_tokens: None,
                },
            },
        ),
        olp_engine::domain::canonical::events::Event::new(
            3,
            Kind::TextDelta {
                output_index: 0,
                text: "héllo".into(),
            },
        ),
        olp_engine::domain::canonical::events::Event::new(
            4,
            Kind::Usage {
                usage: olp_engine::domain::canonical::events::Usage {
                    input_tokens: 4,
                    output_tokens: 2,
                    total_tokens: 6,
                    cached_input_tokens: None,
                    reasoning_tokens: None,
                },
            },
        ),
        olp_engine::domain::canonical::events::Event::new(
            5,
            Kind::Finish {
                output_index: 0,
                reason: FinishReason::Stop,
            },
        ),
        olp_engine::domain::canonical::events::Event::new(6, Kind::Done),
    ];
    let mut encoder = Encoder::new("public-route", "fallback");
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
    let mut decoder = Decoder::new();
    let mut decoded = Vec::new();
    for chunk in wire.as_bytes().chunks(2) {
        decoded.extend(decoder.push(chunk).unwrap());
    }
    decoded.extend(decoder.finish().unwrap());
    assert!(decoded.iter().any(|event| matches!(
        &event.kind,
        Kind::TextDelta { text, .. } if text == "héllo"
    )));

    // A Gemini-surface extension has no Anthropic representation. The response
    // is already in flight, so it is dropped rather than killing the stream.
    let mut encoder = Encoder::new("route", "fallback");
    assert!(
        encoder
            .push(olp_engine::domain::canonical::events::Event::new(
                0,
                Kind::SourceExtension {
                    extensions: olp_engine::domain::canonical::requests::SourceExtensions::new(
                        Surface::Gemini,
                        [("/safety".into(), json!({}))].into(),
                    ),
                },
            ))
            .unwrap()
            .is_empty()
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
    let mut decoder = Decoder::with_max_event_bytes_and_raw_passthrough(1024 * 1024, true);
    let mut events = decoder.push(wire.as_bytes()).unwrap();
    events.extend(decoder.finish().unwrap());
    validate_event_sequence(&events).unwrap();

    let mut encoder = Encoder::new("public-route", "fallback");
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

/// A3: a request whose only tool is a server-side (typed) tool has an empty
/// canonical `tools`, so the encoder used to omit the key entirely and the
/// `/tools/0` extension then had nothing to walk into.
#[test]
fn server_side_only_tools_survive_the_request_round_trip() {
    let dto: MessagesRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "max_tokens": 256,
        "messages": [{"role": "user", "content": "search please"}],
        "tools": [{"type": "web_search_20250305", "name": "web_search", "max_uses": 2}]
    }))
    .unwrap();
    let Operation::Generation(canonical) = decode_request(dto).unwrap() else {
        panic!("wrong operation");
    };
    assert!(canonical.tools.is_empty());
    assert_eq!(
        canonical.extensions.values["/tools/0"]["type"],
        "web_search_20250305"
    );

    let encoded = encode_request(&canonical, "claude-upstream").unwrap();
    let wire = serde_json::to_value(&encoded).unwrap();
    assert_eq!(wire["tools"][0]["type"], "web_search_20250305");
    assert_eq!(wire["tools"][0]["name"], "web_search");
    assert_eq!(wire["tools"][0]["max_uses"], 2);
    assert_eq!(wire["tools"].as_array().unwrap().len(), 1);
}

/// A5: Anthropic requires the signed `thinking` block to be echoed back on the
/// next turn of an extended-thinking tool loop. Rejecting it broke turn two of
/// every such conversation.
#[test]
fn thinking_and_unmodelled_assistant_blocks_round_trip_through_canonical() {
    let dto: MessagesRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "max_tokens": 256,
        "messages": [
            {"role": "user", "content": "weather?"},
            {"role": "assistant", "content": [
                {"type": "thinking", "thinking": "let me check", "signature": "sig-abc"},
                {"type": "text", "text": "checking"},
                {"type": "tool_use", "id": "toolu_1", "name": "weather", "input": {}}
            ]},
            {"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": "toolu_1", "content": "sunny"}
            ]}
        ]
    }))
    .unwrap();
    let Operation::Generation(canonical) = decode_request(dto).unwrap() else {
        panic!("wrong operation");
    };
    assert_eq!(
        canonical.extensions.values["/messages/1/content/0"]["signature"],
        "sig-abc"
    );

    let wire =
        serde_json::to_value(encode_request(&canonical, "claude-upstream").unwrap()).unwrap();
    let assistant = &wire["messages"][1]["content"];
    assert_eq!(assistant[0]["type"], "thinking");
    assert_eq!(assistant[0]["signature"], "sig-abc");
    assert_eq!(assistant[0]["thinking"], "let me check");
    assert_eq!(assistant[1]["type"], "text");
    assert_eq!(assistant[1]["text"], "checking");
    assert_eq!(assistant[2]["type"], "tool_use");
    assert_eq!(assistant[2]["id"], "toolu_1");
}

/// A5: an assistant turn made only of a redacted thinking block still has to
/// reach the upstream; it used to be rejected as an empty message.
#[test]
fn a_message_of_only_unmodelled_blocks_is_not_dropped() {
    let dto: MessagesRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "max_tokens": 256,
        "messages": [
            {"role": "user", "content": "hi"},
            {"role": "assistant", "content": [
                {"type": "redacted_thinking", "data": "opaque"}
            ]},
            {"role": "user", "content": "continue"}
        ]
    }))
    .unwrap();
    let Operation::Generation(canonical) = decode_request(dto).unwrap() else {
        panic!("wrong operation");
    };
    let wire =
        serde_json::to_value(encode_request(&canonical, "claude-upstream").unwrap()).unwrap();
    assert_eq!(
        wire["messages"][1]["content"][0]["type"],
        "redacted_thinking"
    );
    assert_eq!(wire["messages"][1]["content"][0]["data"], "opaque");
}

/// A4: Anthropic emits `citations_delta` on a *text* block whenever citations
/// are enabled. Erroring killed the response after partial text had shipped.
#[test]
fn unmodelled_deltas_on_a_text_block_do_not_kill_the_stream() {
    let wire = [
        sse(
            "message_start",
            json!({"type": "message_start", "message": {
                "id": "msg_1", "type": "message", "role": "assistant", "content": [],
                "model": "claude-upstream", "usage": {"input_tokens": 4, "output_tokens": 0}
            }}),
        ),
        sse(
            "content_block_start",
            json!({"type": "content_block_start", "index": 0,
                   "content_block": {"type": "text", "text": ""}}),
        ),
        sse(
            "content_block_delta",
            json!({"type": "content_block_delta", "index": 0,
                   "delta": {"type": "text_delta", "text": "per the source"}}),
        ),
        sse(
            "content_block_delta",
            json!({"type": "content_block_delta", "index": 0, "delta": {
                "type": "citations_delta",
                "citation": {"type": "char_location", "cited_text": "source"}
            }}),
        ),
        sse(
            "content_block_stop",
            json!({"type": "content_block_stop", "index": 0}),
        ),
        sse(
            "message_delta",
            json!({"type": "message_delta", "delta": {"stop_reason": "end_turn"},
                   "usage": {"output_tokens": 3}}),
        ),
        sse("message_stop", json!({"type": "message_stop"})),
    ]
    .concat();
    let mut decoder = Decoder::new();
    let mut events = decoder.push(wire.as_bytes()).unwrap();
    events.extend(decoder.finish().unwrap());
    validate_event_sequence(&events).unwrap();

    assert!(events.iter().any(|event| matches!(
        &event.kind,
        Kind::TextDelta { text, .. } if text == "per the source"
    )));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        Kind::SourceExtension { extensions }
            if extensions.values.contains_key("/content/0/delta/citations_delta")
    )));
    // A `text_delta` aimed at a tool block is still a real mismatch.
    let mismatched = [
        sse(
            "message_start",
            json!({"type": "message_start", "message": {
                "id": "msg_1", "type": "message", "role": "assistant", "content": [],
                "model": "claude-upstream", "usage": {"input_tokens": 1, "output_tokens": 0}
            }}),
        ),
        sse(
            "content_block_start",
            json!({"type": "content_block_start", "index": 0,
                   "content_block": {"type": "tool_use", "id": "t1", "name": "x", "input": {}}}),
        ),
        sse(
            "content_block_delta",
            json!({"type": "content_block_delta", "index": 0,
                   "delta": {"type": "text_delta", "text": "nope"}}),
        ),
    ]
    .concat();
    let mut decoder = Decoder::new();
    assert!(matches!(
        decoder.push(mismatched.as_bytes()),
        Err(StreamError::DeltaBlockMismatch { .. })
    ));
}

fn assistant_events(
    usage: olp_engine::domain::canonical::events::Usage,
    reason: FinishReason,
    extensions: Vec<(&str, Value)>,
) -> Vec<olp_engine::domain::canonical::events::Event> {
    use olp_engine::domain::canonical::events::Event;
    let mut events = vec![
        Event::new(
            0,
            Kind::ResponseStart {
                response_id: Some("msg_1".into()),
                provider_model: Some("upstream".into()),
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
                text: "hello".into(),
            },
        ),
        Event::new(3, Kind::Usage { usage }),
    ];
    let mut next = 4;
    if !extensions.is_empty() {
        events.push(Event::new(
            next,
            Kind::SourceExtension {
                extensions: olp_engine::domain::canonical::requests::SourceExtensions::new(
                    Surface::Anthropic,
                    extensions
                        .into_iter()
                        .map(|(path, value)| (path.to_owned(), value))
                        .collect(),
                ),
            },
        ));
        next += 1;
    }
    events.push(Event::new(
        next,
        Kind::Finish {
            output_index: 0,
            reason,
        },
    ));
    events.push(Event::new(next + 1, Kind::Done));
    events
}

/// A6: `reasoning_tokens.is_some()` rejected the whole response. OpenAI reports
/// `reasoning_tokens: 0` for non-reasoning models and Gemini 2.5 reports a real
/// count on nearly every response, so both directions were broken.
#[test]
fn reasoning_token_reporting_never_fails_an_anthropic_response() {
    use olp_engine::domain::canonical::events::Usage as CanonicalUsage;
    use olp_engine::protocols::anthropic::client::encode_messages_response;

    for reasoning in [Some(0), Some(97)] {
        let events = assistant_events(
            CanonicalUsage {
                input_tokens: 10,
                output_tokens: 4,
                total_tokens: 14 + reasoning.unwrap_or(0),
                cached_input_tokens: None,
                reasoning_tokens: reasoning,
            },
            FinishReason::Stop,
            Vec::new(),
        );
        let response = encode_messages_response(&events, "route", "fallback").unwrap();
        assert_eq!(response.usage.input_tokens, 10);
        assert_eq!(response.usage.output_tokens, 4);
    }

    // The same gap on the streaming path.
    let mut encoder = Encoder::new("route", "fallback");
    let mut frames = Vec::new();
    for event in assistant_events(
        CanonicalUsage {
            input_tokens: 10,
            output_tokens: 4,
            total_tokens: 111,
            cached_input_tokens: None,
            reasoning_tokens: Some(97),
        },
        FinishReason::Stop,
        Vec::new(),
    ) {
        frames.extend(encoder.push(event).unwrap());
    }
    assert!(
        frames
            .iter()
            .any(|frame| frame.data.contains("message_delta"))
    );
}

/// A14: canonical `input_tokens` is cache-inclusive. Subtracting only the read
/// tier double-counted cache-creation tokens, and the streaming encoder
/// subtracted nothing and dropped the creation count entirely.
#[test]
fn anthropic_usage_splits_both_cache_tiers_identically_unary_and_streamed() {
    use olp_engine::domain::canonical::events::Usage as CanonicalUsage;
    use olp_engine::protocols::anthropic::client::encode_messages_response;

    // input 20 fresh + 100 created + 4 read == 124 canonical input tokens.
    let usage = CanonicalUsage {
        input_tokens: 124,
        output_tokens: 7,
        total_tokens: 131,
        cached_input_tokens: Some(4),
        reasoning_tokens: None,
    };
    let extensions = vec![("/usage/cache_creation_input_tokens", json!(100))];

    let response = encode_messages_response(
        &assistant_events(usage, FinishReason::Stop, extensions.clone()),
        "route",
        "fallback",
    )
    .unwrap();
    assert_eq!(response.usage.input_tokens, 20);
    assert_eq!(response.usage.cache_creation_input_tokens, Some(100));
    assert_eq!(response.usage.cache_read_input_tokens, Some(4));

    let mut encoder = Encoder::new("route", "fallback");
    let mut frames = Vec::new();
    for event in assistant_events(usage, FinishReason::Stop, extensions) {
        frames.extend(encoder.push(event).unwrap());
    }
    let delta: Value = frames
        .iter()
        .find(|frame| frame.event.as_deref() == Some("message_delta"))
        .map(|frame| serde_json::from_str(&frame.data).unwrap())
        .expect("the stream must contain a message_delta");
    assert_eq!(delta["usage"]["input_tokens"], 20);
    assert_eq!(delta["usage"]["cache_creation_input_tokens"], 100);
    assert_eq!(delta["usage"]["cache_read_input_tokens"], 4);
}

/// A16: Anthropic guarantees `stop_reason` and `stop_sequence` agree. The
/// canonical fold to `Stop` re-encoded as `end_turn` next to a restored
/// `stop_sequence`.
#[test]
fn a_matched_stop_sequence_reports_stop_sequence_as_the_reason() {
    use olp_engine::domain::canonical::events::Usage as CanonicalUsage;
    use olp_engine::protocols::anthropic::client::encode_messages_response;

    let usage = CanonicalUsage::default();
    let response = encode_messages_response(
        &assistant_events(
            usage,
            FinishReason::Stop,
            vec![("/stop_sequence", json!("END"))],
        ),
        "route",
        "fallback",
    )
    .unwrap();
    assert_eq!(response.stop_reason.as_deref(), Some("stop_sequence"));
    assert_eq!(response.stop_sequence.as_deref(), Some("END"));

    let mut encoder = Encoder::new("route", "fallback");
    let mut wire = String::new();
    for event in assistant_events(
        usage,
        FinishReason::Stop,
        vec![("/delta/stop_sequence", json!("END"))],
    ) {
        for frame in encoder.push(event).unwrap() {
            wire.push_str(&frame.data);
        }
    }
    assert!(wire.contains("\"stop_reason\":\"stop_sequence\""));
    assert!(wire.contains("\"stop_sequence\":\"END\""));
}

/// A11: a no-parameter tool aggregates to an empty argument string, which
/// `serde_json` cannot parse — the whole response failed to encode.
#[test]
fn a_zero_argument_tool_call_encodes_as_an_empty_object() {
    use olp_engine::domain::canonical::events::{Event, Usage as CanonicalUsage};
    use olp_engine::protocols::anthropic::client::encode_messages_response;

    let events = vec![
        Event::new(
            0,
            Kind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            },
        ),
        Event::new(
            1,
            Kind::ToolCallDelta {
                output_index: 0,
                tool_index: 0,
                id: Some("toolu_1".into()),
                name: Some("now".into()),
                arguments_delta: String::new(),
            },
        ),
        Event::new(
            2,
            Kind::Usage {
                usage: CanonicalUsage::default(),
            },
        ),
        Event::new(
            3,
            Kind::Finish {
                output_index: 0,
                reason: FinishReason::ToolCalls,
            },
        ),
        Event::new(4, Kind::Done),
    ];
    let response = encode_messages_response(&events, "route", "fallback").unwrap();
    let wire = serde_json::to_value(&response).unwrap();
    assert_eq!(wire["content"][0]["type"], "tool_use");
    assert_eq!(wire["content"][0]["input"], json!({}));
}

/// A17: `pause_turn` is a real Anthropic value that server-tool loops depend
/// on, so it passes through; a value outside the enum is clamped.
#[test]
fn anthropic_stop_reasons_pass_through_only_inside_the_declared_enum() {
    use olp_engine::domain::canonical::events::Usage as CanonicalUsage;
    use olp_engine::protocols::anthropic::client::encode_messages_response;

    for (reason, expected) in [
        (FinishReason::Other("pause_turn".to_owned()), "pause_turn"),
        (FinishReason::Other("LANGUAGE".to_owned()), "end_turn"),
        (FinishReason::Error, "refusal"),
        (FinishReason::ToolCalls, "tool_use"),
    ] {
        let response = encode_messages_response(
            &assistant_events(CanonicalUsage::default(), reason.clone(), Vec::new()),
            "route",
            "fallback",
        )
        .unwrap();
        assert_eq!(
            response.stop_reason.as_deref(),
            Some(expected),
            "{reason:?} must encode as {expected}"
        );
    }
}

#[test]
fn thinking_block_with_tool_use_fields_round_trips_idempotently() {
    // Found by the protocol_json fuzz target. Block classification must follow
    // `type`; guessing from field shape turned this thinking block into a tool
    // use on the second decode and rejected the encoder's own output.
    let document = json!({
        "max_tokens": 1024,
        "model": "claude",
        "messages": [
            { "role": "user", "content": "weather?" },
            { "role": "assistant", "content": [
                { "type": "thinking", "thinking": "check the tool", "signature": "sig-abc",
                  "id": "toolu_1", "name": "weather", "input": {} },
                { "type": "text", "text": "checking" }
            ] },
            { "role": "user", "content": [
                { "type": "tool_result", "tool_use_id": "toolu_1", "content": "sunny" }
            ] }
        ]
    });
    let request: MessagesRequest = serde_json::from_value(document).unwrap();
    let Operation::Generation(first) = decode_request(request).unwrap() else {
        panic!("expected a generation");
    };
    let encoded = serde_json::to_value(encode_request(&first, "upstream").unwrap()).unwrap();
    let reparsed: MessagesRequest = serde_json::from_value(encoded.clone()).unwrap();
    let Operation::Generation(second) = decode_request(reparsed).unwrap() else {
        panic!("the encoder's own output must decode to the same operation");
    };
    let re_encoded = serde_json::to_value(encode_request(&second, "upstream").unwrap()).unwrap();
    assert_eq!(encoded, re_encoded);
    assert_eq!(encoded["messages"][1]["content"][0]["type"], "thinking");
    assert_eq!(encoded["messages"][1]["content"][0]["id"], "toolu_1");
}

#[test]
fn document_block_with_base64_source_is_rejected_by_decoder() {
    let dto: MessagesRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "max_tokens": 256,
        "messages": [
            {
                "role": "user",
                "content": [
                    {
                        "type": "document",
                        "source": {
                            "type": "base64",
                            "media_type": "application/pdf",
                            "data": "JVBERi0xLjQK"
                        }
                    }
                ]
            }
        ]
    }))
    .unwrap();
    assert!(decode_request(dto).is_err());
}

#[test]
fn document_block_with_url_source_round_trips_through_canonical() {
    let dto: MessagesRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "max_tokens": 256,
        "messages": [
            {
                "role": "user",
                "content": [
                    {
                        "type": "document",
                        "source": {
                            "type": "url",
                            "url": "https://example.com/spec.pdf"
                        }
                    }
                ]
            }
        ]
    }))
    .unwrap();
    let Operation::Generation(canonical) = decode_request(dto).unwrap() else {
        panic!("wrong operation");
    };
    let wire =
        serde_json::to_value(encode_request(&canonical, "claude-upstream").unwrap()).unwrap();
    let document = &wire["messages"][0]["content"][0];
    assert_eq!(document["type"], "document");
    assert_eq!(document["source"]["type"], "url");
    assert_eq!(document["source"]["url"], "https://example.com/spec.pdf");
}

#[test]
fn unrecognised_content_block_type_is_rejected() {
    let dto: MessagesRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "max_tokens": 256,
        "messages": [
            {
                "role": "user",
                "content": [
                    {
                        "type": "container_upload",
                        "container_id": "cnt_123"
                    }
                ]
            }
        ]
    }))
    .unwrap();
    assert!(decode_request(dto).is_err());
}

#[test]
fn document_block_nesting_base64_content_is_rejected() {
    let dto: MessagesRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "max_tokens": 256,
        "messages": [
            {
                "role": "user",
                "content": [
                    {
                        "type": "document",
                        "source": {
                            "type": "content",
                            "content": [
                                {
                                    "type": "image",
                                    "source": {
                                        "type": "base64",
                                        "media_type": "image/png",
                                        "data": "AAAA"
                                    }
                                }
                            ]
                        }
                    }
                ]
            }
        ]
    }))
    .unwrap();
    assert!(decode_request(dto).is_err());
}

#[test]
fn count_tokens_rejects_a_base64_document_like_the_messages_surface() {
    let dto: CountTokensRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "messages": [
            {
                "role": "user",
                "content": [
                    {
                        "type": "document",
                        "source": {
                            "type": "base64",
                            "media_type": "application/pdf",
                            "data": "JVBERi0xLjQK"
                        }
                    }
                ]
            }
        ]
    }))
    .unwrap();
    assert!(decode_count_tokens_request(dto).is_err());
}

/// Raw passthrough replays every frame that does not carry the model
/// byte-for-byte, including its original key order and whitespace; only
/// `message_start` is re-serialised to swap in the public model.
#[test]
fn raw_passthrough_replays_frames_without_a_model_byte_for_byte() {
    let raw_delta = r#"{"type": "content_block_delta",  "index":0, "delta":{"text":"hi ","type":"text_delta"}}"#;
    let wire = [
        sse(
            "message_start",
            json!({"type": "message_start", "message": {
                "id": "msg_1", "type": "message", "role": "assistant", "model": "claude-upstream",
                "content": [], "stop_reason": null, "usage": {"input_tokens": 1, "output_tokens": 0}
            }}),
        ),
        sse(
            "content_block_start",
            json!({"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
        ),
        format!("event: content_block_delta\ndata: {raw_delta}\n\n"),
    ]
    .concat();
    let mut decoder = Decoder::with_max_event_bytes_and_raw_passthrough(1024 * 1024, true);
    let events = decoder.push(wire.as_bytes()).unwrap();

    let mut encoder = Encoder::new("public-route", "fallback");
    let frames = events
        .into_iter()
        .flat_map(|event| encoder.push(event).unwrap())
        .collect::<Vec<_>>();
    assert_eq!(frames.len(), 3);
    assert!(frames[0].data.contains("\"model\":\"public-route\""));
    assert_eq!(frames[2].data, raw_delta);
    assert_eq!(frames[2].event.as_deref(), Some("content_block_delta"));
}

#[test]
fn raw_passthrough_rewrites_an_escaped_model_key() {
    let raw_start = r#"{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","m\u006fdel":"claude-upstream","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}"#;
    let wire = format!("event: message_start\ndata: {raw_start}\n\n");
    let mut decoder = Decoder::with_max_event_bytes_and_raw_passthrough(1024 * 1024, true);
    let events = decoder.push(wire.as_bytes()).unwrap();

    let mut encoder = Encoder::new("public-route", "fallback");
    let frames = events
        .into_iter()
        .flat_map(|event| encoder.push(event).unwrap())
        .collect::<Vec<_>>();
    assert_eq!(frames.len(), 1);
    let output: Value = serde_json::from_str(&frames[0].data).unwrap();
    assert_eq!(output["message"]["model"], "public-route");
}

use olp_engine::domain::canonical::{
    events::{FinishReason, Kind, validate_event_sequence},
    requests::MessageRole,
};
use olp_engine::protocols::anthropic::{
    client_stream::Encoder,
    dto::MessagesResponse,
    stream::{Decoder, Error as StreamError},
    translate::response::decode as decode_response,
};
use serde_json::{Value, json};

use super::common::{assistant_events, sse};

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

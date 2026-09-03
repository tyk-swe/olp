use olp_engine::domain::canonical::{
    events::{FinishReason, Kind, validate_event_sequence},
    identity::Surface,
    requests::{MessageRole, Operation},
};
use olp_engine::protocols::anthropic::{
    client_stream::Encoder,
    count::{decode_count_tokens_request, encode_count_tokens_result},
    dto::{CountTokensRequest, CountTokensResponse},
    stream::Decoder,
};
use serde_json::{Value, json};

use super::common::sse;

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
#[test]
fn native_anthropic_errors_remain_visible_to_failover() {
    let wire = sse(
        "error",
        json!({
            "type": "error",
            "error": {
                "type": "overloaded_error",
                "message": "try another target",
                "request_id": "provider-request"
            }
        }),
    );
    let mut decoder = Decoder::with_max_event_bytes_and_raw_passthrough(1024 * 1024, true);
    let events = decoder.push(wire.as_bytes()).unwrap();

    validate_event_sequence(&events).unwrap();
    assert!(matches!(
        &events[0].kind,
        Kind::Error { error } if error.retryable
    ));
    assert!(matches!(events[1].kind, Kind::SourceExtension { .. }));
    assert!(matches!(events[2].kind, Kind::Done));
    assert_eq!(events.len(), 3);
}
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

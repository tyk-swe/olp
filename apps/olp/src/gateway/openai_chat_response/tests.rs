use std::collections::BTreeMap;

use axum::body::Bytes;
use olp_engine::domain::canonical::{
    events::{Error, ErrorClass, Event, FinishReason, Kind, Usage, validate_event_sequence},
    identity::Surface,
    requests::{MessageRole, SourceExtensions},
};
use olp_engine::protocols::openai::response::{Decoder, OpenAiStreamError};
use serde_json::{Value, json};

use super::{
    OpenAiChatCompletionStreamEncoder, aggregate_chat_completion_response, set_json_pointer,
};
use crate::gateway::openai_http::unix_seconds;

fn only_frame(mut frames: Vec<Bytes>) -> Bytes {
    assert_eq!(frames.len(), 1);
    frames.pop().unwrap()
}

fn sse_json_value(frame: &Bytes) -> Value {
    let bytes = frame.as_ref();
    assert!(bytes.starts_with(b"data: "));
    assert!(bytes.ends_with(b"\n\n"));
    serde_json::from_slice(&bytes[b"data: ".len()..bytes.len() - b"\n\n".len()]).unwrap()
}

fn assert_created_within_window_and_remove(value: &mut Value, before: i64, after: i64) -> i64 {
    let created = value
        .get("created")
        .and_then(Value::as_i64)
        .expect("OpenAI response must include an integer created timestamp");
    assert!(
        (before..=after).contains(&created),
        "created timestamp {created} was outside [{before}, {after}]"
    );
    value
        .as_object_mut()
        .expect("OpenAI response must be a JSON object")
        .remove("created");
    created
}

fn assert_sse_chunk(frame: &Bytes, before: i64, after: i64, expected: Value) -> i64 {
    let mut actual = sse_json_value(frame);
    let created = assert_created_within_window_and_remove(&mut actual, before, after);
    assert_eq!(actual, expected);
    created
}

fn join_sse_frames(frames: &[Bytes]) -> Vec<u8> {
    frames
        .iter()
        .flat_map(|frame| frame.iter().copied())
        .collect()
}

#[test]
fn stream_encoder_new_emits_semantic_sse_frames_and_round_trips_success_stream() {
    let request_id = uuid::Uuid::from_u128(0x1234_5678_1234_5678_1234_5678_1234_5678);
    let before = unix_seconds();
    let mut encoder = OpenAiChatCompletionStreamEncoder::new(request_id, "route-model");
    let after = unix_seconds();
    assert!(
        encoder
            .encode(Event::new(
                0,
                Kind::ResponseStart {
                    response_id: None,
                    provider_model: Some("upstream-model".to_owned()),
                },
            ))
            .unwrap()
            .is_empty()
    );

    let message_start = only_frame(
        encoder
            .encode(Event::new(
                1,
                Kind::MessageStart {
                    output_index: 0,
                    role: MessageRole::Assistant,
                },
            ))
            .unwrap(),
    );
    let created = assert_sse_chunk(
        &message_start,
        before,
        after,
        json!({
            "id": "chatcmpl-12345678-1234-5678-1234-567812345678",
            "object": "chat.completion.chunk",
            "model": "route-model",
            "choices": [{
                "index": 0,
                "delta": {"role": "assistant"},
                "finish_reason": null
            }]
        }),
    );

    let text_delta = only_frame(
        encoder
            .encode(Event::new(
                2,
                Kind::TextDelta {
                    output_index: 0,
                    text: "hello".to_owned(),
                },
            ))
            .unwrap(),
    );
    assert_eq!(
        assert_sse_chunk(
            &text_delta,
            before,
            after,
            json!({
                "id": "chatcmpl-12345678-1234-5678-1234-567812345678",
                "object": "chat.completion.chunk",
                "model": "route-model",
                "choices": [{
                    "index": 0,
                    "delta": {"content": "hello"},
                    "finish_reason": null
                }]
            }),
        ),
        created
    );

    let finish = only_frame(
        encoder
            .encode(Event::new(
                3,
                Kind::Finish {
                    output_index: 0,
                    reason: FinishReason::Stop,
                },
            ))
            .unwrap(),
    );
    assert_eq!(
        assert_sse_chunk(
            &finish,
            before,
            after,
            json!({
                "id": "chatcmpl-12345678-1234-5678-1234-567812345678",
                "object": "chat.completion.chunk",
                "model": "route-model",
                "choices": [{
                    "index": 0,
                    "delta": {},
                    "finish_reason": "stop"
                }]
            }),
        ),
        created
    );

    let done = only_frame(encoder.encode(Event::new(4, Kind::Done)).unwrap());
    assert_eq!(done, Bytes::from_static(b"data: [DONE]\n\n"));

    let mut decoder = Decoder::new();
    let mut decoded = decoder
        .push(&join_sse_frames(&[message_start, text_delta, finish, done]))
        .unwrap();
    decoded.extend(decoder.finish().unwrap());
    assert!(decoder.is_done());
    assert_eq!(
        decoded,
        vec![
            Event::new(
                0,
                Kind::ResponseStart {
                    response_id: Some("chatcmpl-12345678-1234-5678-1234-567812345678".to_owned(),),
                    provider_model: Some("route-model".to_owned()),
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
                    text: "hello".to_owned(),
                },
            ),
            Event::new(
                3,
                Kind::Finish {
                    output_index: 0,
                    reason: FinishReason::Stop,
                },
            ),
            Event::new(4, Kind::Done),
        ]
    );
}

#[test]
fn stream_encoder_preserves_tool_usage_finish_extension_and_done_frames() {
    let before = unix_seconds();
    let mut encoder = OpenAiChatCompletionStreamEncoder::new(
        uuid::Uuid::from_u128(0x1234_5678_1234_5678_1234_5678_1234_5678),
        "route-model",
    );
    let after = unix_seconds();
    assert!(
        encoder
            .encode(Event::new(
                0,
                Kind::ResponseStart {
                    response_id: Some("chatcmpl-upstream".to_owned()),
                    provider_model: Some("upstream-model".to_owned()),
                },
            ))
            .unwrap()
            .is_empty()
    );

    let fixtures = [
        (
            Event::new(
                1,
                Kind::MessageStart {
                    output_index: 0,
                    role: MessageRole::Assistant,
                },
            ),
            json!({
                "id": "chatcmpl-upstream",
                "object": "chat.completion.chunk",
                "model": "route-model",
                "choices": [{
                    "index": 0,
                    "delta": {"role": "assistant"},
                    "finish_reason": null
                }]
            }),
        ),
        (
            Event::new(
                2,
                Kind::ToolCallDelta {
                    output_index: 0,
                    tool_index: 0,
                    id: Some("call_weather".to_owned()),
                    name: Some("weather".to_owned()),
                    arguments_delta: "{\"city\":".to_owned(),
                },
            ),
            json!({
                "id": "chatcmpl-upstream",
                "object": "chat.completion.chunk",
                "model": "route-model",
                "choices": [{
                    "index": 0,
                    "delta": {"tool_calls": [{
                        "index": 0,
                        "id": "call_weather",
                        "type": "function",
                        "function": {"name": "weather", "arguments": "{\"city\":"}
                    }]},
                    "finish_reason": null
                }]
            }),
        ),
        (
            Event::new(
                3,
                Kind::ToolCallDelta {
                    output_index: 0,
                    tool_index: 0,
                    id: None,
                    name: None,
                    arguments_delta: "\"Paris\"}".to_owned(),
                },
            ),
            json!({
                "id": "chatcmpl-upstream",
                "object": "chat.completion.chunk",
                "model": "route-model",
                "choices": [{
                    "index": 0,
                    "delta": {"tool_calls": [{
                        "index": 0,
                        "id": null,
                        "type": "function",
                        "function": {"name": null, "arguments": "\"Paris\"}"}
                    }]},
                    "finish_reason": null
                }]
            }),
        ),
        (
            Event::new(
                4,
                Kind::ToolCallDelta {
                    output_index: 0,
                    tool_index: 1,
                    id: Some("call_lookup".to_owned()),
                    name: Some("lookup".to_owned()),
                    arguments_delta: "{}".to_owned(),
                },
            ),
            json!({
                "id": "chatcmpl-upstream",
                "object": "chat.completion.chunk",
                "model": "route-model",
                "choices": [{
                    "index": 0,
                    "delta": {"tool_calls": [{
                        "index": 1,
                        "id": "call_lookup",
                        "type": "function",
                        "function": {"name": "lookup", "arguments": "{}"}
                    }]},
                    "finish_reason": null
                }]
            }),
        ),
        (
            Event::new(
                5,
                Kind::Usage {
                    usage: Usage {
                        input_tokens: 21,
                        output_tokens: 8,
                        total_tokens: 29,
                        cached_input_tokens: Some(3),
                        reasoning_tokens: Some(2),
                    },
                },
            ),
            json!({
                "id": "chatcmpl-upstream",
                "object": "chat.completion.chunk",
                "model": "route-model",
                "choices": [],
                "usage": {
                    "prompt_tokens": 21,
                    "completion_tokens": 8,
                    "total_tokens": 29,
                    "prompt_tokens_details": {"cached_tokens": 3},
                    "completion_tokens_details": {"reasoning_tokens": 2}
                }
            }),
        ),
        (
            Event::new(
                6,
                Kind::SourceExtension {
                    extensions: SourceExtensions::new(
                        Surface::OpenAi,
                        BTreeMap::from([("/system_fingerprint".to_owned(), json!("fp_fixture"))]),
                    ),
                },
            ),
            json!({
                "id": "chatcmpl-upstream",
                "object": "chat.completion.chunk",
                "model": "route-model",
                "choices": [],
                "system_fingerprint": "fp_fixture"
            }),
        ),
    ];
    let mut frames = Vec::new();
    for (event, expected) in fixtures {
        let frame = only_frame(encoder.encode(event).unwrap());
        assert_sse_chunk(&frame, before, after, expected);
        frames.push(frame);
    }

    for (output_index, reason, expected_reason) in [
        (0, FinishReason::Stop, "stop"),
        (1, FinishReason::Length, "length"),
        (2, FinishReason::ToolCalls, "tool_calls"),
        (3, FinishReason::ContentFilter, "content_filter"),
        (4, FinishReason::Error, "error"),
        (
            5,
            FinishReason::Other("provider_stop".to_owned()),
            "provider_stop",
        ),
    ] {
        let frame = only_frame(
            encoder
                .encode(Event::new(
                    7 + u64::from(output_index),
                    Kind::Finish {
                        output_index,
                        reason,
                    },
                ))
                .unwrap(),
        );
        assert_sse_chunk(
            &frame,
            before,
            after,
            json!({
                "id": "chatcmpl-upstream",
                "object": "chat.completion.chunk",
                "model": "route-model",
                "choices": [{
                    "index": output_index,
                    "delta": {},
                    "finish_reason": expected_reason
                }]
            }),
        );
        frames.push(frame);
    }

    let done = only_frame(encoder.encode(Event::new(13, Kind::Done)).unwrap());
    assert_eq!(done, Bytes::from_static(b"data: [DONE]\n\n"));
    frames.push(done);

    let mut decoder = Decoder::new();
    let mut decoded = decoder.push(&join_sse_frames(&frames)).unwrap();
    decoded.extend(decoder.finish().unwrap());
    validate_event_sequence(&decoded).unwrap();
    assert!(decoder.is_done());
    assert!(matches!(
        &decoded[0].kind,
        Kind::ResponseStart {
            response_id: Some(response_id),
            provider_model: Some(model),
        } if response_id == "chatcmpl-upstream" && model == "route-model"
    ));
    assert!(matches!(&decoded.last().unwrap().kind, Kind::Done));
}

#[test]
fn stream_encoder_error_frame_is_terminal() {
    let mut encoder = OpenAiChatCompletionStreamEncoder::new(
        uuid::Uuid::from_u128(0x1234_5678_1234_5678_1234_5678_1234_5678),
        "route-model",
    );
    let error_frame = only_frame(
        encoder
            .encode(Event::new(
                0,
                Kind::Error {
                    error: Error {
                        class: ErrorClass::RateLimit,
                        message: "provider throttled".to_owned(),
                        provider_code: Some("rate_limited".to_owned()),
                        retryable: true,
                    },
                },
            ))
            .unwrap(),
    );
    assert_eq!(
        sse_json_value(&error_frame),
        json!({
            "error": {
                "message": "provider throttled",
                "type": "rate_limit_error",
                "code": "rate_limited"
            }
        })
    );

    let mut decoder = Decoder::new();
    let events = decoder.push(&join_sse_frames(&[error_frame])).unwrap();
    assert_eq!(
        events,
        vec![
            Event::new(
                0,
                Kind::Error {
                    error: Error {
                        class: ErrorClass::RateLimit,
                        message: "provider throttled".to_owned(),
                        provider_code: Some("rate_limited".to_owned()),
                        retryable: true,
                    },
                },
            ),
            Event::new(1, Kind::Done),
        ]
    );
    assert!(decoder.is_done());
    assert!(decoder.finish().unwrap().is_empty());
    assert!(matches!(
        decoder.push(b"data: [DONE]\n\n"),
        Err(OpenAiStreamError::DataAfterDone)
    ));
}

#[test]
fn unary_aggregation_preserves_openai_json() {
    let events = vec![
        Event::new(
            0,
            Kind::ResponseStart {
                response_id: Some("chatcmpl-upstream".to_owned()),
                provider_model: Some("upstream-model".to_owned()),
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
                text: "hello ".to_owned(),
            },
        ),
        Event::new(
            3,
            Kind::TextDelta {
                output_index: 0,
                text: "world".to_owned(),
            },
        ),
        Event::new(
            4,
            Kind::RefusalDelta {
                output_index: 0,
                text: "not refused".to_owned(),
            },
        ),
        Event::new(
            5,
            Kind::ToolCallDelta {
                output_index: 0,
                tool_index: 0,
                id: Some("call_123".to_owned()),
                name: Some("weather".to_owned()),
                arguments_delta: "{\"city\":\"Paris\"}".to_owned(),
            },
        ),
        Event::new(
            6,
            Kind::Finish {
                output_index: 0,
                reason: FinishReason::ToolCalls,
            },
        ),
        Event::new(
            7,
            Kind::Usage {
                usage: Usage {
                    input_tokens: 8,
                    output_tokens: 5,
                    total_tokens: 13,
                    cached_input_tokens: Some(2),
                    reasoning_tokens: Some(1),
                },
            },
        ),
        Event::new(
            8,
            Kind::SourceExtension {
                extensions: SourceExtensions::new(
                    Surface::OpenAi,
                    BTreeMap::from([(
                        "/choices/0/message/vendor_trace".to_owned(),
                        json!({"kept": true}),
                    )]),
                ),
            },
        ),
        Event::new(9, Kind::Done),
    ];

    let before = unix_seconds();
    let mut response =
        aggregate_chat_completion_response(uuid::Uuid::nil(), "route-model", &events).unwrap();
    let after = unix_seconds();
    assert_created_within_window_and_remove(&mut response, before, after);

    assert_eq!(
        response,
        json!({
            "id": "chatcmpl-upstream",
            "object": "chat.completion",
            "model": "route-model",
            "choices": [{
                "index": 0,
                "message": {
                    "role": "assistant",
                    "content": "hello world",
                    "refusal": "not refused",
                    "tool_calls": [{
                        "id": "call_123",
                        "type": "function",
                        "function": {
                            "name": "weather",
                            "arguments": "{\"city\":\"Paris\"}"
                        }
                    }],
                    "vendor_trace": {"kept": true}
                },
                "finish_reason": "tool_calls"
            }],
            "usage": {
                "prompt_tokens": 8,
                "completion_tokens": 5,
                "total_tokens": 13,
                "prompt_tokens_details": {"cached_tokens": 2},
                "completion_tokens_details": {"reasoning_tokens": 1}
            }
        })
    );
}

#[test]
fn unary_aggregation_preserves_multiple_choices_and_tool_calls() {
    let events = vec![
        Event::new(
            0,
            Kind::ResponseStart {
                response_id: None,
                provider_model: Some("upstream-model".to_owned()),
            },
        ),
        Event::new(
            1,
            Kind::MessageStart {
                output_index: 1,
                role: MessageRole::Assistant,
            },
        ),
        Event::new(
            2,
            Kind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            },
        ),
        Event::new(
            3,
            Kind::TextDelta {
                output_index: 1,
                text: "second choice".to_owned(),
            },
        ),
        Event::new(
            4,
            Kind::TextDelta {
                output_index: 0,
                text: "first ".to_owned(),
            },
        ),
        Event::new(
            5,
            Kind::TextDelta {
                output_index: 0,
                text: "choice".to_owned(),
            },
        ),
        Event::new(
            6,
            Kind::ToolCallDelta {
                output_index: 0,
                tool_index: 1,
                id: Some("call_lookup".to_owned()),
                name: Some("lookup".to_owned()),
                arguments_delta: "{\"query\":\"rust\"}".to_owned(),
            },
        ),
        Event::new(
            7,
            Kind::ToolCallDelta {
                output_index: 0,
                tool_index: 0,
                id: Some("call_weather".to_owned()),
                name: Some("weather".to_owned()),
                arguments_delta: "{\"city\":".to_owned(),
            },
        ),
        Event::new(
            8,
            Kind::ToolCallDelta {
                output_index: 0,
                tool_index: 0,
                id: None,
                name: None,
                arguments_delta: "\"Paris\"}".to_owned(),
            },
        ),
        Event::new(
            9,
            Kind::ToolCallDelta {
                output_index: 1,
                tool_index: 0,
                id: Some("call_search".to_owned()),
                name: Some("search".to_owned()),
                arguments_delta: "{\"q\":\"fixtures\"}".to_owned(),
            },
        ),
        Event::new(
            10,
            Kind::Finish {
                output_index: 0,
                reason: FinishReason::ToolCalls,
            },
        ),
        Event::new(
            11,
            Kind::Finish {
                output_index: 1,
                reason: FinishReason::Length,
            },
        ),
        Event::new(
            12,
            Kind::Usage {
                usage: Usage {
                    input_tokens: 34,
                    output_tokens: 13,
                    total_tokens: 47,
                    cached_input_tokens: Some(5),
                    reasoning_tokens: Some(3),
                },
            },
        ),
        Event::new(
            13,
            Kind::SourceExtension {
                extensions: SourceExtensions::new(
                    Surface::OpenAi,
                    BTreeMap::from([
                        (
                            "/choices/0/message/vendor_call_trace".to_owned(),
                            json!({"attempt": 1}),
                        ),
                        ("/choices/1/message/vendor_rank".to_owned(), json!(2)),
                        ("/system_fingerprint".to_owned(), json!("fp_fixture")),
                    ]),
                ),
            },
        ),
        Event::new(14, Kind::Done),
    ];

    let request_id = uuid::Uuid::from_u128(0x1234_5678_1234_5678_1234_5678_1234_5678);
    let before = unix_seconds();
    let mut response =
        aggregate_chat_completion_response(request_id, "route-model", &events).unwrap();
    let after = unix_seconds();
    assert_created_within_window_and_remove(&mut response, before, after);

    assert_eq!(
        response,
        json!({
            "id": "chatcmpl-12345678-1234-5678-1234-567812345678",
            "object": "chat.completion",
            "model": "route-model",
            "choices": [
                {
                    "index": 0,
                    "message": {
                        "role": "assistant",
                        "content": "first choice",
                        "refusal": null,
                        "tool_calls": [
                            {
                                "id": "call_weather",
                                "type": "function",
                                "function": {
                                    "name": "weather",
                                    "arguments": "{\"city\":\"Paris\"}"
                                }
                            },
                            {
                                "id": "call_lookup",
                                "type": "function",
                                "function": {
                                    "name": "lookup",
                                    "arguments": "{\"query\":\"rust\"}"
                                }
                            }
                        ],
                        "vendor_call_trace": {"attempt": 1}
                    },
                    "finish_reason": "tool_calls"
                },
                {
                    "index": 1,
                    "message": {
                        "role": "assistant",
                        "content": "second choice",
                        "refusal": null,
                        "tool_calls": [{
                            "id": "call_search",
                            "type": "function",
                            "function": {
                                "name": "search",
                                "arguments": "{\"q\":\"fixtures\"}"
                            }
                        }],
                        "vendor_rank": 2
                    },
                    "finish_reason": "length"
                }
            ],
            "usage": {
                "prompt_tokens": 34,
                "completion_tokens": 13,
                "total_tokens": 47,
                "prompt_tokens_details": {"cached_tokens": 5},
                "completion_tokens_details": {"reasoning_tokens": 3}
            },
            "system_fingerprint": "fp_fixture"
        })
    );
}

#[test]
fn source_extension_pointer_materializes_nested_arrays_without_loss() {
    let mut value = json!({ "choices": [] });
    set_json_pointer(
        &mut value,
        "/choices/2/delta/vendor_field",
        json!({ "preserved": true }),
    )
    .unwrap();
    assert_eq!(value["choices"][2]["index"], 2);
    assert_eq!(
        value["choices"][2]["delta"]["vendor_field"]["preserved"],
        true
    );
}

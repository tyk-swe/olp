use super::*;

#[test]
fn stream_encoder_new_emits_semantic_sse_frames_and_round_trips_success_stream() {
    let request_id = uuid::Uuid::from_u128(0x1234_5678_1234_5678_1234_5678_1234_5678);
    let before = unix_seconds();
    let mut encoder = OpenAiChatCompletionStreamEncoder::new(request_id, "route-model", false);
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
        true,
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
            // Real OpenAI omits `id`, `type`, and `name` on continuation
            // chunks: an accumulator that assigns unconditionally would
            // otherwise clobber the id with null.
            json!({
                "id": "chatcmpl-upstream",
                "object": "chat.completion.chunk",
                "model": "route-model",
                "choices": [{
                    "index": 0,
                    "delta": {"tool_calls": [{
                        "index": 0,
                        "function": {"arguments": "\"Paris\"}"}
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

    // A provider may report running totals on every chunk; OpenAI sends one
    // usage-only chunk, immediately before `[DONE]`, so the encoder buffers it.
    assert!(
        encoder
            .encode(Event::new(
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
            ))
            .unwrap()
            .is_empty()
    );

    // An unrecognized `Other` value fails a strictly typed SDK on an otherwise
    // successful response, so only that case is clamped to `stop`; a real
    // OpenAI literal arriving as `Other` still passes through, and a failed
    // turn reports `error` instead of masquerading as a clean stop.
    for (output_index, reason, expected_reason) in [
        (0, FinishReason::Stop, "stop"),
        (1, FinishReason::Length, "length"),
        (2, FinishReason::ToolCalls, "tool_calls"),
        (3, FinishReason::ContentFilter, "content_filter"),
        (4, FinishReason::Error, "error"),
        (5, FinishReason::Other("provider_stop".to_owned()), "stop"),
        (
            6,
            FinishReason::Other("function_call".to_owned()),
            "function_call",
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

    let mut terminal = encoder.encode(Event::new(14, Kind::Done)).unwrap();
    let done = terminal.pop().unwrap();
    let usage = terminal.pop().unwrap();
    assert!(terminal.is_empty());
    assert_sse_chunk(
        &usage,
        before,
        after,
        json!({
            "id": "chatcmpl-upstream",
            "object": "chat.completion.chunk",
            "model": "route-model",
            "choices": [],
            "usage": {
                // Canonical `output_tokens` excludes reasoning; OpenAI's
                // `completion_tokens` includes it.
                "prompt_tokens": 21,
                "completion_tokens": 10,
                "total_tokens": 29,
                "prompt_tokens_details": {"cached_tokens": 3},
                "completion_tokens_details": {"reasoning_tokens": 2}
            }
        }),
    );
    assert_eq!(done, Bytes::from_static(b"data: [DONE]\n\n"));
    frames.push(usage);
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
        true,
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

/// A18: nothing read the client's `stream_options.include_usage`, so every
/// stream ended with an unrequested `{"choices": [], "usage": …}` chunk — which
/// breaks the very common `chunk.choices[0].delta` loop with an IndexError.
#[test]
fn the_trailing_usage_chunk_appears_only_when_the_client_asked_for_it() {
    for include_usage in [false, true] {
        let mut encoder =
            OpenAiChatCompletionStreamEncoder::new(uuid::Uuid::nil(), "route-model", include_usage);
        let mut frames = Vec::new();
        for event in [
            Event::new(
                0,
                Kind::MessageStart {
                    output_index: 0,
                    role: MessageRole::Assistant,
                },
            ),
            Event::new(
                1,
                Kind::TextDelta {
                    output_index: 0,
                    text: "hi".to_owned(),
                },
            ),
            // Cumulative per-chunk usage, as Gemini reports it.
            Event::new(
                2,
                Kind::Usage {
                    usage: Usage {
                        input_tokens: 7,
                        output_tokens: 1,
                        total_tokens: 8,
                        cached_input_tokens: None,
                        reasoning_tokens: None,
                    },
                },
            ),
            Event::new(
                3,
                Kind::Usage {
                    usage: Usage {
                        input_tokens: 7,
                        output_tokens: 2,
                        total_tokens: 9,
                        cached_input_tokens: None,
                        reasoning_tokens: None,
                    },
                },
            ),
            Event::new(
                4,
                Kind::Finish {
                    output_index: 0,
                    reason: FinishReason::Stop,
                },
            ),
            Event::new(5, Kind::Done),
        ] {
            frames.extend(encoder.encode(event).unwrap());
        }
        let body = String::from_utf8(join_sse_frames(&frames)).unwrap();
        assert_eq!(
            body.contains("\"total_tokens\":9"),
            include_usage,
            "include_usage = {include_usage}"
        );
        // A20: Gemini attaches running totals to nearly every chunk. Only the
        // last one is forwarded, so a summing consumer cannot multiply it.
        assert!(body.matches("\"prompt_tokens\"").count() <= 1);
        // Every emitted chunk still carries at least one choice when usage was
        // not requested, so `choices[0]` is always safe.
        if !include_usage {
            assert!(!body.contains("\"choices\":[]"));
        }
    }
}

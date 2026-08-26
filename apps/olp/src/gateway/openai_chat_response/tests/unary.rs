use super::*;

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
                // Canonical totals: input + output + reasoning.
                usage: Usage {
                    input_tokens: 8,
                    output_tokens: 5,
                    total_tokens: 14,
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
                "completion_tokens": 6,
                "total_tokens": 14,
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
                    total_tokens: 50,
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
                "completion_tokens": 16,
                "total_tokens": 50,
                "prompt_tokens_details": {"cached_tokens": 5},
                "completion_tokens_details": {"reasoning_tokens": 3}
            },
            "system_fingerprint": "fp_fixture"
        })
    );
}

/// A1: `safetyRatings`, `avgLogprobs`, `groundingMetadata`, and
/// `promptTokensDetails` are on essentially every real Gemini response and all
/// land in a Gemini-surface extension. Rejecting them returned
/// `502 provider_protocol_error` to every OpenAI-SDK client on a Gemini route —
/// mid-stream when streaming.
#[test]
fn a_real_gemini_response_reaches_an_openai_client() {
    let response: olp_engine::protocols::gemini::dto::GenerateContentResponse =
        serde_json::from_value(json!({
            "responseId": "gemini-response-1",
            "modelVersion": "gemini-2.5-flash",
            "candidates": [{
                "index": 0,
                "content": {"role": "model", "parts": [{"text": "42"}]},
                "finishReason": "STOP",
                "avgLogprobs": -0.31,
                "safetyRatings": [
                    {"category": "HARM_CATEGORY_HATE_SPEECH", "probability": "NEGLIGIBLE"}
                ],
                "groundingMetadata": {"webSearchQueries": ["meaning of life"]}
            }],
            "usageMetadata": {
                "promptTokenCount": 10,
                "candidatesTokenCount": 5,
                "totalTokenCount": 17,
                "thoughtsTokenCount": 2,
                "promptTokensDetails": [{"modality": "TEXT", "tokenCount": 10}]
            }
        }))
        .unwrap();
    let events =
        olp_engine::protocols::gemini::translate::response::decode(response.clone()).unwrap();
    validate_event_sequence(&events).unwrap();
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        Kind::SourceExtension { extensions } if extensions.source == Some(Surface::Gemini)
    )));

    let mut unary =
        aggregate_chat_completion_response(uuid::Uuid::nil(), "route-model", &events).unwrap();
    unary
        .as_object_mut()
        .unwrap()
        .remove("created")
        .expect("created must be present");
    assert_eq!(
        unary,
        json!({
            "id": "gemini-response-1",
            "object": "chat.completion",
            "model": "route-model",
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": "42", "refusal": null},
                "finish_reason": "stop"
            }],
            "usage": {
                "prompt_tokens": 10,
                "completion_tokens": 7,
                "total_tokens": 17,
                "prompt_tokens_details": {"cached_tokens": null},
                "completion_tokens_details": {"reasoning_tokens": 2}
            }
        })
    );

    // The same events on the streaming path must not fail mid-response either.
    let mut encoder =
        OpenAiChatCompletionStreamEncoder::new(uuid::Uuid::nil(), "route-model", true);
    let frames = events
        .into_iter()
        .flat_map(|event| encoder.encode(event).unwrap())
        .collect::<Vec<_>>();
    let body = String::from_utf8(join_sse_frames(&frames)).unwrap();
    assert!(body.contains("\"content\":\"42\""));
    assert!(body.contains("\"finish_reason\":\"stop\""));
    assert!(body.ends_with("data: [DONE]\n\n"));
    // Gemini-only fields never leak onto the OpenAI wire.
    assert!(!body.contains("safetyRatings"));
    assert!(!body.contains("groundingMetadata"));
}

/// A21: OpenAI returns `"content": null` and omits `tool_calls` when empty.
/// Echoing `content: ""` back through the gateway to an Anthropic upstream
/// produces an empty text block, which Anthropic rejects.
#[test]
fn a_tool_only_completion_reports_null_content_and_omits_empty_tool_calls() {
    let tool_only = vec![
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
                id: Some("call_1".to_owned()),
                name: Some("weather".to_owned()),
                arguments_delta: "{}".to_owned(),
            },
        ),
        Event::new(
            2,
            Kind::Finish {
                output_index: 0,
                reason: FinishReason::ToolCalls,
            },
        ),
        Event::new(3, Kind::Done),
    ];
    let response =
        aggregate_chat_completion_response(uuid::Uuid::nil(), "route-model", &tool_only).unwrap();
    assert_eq!(response["choices"][0]["message"]["content"], Value::Null);
    assert_eq!(
        response["choices"][0]["message"]["tool_calls"][0]["id"],
        "call_1"
    );

    let text_only = vec![
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
                text: "hello".to_owned(),
            },
        ),
        Event::new(
            2,
            Kind::Finish {
                output_index: 0,
                reason: FinishReason::Stop,
            },
        ),
        Event::new(3, Kind::Done),
    ];
    let response =
        aggregate_chat_completion_response(uuid::Uuid::nil(), "route-model", &text_only).unwrap();
    assert_eq!(response["choices"][0]["message"]["content"], "hello");
    assert!(
        response["choices"][0]["message"]
            .as_object()
            .unwrap()
            .get("tool_calls")
            .is_none()
    );
}

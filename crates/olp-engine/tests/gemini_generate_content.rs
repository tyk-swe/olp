use olp_engine::domain::{
    CanonicalEventKind, FinishReason, MessageRole, Operation, ResponseFormat, Surface,
    validate_event_sequence,
};
use olp_engine::protocols::gemini::{
    CountTokensError, CountTokensRequest, CountTokensResponse,
    GeminiGenerateContentClientStreamEncoder, GeminiGenerateContentStreamDecoder,
    GenerateContentRequest, GenerateContentResponse, StreamError, decode_count_tokens_request,
    decode_generate_content_request, decode_generate_content_response, encode_count_tokens_result,
    encode_generate_content_request, encode_generate_content_response,
    validate_count_tokens_request,
};
use serde_json::{Value, json};

#[test]
fn request_translation_round_trips_structured_tools_results_and_extensions() {
    let wire = json!({
        "systemInstruction": {
            "parts": [{"text": "Be concise", "vendorSystem": true}],
            "vendorInstruction": "kept"
        },
        "contents": [
            {"role": "user", "parts": [{"text": "Weather?", "vendorText": 7}], "vendorTurn": true},
            {"role": "model", "parts": [
                {"text": "I'll check."},
                {"functionCall": {"name": "weather", "args": {"city": "Paris"}, "vendorCall": true}}
            ]},
            {"role": "user", "parts": [{
                "functionResponse": {"name": "weather", "response": {"temperature": 21}, "vendorResult": "kept"}
            }]}
        ],
        "tools": [
            {"functionDeclarations": [
                {"name": "weather", "description": "Weather lookup", "parameters": {"type": "object"}, "vendorDecl": 1},
                {"name": "forecast", "parameters": {"type": "object"}}
            ], "vendorTool": true},
            {"googleSearch": {}}
        ],
        "toolConfig": {
            "functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": ["weather"], "vendorChoice": true}
        },
        "generationConfig": {
            "candidateCount": 1,
            "maxOutputTokens": 256,
            "temperature": 0.5,
            "topP": 0.9,
            "seed": 42,
            "responseMimeType": "application/json",
            "responseSchema": {"type": "object"},
            "topK": 20
        },
        "safetySettings": [{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "BLOCK_ONLY_HIGH"}]
    });
    let dto: GenerateContentRequest = serde_json::from_value(wire).unwrap();
    let Operation::Generation(canonical) =
        decode_generate_content_request("team-gemini", dto, true).unwrap()
    else {
        panic!("wrong operation");
    };

    assert_eq!(canonical.route.as_str(), "team-gemini");
    assert_eq!(canonical.messages.len(), 4);
    assert_eq!(canonical.messages[0].role, MessageRole::System);
    assert_eq!(canonical.messages[3].role, MessageRole::Tool);
    assert_eq!(canonical.messages[3].name.as_deref(), Some("weather"));
    assert_eq!(canonical.messages[2].tool_calls[0].name, "weather");
    assert_eq!(canonical.tools.len(), 2);
    assert_eq!(canonical.parameters.seed, Some(42));
    assert!(canonical.parameters.stream);
    assert!(matches!(
        canonical.response_format,
        Some(ResponseFormat::JsonSchema { .. })
    ));
    assert_eq!(canonical.extensions.source, Some(Surface::Gemini));
    assert!(canonical.extensions.values.contains_key("/safetySettings"));
    assert_eq!(canonical.extensions.values["/generationConfig/topK"], 20);
    assert_eq!(
        canonical.extensions.values["/contents/1/parts/1/functionCall/id"],
        Value::Null
    );
    assert_eq!(
        canonical.extensions.values["/contents/2/parts/0/functionResponse/id"],
        Value::Null
    );

    let mut wrong_source = canonical.clone();
    wrong_source.extensions.source = Some(Surface::Anthropic);
    assert!(encode_generate_content_request(&wrong_source).is_err());

    let encoded = encode_generate_content_request(&canonical).unwrap();
    let encoded = serde_json::to_value(encoded).unwrap();
    assert_eq!(
        encoded["systemInstruction"]["parts"][0]["vendorSystem"],
        true
    );
    assert_eq!(encoded["contents"][0]["parts"][0]["vendorText"], 7);
    assert!(
        encoded["contents"][1]["parts"][1]["functionCall"]
            .as_object()
            .unwrap()
            .get("id")
            .is_none()
    );
    assert!(
        encoded["contents"][2]["parts"][0]["functionResponse"]
            .as_object()
            .unwrap()
            .get("id")
            .is_none()
    );
    assert_eq!(encoded["generationConfig"]["topK"], 20);
    assert_eq!(encoded["tools"].as_array().unwrap().len(), 3);
    assert!(encoded["tools"][2].get("googleSearch").is_some());
    assert_eq!(encoded["safetySettings"][0]["threshold"], "BLOCK_ONLY_HIGH");
}

#[test]
fn request_translation_round_trips_signed_text_and_hidden_thoughts() {
    let wire = json!({
        "contents": [{"role": "model", "parts": [
            {"text": "hidden", "thought": true, "thoughtSignature": "hidden-sig"},
            {"text": "visible", "thoughtSignature": "visible-sig"},
            {"functionCall": {"name": "lookup", "args": {"q": "x"}}, "thoughtSignature": "call-sig"}
        ]}]
    });
    let dto: GenerateContentRequest = serde_json::from_value(wire.clone()).unwrap();
    let Operation::Generation(canonical) =
        decode_generate_content_request("default", dto, false).unwrap()
    else {
        unreachable!();
    };
    assert!(matches!(
        canonical.messages[0].content.as_slice(),
        [olp_engine::domain::ContentPart::Text { text }] if text == "visible"
    ));
    let encoded =
        serde_json::to_value(encode_generate_content_request(&canonical).unwrap()).unwrap();
    assert_eq!(
        encoded["contents"][0]["parts"],
        wire["contents"][0]["parts"]
    );
}

#[test]
fn request_translation_round_trips_a_hidden_thought_only_message() {
    let wire = json!({
        "contents": [
            {"role": "model", "parts": [{
                "text": "hidden",
                "thought": true,
                "thoughtSignature": "hidden-sig"
            }]},
            {"role": "user", "parts": [{"text": "continue"}]}
        ]
    });
    let dto: GenerateContentRequest = serde_json::from_value(wire.clone()).unwrap();
    let Operation::Generation(canonical) =
        decode_generate_content_request("default", dto, false).unwrap()
    else {
        unreachable!();
    };
    assert!(canonical.messages[0].content.is_empty());
    assert!(canonical.messages[0].tool_calls.is_empty());
    let encoded =
        serde_json::to_value(encode_generate_content_request(&canonical).unwrap()).unwrap();
    assert_eq!(encoded["contents"], wire["contents"]);
}

#[test]
fn request_translation_round_trips_media_after_a_hidden_thought() {
    let wire = json!({
        "contents": [{"role": "model", "parts": [
            {"text": "hidden", "thought": true, "thoughtSignature": "hidden-sig"},
            {"fileData": {"mimeType": "image/png", "fileUri": "https://files.example/image"}},
            {"inlineData": {
                "mimeType": "image/jpeg",
                "data": "urn:olp:inline-media:image-handle"
            }}
        ]}]
    });
    let dto: GenerateContentRequest = serde_json::from_value(wire.clone()).unwrap();
    let Operation::Generation(canonical) =
        decode_generate_content_request("default", dto, false).unwrap()
    else {
        unreachable!();
    };

    let encoded =
        serde_json::to_value(encode_generate_content_request(&canonical).unwrap()).unwrap();
    assert_eq!(encoded["contents"], wire["contents"]);
}

#[test]
fn file_media_round_trips_mime_and_inline_media_is_rejected() {
    let file_request: GenerateContentRequest = serde_json::from_value(json!({
        "contents": [{"role": "user", "parts": [{
            "fileData": {"mimeType": "image/png", "fileUri": "https://files.example/image"}
        }]}]
    }))
    .unwrap();
    let Operation::Generation(canonical) =
        decode_generate_content_request("default", file_request, false).unwrap()
    else {
        unreachable!();
    };
    let encoded =
        serde_json::to_value(encode_generate_content_request(&canonical).unwrap()).unwrap();
    assert_eq!(
        encoded["contents"][0]["parts"][0]["fileData"]["mimeType"],
        "image/png"
    );

    let inline_request: GenerateContentRequest = serde_json::from_value(json!({
        "contents": [{"role": "user", "parts": [{
            "inlineData": {"mimeType": "image/png", "data": "AAAA"}
        }]}]
    }))
    .unwrap();
    assert!(decode_generate_content_request("default", inline_request, false).is_err());
}

#[test]
fn unary_response_maps_text_tools_usage_and_preserves_thought_and_safety() {
    let response: GenerateContentResponse = serde_json::from_value(json!({
        "responseId": "response-1",
        "modelVersion": "gemini-upstream",
        "candidates": [{
            "index": 0,
            "content": {"role": "model", "parts": [
                {"text": "private reasoning", "thought": true, "thoughtSignature": "sig"},
                {"text": "Calling weather"},
                {"functionCall": {"id": "call-1", "name": "weather", "args": {"city": "Paris"}}}
            ]},
            "finishReason": "STOP",
            "safetyRatings": [{"category": "HARM_CATEGORY_HATE_SPEECH", "probability": "NEGLIGIBLE"}]
        }],
        "usageMetadata": {
            "promptTokenCount": 10,
            "candidatesTokenCount": 5,
            "totalTokenCount": 17,
            "cachedContentTokenCount": 2,
            "thoughtsTokenCount": 2
        }
    })).unwrap();
    let events = decode_generate_content_response(response).unwrap();
    validate_event_sequence(&events).unwrap();
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::TextDelta { text, .. } if text == "Calling weather"
    )));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::ToolCallDelta { name: Some(name), arguments_delta, .. }
            if name == "weather" && arguments_delta.contains("Paris")
    )));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::SourceExtension { extensions }
            if extensions.values.contains_key("/candidates/0/content/parts/0")
                && extensions.values.contains_key("/candidates/0/safetyRatings")
    )));
    assert!(events.iter().any(|event| matches!(
        event.kind,
        CanonicalEventKind::Usage { usage } if usage.input_tokens == 10
            && usage.output_tokens == 7 && usage.total_tokens == 17
            && usage.reasoning_tokens == Some(2)
    )));
    assert!(events.iter().any(|event| matches!(
        event.kind,
        CanonicalEventKind::Finish {
            reason: FinishReason::Stop,
            ..
        }
    )));
}

#[test]
fn signed_visible_text_is_canonical_and_native_parts_round_trip_in_order() {
    let wire = json!({
        "responseId": "response-1",
        "modelVersion": "gemini-upstream",
        "candidates": [{
            "index": 0,
            "content": {"role": "model", "parts": [
                {"text": "Running code. "},
                {"executableCode": {"language": "PYTHON", "code": "print(1)"}},
                {"codeExecutionResult": {"outcome": "OUTCOME_OK", "output": "1"}},
                {"text": "The answer is 1.", "thoughtSignature": "sig"}
            ]},
            "finishReason": "STOP"
        }]
    });
    let response: GenerateContentResponse = serde_json::from_value(wire.clone()).unwrap();
    let events = decode_generate_content_response(response).unwrap();
    let text = events
        .iter()
        .filter_map(|event| match &event.kind {
            CanonicalEventKind::TextDelta { text, .. } => Some(text.as_str()),
            _ => None,
        })
        .collect::<String>();
    assert_eq!(text, "Running code. The answer is 1.");
    let encoded = serde_json::to_value(
        encode_generate_content_response(&events, "public-route", "fallback").unwrap(),
    )
    .unwrap();
    assert_eq!(
        encoded["candidates"][0]["content"]["parts"],
        wire["candidates"][0]["content"]["parts"]
    );
}

#[test]
fn signed_visible_text_streams_without_native_extensions_cross_surface() {
    let mut decoder = GeminiGenerateContentStreamDecoder::new();
    let mut events = decoder
        .push(
            sse(json!({
                "candidates": [{
                    "index": 0,
                    "content": {"role": "model", "parts": [{
                        "text": "visible answer",
                        "thoughtSignature": "sig"
                    }]},
                    "finishReason": "STOP"
                }]
            }))
            .as_bytes(),
        )
        .unwrap();
    events.extend(decoder.finish().unwrap());
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::TextDelta { text, .. } if text == "visible answer"
    )));
    assert!(
        !events
            .iter()
            .any(|event| matches!(event.kind, CanonicalEventKind::SourceExtension { .. }))
    );
}

#[test]
fn usage_requires_all_counters_and_folds_detailed_components() {
    let response = |usage_metadata: Value| {
        serde_json::from_value(json!({
            "candidates": [{
                "index": 0,
                "content": {"role": "model", "parts": [{"text": "ok"}]},
                "finishReason": "STOP"
            }],
            "usageMetadata": usage_metadata
        }))
        .unwrap()
    };

    for (partial, expected_input, expected_output, expected_total) in [
        (json!({}), None, None, None),
        (
            json!({"promptTokenCount": null, "candidatesTokenCount": 2, "totalTokenCount": 2}),
            None,
            Some(2),
            Some(2),
        ),
        (
            json!({"promptTokenCount": 1, "candidatesTokenCount": 2}),
            Some(1),
            Some(2),
            None,
        ),
    ] {
        let events = decode_generate_content_response(response(partial)).unwrap();
        assert!(
            !events
                .iter()
                .any(|event| matches!(event.kind, CanonicalEventKind::Usage { .. }))
        );
        let observation = events.last().unwrap().usage_observation.unwrap();
        assert_eq!(observation.input_tokens, expected_input);
        assert_eq!(observation.output_tokens, expected_output);
        assert_eq!(observation.total_tokens, expected_total);
    }

    let events = decode_generate_content_response(response(json!({
        "promptTokenCount": 10,
        "candidatesTokenCount": 5,
        "thoughtsTokenCount": 2,
        "toolUsePromptTokenCount": 3,
        "totalTokenCount": 20
    })))
    .unwrap();
    assert!(events.iter().any(|event| matches!(
        event.kind,
        CanonicalEventKind::Usage { usage }
            if usage.input_tokens == 13
                && usage.output_tokens == 7
                && usage.total_tokens == 20
                && usage.reasoning_tokens == Some(2)
    )));

    let encoded = encode_generate_content_response(&events, "public-route", "fallback").unwrap();
    let usage = encoded.usage_metadata.unwrap();
    assert_eq!(usage.prompt_token_count, Some(10));
    assert_eq!(usage.candidates_token_count, Some(5));
    assert_eq!(usage.thoughts_token_count, Some(2));
    assert_eq!(usage.tool_use_prompt_token_count, Some(3));
    assert_eq!(usage.total_token_count, Some(20));

    assert!(
        decode_generate_content_response(response(json!({
            "promptTokenCount": 1,
            "candidatesTokenCount": 2,
            "totalTokenCount": 4
        })))
        .is_err()
    );
}

fn sse(data: Value) -> String {
    format!("data: {data}\n\n")
}

#[test]
fn fragmented_stream_maps_unicode_tool_usage_finish_and_eof_done() {
    let first = json!({
        "responseId": "stream-1",
        "modelVersion": "gemini-upstream",
        "candidates": [{"index": 0, "content": {"role": "model", "parts": [{"text": "héllo "}]}}]
    });
    let second = json!({
        "responseId": "stream-1",
        "modelVersion": "gemini-upstream",
        "candidates": [{
            "index": 0,
            "content": {"role": "model", "parts": [
                {"text": "🌍"},
                {"functionCall": {"name": "weather", "args": {"city": "Paris"}}}
            ]},
            "finishReason": "STOP"
        }],
        "usageMetadata": {"promptTokenCount": 8, "candidatesTokenCount": 4, "totalTokenCount": 12}
    });
    let wire = format!("{}{}", sse(first), sse(second));
    let mut decoder = GeminiGenerateContentStreamDecoder::new();
    let mut events = Vec::new();
    for byte in wire.as_bytes() {
        events.extend(decoder.push(std::slice::from_ref(byte)).unwrap());
    }
    events.extend(decoder.finish().unwrap());

    validate_event_sequence(&events).unwrap();
    assert!(decoder.is_done());
    let text = events
        .iter()
        .filter_map(|event| match &event.kind {
            CanonicalEventKind::TextDelta { text, .. } => Some(text.as_str()),
            _ => None,
        })
        .collect::<String>();
    assert_eq!(text, "héllo 🌍");
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::ToolCallDelta { name: Some(name), arguments_delta, .. }
            if name == "weather" && arguments_delta.contains("Paris")
    )));
    assert!(events.iter().any(|event| matches!(
        event.kind,
        CanonicalEventKind::Usage { usage } if usage.total_tokens == 12
    )));
    assert!(matches!(
        events.last().unwrap().kind,
        CanonicalEventKind::Done
    ));
}

#[test]
fn stream_error_is_terminal_and_missing_finish_reason_is_truncation() {
    let mut decoder = GeminiGenerateContentStreamDecoder::new();
    let events = decoder
        .push(
            sse(json!({
                "error": {"code": 429, "message": "quota", "status": "RESOURCE_EXHAUSTED"}
            }))
            .as_bytes(),
        )
        .unwrap();
    assert!(decoder.is_done());
    assert!(matches!(
        &events[0].kind,
        CanonicalEventKind::Error { error } if error.retryable
    ));
    assert!(matches!(events[1].kind, CanonicalEventKind::Done));

    let mut truncated = GeminiGenerateContentStreamDecoder::new();
    truncated.push(sse(json!({
        "candidates": [{"index": 0, "content": {"role": "model", "parts": [{"text": "partial"}]}}]
    })).as_bytes()).unwrap();
    assert!(matches!(
        truncated.finish(),
        Err(StreamError::UnexpectedEof)
    ));
}

#[test]
fn stream_uses_latest_usage_snapshot_and_preserves_incomplete_observations() {
    let mut decoder = GeminiGenerateContentStreamDecoder::new();
    let mut events = decoder
        .push(
            sse(json!({
                "candidates": [{
                    "index": 0,
                    "content": {"role": "model", "parts": [{"text": "ok"}]}
                }],
                    "usageMetadata": {"promptTokenCount": 5, "candidatesTokenCount": 1, "totalTokenCount": 6}
            }))
            .as_bytes(),
        )
        .unwrap();
    events.extend(
        decoder
            .push(
                sse(json!({
                    "candidates": [{"index": 0, "finishReason": "STOP"}],
                    "usageMetadata": {"promptTokenCount": 5, "candidatesTokenCount": 2, "totalTokenCount": 7}
                }))
                .as_bytes(),
            )
            .unwrap(),
    );
    events.extend(decoder.finish().unwrap());
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
            if usage.input_tokens == 5 && usage.output_tokens == 2 && usage.total_tokens == 7
    )));
    assert!(events.last().unwrap().usage_observation.is_none());

    let mut incomplete = GeminiGenerateContentStreamDecoder::new();
    incomplete
        .push(
            sse(json!({
                "candidates": [{"index": 0, "finishReason": "STOP"}],
                "usageMetadata": {"promptTokenCount": 9, "candidatesTokenCount": 0}
            }))
            .as_bytes(),
        )
        .unwrap();
    let events = incomplete.finish().unwrap();
    assert!(
        !events
            .iter()
            .any(|event| matches!(event.kind, CanonicalEventKind::Usage { .. }))
    );
    let observation = events.last().unwrap().usage_observation.unwrap();
    assert_eq!(observation.input_tokens, Some(9));
    assert_eq!(observation.output_tokens, Some(0));
    assert_eq!(observation.total_tokens, None);

    let mut total_only = GeminiGenerateContentStreamDecoder::new();
    total_only
        .push(
            sse(json!({
                "candidates": [{"index": 0, "finishReason": "STOP"}],
                "usageMetadata": {"promptTokenCount": 9, "totalTokenCount": 10}
            }))
            .as_bytes(),
        )
        .unwrap();
    let events = total_only.finish().unwrap();
    let observation = events.last().unwrap().usage_observation.unwrap();
    assert_eq!(observation.input_tokens, Some(9));
    assert_eq!(observation.output_tokens, None);
    assert_eq!(observation.total_tokens, Some(10));
}

#[test]
fn stream_uses_usage_snapshot_after_candidate_finish() {
    let mut decoder = GeminiGenerateContentStreamDecoder::new();
    let events = decoder
        .push(
            sse(json!({
                "candidates": [{
                    "index": 0,
                    "content": {"role": "model", "parts": [{"text": "ok"}]},
                    "finishReason": "STOP"
                }],
                "usageMetadata": {
                    "promptTokenCount": 5,
                    "candidatesTokenCount": 1,
                    "totalTokenCount": 6
                }
            }))
            .as_bytes(),
        )
        .unwrap();
    assert!(
        !events
            .iter()
            .any(|event| matches!(event.kind, CanonicalEventKind::Usage { .. }))
    );
    decoder
        .push(
            sse(json!({
                "usageMetadata": {
                    "promptTokenCount": 5,
                    "candidatesTokenCount": 2,
                    "totalTokenCount": 7
                }
            }))
            .as_bytes(),
        )
        .unwrap();
    assert!(
        !events
            .iter()
            .any(|event| matches!(event.kind, CanonicalEventKind::Usage { .. }))
    );

    let events = decoder.finish().unwrap();
    assert!(matches!(
        events.as_slice(),
        [
            olp_engine::domain::CanonicalEvent {
                kind: CanonicalEventKind::Usage { usage },
                ..
            },
            olp_engine::domain::CanonicalEvent {
                kind: CanonicalEventKind::Done,
                ..
            }
        ] if usage.input_tokens == 5
            && usage.output_tokens == 2
            && usage.total_tokens == 7
    ));
}

#[test]
fn stream_rejects_prompt_feedback_that_hides_an_unfinished_candidate() {
    let mut decoder = GeminiGenerateContentStreamDecoder::new();
    decoder
        .push(
            sse(json!({
                "candidates": [{
                    "index": 0,
                    "content": {"role": "model", "parts": [{"text": "partial"}]}
                }]
            }))
            .as_bytes(),
        )
        .unwrap();
    assert!(matches!(
        decoder.push(sse(json!({"promptFeedback": {"blockReason": "SAFETY"}})).as_bytes()),
        Err(StreamError::PromptFeedbackAfterCandidateStart)
    ));
}

#[test]
fn stream_allows_repeated_and_reset_usage_snapshots() {
    let mut decoder = GeminiGenerateContentStreamDecoder::new();
    decoder
        .push(
            sse(json!({
                "candidates": [{
                    "index": 0,
                    "content": {"role": "model", "parts": [{"text": "ok"}]}
                }],
                "usageMetadata": {"promptTokenCount": 13, "candidatesTokenCount": 67, "thoughtsTokenCount": 212, "totalTokenCount": 292}
            }))
            .as_bytes(),
        )
        .unwrap();
    decoder
        .push(
            sse(json!({
                "candidates": [{"index": 0}],
                "usageMetadata": {"promptTokenCount": 13, "totalTokenCount": 13}
            }))
            .as_bytes(),
        )
        .unwrap();
    decoder
        .push(
            sse(json!({
                "candidates": [{"index": 0, "finishReason": "STOP"}],
                "usageMetadata": {
                    "promptTokenCount": 225,
                    "candidatesTokenCount": 119,
                    "toolUsePromptTokenCount": 300,
                    "thoughtsTokenCount": 212,
                    "totalTokenCount": 856
                }
            }))
            .as_bytes(),
        )
        .unwrap();
    let events = decoder.finish().unwrap();
    assert!(events.iter().any(|event| matches!(
        event.kind,
        CanonicalEventKind::Usage { usage }
            if usage.input_tokens == 525 && usage.output_tokens == 331 && usage.total_tokens == 856
    )));
}

#[test]
fn stream_partial_snapshots_replace_omitted_counters() {
    let usage_frame = |field: &str, value: u64| {
        let mut usage = serde_json::Map::new();
        usage.insert(field.to_owned(), Value::from(value));
        sse(json!({"usageMetadata": usage}))
    };

    for field in [
        "promptTokenCount",
        "candidatesTokenCount",
        "totalTokenCount",
        "cachedContentTokenCount",
        "thoughtsTokenCount",
        "toolUsePromptTokenCount",
    ] {
        let mut decoder = GeminiGenerateContentStreamDecoder::new();
        decoder.push(usage_frame(field, 3).as_bytes()).unwrap();
        decoder.push(usage_frame(field, 3).as_bytes()).unwrap();
        decoder.push(usage_frame(field, 4).as_bytes()).unwrap();
    }
}

#[test]
fn stream_rejects_fragmented_usage_contradictions_and_keeps_error_usage_incomplete() {
    let mut contradictory = GeminiGenerateContentStreamDecoder::new();
    assert!(matches!(
        contradictory.push(
            sse(json!({
                "usageMetadata": {"cachedContentTokenCount": 5, "thoughtsTokenCount": 4, "totalTokenCount": 8}
            }))
            .as_bytes()
        ),
        Err(StreamError::Response(_))
    ));

    let mut additive_lower_bound = GeminiGenerateContentStreamDecoder::new();
    assert!(matches!(
        additive_lower_bound.push(
            sse(json!({
                "usageMetadata": {
                    "cachedContentTokenCount": 5,
                    "toolUsePromptTokenCount": 4,
                    "totalTokenCount": 8
                }
            }))
            .as_bytes()
        ),
        Err(StreamError::Response(_))
    ));

    let mut failed = GeminiGenerateContentStreamDecoder::new();
    failed
        .push(
            sse(json!({
                "usageMetadata": {
                    "promptTokenCount": 3,
                    "candidatesTokenCount": 1,
                    "totalTokenCount": 4
                }
            }))
            .as_bytes(),
        )
        .unwrap();
    let events = failed
        .push(
            sse(json!({
                "error": {"code": 503, "message": "failed", "status": "UNAVAILABLE"}
            }))
            .as_bytes(),
        )
        .unwrap();
    let observation = events.last().unwrap().usage_observation.unwrap();
    assert_eq!(observation.input_tokens, Some(3));
    assert_eq!(observation.output_tokens, Some(1));
    assert_eq!(observation.total_tokens, None);
}

#[test]
fn stream_timeout_error_round_trips_as_deadline_exceeded() {
    let mut decoder = GeminiGenerateContentStreamDecoder::new();
    let events = decoder
        .push(
            sse(json!({
                "error": {"code": 504, "message": "deadline", "status": "DEADLINE_EXCEEDED"}
            }))
            .as_bytes(),
        )
        .unwrap();
    assert!(matches!(
        &events[0].kind,
        CanonicalEventKind::Error { error }
            if error.class == olp_engine::domain::ErrorClass::Timeout && error.retryable
    ));
    let mut encoder = GeminiGenerateContentClientStreamEncoder::new("public-route", "fallback");
    let frames = events
        .into_iter()
        .flat_map(|event| encoder.push(event).unwrap())
        .collect::<Vec<_>>();
    let output: Value = serde_json::from_str(&frames[0].data).unwrap();
    assert_eq!(output["error"]["code"], 504);
    assert_eq!(output["error"]["status"], "DEADLINE_EXCEEDED");
}

#[test]
fn prompt_blocked_unary_round_trips_no_candidates_and_partial_usage() {
    let wire = json!({
        "promptFeedback": {"blockReason": "PROHIBITED_CONTENT"},
        "usageMetadata": {"promptTokenCount": 18, "totalTokenCount": 18}
    });
    let response: GenerateContentResponse = serde_json::from_value(wire).unwrap();
    let events = decode_generate_content_response(response).unwrap();
    let encoded = serde_json::to_value(
        encode_generate_content_response(&events, "public-route", "fallback").unwrap(),
    )
    .unwrap();
    assert_eq!(encoded["candidates"], json!([]));
    assert_eq!(
        encoded["promptFeedback"]["blockReason"],
        "PROHIBITED_CONTENT"
    );
    assert_eq!(encoded["usageMetadata"]["promptTokenCount"], 18);
    assert_eq!(encoded["usageMetadata"]["totalTokenCount"], 18);
    assert!(
        encoded["usageMetadata"]
            .get("candidatesTokenCount")
            .is_none()
    );
}

#[test]
fn count_token_dtos_enforce_mutually_exclusive_inputs() {
    let contents_only: CountTokensRequest = serde_json::from_value(json!({
        "contents": [{"role": "user", "parts": [{"text": "hello"}]}]
    }))
    .unwrap();
    assert_eq!(validate_count_tokens_request(&contents_only), Ok(()));

    let both: CountTokensRequest = serde_json::from_value(json!({
        "contents": [{"role": "user", "parts": [{"text": "hello"}]}],
        "generateContentRequest": {"contents": [{"role": "user", "parts": [{"text": "hello"}]}]}
    }))
    .unwrap();
    assert_eq!(
        validate_count_tokens_request(&both),
        Err(CountTokensError::ExactlyOneInput)
    );
    let response: CountTokensResponse = serde_json::from_value(json!({
        "totalTokens": 11,
        "cachedContentTokenCount": 3,
        "vendorCount": true
    }))
    .unwrap();
    assert_eq!(response.total_tokens, 11);
    assert_eq!(response.cached_content_token_count, Some(3));
    assert_eq!(response.extra["vendorCount"], Value::Bool(true));
}

#[test]
fn count_tokens_preserves_nested_request_and_encodes_native_result() {
    let request: CountTokensRequest = serde_json::from_value(json!({
        "generateContentRequest": {
            "model": "models/team-gemini",
            "contents": [{"role": "user", "parts": [{"text": "hello"}]}],
            "tools": [{"functionDeclarations": [{"name": "lookup", "parameters": {"type": "object"}}]}],
            "safetySettings": [{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "BLOCK_NONE"}]
        },
        "vendorCountOption": true
    }))
    .unwrap();
    let Operation::TokenCount(canonical) =
        decode_count_tokens_request("team-gemini", request).unwrap()
    else {
        panic!("wrong operation")
    };
    assert_eq!(canonical.route.as_str(), "team-gemini");
    let preserved =
        &canonical.extensions.values[olp_engine::protocols::gemini::GEMINI_COUNT_REQUEST_EXTENSION];
    assert_eq!(preserved["vendorCountOption"], true);
    assert!(preserved["generateContentRequest"]["safetySettings"].is_array());

    let response = encode_count_tokens_result(&olp_engine::domain::TokenCountResult {
        input_tokens: 21,
        extensions: olp_engine::domain::SourceExtensions::new(
            Surface::Gemini,
            [("/cachedContentTokenCount".into(), Value::from(3))].into(),
        ),
    })
    .unwrap();
    assert_eq!(response.total_tokens, 21);
    assert_eq!(response.cached_content_token_count, Some(3));
}

#[test]
fn count_tokens_plain_user_text_is_cross_protocol_representable() {
    let request: CountTokensRequest = serde_json::from_value(json!({
        "contents": [{"role": "user", "parts": [{"text": "plain text"}]}]
    }))
    .unwrap();
    let Operation::TokenCount(canonical) =
        decode_count_tokens_request("team-gemini", request).unwrap()
    else {
        panic!("wrong operation")
    };
    assert!(canonical.extensions.values.is_empty());
    assert_eq!(canonical.extensions.source, Some(Surface::Gemini));
    canonical
        .extensions
        .ensure_representable_on(Surface::Anthropic)
        .unwrap();
}

#[test]
fn client_stream_encoder_emits_sdk_sse_chunks_and_buffers_fragmented_tools() {
    let canonical = vec![
        olp_engine::domain::CanonicalEvent::new(
            0,
            CanonicalEventKind::ResponseStart {
                response_id: Some("gem-response".into()),
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
            CanonicalEventKind::ToolCallDelta {
                output_index: 0,
                tool_index: 0,
                id: Some("call-1".into()),
                name: Some("lookup".into()),
                arguments_delta: "{\"city\":".into(),
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            4,
            CanonicalEventKind::ToolCallDelta {
                output_index: 0,
                tool_index: 0,
                id: None,
                name: None,
                arguments_delta: "\"Paris\"}".into(),
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            5,
            CanonicalEventKind::Finish {
                output_index: 0,
                reason: FinishReason::ToolCalls,
            },
        ),
        olp_engine::domain::CanonicalEvent::new(
            6,
            CanonicalEventKind::Usage {
                usage: olp_engine::domain::Usage {
                    input_tokens: 3,
                    output_tokens: 3,
                    total_tokens: 6,
                    cached_input_tokens: None,
                    reasoning_tokens: Some(1),
                },
            },
        ),
        olp_engine::domain::CanonicalEvent::new(7, CanonicalEventKind::Done),
    ];
    let mut encoder = GeminiGenerateContentClientStreamEncoder::new("public-route", "fallback");
    let mut wire = String::new();
    for event in canonical {
        for frame in encoder.push(event).unwrap() {
            wire.push_str(&format!("data: {}\n\n", frame.data));
        }
    }
    assert!(wire.contains("\"modelVersion\":\"public-route\""));
    assert!(wire.contains("\"candidatesTokenCount\":2"));
    assert!(wire.contains("\"thoughtsTokenCount\":1"));
    let mut decoder = GeminiGenerateContentStreamDecoder::new();
    let mut decoded = Vec::new();
    for byte in wire.as_bytes() {
        decoded.extend(decoder.push(std::slice::from_ref(byte)).unwrap());
    }
    decoded.extend(decoder.finish().unwrap());
    assert!(decoded.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::ToolCallDelta { name: Some(name), arguments_delta, .. }
            if name == "lookup" && arguments_delta.contains("Paris")
    )));
    assert!(decoded.iter().any(|event| matches!(
        event.kind,
        CanonicalEventKind::Usage { usage }
            if usage.output_tokens == 3 && usage.reasoning_tokens == Some(1)
    )));
}

#[test]
fn native_gemini_stream_losslessly_preserves_safety_and_grounding_metadata() {
    let wire = sse(json!({
        "responseId": "native-1",
        "modelVersion": "gemini-upstream",
        "candidates": [{
            "index": 0,
            "content": {"role": "model", "parts": [{"text": "answer"}]},
            "finishReason": "STOP",
            "safetyRatings": [{"category": "HARM_CATEGORY_HATE_SPEECH", "probability": "NEGLIGIBLE"}],
            "groundingMetadata": {"webSearchQueries": ["source query"]}
        }],
        "usageMetadata": {"promptTokenCount": 2, "candidatesTokenCount": 1, "totalTokenCount": 3},
        "promptFeedback": {"safetyRatings": []}
    }));
    let mut decoder = GeminiGenerateContentStreamDecoder::with_max_event_bytes_and_raw_passthrough(
        1024 * 1024,
        true,
    );
    let mut events = decoder.push(wire.as_bytes()).unwrap();
    events.extend(decoder.finish().unwrap());
    validate_event_sequence(&events).unwrap();

    let mut encoder = GeminiGenerateContentClientStreamEncoder::new("public-route", "fallback");
    let mut frames = Vec::new();
    for event in events {
        frames.extend(encoder.push(event).unwrap());
    }
    assert_eq!(frames.len(), 1);
    let output: Value = serde_json::from_str(&frames[0].data).unwrap();
    assert_eq!(output["modelVersion"], "public-route");
    assert_eq!(
        output["candidates"][0]["groundingMetadata"]["webSearchQueries"][0],
        "source query"
    );
    assert_eq!(
        output["candidates"][0]["safetyRatings"][0]["probability"],
        "NEGLIGIBLE"
    );
    assert!(output.get("promptFeedback").is_some());
}

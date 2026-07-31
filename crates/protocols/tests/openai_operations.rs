use base64::{Engine as _, engine::general_purpose::STANDARD};
use olp_domain::{
    CanonicalEvent, CanonicalEventKind, FinishReason, GenerationParameters, GenerationRequest,
    ImageOperation, MediaHandle, MediaSource, Message, MessageRole, Operation, RouteSlug,
    SourceExtensions, Surface, VideoOperation, validate_event_sequence,
};
use olp_protocols::openai::{
    BoundedMediaPart, EmbeddingData, EmbeddingRequest, EmbeddingResponse, EmbeddingUsage,
    EmbeddingWireVector, OpenAiImageEditRequest, OpenAiImageGenerationRequest, OpenAiImageResponse,
    OpenAiModerationRequest, OpenAiModerationResponse, OpenAiResponsesStreamDecoder,
    OpenAiResponsesStreamEncoder, OpenAiSpeechRequest, OpenAiTranscriptionRequest,
    OpenAiTranscriptionResponse, OpenAiVideoCreateRequest, OpenAiVideoDeleteResponse,
    OpenAiVideoListQuery, OpenAiVideoListResponse, OpenAiVideoObject, ResponseCreateRequest,
    ResponseInputTokensRequest, ResponseInputTokensResponse, ResponseObject,
    decode_embedding_request, decode_embedding_response, decode_image_edit,
    decode_image_generation, decode_image_response, decode_moderation, decode_moderation_response,
    decode_response_create, decode_response_input_tokens, decode_response_input_tokens_result,
    decode_response_object, decode_speech, decode_transcription, decode_transcription_response,
    decode_video_create, decode_video_delete_response, decode_video_list,
    decode_video_list_response, decode_video_object, encode_embedding_request,
    encode_embedding_response, encode_image_generation, encode_image_response,
    encode_moderation_response, encode_response_create, encode_response_object,
    encode_transcription_response, encode_video_delete_response, encode_video_list_response,
    encode_video_object, is_valid_video_job_id,
};
use serde_json::json;

#[test]
fn responses_request_round_trips_supported_semantics_and_extensions() {
    let wire: ResponseCreateRequest = serde_json::from_value(json!({
        "model": "team-responses",
        "instructions": "Be concise",
        "input": [
            {
                "type": "message",
                "role": "user",
                "content": [
                    {"type": "input_text", "text": "describe this", "cache_hint": "short"},
                    {"type": "input_image", "image_url": "https://example.test/a.png", "detail": "low"},
                    {"type": "input_image", "image_url": "https://example.test/b.png"}
                ],
                "vendor_message": true
            },
            {"id": "fc_input", "type": "function_call", "call_id": "call_input", "name": "lookup", "arguments": "{}"},
            {"type": "function_call_output", "call_id": "call_input", "output": "found"}
        ],
        "max_output_tokens": 80,
        "parallel_tool_calls": false,
        "tools": [{
            "type": "function",
            "name": "lookup",
            "description": "lookup",
            "parameters": {"type": "object"},
            "strict": true,
            "vendor_tool": 3
        }],
        "tool_choice": {"type": "function", "name": "lookup"},
        "text": {
            "format": {"type": "json_schema", "name": "answer", "schema": {"type": "object"}, "strict": true},
            "verbosity": "low"
        },
        "service_tier": "priority"
    }))
    .unwrap();
    let Operation::Generation(canonical) = decode_response_create(wire).unwrap() else {
        panic!("wrong operation")
    };
    assert_eq!(canonical.route.as_str(), "team-responses");
    assert_eq!(canonical.messages.len(), 4);
    assert_eq!(canonical.tools[0].name, "lookup");
    assert_eq!(canonical.messages[2].tool_calls[0].id, "call_input");
    assert_eq!(
        canonical.messages[3].tool_call_id.as_deref(),
        Some("call_input")
    );
    assert_eq!(canonical.extensions.values["/service_tier"], "priority");
    assert_eq!(canonical.extensions.values["/input/0/vendor_message"], true);
    assert_eq!(
        canonical.extensions.values["/input/0/content/0/cache_hint"],
        "short"
    );

    let encoded = encode_response_create(&canonical, "gpt-upstream").unwrap();
    let encoded = serde_json::to_value(encoded).unwrap();
    assert_eq!(encoded["model"], "gpt-upstream");
    assert_eq!(encoded["instructions"], "Be concise");
    assert_eq!(encoded["input"][0]["vendor_message"], true);
    assert_eq!(encoded["input"][0]["content"][0]["cache_hint"], "short");
    assert!(encoded["input"][0]["content"][2].get("detail").is_none());
    assert_eq!(encoded["input"][1]["id"], "fc_input");
    assert_eq!(encoded["input"][1]["call_id"], "call_input");
    assert_eq!(encoded["tools"][0]["strict"], true);
    assert_eq!(encoded["service_tier"], "priority");
}

#[test]
fn responses_rejects_stateful_and_unspooled_media_semantics() {
    let stateful: ResponseCreateRequest = serde_json::from_value(json!({
        "model": "default",
        "input": "hello",
        "previous_response_id": "resp_previous"
    }))
    .unwrap();
    assert!(decode_response_create(stateful).is_err());

    let conversation: ResponseCreateRequest = serde_json::from_value(json!({
        "model": "default",
        "input": "hello",
        "conversation": {"id": "conv_stateful"}
    }))
    .unwrap();
    assert!(decode_response_create(conversation).is_err());

    let inline_file: ResponseCreateRequest = serde_json::from_value(json!({
        "model": "default",
        "input": [{"type": "message", "role": "user", "content": [{
            "type": "input_file", "file_data": "large-inline-payload"
        }]}]
    }))
    .unwrap();
    assert!(decode_response_create(inline_file).is_err());

    let empty_content: ResponseCreateRequest = serde_json::from_value(json!({
        "model": "default",
        "input": [{"type": "message", "role": "user", "content": []}]
    }))
    .unwrap();
    assert!(decode_response_create(empty_content).is_err());
}

#[test]
fn responses_preserves_builtin_tools_only_for_same_protocol() {
    let wire: ResponseCreateRequest = serde_json::from_value(json!({
        "model": "team-responses",
        "input": "search",
        "tools": [{
            "type": "web_search_preview",
            "search_context_size": "low",
            "user_location": {"type": "approximate", "country": "FR"}
        }],
        "tool_choice": {"type": "web_search_preview"}
    }))
    .unwrap();
    let Operation::Generation(canonical) = decode_response_create(wire).unwrap() else {
        panic!("wrong operation")
    };
    assert!(canonical.tools.is_empty());
    assert_eq!(canonical.extensions.source, Some(Surface::OpenAi));
    let encoded =
        serde_json::to_value(encode_response_create(&canonical, "gpt-upstream").unwrap()).unwrap();
    assert_eq!(encoded["tools"][0]["type"], "web_search_preview");
    assert_eq!(encoded["tools"][0]["user_location"]["country"], "FR");
    assert_eq!(encoded["tool_choice"]["type"], "web_search_preview");
    assert!(
        canonical
            .extensions
            .ensure_representable_on(Surface::Gemini)
            .is_err()
    );
}

#[test]
fn responses_unary_and_fragmented_stream_become_ordered_events() {
    let response: ResponseObject = serde_json::from_value(json!({
        "id": "resp_1",
        "object": "response",
        "created_at": 1800000000,
        "status": "incomplete",
        "model": "gpt-upstream",
        "output": [
            {
                "id": "msg_1",
                "type": "message",
                "role": "assistant",
                "status": "incomplete",
                "content": [{"type": "output_text", "text": "hello", "annotations": []}]
            },
            {"id": "fc_output", "type": "function_call", "call_id": "call_output", "name": "lookup", "arguments": "{}", "status": "incomplete"}
        ],
        "usage": {"input_tokens": 3, "output_tokens": 2, "total_tokens": 5},
        "incomplete_details": {"reason": "max_output_tokens"}
    }))
    .unwrap();
    let events = decode_response_object(response).unwrap();
    validate_event_sequence(&events).unwrap();
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::TextDelta { text, .. } if text == "hello"
    )));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::ToolCallDelta { id: Some(id), .. } if id == "call_output"
    )));
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::Finish {
            reason: FinishReason::Length,
            ..
        }
    )));
    let client = encode_response_object(&events, "team-route", "fallback").unwrap();
    assert_eq!(client.model, "team-route");
    assert_eq!(client.output[1]["id"], "fc_output");
    assert_eq!(client.output[1]["call_id"], "call_output");
    assert_eq!(client.status, "incomplete");
    validate_event_sequence(&decode_response_object(client).unwrap()).unwrap();

    let mut encoder = OpenAiResponsesStreamEncoder::new("team-route", "fallback", 1_800_000_000);
    let mut client_frames = Vec::new();
    for event in events.clone() {
        client_frames.extend(encoder.push(event).unwrap());
    }
    assert_eq!(
        client_frames.last().unwrap().event.as_deref(),
        Some("response.incomplete")
    );

    let frames = [
        json!({"type":"response.created","response":{"id":"resp_s","model":"gpt-upstream"}}),
        json!({"type":"response.output_item.added","output_index":0,"item":{"id":"msg_stream","type":"message","role":"assistant","content":[]}}),
        json!({"type":"response.output_text.delta","output_index":0,"delta":"hé 🌍"}),
        json!({"type":"response.output_item.added","output_index":1,"item":{"id":"fc_stream","type":"function_call","call_id":"call_stream","name":"lookup","arguments":""}}),
        json!({"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_stream","delta":"{}"}),
        json!({"type":"response.output_item.done","output_index":1,"item":{"type":"function_call"}}),
        json!({"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"},"usage":{"input_tokens":2,"output_tokens":2,"total_tokens":4}}}),
    ];
    let wire = frames
        .iter()
        .map(|frame| {
            format!(
                "event: {}\ndata: {frame}\n\n",
                frame["type"].as_str().unwrap()
            )
        })
        .collect::<String>();
    let mut decoder = OpenAiResponsesStreamDecoder::new();
    let mut streamed = Vec::new();
    for byte in wire.as_bytes() {
        streamed.extend(decoder.push(std::slice::from_ref(byte)).unwrap());
    }
    streamed.extend(decoder.finish().unwrap());
    validate_event_sequence(&streamed).unwrap();
    assert!(streamed.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::TextDelta { text, .. } if text == "hé 🌍"
    )));
    assert_eq!(
        streamed
            .iter()
            .filter_map(|event| match &event.kind {
                CanonicalEventKind::ToolCallDelta { id, .. } => Some(id.as_deref()),
                _ => None,
            })
            .collect::<Vec<_>>(),
        [Some("call_stream"), None]
    );
    assert!(streamed.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::Finish {
            reason: FinishReason::ContentFilter,
            ..
        }
    )));
}

#[test]
fn responses_emit_distinct_items_for_mixed_text_and_multiple_tools() {
    let events = [
        CanonicalEventKind::ResponseStart {
            response_id: Some("resp_mixed".into()),
            provider_model: Some("provider-model".into()),
        },
        CanonicalEventKind::ToolCallDelta {
            output_index: 0,
            tool_index: 0,
            id: Some("call_a".into()),
            name: Some("tool_a".into()),
            arguments_delta: "{\"a\":1}".into(),
        },
        CanonicalEventKind::TextDelta {
            output_index: 0,
            text: "answer".into(),
        },
        CanonicalEventKind::ToolCallDelta {
            output_index: 0,
            tool_index: 1,
            id: Some("call_b".into()),
            name: Some("tool_b".into()),
            arguments_delta: "{\"b\":2}".into(),
        },
        CanonicalEventKind::Finish {
            output_index: 0,
            reason: FinishReason::ToolCalls,
        },
        CanonicalEventKind::Done,
    ]
    .into_iter()
    .enumerate()
    .map(|(sequence, kind)| CanonicalEvent::new(sequence.try_into().unwrap(), kind));

    let mut encoder = OpenAiResponsesStreamEncoder::new("team-route", "fallback", 1);
    let frames = events
        .flat_map(|event| encoder.push(event).unwrap())
        .collect::<Vec<_>>();
    let values = frames
        .iter()
        .map(|frame| serde_json::from_str::<serde_json::Value>(&frame.data).unwrap())
        .collect::<Vec<_>>();
    let added = values
        .iter()
        .filter(|value| value["type"] == "response.output_item.added")
        .map(|value| {
            (
                value["output_index"].as_u64().unwrap(),
                value["item"]["id"].as_str().unwrap(),
                value["item"]["call_id"].as_str(),
            )
        })
        .collect::<Vec<_>>();
    assert_eq!(
        added,
        [
            (0, "fc_0", Some("call_a")),
            (1, "msg_1", None),
            (2, "fc_2", Some("call_b"))
        ]
    );
    assert_eq!(
        values
            .iter()
            .filter(|value| value["type"] == "response.function_call_arguments.delta")
            .map(|value| value["item_id"].as_str().unwrap())
            .collect::<Vec<_>>(),
        ["fc_0", "fc_2"]
    );
    let output = &values
        .iter()
        .find(|value| value["type"] == "response.completed")
        .unwrap()["response"]["output"];
    assert_eq!(
        output
            .as_array()
            .unwrap()
            .iter()
            .map(|item| item["id"].as_str().unwrap())
            .collect::<Vec<_>>(),
        ["fc_0", "msg_1", "fc_2"]
    );
}

#[test]
fn responses_reasoning_output_round_trips_without_becoming_message_content() {
    let response: ResponseObject = serde_json::from_value(json!({
        "id": "resp_reasoning",
        "object": "response",
        "created_at": 1800000000,
        "status": "completed",
        "model": "gpt-upstream",
        "output": [
            {
                "id": "rs_1", "type": "reasoning", "status": "completed",
                "summary": [{"type": "summary_text", "text": "checked constraints"}],
                "encrypted_content": "opaque"
            },
            {
                "id": "msg_1", "type": "message", "role": "assistant", "status": "completed",
                "content": [{"type": "output_text", "text": "answer", "annotations": []}]
            }
        ]
    }))
    .unwrap();
    let events = decode_response_object(response).unwrap();
    let encoded =
        serde_json::to_value(encode_response_object(&events, "team-route", "fallback").unwrap())
            .unwrap();
    assert_eq!(encoded["output"][0]["type"], "reasoning");
    assert_eq!(encoded["output"][0]["encrypted_content"], "opaque");
    assert_eq!(encoded["output"][1]["content"][0]["text"], "answer");

    let reasoning = encoded["output"][0].clone();
    let wire = [
        json!({"type":"response.created","response":{"id":"resp_reasoning","model":"gpt-upstream"}}),
        json!({"type":"response.output_item.added","output_index":0,"item":reasoning}),
        json!({"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning"}}),
        json!({"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}),
        json!({"type":"response.output_text.delta","output_index":1,"delta":"answer"}),
        json!({"type":"response.output_item.done","output_index":1,"item":{"type":"message"}}),
        json!({"type":"response.completed","response":{"status":"completed","output":encoded["output"].clone()}}),
    ]
    .into_iter()
    .map(|frame| format!("event: {}\ndata: {frame}\n\n", frame["type"].as_str().unwrap()))
    .collect::<String>();
    let mut decoder = OpenAiResponsesStreamDecoder::new();
    let streamed = decoder.push(wire.as_bytes()).unwrap();
    assert!(!streamed.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::MessageStart {
            output_index: 0,
            ..
        } | CanonicalEventKind::Finish {
            output_index: 0,
            ..
        }
    )));
    let mut encoder = OpenAiResponsesStreamEncoder::new("team-route", "fallback", 1);
    let frames = streamed
        .into_iter()
        .flat_map(|event| encoder.push(event).unwrap())
        .map(|frame| serde_json::from_str::<serde_json::Value>(&frame.data).unwrap())
        .collect::<Vec<_>>();
    let added = frames
        .iter()
        .filter(|value| value["type"] == "response.output_item.added")
        .map(|value| (value["output_index"].as_u64(), value["item"]["id"].as_str()))
        .collect::<Vec<_>>();
    assert_eq!(added, [(Some(0), Some("rs_1")), (Some(1), Some("msg_1"))]);
    let output = &frames.last().unwrap()["response"]["output"];
    assert_eq!(output[0]["id"], "rs_1");
    assert_eq!(output[1]["id"], "msg_1");
}

#[test]
fn response_input_tokens_preserves_full_stateless_multi_item_input() {
    let request: ResponseInputTokensRequest = serde_json::from_value(json!({
        "model": "count-route",
        "input": [
            {
                "type": "message",
                "role": "developer",
                "content": [{"type": "input_text", "text": "Be concise"}],
                "vendor_message": true
            },
            {
                "type": "message",
                "role": "user",
                "content": [
                    {"type": "input_text", "text": "Use the tool"},
                    {"type": "input_image", "image_url": "https://example.test/input.png", "detail": "low"}
                ]
            },
            {
                "type": "function_call",
                "call_id": "call_1",
                "name": "lookup",
                "arguments": "{\"id\":1}",
                "vendor_call": 7
            },
            {
                "type": "function_call_output",
                "call_id": "call_1",
                "output": "found"
            }
        ],
        "tools": [{"type": "function", "name": "lookup", "parameters": {"type": "object"}}],
        "vendor_flag": 1
    }))
    .unwrap();
    let Operation::TokenCount(request) = decode_response_input_tokens(request).unwrap() else {
        panic!("wrong operation")
    };
    assert_eq!(request.input.len(), 5);
    let forwarded =
        olp_protocols::openai::encode_response_input_tokens(&request, "gpt-upstream").unwrap();
    let forwarded = serde_json::to_value(forwarded).unwrap();
    assert_eq!(forwarded["model"], "gpt-upstream");
    assert_eq!(forwarded["input"].as_array().unwrap().len(), 4);
    assert_eq!(forwarded["input"][0]["vendor_message"], true);
    assert_eq!(forwarded["input"][2]["vendor_call"], 7);
    assert_eq!(forwarded["tools"][0]["name"], "lookup");
    assert_eq!(forwarded["vendor_flag"], 1);

    let stateful: ResponseInputTokensRequest = serde_json::from_value(json!({
        "model": "count-route",
        "input": "hello",
        "previous_response_id": "resp_stateful"
    }))
    .unwrap();
    assert!(decode_response_input_tokens(stateful).is_err());

    let response: ResponseInputTokensResponse = serde_json::from_value(json!({
        "object": "response.input_tokens",
        "input_tokens": 7
    }))
    .unwrap();
    assert_eq!(
        decode_response_input_tokens_result(response).input_tokens,
        7
    );
}

#[test]
fn response_input_tokens_plain_text_is_cross_protocol_representable() {
    let request: ResponseInputTokensRequest = serde_json::from_value(json!({
        "model": "count-route",
        "input": "plain text"
    }))
    .unwrap();
    let Operation::TokenCount(request) = decode_response_input_tokens(request).unwrap() else {
        panic!("wrong operation")
    };
    assert!(request.extensions.values.is_empty());
    assert_eq!(request.extensions.source, Some(Surface::OpenAi));
    request
        .extensions
        .ensure_representable_on(Surface::Anthropic)
        .unwrap();
}

#[test]
fn embeddings_support_text_tokens_float_and_bounded_base64_forms() {
    let request: EmbeddingRequest = serde_json::from_value(json!({
        "model": "embed-route",
        "input": ["one", "two"],
        "dimensions": 2,
        "encoding_format": "float",
        "vendor": true
    }))
    .unwrap();
    let Operation::Embeddings(canonical) = decode_embedding_request(request).unwrap() else {
        panic!("wrong operation")
    };
    let encoded = serde_json::to_value(
        encode_embedding_request(&canonical, "text-embedding-upstream").unwrap(),
    )
    .unwrap();
    assert_eq!(encoded["encoding_format"], "float");
    assert_eq!(encoded["vendor"], true);

    let bytes = [1.0_f32.to_le_bytes(), (-2.5_f32).to_le_bytes()].concat();
    let response: EmbeddingResponse = serde_json::from_value(json!({
        "object": "list",
        "model": "text-embedding-upstream",
        "data": [{"object": "embedding", "index": 0, "embedding": STANDARD.encode(bytes)}],
        "usage": {"prompt_tokens": 3, "total_tokens": 3}
    }))
    .unwrap();
    let result = decode_embedding_response(response).unwrap();
    assert_eq!(result.data[0].values, vec![1.0, -2.5]);
    let wire = encode_embedding_response(&result, "embed-route", Some("base64")).unwrap();
    let decoded = decode_embedding_response(wire).unwrap();
    assert_eq!(decoded.data[0].values, vec![1.0, -2.5]);

    let non_finite: EmbeddingResponse = serde_json::from_value(json!({
        "object": "list",
        "model": "text-embedding-upstream",
        "data": [{
            "object": "embedding",
            "index": 0,
            "embedding": STANDARD.encode(f32::NAN.to_le_bytes())
        }],
        "usage": {"prompt_tokens": 1, "total_tokens": 1}
    }))
    .unwrap();
    assert!(decode_embedding_response(non_finite).is_err());
}

#[test]
fn embeddings_reject_malformed_indices_and_vectors_on_decode_and_encode() {
    let response = |items: Vec<(u32, Vec<f32>)>| EmbeddingResponse {
        object: "list".into(),
        data: items
            .into_iter()
            .map(|(index, values)| EmbeddingData {
                object: "embedding".into(),
                embedding: EmbeddingWireVector::Floats(values),
                index,
                extra: Default::default(),
            })
            .collect(),
        model: "text-embedding-upstream".into(),
        usage: EmbeddingUsage {
            prompt_tokens: 1,
            total_tokens: 1,
            extra: Default::default(),
        },
        extra: Default::default(),
    };

    for invalid in [
        response(Vec::new()),
        response(vec![(0, vec![1.0]), (0, vec![2.0])]),
        response(vec![(0, vec![1.0]), (2, vec![2.0])]),
        response(vec![(0, Vec::new())]),
        response(vec![(0, vec![1.0]), (1, vec![2.0, 3.0])]),
        response(vec![(0, vec![f32::NAN])]),
    ] {
        assert!(decode_embedding_response(invalid).is_err());
    }

    let mut invalid = decode_embedding_response(response(vec![(0, vec![1.0])])).unwrap();
    invalid.data[0].values[0] = f32::INFINITY;
    assert!(encode_embedding_response(&invalid, "embed-route", None).is_err());
}

#[test]
fn image_json_and_multipart_forms_use_handles_and_preserve_extensions() {
    let request: OpenAiImageGenerationRequest = serde_json::from_value(json!({
        "model": "image-route",
        "prompt": "a cobalt fox",
        "n": 1,
        "quality": "high",
        "output_format": "png",
        "vendor": "kept"
    }))
    .unwrap();
    let Operation::Images(ImageOperation::Generation(canonical)) =
        decode_image_generation(request).unwrap()
    else {
        panic!("wrong operation")
    };
    let encoded =
        serde_json::to_value(encode_image_generation(&canonical, "gpt-image-2").unwrap()).unwrap();
    assert_eq!(encoded["quality"], "high");
    assert_eq!(encoded["vendor"], "kept");

    let part = media_part("image-ref", "input.png", 128);
    let edit = OpenAiImageEditRequest {
        model: "edit-route".into(),
        images: vec![part],
        mask: None,
        prompt: "edit".into(),
        n: Some(1),
        size: None,
        stream: false,
        quality: None,
        response_format: None,
        user: None,
        background: None,
        input_fidelity: None,
        output_compression: None,
        output_format: None,
        partial_images: None,
        extra: Default::default(),
    };
    let Operation::Images(ImageOperation::Edit(edit)) = decode_image_edit(edit).unwrap() else {
        panic!("wrong operation")
    };
    assert_eq!(edit.images[0].as_str(), "image-ref");

    let response: OpenAiImageResponse = serde_json::from_value(json!({
        "created": 1800000000,
        "data": [{"b64_json": "opaque-base64", "revised_prompt": "revised"}],
        "usage": {"input_tokens": 2, "output_tokens": 5, "total_tokens": 7}
    }))
    .unwrap();
    let result =
        decode_image_response(response, |_| Ok(MediaHandle::new("spooled-image"))).unwrap();
    assert!(matches!(
        &result.images[0].source,
        MediaSource::Handle(handle) if handle.as_str() == "spooled-image"
    ));
    let wire = encode_image_response(&result, |_| {
        Ok(olp_protocols::openai::OpenAiImagePayload::Base64Json(
            "re-encoded".into(),
        ))
    })
    .unwrap();
    assert_eq!(wire.data[0].b64_json.as_deref(), Some("re-encoded"));
}

#[test]
fn audio_requests_never_embed_uploaded_bytes() {
    let speech: OpenAiSpeechRequest = serde_json::from_value(json!({
        "model": "speech-route",
        "input": "hello",
        "voice": "coral",
        "response_format": "mp3",
        "speed": 1.1,
        "stream_format": "sse"
    }))
    .unwrap();
    let Operation::Speech(speech) = decode_speech(speech).unwrap() else {
        panic!("wrong operation")
    };
    assert!(speech.stream);

    let transcription = OpenAiTranscriptionRequest {
        model: "transcribe-route".into(),
        file: media_part("audio-ref", "audio.wav", 1024),
        language: Some("en".into()),
        prompt: None,
        response_format: Some("verbose_json".into()),
        temperature: Some(0.0),
        include: Vec::new(),
        timestamp_granularities: vec!["segment".into()],
        chunking_strategy: None,
        stream: false,
        extra: Default::default(),
    };
    let Operation::Transcription(canonical) = decode_transcription(transcription).unwrap() else {
        panic!("wrong operation")
    };
    assert_eq!(canonical.audio.as_str(), "audio-ref");

    let response: OpenAiTranscriptionResponse = serde_json::from_value(json!({
        "text": "hello",
        "language": "en",
        "duration": 1.5,
        "segments": [{"id": 0, "start": 0.0, "end": 1.5, "text": "hello", "speaker": "A"}]
    }))
    .unwrap();
    let result = decode_transcription_response(response).unwrap();
    assert_eq!(result.segments[0].speaker.as_deref(), Some("A"));
    let encoded = encode_transcription_response(&result).unwrap();
    assert_eq!(
        decode_transcription_response(encoded).unwrap().text,
        "hello"
    );
}

#[test]
fn transcription_response_timestamps_are_finite_bounded_and_ordered() {
    let response: OpenAiTranscriptionResponse = serde_json::from_value(json!({
        "text": "overlap",
        "duration": 1.5,
        "segments": [
            {"start": 0.0, "end": 1.0, "text": "first"},
            {"start": 0.5, "end": 1.5, "text": "second"}
        ]
    }))
    .unwrap();
    let mut canonical = decode_transcription_response(response.clone()).unwrap();
    for (duration, start, end) in [
        (f64::NAN, 0.0, 1.0),
        (1.5, f64::NAN, 1.0),
        (1.5, -1.0, 1.0),
        (1.5, 1.0, 0.5),
        (1.0, 0.0, 1.5),
    ] {
        let OpenAiTranscriptionResponse::Json(mut invalid) = response.clone() else {
            unreachable!()
        };
        invalid.duration = Some(duration);
        invalid.segments[0].start = start;
        invalid.segments[0].end = end;
        assert!(decode_transcription_response(OpenAiTranscriptionResponse::Json(invalid)).is_err());
    }
    let OpenAiTranscriptionResponse::Json(mut unordered) = response else {
        unreachable!()
    };
    unordered.segments.swap(0, 1);
    assert!(decode_transcription_response(OpenAiTranscriptionResponse::Json(unordered)).is_err());
    canonical.segments[1].end_seconds = 2.0;
    assert!(encode_transcription_response(&canonical).is_err());
}

#[test]
fn transcription_formats_and_known_speakers_are_validated_and_preserved() {
    for format in [
        "json",
        "text",
        "srt",
        "verbose_json",
        "vtt",
        "diarized_json",
    ] {
        let request = OpenAiTranscriptionRequest {
            model: "transcribe-route".into(),
            file: media_part("audio-ref", "audio.wav", 1024),
            language: None,
            prompt: None,
            response_format: Some(format.into()),
            temperature: None,
            include: Vec::new(),
            timestamp_granularities: Vec::new(),
            chunking_strategy: None,
            stream: false,
            extra: Default::default(),
        };
        assert!(decode_transcription(request).is_ok(), "format {format}");
    }

    let request = OpenAiTranscriptionRequest {
        model: "transcribe-route".into(),
        file: media_part("audio-ref", "audio.wav", 1024),
        language: None,
        prompt: None,
        response_format: Some("diarized_json".into()),
        temperature: None,
        include: Vec::new(),
        timestamp_granularities: Vec::new(),
        chunking_strategy: Some(json!("auto")),
        stream: false,
        extra: [
            ("known_speaker_names".into(), json!(["agent", "customer"])),
            (
                "known_speaker_references".into(),
                json!(["data:audio/wav;base64,AAAA", "data:audio/wav;base64,BBBB"]),
            ),
        ]
        .into(),
    };
    let Operation::Transcription(canonical) = decode_transcription(request).unwrap() else {
        panic!("wrong operation")
    };
    assert_eq!(
        canonical.extensions.values["/known_speaker_names"],
        json!(["agent", "customer"])
    );
    let encoded = olp_protocols::openai::encode_transcription(
        &canonical,
        "gpt-4o-transcribe-diarize",
        |_| Ok(media_part("audio-ref", "audio.wav", 1024)),
    )
    .unwrap();
    assert_eq!(
        encoded.extra["known_speaker_references"],
        json!(["data:audio/wav;base64,AAAA", "data:audio/wav;base64,BBBB"])
    );

    let invalid = OpenAiTranscriptionRequest {
        response_format: Some("xml".into()),
        ..encoded
    };
    assert!(decode_transcription(invalid).is_err());
}

#[test]
fn moderation_preserves_dynamic_categories_and_multimodal_input() {
    let empty: OpenAiModerationRequest = serde_json::from_value(json!({
        "model": "moderation-route",
        "input": [{"type": "text", "text": ""}]
    }))
    .unwrap();
    assert!(decode_moderation(empty).is_err());

    let request: OpenAiModerationRequest = serde_json::from_value(json!({
        "model": "moderation-route",
        "input": [
            {"type": "text", "text": "hello", "locale": "en"},
            {"type": "image_url", "image_url": {"url": "https://example.test/a.png"}}
        ]
    }))
    .unwrap();
    let Operation::Moderation(canonical) = decode_moderation(request).unwrap() else {
        panic!("wrong operation")
    };
    assert_eq!(canonical.input.len(), 2);
    assert_eq!(canonical.extensions.values["/input/0/locale"], "en");

    let response: OpenAiModerationResponse = serde_json::from_value(json!({
        "id": "modr_1",
        "model": "omni-moderation-latest",
        "results": [{
            "flagged": true,
            "categories": {"violence": true, "new/category": false},
            "category_scores": {"violence": 0.9, "new/category": 0.1},
            "category_applied_input_types": {"violence": ["text", "image"]}
        }]
    }))
    .unwrap();
    let result = decode_moderation_response(response).unwrap();
    assert!(result.results[0].categories["violence"]);
    assert_eq!(result.results[0].category_scores["new/category"], 0.1);
    let mut encoded =
        encode_moderation_response(&result, "moderation-route", "modr_fallback").unwrap();
    assert_eq!(encoded.model, "moderation-route");
    for score in [f64::NAN, -0.1, 1.1] {
        encoded.results[0]
            .category_scores
            .insert("violence".into(), score);
        assert!(decode_moderation_response(encoded.clone()).is_err());
    }

    let mut invalid = result;
    invalid.results[0]
        .category_scores
        .insert("violence".into(), -0.1);
    assert!(encode_moderation_response(&invalid, "moderation-route", "modr_fallback").is_err());
}

#[test]
fn video_async_lifecycle_uses_current_videos_contract() {
    for valid in [
        "a".to_owned(),
        "a".repeat(1_024),
        "folder/video-café".to_owned(),
    ] {
        assert!(is_valid_video_job_id(&valid));
    }
    for invalid in [
        String::new(),
        ".".to_owned(),
        "..".to_owned(),
        "a".repeat(1_025),
        " video1".to_owned(),
        "video\n1".to_owned(),
    ] {
        assert!(!is_valid_video_job_id(&invalid));
    }

    let request = OpenAiVideoCreateRequest {
        model: "video-route".to_owned(),
        prompt: "a calm ocean".to_owned(),
        input_reference: Some(
            BoundedMediaPart::new(
                MediaHandle::new("video-reference"),
                "reference.png",
                Some("image/png".to_owned()),
                4,
                20 * 1024 * 1024,
            )
            .unwrap(),
        ),
        seconds: Some("8".to_owned()),
        size: Some("1280x720".to_owned()),
        extra: Default::default(),
    };
    let Operation::Video(VideoOperation::Create(create)) = decode_video_create(request).unwrap()
    else {
        panic!("wrong operation")
    };
    assert_eq!(create.input.unwrap().as_str(), "video-reference");

    let query: OpenAiVideoListQuery = serde_json::from_value(json!({
        "after": "video_1", "limit": 20, "order": "desc"
    }))
    .unwrap();
    assert!(matches!(
        decode_video_list(query).unwrap(),
        Operation::Video(VideoOperation::List(_))
    ));

    assert!(decode_video_object(video_object(" invalid", "completed")).is_err());
    let mut invalid_progress = video_object("video_1", "in_progress");
    invalid_progress.progress = Some(f32::NAN);
    assert!(decode_video_object(invalid_progress).is_err());
    let object: OpenAiVideoObject = video_object("video_2", "completed");
    let mut second = video_object("video_3", "in_progress");
    second.model = "second-public-route".into();
    let list = OpenAiVideoListResponse {
        object: "list".into(),
        data: vec![object, second],
        first_id: Some("video_2".into()),
        last_id: Some("video_3".into()),
        has_more: false,
        extra: Default::default(),
    };
    let result = decode_video_list_response(list).unwrap();
    assert_eq!(result.jobs[0].id, "video_2");
    let mut invalid_progress = result.jobs[0].clone();
    invalid_progress.progress_percent = Some(100.1);
    assert!(encode_video_object(&invalid_progress, "sora-2").is_err());
    let encoded = encode_video_list_response(&result, "sora-2").unwrap();
    assert_eq!(encoded.data[0].model, "sora-2");
    assert_eq!(encoded.data[1].model, "second-public-route");
    assert_eq!(
        decode_video_list_response(encoded).unwrap().jobs[0].id,
        "video_2"
    );

    let deleted = decode_video_delete_response(OpenAiVideoDeleteResponse {
        id: "video_2".into(),
        object: Some("video.deleted".into()),
        deleted: true,
        extra: Default::default(),
    });
    assert!(deleted.deleted);
    let encoded = encode_video_delete_response(&deleted).unwrap();
    assert!(decode_video_delete_response(encoded).deleted);
}

#[test]
fn cross_protocol_extensions_fail_closed() {
    let request = GenerationRequest {
        route: RouteSlug::parse("route").unwrap(),
        messages: vec![Message {
            role: MessageRole::User,
            content: vec![olp_domain::ContentPart::Text {
                text: "hello".into(),
            }],
            name: None,
            tool_call_id: None,
            tool_calls: Vec::new(),
        }],
        parameters: GenerationParameters::default(),
        tools: Vec::new(),
        tool_choice: None,
        response_format: None,
        extensions: SourceExtensions::new(
            Surface::Anthropic,
            std::collections::BTreeMap::from([("/vendor".into(), json!(true))]),
        ),
    };
    assert!(encode_response_create(&request, "upstream").is_err());
}

fn media_part(handle: &str, filename: &str, length: u64) -> BoundedMediaPart {
    BoundedMediaPart::new(
        MediaHandle::new(handle),
        filename,
        Some("application/octet-stream".into()),
        length,
        2 * 1024 * 1024,
    )
    .unwrap()
}

fn video_object(id: &str, status: &str) -> OpenAiVideoObject {
    OpenAiVideoObject {
        id: id.into(),
        object: "video".into(),
        model: "sora-2".into(),
        status: status.into(),
        progress: Some(100.0),
        created_at: Some(1_800_000_000),
        completed_at: Some(1_800_000_010),
        expires_at: None,
        prompt: Some("a calm ocean".into()),
        seconds: Some("8".into()),
        size: Some("1280x720".into()),
        remixed_from_video_id: None,
        error: None,
        extra: Default::default(),
    }
}

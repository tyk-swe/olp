use base64::{Engine as _, engine::general_purpose::STANDARD};
use olp_engine::domain::{
    canonical::{
        events::{Kind, validate_event_sequence},
        identity::Surface,
        requests::{
            GenerationParameters, GenerationRequest, ImageOperation, MediaHandle, MediaSource,
            Message, MessageRole, Operation, SourceExtensions, VideoOperation,
        },
        results::MediaArtifact,
    },
    ids::RouteSlug,
};
use olp_engine::protocols::openai::{
    audio::{
        Decoder as AudioDecoder, Encoder as AudioEncoder, SpeechRequest, SpeechStreamEvent,
        SpeechStreamUpdate, TranscriptionRequest, TranscriptionResponse, decode_speech,
        decode_speech_stream_event, decode_transcription, decode_transcription_response,
        encode_speech_stream_update, encode_transcription, encode_transcription_response,
    },
    client::{Encoder as ResponseEncoder, encode_response_object},
    embeddings::{
        EmbeddingRequest, EmbeddingResponse, decode_embedding_request, decode_embedding_response,
        encode_embedding_request, encode_embedding_response,
    },
    images::{
        ImageStreamOperation, ImageStreamUpdate, OpenAiImageEditRequest,
        OpenAiImageGenerationRequest, OpenAiImagePayload, OpenAiImageResponse,
        OpenAiImageStreamEvent, decode_image_edit, decode_image_generation, decode_image_response,
        decode_image_stream_event, encode_image_generation, encode_image_response,
        encode_image_stream_update,
    },
    media::BoundedMediaPart,
    moderation::{Request, Response, decode, decode_response, encode_response},
    responses::{
        request::{Create, decode_response_create, encode_response_create},
        response::{Object, decode_response_object},
        stream::Decoder as ResponseDecoder,
        token_count::{
            ResponseInputTokensRequest, ResponseInputTokensResponse, decode_response_input_tokens,
            decode_response_input_tokens_result, encode_response_input_tokens,
        },
    },
    video::{
        OpenAiVideoCreateRequest, OpenAiVideoDeleteResponse, OpenAiVideoListQuery,
        OpenAiVideoListResponse, OpenAiVideoObject, decode_video_create,
        decode_video_delete_response, decode_video_list, decode_video_list_response,
        encode_video_delete_response, encode_video_list_response,
    },
};
use serde_json::json;

#[test]
fn responses_request_round_trips_supported_semantics_and_extensions() {
    let wire: Create = serde_json::from_value(json!({
        "model": "team-responses",
        "instructions": "Be concise",
        "input": [{
            "type": "message",
            "role": "user",
            "content": [
                {"type": "input_text", "text": "describe this", "cache_hint": "short"},
                {"type": "input_image", "image_url": "https://example.test/a.png", "detail": "low"}
            ],
            "vendor_message": true
        }],
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
    assert_eq!(canonical.messages.len(), 2);
    assert_eq!(canonical.tools[0].name, "lookup");
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
    assert_eq!(encoded["tools"][0]["strict"], true);
    assert_eq!(encoded["service_tier"], "priority");
}

#[test]
fn responses_rejects_stateful_and_unspooled_media_semantics() {
    let stateful: Create = serde_json::from_value(json!({
        "model": "default",
        "input": "hello",
        "previous_response_id": "resp_previous"
    }))
    .unwrap();
    assert!(decode_response_create(stateful).is_err());

    let conversation: Create = serde_json::from_value(json!({
        "model": "default",
        "input": "hello",
        "conversation": {"id": "conv_stateful"}
    }))
    .unwrap();
    assert!(decode_response_create(conversation).is_err());

    let inline_file: Create = serde_json::from_value(json!({
        "model": "default",
        "input": [{"type": "message", "role": "user", "content": [{
            "type": "input_file", "file_data": "large-inline-payload"
        }]}]
    }))
    .unwrap();
    assert!(decode_response_create(inline_file).is_err());
}

#[test]
fn responses_preserves_builtin_tools_only_for_same_protocol() {
    let wire: Create = serde_json::from_value(json!({
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
    let response: Object = serde_json::from_value(json!({
        "id": "resp_1",
        "object": "response",
        "created_at": 1800000000,
        "status": "completed",
        "model": "gpt-upstream",
        "output": [{
            "id": "msg_1",
            "type": "message",
            "role": "assistant",
            "status": "completed",
            "content": [{"type": "output_text", "text": "hello", "annotations": []}]
        }],
        "usage": {"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}
    }))
    .unwrap();
    let events = decode_response_object(response).unwrap();
    validate_event_sequence(&events).unwrap();
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        Kind::TextDelta { text, .. } if text == "hello"
    )));
    let client = encode_response_object(&events, "team-route", "fallback", 1_800_000_000).unwrap();
    assert_eq!(client.model, "team-route");
    validate_event_sequence(&decode_response_object(client).unwrap()).unwrap();

    let mut encoder = ResponseEncoder::new("team-route", "fallback", 1_800_000_000);
    let mut client_frames = Vec::new();
    for event in events.clone() {
        client_frames.extend(encoder.push(event).unwrap());
    }
    assert_eq!(
        client_frames.last().unwrap().event.as_deref(),
        Some("response.completed")
    );

    let frames = [
        json!({"type":"response.created","response":{"id":"resp_s","model":"gpt-upstream"}}),
        json!({"type":"response.output_text.delta","output_index":0,"delta":"hé 🌍"}),
        json!({"type":"response.completed","response":{"usage":{"input_tokens":2,"output_tokens":2,"total_tokens":4}}}),
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
    let mut decoder = ResponseDecoder::new();
    let mut streamed = Vec::new();
    for byte in wire.as_bytes() {
        streamed.extend(decoder.push(std::slice::from_ref(byte)).unwrap());
    }
    streamed.extend(decoder.finish().unwrap());
    validate_event_sequence(&streamed).unwrap();
    assert!(streamed.iter().any(|event| matches!(
        &event.kind,
        Kind::TextDelta { text, .. } if text == "hé 🌍"
    )));
}

#[test]
fn unary_incomplete_response_remains_incomplete_when_streamed_to_the_client() {
    let response: Object = serde_json::from_value(json!({
        "id": "resp_incomplete",
        "object": "response",
        "created_at": 1800000000,
        "status": "incomplete",
        "model": "gpt-upstream",
        "output": [{
            "id": "msg_1",
            "type": "message",
            "role": "assistant",
            "status": "incomplete",
            "content": [{"type": "output_text", "text": "partial", "annotations": []}]
        }],
        "incomplete_details": {"reason": "max_output_tokens"}
    }))
    .unwrap();
    let events = decode_response_object(response).unwrap();
    let mut encoder = ResponseEncoder::new("team-route", "fallback", 1_800_000_000);
    let mut frames = Vec::new();
    for event in events {
        frames.extend(encoder.push(event).unwrap());
    }

    let terminal = frames.last().unwrap();
    assert_eq!(terminal.event.as_deref(), Some("response.incomplete"));
    let payload: serde_json::Value = serde_json::from_str(&terminal.data).unwrap();
    assert_eq!(payload["response"]["status"], "incomplete");
    assert_eq!(
        payload["response"]["incomplete_details"]["reason"],
        "max_output_tokens"
    );
}

#[test]
fn responses_reasoning_output_round_trips_without_becoming_message_content() {
    let response: Object = serde_json::from_value(json!({
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
    let encoded = serde_json::to_value(
        encode_response_object(&events, "team-route", "fallback", 1_800_000_000).unwrap(),
    )
    .unwrap();
    assert_eq!(encoded["output"][0]["type"], "reasoning");
    assert_eq!(encoded["output"][0]["encrypted_content"], "opaque");
    assert_eq!(encoded["output"][1]["content"][0]["text"], "answer");
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
    let forwarded = encode_response_input_tokens(&request, "gpt-upstream").unwrap();
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
        Ok(OpenAiImagePayload::Base64Json("re-encoded".into()))
    })
    .unwrap();
    assert_eq!(wire.data[0].b64_json.as_deref(), Some("re-encoded"));
}

#[test]
fn audio_requests_never_embed_uploaded_bytes() {
    let speech: SpeechRequest = serde_json::from_value(json!({
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

    let transcription = TranscriptionRequest {
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

    let response: TranscriptionResponse = serde_json::from_value(json!({
        "text": "hello",
        "language": "en",
        "duration": 1.5,
        "segments": [{"id": 0, "start": 0.0, "end": 1.5, "text": "hello", "speaker": "A"}]
    }))
    .unwrap();
    let result = decode_transcription_response(response);
    assert_eq!(result.segments[0].speaker.as_deref(), Some("A"));
    let encoded = encode_transcription_response(&result).unwrap();
    assert_eq!(decode_transcription_response(encoded).text, "hello");
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
        let request = TranscriptionRequest {
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

    let request = TranscriptionRequest {
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
    let encoded = encode_transcription(&canonical, "gpt-4o-transcribe-diarize", |_| {
        Ok(media_part("audio-ref", "audio.wav", 1024))
    })
    .unwrap();
    assert_eq!(
        encoded.extra["known_speaker_references"],
        json!(["data:audio/wav;base64,AAAA", "data:audio/wav;base64,BBBB"])
    );

    let invalid = TranscriptionRequest {
        response_format: Some("xml".into()),
        ..encoded
    };
    assert!(decode_transcription(invalid).is_err());
}

#[test]
fn moderation_preserves_dynamic_categories_and_multimodal_input() {
    let request: Request = serde_json::from_value(json!({
        "model": "moderation-route",
        "input": [
            {"type": "text", "text": "hello", "locale": "en"},
            {"type": "image_url", "image_url": {"url": "https://example.test/a.png"}}
        ]
    }))
    .unwrap();
    let Operation::Moderation(canonical) = decode(request).unwrap() else {
        panic!("wrong operation")
    };
    assert_eq!(canonical.input.len(), 2);
    assert_eq!(canonical.extensions.values["/input/0/locale"], "en");

    let response: Response = serde_json::from_value(json!({
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
    let result = decode_response(response);
    assert!(result.results[0].categories["violence"]);
    assert_eq!(result.results[0].category_scores["new/category"], 0.1);
    let encoded = encode_response(&result, "moderation-route", "modr_fallback").unwrap();
    assert_eq!(encoded.model, "moderation-route");
    assert!(decode_response(encoded).results[0].flagged);
}

#[test]
fn video_async_lifecycle_uses_current_videos_contract() {
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
fn media_stream_updates_are_bounded_handles_and_fragment_safe() {
    let image: OpenAiImageStreamEvent = serde_json::from_value(json!({
        "type": "image_generation.partial_image",
        "partial_image_index": 2,
        "b64_json": "opaque"
    }))
    .unwrap();
    let update =
        decode_image_stream_event(image, |_| Ok(MediaHandle::new("partial-image"))).unwrap();
    assert!(matches!(
        &update,
        ImageStreamUpdate::Partial {
            index: 2,
            image: olp_engine::domain::canonical::results::ImageArtifact {
                source: MediaSource::Handle(handle),
                ..
            },
            ..
        } if handle.as_str() == "partial-image"
    ));
    let encoded = encode_image_stream_update(&update, ImageStreamOperation::Generation, |_| {
        Ok("re-encoded".into())
    })
    .unwrap();
    assert_eq!(encoded.b64_json.as_deref(), Some("re-encoded"));

    let speech: SpeechStreamEvent = serde_json::from_value(json!({
        "type": "speech.audio.delta",
        "delta": "opaque"
    }))
    .unwrap();
    let update = decode_speech_stream_event(speech, |_| {
        Ok(MediaArtifact {
            handle: MediaHandle::new("speech-chunk"),
            content_type: Some("audio/mpeg".into()),
            content_length: Some(6),
        })
    })
    .unwrap();
    assert!(matches!(
        &update,
        SpeechStreamUpdate::Audio { media, .. }
            if media.handle.as_str() == "speech-chunk"
    ));
    let encoded = encode_speech_stream_update(&update, |_| Ok("re-encoded".into())).unwrap();
    assert_eq!(encoded.audio.as_deref(), Some("re-encoded"));

    let wire = concat!(
        "event: transcript.text.delta\n",
        "data: {\"type\":\"transcript.text.delta\",\"delta\":\"hé 🌍\"}\n\n",
        "event: transcript.text.done\n",
        "data: {\"type\":\"transcript.text.done\",\"usage\":{\"input_tokens\":2,\"output_tokens\":2,\"total_tokens\":4}}\n\n"
    );
    let mut decoder = AudioDecoder::new();
    let mut events = Vec::new();
    for byte in wire.as_bytes() {
        events.extend(decoder.push(std::slice::from_ref(byte)).unwrap());
    }
    events.extend(decoder.finish().unwrap());
    validate_event_sequence(&events).unwrap();
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        Kind::TextDelta { text, .. } if text == "hé 🌍"
    )));
    let mut encoder = AudioEncoder::new();
    let frames = events
        .iter()
        .flat_map(|event| encoder.push(event).unwrap())
        .collect::<Vec<_>>();
    assert_eq!(
        frames.last().unwrap().event.as_deref(),
        Some("transcript.text.done")
    );
}

#[test]
fn cross_protocol_extensions_fail_closed() {
    let request = GenerationRequest {
        route: RouteSlug::parse("route").unwrap(),
        messages: vec![Message {
            role: MessageRole::User,
            content: vec![olp_engine::domain::canonical::requests::ContentPart::Text {
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

fn responses_stream_frames(wire: &str, client_model: &str) -> Vec<serde_json::Value> {
    let mut decoder = ResponseDecoder::new();
    let mut events = decoder.push(wire.as_bytes()).unwrap();
    events.extend(decoder.finish().unwrap());
    validate_event_sequence(&events).unwrap();
    let mut encoder = ResponseEncoder::new(client_model, "resp_fallback", 1_800_000_000);
    events
        .into_iter()
        .flat_map(|event| encoder.push(event).unwrap())
        .map(|frame| serde_json::from_str(&frame.data).unwrap())
        .collect()
}

/// A2: `fc_…` (the output item) and `call_…` (the tool call) are distinct ids,
/// and aggregation is last-write-wins. Re-emitting `item_id` on every argument
/// delta overwrote the id the client has to post the tool result back with.
#[test]
fn streamed_tool_call_keeps_the_call_id_not_the_item_id() {
    let wire = concat!(
        "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-upstream\"}}\n\n",
        "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_abc\",\"call_id\":\"call_xyz\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\n",
        "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"fc_abc\",\"delta\":\"{\\\"city\\\":\\\"Paris\\\"}\"}\n\n",
        "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\"}}\n\n",
        "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":6,\"total_tokens\":10}}}\n\n"
    );
    let mut decoder = ResponseDecoder::new();
    let mut events = decoder.push(wire.as_bytes()).unwrap();
    events.extend(decoder.finish().unwrap());

    let ids = events
        .iter()
        .filter_map(|event| match &event.kind {
            Kind::ToolCallDelta { id, .. } => Some(id.clone()),
            _ => None,
        })
        .collect::<Vec<_>>();
    assert_eq!(ids, vec![Some("call_xyz".to_owned()), None]);

    let object = encode_response_object(&events, "team-route", "fallback", 0).unwrap();
    let wire = serde_json::to_value(&object).unwrap();
    assert_eq!(wire["output"][0]["call_id"], "call_xyz");
}

/// A8: the streaming decoder maps `incomplete_details` to a real finish reason;
/// the unary decoder hardcoded `Stop`, so truncation was indistinguishable from
/// completion.
#[test]
fn a_truncated_unary_response_reports_length_not_stop() {
    use olp_engine::domain::canonical::events::FinishReason;

    for (reason, expected) in [
        ("max_output_tokens", FinishReason::Length),
        ("content_filter", FinishReason::ContentFilter),
    ] {
        let response: Object = serde_json::from_value(json!({
            "id": "resp_truncated",
            "object": "response",
            "created_at": 1_800_000_000_i64,
            "status": "incomplete",
            "model": "gpt-upstream",
            "output": [{
                "id": "msg_1", "type": "message", "role": "assistant", "status": "incomplete",
                "content": [{"type": "output_text", "text": "partial", "annotations": []}]
            }],
            "incomplete_details": {"reason": reason}
        }))
        .unwrap();
        let events = decode_response_object(response).unwrap();
        let finish = events
            .iter()
            .find_map(|event| match &event.kind {
                Kind::Finish { reason, .. } => Some(reason.clone()),
                _ => None,
            })
            .expect("a finish reason must be decoded");
        assert_eq!(finish, expected, "incomplete_details.reason = {reason}");
    }
}

/// A23: parallel tool calls became separate chat *choices* with `n` unset, so a
/// client reading `choices[0]` lost the second call. A10: both calls also
/// consumed the same `/output/{n}/id` extension and shipped duplicate ids.
#[test]
fn parallel_tool_calls_stay_in_one_turn_with_distinct_item_ids() {
    let response: Object = serde_json::from_value(json!({
        "id": "resp_parallel",
        "object": "response",
        "created_at": 1_800_000_000_i64,
        "status": "completed",
        "model": "gpt-upstream",
        "output": [
            {"id": "fc_1", "type": "function_call", "call_id": "call_1",
             "name": "weather", "arguments": "{\"city\":\"Paris\"}", "status": "completed"},
            {"id": "fc_2", "type": "function_call", "call_id": "call_2",
             "name": "lookup", "arguments": "{\"q\":\"rust\"}", "status": "completed"}
        ]
    }))
    .unwrap();
    let events = decode_response_object(response).unwrap();
    validate_event_sequence(&events).unwrap();

    let outputs = events
        .iter()
        .filter_map(|event| match &event.kind {
            Kind::ToolCallDelta {
                output_index,
                tool_index,
                ..
            } => Some((*output_index, *tool_index)),
            _ => None,
        })
        .collect::<Vec<_>>();
    assert_eq!(outputs, vec![(0, 0), (0, 1)]);
    assert_eq!(
        events
            .iter()
            .filter(|event| matches!(event.kind, Kind::MessageStart { .. }))
            .count(),
        1
    );

    let wire =
        serde_json::to_value(encode_response_object(&events, "team-route", "fallback", 0).unwrap())
            .unwrap();
    assert_eq!(wire["output"][0]["id"], "fc_1");
    assert_eq!(wire["output"][0]["call_id"], "call_1");
    assert_eq!(wire["output"][1]["id"], "fc_2");
    assert_eq!(wire["output"][1]["call_id"], "call_2");
}

/// A9: clients that build tool calls from the item lifecycle (the OpenAI Agents
/// SDK among them) used to be told the tool was literally named `function`.
/// A19: every Responses event carries a monotonic `sequence_number`, and the
/// lifecycle events around a delta are required, not optional.
#[test]
fn the_responses_stream_encoder_emits_a_complete_numbered_lifecycle() {
    let wire = concat!(
        "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-upstream\"}}\n\n",
        "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_abc\",\"call_id\":\"call_xyz\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\n",
        "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"fc_abc\",\"delta\":\"{}\"}\n\n",
        "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\"}}\n\n",
        "data: {\"type\":\"response.completed\",\"response\":{}}\n\n"
    );
    let frames = responses_stream_frames(wire, "team-route");
    let kinds = frames
        .iter()
        .map(|frame| frame["type"].as_str().unwrap().to_owned())
        .collect::<Vec<_>>();
    assert_eq!(
        kinds,
        vec![
            "response.created",
            "response.in_progress",
            "response.output_item.added",
            "response.function_call_arguments.delta",
            "response.function_call_arguments.done",
            "response.output_item.done",
            "response.completed",
        ]
    );
    let sequence_numbers = frames
        .iter()
        .map(|frame| frame["sequence_number"].as_u64().unwrap())
        .collect::<Vec<_>>();
    assert_eq!(
        sequence_numbers,
        (0..frames.len() as u64).collect::<Vec<_>>()
    );

    let added = &frames[2];
    assert_eq!(added["item"]["name"], "lookup");
    assert_eq!(added["item"]["call_id"], "call_xyz");
    assert_ne!(added["item"]["name"], "function");
    assert_eq!(frames[3]["item_id"], added["item"]["id"]);
    assert_eq!(frames[5]["item"]["arguments"], "{}");
}

/// A19: a text turn gets the content-part and text lifecycle events too, and
/// the delta carries the `item_id` clients key on.
#[test]
fn a_streamed_text_turn_carries_item_ids_and_content_part_events() {
    let wire = concat!(
        "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-upstream\"}}\n\n",
        "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"hello\"}\n\n",
        "data: {\"type\":\"response.completed\",\"response\":{}}\n\n"
    );
    let frames = responses_stream_frames(wire, "team-route");
    let kinds = frames
        .iter()
        .map(|frame| frame["type"].as_str().unwrap().to_owned())
        .collect::<Vec<_>>();
    assert_eq!(
        kinds,
        vec![
            "response.created",
            "response.in_progress",
            "response.output_item.added",
            "response.content_part.added",
            "response.output_text.delta",
            "response.output_text.done",
            "response.content_part.done",
            "response.output_item.done",
            "response.completed",
        ]
    );
    let item_id = frames[2]["item"]["id"].as_str().unwrap().to_owned();
    assert_eq!(frames[4]["item_id"], item_id);
    assert_eq!(frames[5]["text"], "hello");
    assert_eq!(frames[7]["item"]["content"][0]["text"], "hello");
    assert_eq!(frames[7]["item"]["status"], "completed");
}

/// A13: `created_at` and `status` fell back to hardcoded values for any upstream
/// that was not literally OpenAI Responses, so `response.created` carried a real
/// timestamp while the terminal `response.completed` carried `0`.
#[test]
fn a_non_responses_upstream_still_reports_created_at_and_truncation() {
    use olp_engine::domain::canonical::events::{Event, FinishReason};

    let events = vec![
        Event::new(
            0,
            Kind::ResponseStart {
                response_id: Some("gen-1".into()),
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
                text: "partial".into(),
            },
        ),
        Event::new(
            3,
            Kind::Finish {
                output_index: 0,
                reason: FinishReason::Length,
            },
        ),
        Event::new(4, Kind::Done),
    ];

    let object = encode_response_object(&events, "team-route", "fallback", 1_800_000_000).unwrap();
    assert_eq!(object.created_at, 1_800_000_000);
    assert_eq!(object.status, "incomplete");
    assert_eq!(
        object
            .incomplete_details
            .as_ref()
            .and_then(|details| details["reason"].as_str()),
        Some("max_output_tokens")
    );

    let mut encoder = ResponseEncoder::new("team-route", "fallback", 1_800_000_000);
    let frames = events
        .into_iter()
        .flat_map(|event| encoder.push(event).unwrap())
        .map(|frame| serde_json::from_str::<serde_json::Value>(&frame.data).unwrap())
        .collect::<Vec<_>>();
    let created = frames.first().unwrap();
    let terminal = frames.last().unwrap();
    assert_eq!(terminal["type"], "response.incomplete");
    assert_eq!(
        terminal["response"]["created_at"],
        created["response"]["created_at"]
    );
    assert_eq!(terminal["response"]["created_at"], 1_800_000_000);
}

/// A12: `encode_response_content_part` was role-blind, so assistant history was
/// re-encoded as `input_text` — which a Responses upstream rejects — and the
/// codec did not survive its own round trip.
#[test]
fn assistant_history_is_re_encoded_as_output_text() {
    let wire: Create = serde_json::from_value(json!({
        "model": "team-responses",
        "input": [
            {"type": "message", "role": "user",
             "content": [{"type": "input_text", "text": "hi"}]},
            {"type": "message", "role": "assistant",
             "content": [{"type": "output_text", "text": "hello back"}]},
            {"type": "message", "role": "user",
             "content": [{"type": "input_text", "text": "and again"}]}
        ]
    }))
    .unwrap();
    let Operation::Generation(canonical) = decode_response_create(wire).unwrap() else {
        panic!("wrong operation");
    };
    let encoded =
        serde_json::to_value(encode_response_create(&canonical, "gpt-upstream").unwrap()).unwrap();
    assert_eq!(encoded["input"][0]["content"][0]["type"], "input_text");
    assert_eq!(encoded["input"][1]["role"], "assistant");
    assert_eq!(encoded["input"][1]["content"][0]["type"], "output_text");
    assert_eq!(encoded["input"][1]["content"][0]["text"], "hello back");
    assert_eq!(encoded["input"][2]["content"][0]["type"], "input_text");
}

/// A24: an invalid `encoding_format` used to surface as a 502 after the
/// provider had already been called and billed.
#[test]
fn an_invalid_encoding_format_is_rejected_before_dispatch() {
    let request: EmbeddingRequest = serde_json::from_value(json!({
        "model": "team-embed",
        "input": "hello",
        "encoding_format": "float16"
    }))
    .unwrap();
    let error = decode_embedding_request(request).unwrap_err();
    assert!(
        error.to_string().contains("float16"),
        "unexpected error: {error}"
    );

    for format in ["float", "base64"] {
        let request: EmbeddingRequest = serde_json::from_value(json!({
            "model": "team-embed",
            "input": "hello",
            "encoding_format": format
        }))
        .unwrap();
        assert!(decode_embedding_request(request).is_ok());
    }
}

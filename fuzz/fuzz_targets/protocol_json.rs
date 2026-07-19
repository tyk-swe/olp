#![no_main]
#![allow(clippy::collapsible_if)]

use libfuzzer_sys::fuzz_target;
use olp_domain::{MediaArtifact, MediaHandle, Operation, TokenCountResult};
use olp_protocols::{anthropic, gemini, openai};

fuzz_target!(|data: &[u8]| {
    if let Ok(request) = serde_json::from_slice::<openai::ChatCompletionRequest>(data) {
        if let Ok(Operation::Generation(canonical)) = openai::decode_chat_completion(request) {
            let _ = openai::encode_chat_completion(&canonical, "fuzz-provider-model");
        }
    }
    if let Ok(request) = serde_json::from_slice::<openai::ResponseCreateRequest>(data) {
        if let Ok(Operation::Generation(canonical)) = openai::decode_response_create(request) {
            let _ = openai::encode_response_create(&canonical, "fuzz-provider-model");
        }
    }
    if let Ok(request) = serde_json::from_slice::<openai::ResponseInputTokensRequest>(data) {
        if let Ok(Operation::TokenCount(canonical)) = openai::decode_response_input_tokens(request)
        {
            let _ = openai::encode_response_input_tokens(&canonical, "fuzz-provider-model");
        }
    }
    if let Ok(request) = serde_json::from_slice::<openai::EmbeddingRequest>(data) {
        if let Ok(Operation::Embeddings(canonical)) = openai::decode_embedding_request(request) {
            let _ = openai::encode_embedding_request(&canonical, "fuzz-provider-model");
        }
    }
    if let Ok(request) = serde_json::from_slice::<openai::OpenAiModerationRequest>(data) {
        if let Ok(Operation::Moderation(canonical)) = openai::decode_moderation(request) {
            let _ = openai::encode_moderation(&canonical, "fuzz-provider-model");
        }
    }
    if let Ok(request) = serde_json::from_slice::<openai::OpenAiImageGenerationRequest>(data) {
        if let Ok(Operation::Images(olp_domain::ImageOperation::Generation(canonical))) =
            openai::decode_image_generation(request)
        {
            let _ = openai::encode_image_generation(&canonical, "fuzz-provider-model");
        }
    }
    if let Ok(request) = serde_json::from_slice::<openai::OpenAiSpeechRequest>(data) {
        if let Ok(Operation::Speech(canonical)) = openai::decode_speech(request) {
            let _ = openai::encode_speech(&canonical, "fuzz-provider-model");
        }
    }
    if let Ok(request) = serde_json::from_slice::<openai::OpenAiVideoCreateRequest>(data) {
        if let Ok(Operation::Video(olp_domain::VideoOperation::Create(canonical))) =
            openai::decode_video_create(request)
        {
            let _ = openai::encode_video_create(&canonical, "fuzz-provider-model", |handle| {
                openai::BoundedMediaPart::new(
                    handle.clone(),
                    "fuzz.png",
                    Some("image/png".into()),
                    1,
                    openai::DEFAULT_VIDEO_REFERENCE_LIMIT,
                )
                .map_err(|error| openai::VideoCodecError::Staging(error.to_string()))
            });
        }
    }
    if let Ok(query) = serde_json::from_slice::<openai::OpenAiVideoListQuery>(data) {
        if let Ok(Operation::Video(olp_domain::VideoOperation::List(canonical))) =
            openai::decode_video_list(query)
        {
            let _ = openai::encode_video_list(&canonical);
        }
    }
    if let Ok(query) = serde_json::from_slice::<openai::OpenAiVideoContentQuery>(data) {
        let _ = openai::decode_video_content_with_query("fuzz-job", query);
    }
    if let Ok(request) = serde_json::from_slice::<anthropic::MessagesRequest>(data) {
        if let Ok(Operation::Generation(canonical)) = anthropic::decode_messages_request(request) {
            let _ = anthropic::encode_messages_request(&canonical, "fuzz-provider-model");
        }
    }
    if let Ok(request) = serde_json::from_slice::<anthropic::CountTokensRequest>(data) {
        let _ = anthropic::decode_count_tokens_request(request);
    }
    if let Ok(request) = serde_json::from_slice::<gemini::GenerateContentRequest>(data) {
        if let Ok(Operation::Generation(canonical)) =
            gemini::decode_generate_content_request("fuzz-model", request, false)
        {
            let _ = gemini::encode_generate_content_request(&canonical);
        }
    }
    if let Ok(request) = serde_json::from_slice::<gemini::CountTokensRequest>(data) {
        let _ = gemini::decode_count_tokens_request("fuzz-model", request);
    }

    if let Ok(response) = serde_json::from_slice::<openai::ChatCompletionResponse>(data) {
        if let Ok(events) = openai::decode_chat_completion_response(response) {
            let _ = openai::encode_chat_completion_client_response(
                &events,
                "fuzz-public-model",
                "fuzz-chat",
                1,
            );
        }
    }
    if let Ok(response) = serde_json::from_slice::<openai::ResponseObject>(data) {
        if let Ok(events) = openai::decode_response_object(response) {
            let _ = openai::encode_response_object(&events, "fuzz-public-model", "fuzz-response");
        }
    }
    if let Ok(response) = serde_json::from_slice::<openai::ResponseInputTokensResponse>(data) {
        let canonical = openai::decode_response_input_tokens_result(response);
        let _ = openai::encode_response_input_tokens_result(&canonical);
    }
    if let Ok(response) = serde_json::from_slice::<openai::EmbeddingResponse>(data) {
        if let Ok(canonical) = openai::decode_embedding_response(response) {
            let _ = openai::encode_embedding_response(&canonical, "fuzz-public-model", None);
        }
    }
    if let Ok(response) = serde_json::from_slice::<openai::OpenAiModelObject>(data) {
        if let Ok(canonical) = openai::decode_model_object(response) {
            let _ = openai::encode_model_object(&canonical);
        }
    }
    if let Ok(response) = serde_json::from_slice::<openai::OpenAiModelListResponse>(data) {
        if let Ok(canonical) = openai::decode_model_list_response(response) {
            let _ = openai::encode_model_list_response(&canonical);
        }
    }
    if let Ok(response) = serde_json::from_slice::<openai::OpenAiImageResponse>(data) {
        if let Ok(canonical) =
            openai::decode_image_response(response, |_| Ok(MediaHandle::new("fuzz-image-response")))
        {
            let _ = openai::encode_image_response(&canonical, |_| {
                Ok(openai::OpenAiImagePayload::Base64Json("Zg==".into()))
            });
        }
    }
    if let Ok(event) = serde_json::from_slice::<openai::OpenAiImageStreamEvent>(data) {
        if let Ok(canonical) =
            openai::decode_image_stream_event(event, |_| Ok(MediaHandle::new("fuzz-image-stream")))
        {
            let _ = openai::encode_image_stream_update(
                &canonical,
                openai::ImageStreamOperation::Generation,
                |_| Ok("Zg==".into()),
            );
        }
    }
    if let Ok(event) = serde_json::from_slice::<openai::OpenAiSpeechStreamEvent>(data) {
        if let Ok(canonical) = openai::decode_speech_stream_event(event, |_| {
            Ok(MediaArtifact {
                handle: MediaHandle::new("fuzz-speech-stream"),
                content_type: Some("audio/mpeg".into()),
                content_length: None,
            })
        }) {
            let _ = openai::encode_speech_stream_update(&canonical, |_| Ok("Zg==".into()));
        }
    }
    if let Ok(response) = serde_json::from_slice::<openai::OpenAiTranscriptionResponse>(data) {
        let canonical = openai::decode_transcription_response(response);
        let _ = openai::encode_transcription_response(&canonical);
    }
    if let Ok(response) = serde_json::from_slice::<openai::OpenAiModerationResponse>(data) {
        let canonical = openai::decode_moderation_response(response);
        let _ =
            openai::encode_moderation_response(&canonical, "fuzz-public-model", "fuzz-moderation");
    }
    if let Ok(response) = serde_json::from_slice::<openai::OpenAiVideoListResponse>(data) {
        if let Ok(canonical) = openai::decode_video_list_response(response) {
            let _ = openai::encode_video_list_response(&canonical, "fuzz-provider-model");
        }
    }
    if let Ok(response) = serde_json::from_slice::<openai::OpenAiVideoObject>(data) {
        if let Ok(canonical) = openai::decode_video_object(response) {
            let _ = openai::encode_video_object(&canonical, "fuzz-provider-model");
        }
    }
    if let Ok(response) = serde_json::from_slice::<openai::OpenAiVideoDeleteResponse>(data) {
        let canonical = openai::decode_video_delete_response(response);
        let _ = openai::encode_video_delete_response(&canonical);
    }
    if let Ok(response) = serde_json::from_slice::<anthropic::MessagesResponse>(data) {
        if let Ok(events) = anthropic::decode_messages_response(response) {
            let _ =
                anthropic::encode_messages_response(&events, "fuzz-public-model", "fuzz-message");
        }
    }
    if let Ok(response) = serde_json::from_slice::<anthropic::CountTokensResponse>(data) {
        let _ = anthropic::encode_count_tokens_result(&TokenCountResult {
            input_tokens: response.input_tokens,
            extensions: olp_domain::SourceExtensions::new(
                olp_domain::Surface::Anthropic,
                response.extra,
            ),
        });
    }
    if let Ok(response) = serde_json::from_slice::<gemini::GenerateContentResponse>(data) {
        if let Ok(events) = gemini::decode_generate_content_response(response) {
            let _ = gemini::encode_generate_content_response(
                &events,
                "fuzz-public-model",
                "fuzz-response",
            );
        }
    }
    if let Ok(response) = serde_json::from_slice::<gemini::CountTokensResponse>(data) {
        let mut values = std::collections::BTreeMap::new();
        if let Some(cached) = response.cached_content_token_count {
            values.insert("/cachedContentTokenCount".into(), cached.into());
        }
        values.extend(
            response
                .extra
                .into_iter()
                .map(|(name, value)| (format!("/{name}"), value)),
        );
        let _ = gemini::encode_count_tokens_result(&TokenCountResult {
            input_tokens: response.total_tokens,
            extensions: olp_domain::SourceExtensions::new(olp_domain::Surface::Gemini, values),
        });
    }

    let _ = openai::decode_model_list();
    let _ = openai::decode_video_get("fuzz-job");
    let _ = openai::decode_video_content("fuzz-job");
    let _ = openai::decode_video_delete("fuzz-job");
    let binary = openai::BinaryMediaBody {
        media: MediaArtifact {
            handle: MediaHandle::new("fuzz-binary"),
            content_type: Some("application/octet-stream".into()),
            content_length: Some(u64::try_from(data.len()).unwrap_or(u64::MAX)),
        },
    };
    let speech = openai::decode_speech_body(binary.clone());
    let _ = openai::encode_speech_body(&speech);
    let video = openai::decode_video_content_body(binary);
    let _ = openai::encode_video_content_body(&video);
});

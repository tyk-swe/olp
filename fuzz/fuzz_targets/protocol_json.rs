#![no_main]

//! Roundtrip oracles for the request and response codecs.
//!
//! Each pair below is driven through `oracle::roundtrip`, which asserts that
//! the encoder's output is accepted by its own decoder and that a second
//! encode reproduces the first exactly. See `oracle.rs` for why the first
//! encode is excluded from the comparison.

use libfuzzer_sys::fuzz_target;
use olp_engine::domain::{MediaArtifact, MediaHandle, Operation, TokenCountResult};
use olp_engine::protocols::{anthropic, gemini, openai};

mod oracle;
use oracle::roundtrip;

const UPSTREAM_MODEL: &str = "fuzz-provider-model";
const PUBLIC_MODEL: &str = "fuzz-public-model";

fuzz_target!(|data: &[u8]| {
    // ---- request codecs -------------------------------------------------

    roundtrip(
        data,
        "openai::chat_completion",
        |request| match openai::decode_chat_completion(request) {
            Ok(Operation::Generation(canonical)) => Some(canonical),
            _ => None,
        },
        |canonical| openai::encode_chat_completion(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::response_create",
        |request| match openai::decode_response_create(request) {
            Ok(Operation::Generation(canonical)) => Some(canonical),
            _ => None,
        },
        |canonical| openai::encode_response_create(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::response_input_tokens",
        |request| match openai::decode_response_input_tokens(request) {
            Ok(Operation::TokenCount(canonical)) => Some(canonical),
            _ => None,
        },
        |canonical| openai::encode_response_input_tokens(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::embedding_request",
        |request| match openai::decode_embedding_request(request) {
            Ok(Operation::Embeddings(canonical)) => Some(canonical),
            _ => None,
        },
        |canonical| openai::encode_embedding_request(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::moderation_request",
        |request| match openai::decode_moderation(request) {
            Ok(Operation::Moderation(canonical)) => Some(canonical),
            _ => None,
        },
        |canonical| openai::encode_moderation(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::image_generation_request",
        |request| match openai::decode_image_generation(request) {
            Ok(Operation::Images(olp_engine::domain::ImageOperation::Generation(canonical))) => {
                Some(canonical)
            }
            _ => None,
        },
        |canonical| openai::encode_image_generation(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::speech_request",
        |request| match openai::decode_speech(request) {
            Ok(Operation::Speech(canonical)) => Some(canonical),
            _ => None,
        },
        |canonical| openai::encode_speech(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::video_create_request",
        |request| match openai::decode_video_create(request) {
            Ok(Operation::Video(olp_engine::domain::VideoOperation::Create(canonical))) => {
                Some(canonical)
            }
            _ => None,
        },
        |canonical| {
            openai::encode_video_create(canonical, UPSTREAM_MODEL, |handle| {
                openai::BoundedMediaPart::new(
                    handle.clone(),
                    "fuzz.png",
                    Some("image/png".into()),
                    1,
                    openai::DEFAULT_VIDEO_REFERENCE_LIMIT,
                )
                .map_err(|error| openai::VideoCodecError::Staging(error.to_string()))
            })
        },
    );
    roundtrip(
        data,
        "openai::video_list_query",
        |query| match openai::decode_video_list(query) {
            Ok(Operation::Video(olp_engine::domain::VideoOperation::List(canonical))) => {
                Some(canonical)
            }
            _ => None,
        },
        openai::encode_video_list,
    );
    roundtrip(
        data,
        "anthropic::messages_request",
        |request| match anthropic::decode_messages_request(request) {
            Ok(Operation::Generation(canonical)) => Some(canonical),
            _ => None,
        },
        |canonical| anthropic::encode_messages_request(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "gemini::generate_content_request",
        |request| match gemini::decode_generate_content_request("fuzz-model", request, false) {
            Ok(Operation::Generation(canonical)) => Some(canonical),
            _ => None,
        },
        gemini::encode_generate_content_request,
    );

    // ---- response codecs ------------------------------------------------

    roundtrip(
        data,
        "openai::response_object",
        |response| openai::decode_response_object(response).ok(),
        |events| openai::encode_response_object(events, PUBLIC_MODEL, "fuzz-response"),
    );
    roundtrip(
        data,
        "openai::response_input_tokens_result",
        |response| Some(openai::decode_response_input_tokens_result(response)),
        openai::encode_response_input_tokens_result,
    );
    roundtrip(
        data,
        "openai::embedding_response",
        |response| openai::decode_embedding_response(response).ok(),
        |canonical| openai::encode_embedding_response(canonical, PUBLIC_MODEL, None),
    );
    roundtrip(
        data,
        "openai::transcription_response",
        |response| openai::decode_transcription_response(response).ok(),
        openai::encode_transcription_response,
    );
    roundtrip(
        data,
        "openai::moderation_response",
        |response| Some(openai::decode_moderation_response(response)),
        |canonical| openai::encode_moderation_response(canonical, PUBLIC_MODEL, "fuzz-moderation"),
    );
    roundtrip(
        data,
        "openai::video_list_response",
        |response| openai::decode_video_list_response(response).ok(),
        |canonical| openai::encode_video_list_response(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::video_object",
        |response| openai::decode_video_object(response).ok(),
        |canonical| openai::encode_video_object(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::video_delete_response",
        |response| Some(openai::decode_video_delete_response(response)),
        openai::encode_video_delete_response,
    );
    roundtrip(
        data,
        "openai::image_response",
        |response| {
            openai::decode_image_response(response, |_| Ok(MediaHandle::new("fuzz-image"))).ok()
        },
        |canonical| {
            openai::encode_image_response(canonical, |_| {
                Ok(openai::OpenAiImagePayload::Base64Json("Zg==".into()))
            })
        },
    );
    roundtrip(
        data,
        "openai::image_stream_event",
        |event| {
            openai::decode_image_stream_event(event, |_| Ok(MediaHandle::new("fuzz-image-stream")))
                .ok()
        },
        |canonical| {
            openai::encode_image_stream_update(
                canonical,
                openai::ImageStreamOperation::Generation,
                |_| Ok("Zg==".into()),
            )
        },
    );
    roundtrip(
        data,
        "openai::speech_stream_event",
        |event| {
            openai::decode_speech_stream_event(event, |_| {
                Ok(MediaArtifact {
                    handle: MediaHandle::new("fuzz-speech-stream"),
                    content_type: Some("audio/mpeg".into()),
                    content_length: None,
                })
            })
            .ok()
        },
        |canonical| openai::encode_speech_stream_update(canonical, |_| Ok("Zg==".into())),
    );
    roundtrip(
        data,
        "anthropic::messages_response",
        |response| anthropic::decode_messages_response(response).ok(),
        |events| anthropic::encode_messages_response(events, PUBLIC_MODEL, "fuzz-message"),
    );
    roundtrip(
        data,
        "gemini::generate_content_response",
        |response| gemini::decode_generate_content_response(response).ok(),
        |events| gemini::encode_generate_content_response(events, PUBLIC_MODEL, "fuzz-response"),
    );

    // ---- codecs without a symmetric counterpart -------------------------
    // These have no encode that returns the decoded type, so only the decode
    // side is exercised. Kept so the corpus still reaches them.

    if let Ok(response) = serde_json::from_slice::<openai::ChatCompletionResponse>(data) {
        let _ = openai::decode_chat_completion_response(response);
    }

    if let Ok(request) = serde_json::from_slice::<anthropic::CountTokensRequest>(data) {
        let _ = anthropic::decode_count_tokens_request(request);
    }
    if let Ok(request) = serde_json::from_slice::<gemini::CountTokensRequest>(data) {
        let _ = gemini::decode_count_tokens_request("fuzz-model", request);
    }
    if let Ok(query) = serde_json::from_slice::<openai::OpenAiVideoContentQuery>(data) {
        let _ = openai::decode_video_content_with_query("fuzz-job", query);
    }
    if let Ok(response) = serde_json::from_slice::<anthropic::CountTokensResponse>(data) {
        let _ = anthropic::encode_count_tokens_result(&TokenCountResult {
            input_tokens: response.input_tokens,
            extensions: olp_engine::domain::SourceExtensions::new(
                olp_engine::domain::Surface::Anthropic,
                response.extra,
            ),
        });
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
            extensions: olp_engine::domain::SourceExtensions::new(
                olp_engine::domain::Surface::Gemini,
                values,
            ),
        });
    }

    let _ = openai::decode_video_get("fuzz-job");
    let _ = openai::decode_video_content("fuzz-job");
    let _ = openai::decode_video_delete("fuzz-job");

    // Binary media bodies carry no JSON, so they are driven off the raw length
    // rather than a parsed document.
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

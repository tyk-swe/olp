#![no_main]

//! Roundtrip oracles for the request and response codecs.
//!
//! Each pair below is driven through `oracle::roundtrip`, which asserts that
//! the encoder's output is accepted by its own decoder and that a second
//! encode reproduces the first exactly. See `oracle.rs` for why the first
//! encode is excluded from the comparison.

use libfuzzer_sys::fuzz_target;
use olp_engine::domain::canonical::{
    identity::Surface,
    requests::{ImageOperation, MediaHandle, Operation, SourceExtensions, VideoOperation},
    results::{MediaArtifact, TokenCountResult},
};
use olp_engine::protocols::{
    anthropic::{
        client as anthropic_client, count as anthropic_count, dto as anthropic_dto,
        translate::{
            decode as anthropic_decode, encode as anthropic_encode, response as anthropic_response,
        },
    },
    gemini::{
        client as gemini_client, count as gemini_count, dto as gemini_dto,
        translate::{
            decode as gemini_decode, encode as gemini_encode, response as gemini_response,
        },
    },
    openai::{
        audio, chat, client as openai_client, embeddings, images,
        media::{BinaryMediaBody, BoundedMediaPart},
        moderation,
        responses::{request as response_request, response as response_object, token_count},
        video,
    },
};

mod oracle;
use oracle::roundtrip;

const UPSTREAM_MODEL: &str = "fuzz-provider-model";
const PUBLIC_MODEL: &str = "fuzz-public-model";

fuzz_target!(|data: &[u8]| {
    // ---- request codecs -------------------------------------------------

    roundtrip(
        data,
        "openai::chat_completion",
        |request| match chat::decode::chat_completion(request) {
            Ok(Operation::Generation(canonical)) => Some(canonical),
            _ => None,
        },
        |canonical| chat::encode::chat_completion(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::response_create",
        |request| match response_request::decode_response_create(request) {
            Ok(Operation::Generation(canonical)) => Some(canonical),
            _ => None,
        },
        |canonical| response_request::encode_response_create(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::response_input_tokens",
        |request| match token_count::decode_response_input_tokens(request) {
            Ok(Operation::TokenCount(canonical)) => Some(canonical),
            _ => None,
        },
        |canonical| token_count::encode_response_input_tokens(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::embedding_request",
        |request| match embeddings::decode_embedding_request(request) {
            Ok(Operation::Embeddings(canonical)) => Some(canonical),
            _ => None,
        },
        |canonical| embeddings::encode_embedding_request(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::moderation_request",
        |request| match moderation::decode(request) {
            Ok(Operation::Moderation(canonical)) => Some(canonical),
            _ => None,
        },
        |canonical| moderation::encode(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::image_generation_request",
        |request| match images::decode_image_generation(request) {
            Ok(Operation::Images(ImageOperation::Generation(canonical))) => Some(canonical),
            _ => None,
        },
        |canonical| images::encode_image_generation(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::speech_request",
        |request| match audio::decode_speech(request) {
            Ok(Operation::Speech(canonical)) => Some(canonical),
            _ => None,
        },
        |canonical| audio::encode_speech(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::video_create_request",
        |request| match video::decode_video_create(request) {
            Ok(Operation::Video(VideoOperation::Create(canonical))) => Some(canonical),
            _ => None,
        },
        |canonical| {
            video::encode_video_create(canonical, UPSTREAM_MODEL, |handle| {
                BoundedMediaPart::new(
                    handle.clone(),
                    "fuzz.png",
                    Some("image/png".into()),
                    1,
                    video::DEFAULT_VIDEO_REFERENCE_LIMIT,
                )
                .map_err(|error| video::Error::Staging(error.to_string()))
            })
        },
    );
    roundtrip(
        data,
        "openai::video_list_query",
        |query| match video::decode_video_list(query) {
            Ok(Operation::Video(VideoOperation::List(canonical))) => Some(canonical),
            _ => None,
        },
        video::encode_video_list,
    );
    roundtrip(
        data,
        "anthropic::messages_request",
        |request| match anthropic_decode::request(request) {
            Ok(Operation::Generation(canonical)) => Some(canonical),
            _ => None,
        },
        |canonical| anthropic_encode::request(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "gemini::generate_content_request",
        |request| match gemini_decode::request("fuzz-model", request, false) {
            Ok(Operation::Generation(canonical)) => Some(canonical),
            _ => None,
        },
        gemini_encode::request,
    );

    // ---- response codecs ------------------------------------------------

    roundtrip(
        data,
        "openai::response_object",
        |response| response_object::decode_response_object(response).ok(),
        |events| openai_client::encode_response_object(events, PUBLIC_MODEL, "fuzz-response"),
    );
    roundtrip(
        data,
        "openai::response_input_tokens_result",
        |response| Some(token_count::decode_response_input_tokens_result(response)),
        token_count::encode_response_input_tokens_result,
    );
    roundtrip(
        data,
        "openai::embedding_response",
        |response| embeddings::decode_embedding_response(response).ok(),
        |canonical| embeddings::encode_embedding_response(canonical, PUBLIC_MODEL, None),
    );
    roundtrip(
        data,
        "openai::transcription_response",
        |response| Some(audio::decode_transcription_response(response)),
        audio::encode_transcription_response,
    );
    roundtrip(
        data,
        "openai::moderation_response",
        |response| Some(moderation::decode_response(response)),
        |canonical| moderation::encode_response(canonical, PUBLIC_MODEL, "fuzz-moderation"),
    );
    roundtrip(
        data,
        "openai::video_list_response",
        |response| video::decode_video_list_response(response).ok(),
        |canonical| video::encode_video_list_response(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::video_object",
        |response| video::decode_video_object(response).ok(),
        |canonical| video::encode_video_object(canonical, UPSTREAM_MODEL),
    );
    roundtrip(
        data,
        "openai::video_delete_response",
        |response| Some(video::decode_video_delete_response(response)),
        video::encode_video_delete_response,
    );
    roundtrip(
        data,
        "openai::image_response",
        |response| {
            images::decode_image_response(response, |_| Ok(MediaHandle::new("fuzz-image"))).ok()
        },
        |canonical| {
            images::encode_image_response(canonical, |_| {
                Ok(images::OpenAiImagePayload::Base64Json("Zg==".into()))
            })
        },
    );
    roundtrip(
        data,
        "openai::image_stream_event",
        |event| {
            images::decode_image_stream_event(event, |_| Ok(MediaHandle::new("fuzz-image-stream")))
                .ok()
        },
        |canonical| {
            images::encode_image_stream_update(
                canonical,
                images::ImageStreamOperation::Generation,
                |_| Ok("Zg==".into()),
            )
        },
    );
    roundtrip(
        data,
        "openai::speech_stream_event",
        |event| {
            audio::decode_speech_stream_event(event, |_| {
                Ok(MediaArtifact {
                    handle: MediaHandle::new("fuzz-speech-stream"),
                    content_type: Some("audio/mpeg".into()),
                    content_length: None,
                })
            })
            .ok()
        },
        |canonical| audio::encode_speech_stream_update(canonical, |_| Ok("Zg==".into())),
    );
    roundtrip(
        data,
        "anthropic::messages_response",
        |response| anthropic_response::decode(response).ok(),
        |events| anthropic_client::encode_messages_response(events, PUBLIC_MODEL, "fuzz-message"),
    );
    roundtrip(
        data,
        "gemini::generate_content_response",
        |response| gemini_response::decode(response).ok(),
        |events| {
            gemini_client::encode_generate_content_response(events, PUBLIC_MODEL, "fuzz-response")
        },
    );

    // ---- codecs without a symmetric counterpart -------------------------
    // These have no encode that returns the decoded type, so only the decode
    // side is exercised. Kept so the corpus still reaches them.

    if let Ok(request) = serde_json::from_slice::<anthropic_dto::CountTokensRequest>(data) {
        let _ = anthropic_count::decode_count_tokens_request(request);
    }
    if let Ok(request) = serde_json::from_slice::<gemini_dto::CountTokensRequest>(data) {
        let _ = gemini_count::decode_count_tokens_request("fuzz-model", request);
    }
    if let Ok(query) = serde_json::from_slice::<video::OpenAiVideoContentQuery>(data) {
        let _ = video::decode_video_content_with_query("fuzz-job", query);
    }
    if let Ok(response) = serde_json::from_slice::<anthropic_dto::CountTokensResponse>(data) {
        let _ = anthropic_count::encode_count_tokens_result(&TokenCountResult {
            input_tokens: response.input_tokens,
            extensions: SourceExtensions::new(Surface::Anthropic, response.extra),
        });
    }
    if let Ok(response) = serde_json::from_slice::<gemini_dto::CountTokensResponse>(data) {
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
        let _ = gemini_count::encode_count_tokens_result(&TokenCountResult {
            input_tokens: response.total_tokens,
            extensions: SourceExtensions::new(Surface::Gemini, values),
        });
    }

    let _ = video::decode_video_get("fuzz-job");
    let _ = video::decode_video_content("fuzz-job");
    let _ = video::decode_video_delete("fuzz-job");

    // Binary media bodies carry no JSON, so they are driven off the raw length
    // rather than a parsed document.
    let binary = BinaryMediaBody {
        media: MediaArtifact {
            handle: MediaHandle::new("fuzz-binary"),
            content_type: Some("application/octet-stream".into()),
            content_length: Some(u64::try_from(data.len()).unwrap_or(u64::MAX)),
        },
    };
    let speech = audio::decode_speech_body(binary.clone());
    let _ = audio::encode_speech_body(&speech);
    let _ = video::decode_video_content_body(binary);
});

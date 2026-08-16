use std::collections::BTreeSet;

use olp_conformance::read_json;
use olp_engine::domain::{
    canonical::{
        identity::{OperationKind, Surface},
        requests::{ImageOperation, MediaHandle, Operation, VideoOperation},
    },
    ids::RouteSlug,
};
use olp_engine::protocols::{
    anthropic::{
        count as anthropic_count,
        translate::{decode as anthropic_decode, encode as anthropic_encode},
    },
    gemini::{
        count as gemini_count,
        translate::{decode as gemini_decode, encode as gemini_encode},
    },
    openai::{
        audio, chat, embeddings, images,
        media::BoundedMediaPart,
        moderation,
        responses::{request as response_request, token_count as response_token_count},
        video,
    },
};
use serde::Deserialize;
use serde_json::Value;

#[derive(Debug, Deserialize)]
struct OperationFixture {
    name: String,
    codec: String,
    surface: Surface,
    expected_operation: OperationKind,
    expected_route: Option<String>,
    wire: Value,
}

#[test]
fn every_selected_operation_family_has_a_decoding_and_encoding_golden_case() {
    let fixtures: Vec<OperationFixture> = read_json("protocols/selected-operation-families.json");
    let mut covered = BTreeSet::new();

    for fixture in fixtures {
        let operation = decode(&fixture);
        assert_eq!(
            operation.kind(),
            fixture.expected_operation,
            "{} decoded to the wrong operation",
            fixture.name
        );
        assert_eq!(
            operation.route().map(RouteSlug::as_str),
            fixture.expected_route.as_deref(),
            "{} decoded to the wrong public route",
            fixture.name
        );
        assert_eq!(
            operation.extensions().and_then(|value| value.source),
            Some(fixture.surface),
            "{} lost its source surface",
            fixture.name
        );
        assert_encodes(&fixture.codec, &operation);
        covered.insert(operation.kind());
    }

    assert_eq!(
        covered,
        BTreeSet::from([
            OperationKind::Generation,
            OperationKind::Embeddings,
            OperationKind::TokenCount,
            OperationKind::ImageGeneration,
            OperationKind::ImageEdit,
            OperationKind::ImageVariation,
            OperationKind::Speech,
            OperationKind::Transcription,
            OperationKind::VideoCreate,
            OperationKind::VideoList,
            OperationKind::VideoGet,
            OperationKind::VideoContent,
            OperationKind::VideoDelete,
            OperationKind::Moderation,
        ]),
        "the golden corpus must cover every selected canonical operation"
    );
}

fn decode(fixture: &OperationFixture) -> Operation {
    let wire = fixture.wire.clone();
    match fixture.codec.as_str() {
        "openai_chat" => chat::decode_chat_completion(from_wire(wire)).unwrap(),
        "openai_responses" => response_request::decode_response_create(from_wire(wire)).unwrap(),
        "openai_response_input_tokens" => {
            response_token_count::decode_response_input_tokens(from_wire(wire)).unwrap()
        }
        "openai_embeddings" => embeddings::decode_embedding_request(from_wire(wire)).unwrap(),
        "openai_image_generation" => images::decode_image_generation(from_wire(wire)).unwrap(),
        "openai_image_edit" => images::decode_image_edit(from_wire(wire)).unwrap(),
        "openai_image_variation" => images::decode_image_variation(from_wire(wire)).unwrap(),
        "openai_speech" => audio::decode_speech(from_wire(wire)).unwrap(),
        "openai_transcription" => audio::decode_transcription(from_wire(wire)).unwrap(),
        "openai_moderation" => moderation::decode(from_wire(wire)).unwrap(),
        "openai_video_create" => video::decode_video_create(from_wire(wire)).unwrap(),
        "openai_video_list" => video::decode_video_list(from_wire(wire)).unwrap(),
        "openai_video_get" => video::decode_video_get(required_string(&wire, "job_id")),
        "openai_video_content" => video::decode_video_content_with_query(
            required_string(&wire, "job_id"),
            video::OpenAiVideoContentQuery {
                variant: Some(required_string(&wire, "variant")),
                extra: Default::default(),
            },
        )
        .unwrap(),
        "openai_video_delete" => video::decode_video_delete(required_string(&wire, "job_id")),
        "anthropic_messages" => anthropic_decode::request(from_wire(wire)).unwrap(),
        "anthropic_count_tokens" => {
            anthropic_count::decode_count_tokens_request(from_wire(wire)).unwrap()
        }
        "gemini_generate_content" => gemini_decode::request(
            fixture.expected_route.as_deref().unwrap(),
            from_wire(wire),
            false,
        )
        .unwrap(),
        "gemini_count_tokens" => gemini_count::decode_count_tokens_request(
            fixture.expected_route.as_deref().unwrap(),
            from_wire(wire),
        )
        .unwrap(),
        codec => panic!("unknown golden codec {codec}"),
    }
}

fn assert_encodes(codec: &str, operation: &Operation) {
    match (codec, operation) {
        ("openai_chat", Operation::Generation(request)) => {
            chat::encode_chat_completion(request, "provider-model").unwrap();
        }
        ("openai_responses", Operation::Generation(request)) => {
            response_request::encode_response_create(request, "provider-model").unwrap();
        }
        ("openai_response_input_tokens", Operation::TokenCount(request)) => {
            response_token_count::encode_response_input_tokens(request, "provider-model").unwrap();
        }
        ("openai_embeddings", Operation::Embeddings(request)) => {
            embeddings::encode_embedding_request(request, "provider-model").unwrap();
        }
        ("openai_image_generation", Operation::Images(ImageOperation::Generation(request))) => {
            images::encode_image_generation(request, "provider-model").unwrap();
        }
        ("openai_image_edit", Operation::Images(ImageOperation::Edit(request))) => {
            images::encode_image_edit(request, "provider-model", bounded_image).unwrap();
        }
        ("openai_image_variation", Operation::Images(ImageOperation::Variation(request))) => {
            images::encode_image_variation(request, "provider-model", bounded_image).unwrap();
        }
        ("openai_speech", Operation::Speech(request)) => {
            audio::encode_speech(request, "provider-model").unwrap();
        }
        ("openai_transcription", Operation::Transcription(request)) => {
            audio::encode_transcription(request, "provider-model", |handle| {
                BoundedMediaPart::new(
                    handle.clone(),
                    "fixture.wav",
                    Some("audio/wav".into()),
                    1,
                    audio::DEFAULT_AUDIO_UPLOAD_LIMIT,
                )
                .map_err(|_| audio::Error::InvalidMediaPart)
            })
            .unwrap();
        }
        ("openai_moderation", Operation::Moderation(request)) => {
            moderation::encode(request, "provider-model").unwrap();
        }
        ("openai_video_create", Operation::Video(VideoOperation::Create(request))) => {
            video::encode_video_create(request, "provider-model", |handle| {
                BoundedMediaPart::new(
                    handle.clone(),
                    "fixture.png",
                    Some("image/png".into()),
                    1,
                    video::DEFAULT_VIDEO_REFERENCE_LIMIT,
                )
                .map_err(|error| video::Error::Staging(error.to_string()))
            })
            .unwrap();
        }
        ("openai_video_list", Operation::Video(VideoOperation::List(request))) => {
            video::encode_video_list(request).unwrap();
        }
        ("anthropic_messages", Operation::Generation(request)) => {
            anthropic_encode::request(request, "provider-model").unwrap();
        }
        ("gemini_generate_content", Operation::Generation(request)) => {
            gemini_encode::request(request).unwrap();
        }
        // These public contracts are encoded by path/query selection or have
        // response-only encoders; successful canonical decoding is the full
        // request-side codec contract for the case.
        (
            "openai_video_get" | "openai_video_content" | "openai_video_delete",
            Operation::Video(_),
        )
        | ("anthropic_count_tokens" | "gemini_count_tokens", Operation::TokenCount(_)) => {}
        _ => panic!("golden codec {codec} did not match its canonical operation"),
    }
}

fn bounded_image(handle: &MediaHandle) -> Result<BoundedMediaPart, images::ImageCodecError> {
    BoundedMediaPart::new(
        handle.clone(),
        "fixture.png",
        Some("image/png".into()),
        1,
        50 * 1024 * 1024,
    )
    .map_err(|error| images::ImageCodecError::InvalidMediaPart(error.to_string()))
}

fn from_wire<T: serde::de::DeserializeOwned>(wire: Value) -> T {
    serde_json::from_value(wire).unwrap()
}

fn required_string(value: &Value, field: &str) -> String {
    value[field].as_str().unwrap().to_owned()
}

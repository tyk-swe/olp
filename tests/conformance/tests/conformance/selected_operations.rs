use std::collections::BTreeSet;

use olp_conformance::read_json;
use olp_domain::{ImageOperation, MediaHandle, Operation, OperationKind, Surface, VideoOperation};
use olp_protocols::{anthropic, gemini, openai};
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
            operation.route().map(olp_domain::RouteSlug::as_str),
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
            OperationKind::ModelList,
            OperationKind::ModelGet,
        ]),
        "the golden corpus must cover every selected canonical operation"
    );
}

fn decode(fixture: &OperationFixture) -> Operation {
    let wire = fixture.wire.clone();
    match fixture.codec.as_str() {
        "openai_chat" => openai::decode_chat_completion(from_wire(wire)).unwrap(),
        "openai_responses" => openai::decode_response_create(from_wire(wire)).unwrap(),
        "openai_response_input_tokens" => {
            openai::decode_response_input_tokens(from_wire(wire)).unwrap()
        }
        "openai_embeddings" => openai::decode_embedding_request(from_wire(wire)).unwrap(),
        "openai_image_generation" => openai::decode_image_generation(from_wire(wire)).unwrap(),
        "openai_image_edit" => openai::decode_image_edit(from_wire(wire)).unwrap(),
        "openai_image_variation" => openai::decode_image_variation(from_wire(wire)).unwrap(),
        "openai_speech" => openai::decode_speech(from_wire(wire)).unwrap(),
        "openai_transcription" => openai::decode_transcription(from_wire(wire)).unwrap(),
        "openai_moderation" => openai::decode_moderation(from_wire(wire)).unwrap(),
        "openai_video_create" => openai::decode_video_create(from_wire(wire)).unwrap(),
        "openai_video_list" => openai::decode_video_list(from_wire(wire)).unwrap(),
        "openai_video_get" => openai::decode_video_get(required_string(&wire, "job_id")),
        "openai_video_content" => openai::decode_video_content_with_query(
            required_string(&wire, "job_id"),
            openai::OpenAiVideoContentQuery {
                variant: Some(required_string(&wire, "variant")),
                extra: Default::default(),
            },
        )
        .unwrap(),
        "openai_video_delete" => openai::decode_video_delete(required_string(&wire, "job_id")),
        "openai_model_list" => openai::decode_model_list(),
        "openai_model_get" => openai::decode_model_get(&required_string(&wire, "model")).unwrap(),
        "anthropic_messages" => anthropic::decode_messages_request(from_wire(wire)).unwrap(),
        "anthropic_count_tokens" => {
            anthropic::decode_count_tokens_request(from_wire(wire)).unwrap()
        }
        "gemini_generate_content" => gemini::decode_generate_content_request(
            fixture.expected_route.as_deref().unwrap(),
            from_wire(wire),
            false,
        )
        .unwrap(),
        "gemini_count_tokens" => gemini::decode_count_tokens_request(
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
            openai::encode_chat_completion(request, "provider-model").unwrap();
        }
        ("openai_responses", Operation::Generation(request)) => {
            openai::encode_response_create(request, "provider-model").unwrap();
        }
        ("openai_response_input_tokens", Operation::TokenCount(request)) => {
            openai::encode_response_input_tokens(request, "provider-model").unwrap();
        }
        ("openai_embeddings", Operation::Embeddings(request)) => {
            openai::encode_embedding_request(request, "provider-model").unwrap();
        }
        ("openai_image_generation", Operation::Images(ImageOperation::Generation(request))) => {
            openai::encode_image_generation(request, "provider-model").unwrap();
        }
        ("openai_image_edit", Operation::Images(ImageOperation::Edit(request))) => {
            openai::encode_image_edit(request, "provider-model", bounded_image).unwrap();
        }
        ("openai_image_variation", Operation::Images(ImageOperation::Variation(request))) => {
            openai::encode_image_variation(request, "provider-model", bounded_image).unwrap();
        }
        ("openai_speech", Operation::Speech(request)) => {
            openai::encode_speech(request, "provider-model").unwrap();
        }
        ("openai_transcription", Operation::Transcription(request)) => {
            openai::encode_transcription(request, "provider-model", |handle| {
                openai::BoundedMediaPart::new(
                    handle.clone(),
                    "fixture.wav",
                    Some("audio/wav".into()),
                    1,
                    openai::DEFAULT_AUDIO_UPLOAD_LIMIT,
                )
                .map_err(|_| openai::AudioCodecError::InvalidMediaPart)
            })
            .unwrap();
        }
        ("openai_moderation", Operation::Moderation(request)) => {
            openai::encode_moderation(request, "provider-model").unwrap();
        }
        ("openai_video_create", Operation::Video(VideoOperation::Create(request))) => {
            openai::encode_video_create(request, "provider-model", |handle| {
                openai::BoundedMediaPart::new(
                    handle.clone(),
                    "fixture.png",
                    Some("image/png".into()),
                    1,
                    openai::DEFAULT_VIDEO_REFERENCE_LIMIT,
                )
                .map_err(|error| openai::VideoCodecError::Staging(error.to_string()))
            })
            .unwrap();
        }
        ("openai_video_list", Operation::Video(VideoOperation::List(request))) => {
            openai::encode_video_list(request).unwrap();
        }
        ("anthropic_messages", Operation::Generation(request)) => {
            anthropic::encode_messages_request(request, "provider-model").unwrap();
        }
        ("gemini_generate_content", Operation::Generation(request)) => {
            gemini::encode_generate_content_request(request).unwrap();
        }
        // These public contracts are encoded by path/query selection or have
        // response-only encoders; successful canonical decoding is the full
        // request-side codec contract for the case.
        (
            "openai_video_get" | "openai_video_content" | "openai_video_delete",
            Operation::Video(_),
        )
        | ("openai_model_list" | "openai_model_get", Operation::Models(_))
        | ("anthropic_count_tokens" | "gemini_count_tokens", Operation::TokenCount(_)) => {}
        _ => panic!("golden codec {codec} did not match its canonical operation"),
    }
}

fn bounded_image(
    handle: &MediaHandle,
) -> Result<openai::BoundedMediaPart, openai::ImageCodecError> {
    openai::BoundedMediaPart::new(
        handle.clone(),
        "fixture.png",
        Some("image/png".into()),
        1,
        50 * 1024 * 1024,
    )
    .map_err(|error| openai::ImageCodecError::InvalidMediaPart(error.to_string()))
}

fn from_wire<T: serde::de::DeserializeOwned>(wire: Value) -> T {
    serde_json::from_value(wire).unwrap()
}

fn required_string(value: &Value, field: &str) -> String {
    value[field].as_str().unwrap().to_owned()
}

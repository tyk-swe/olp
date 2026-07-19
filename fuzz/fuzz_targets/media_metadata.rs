#![no_main]
#![allow(clippy::collapsible_if)]

use libfuzzer_sys::fuzz_target;
use olp_domain::{ImageOperation, MediaHandle, Operation};
use olp_protocols::openai::{
    BoundedMediaPart, OpenAiImageEditRequest, OpenAiImageVariationRequest,
    OpenAiTranscriptionRequest,
};

fuzz_target!(|data: &[u8]| {
    if let Ok(value) = serde_json::from_slice::<OpenAiImageEditRequest>(data) {
        if let Ok(Operation::Images(ImageOperation::Edit(canonical))) =
            olp_protocols::openai::decode_image_edit(value)
        {
            let _ = olp_protocols::openai::encode_image_edit(
                &canonical,
                "fuzz-provider-model",
                bounded_image_part,
            );
        }
    }
    if let Ok(value) = serde_json::from_slice::<OpenAiImageVariationRequest>(data) {
        if let Ok(Operation::Images(ImageOperation::Variation(canonical))) =
            olp_protocols::openai::decode_image_variation(value)
        {
            let _ = olp_protocols::openai::encode_image_variation(
                &canonical,
                "fuzz-provider-model",
                bounded_image_part,
            );
        }
    }
    if let Ok(value) = serde_json::from_slice::<OpenAiTranscriptionRequest>(data) {
        if let Ok(Operation::Transcription(canonical)) =
            olp_protocols::openai::decode_transcription(value)
        {
            let _ = olp_protocols::openai::encode_transcription(
                &canonical,
                "fuzz-provider-model",
                |handle| {
                    BoundedMediaPart::new(
                        handle.clone(),
                        "fuzz.wav",
                        Some("audio/wav".into()),
                        1,
                        olp_protocols::openai::DEFAULT_AUDIO_UPLOAD_LIMIT,
                    )
                    .map_err(|_| olp_protocols::openai::AudioCodecError::InvalidMediaPart)
                },
            );
        }
    }
    let split = data.len() / 2;
    let filename = String::from_utf8_lossy(&data[..split]);
    let content_length = u64::try_from(data.len()).unwrap_or(u64::MAX);
    let maximum = data
        .first()
        .map_or(1_u64, |value| u64::from(*value).saturating_add(1));
    let _ = BoundedMediaPart::new(
        MediaHandle::new("fuzz-handle"),
        filename,
        Some("application/octet-stream".to_owned()),
        content_length,
        maximum,
    );
});

fn bounded_image_part(
    handle: &MediaHandle,
) -> Result<BoundedMediaPart, olp_protocols::openai::ImageCodecError> {
    BoundedMediaPart::new(
        handle.clone(),
        "fuzz.png",
        Some("image/png".into()),
        1,
        50 * 1024 * 1024,
    )
    .map_err(|error| olp_protocols::openai::ImageCodecError::InvalidMediaPart(error.to_string()))
}

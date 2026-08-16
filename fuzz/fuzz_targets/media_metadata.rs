#![no_main]

//! Oracles for bounded media handling.
//!
//! Two kinds of invariant are checked here. The multipart-bearing request
//! codecs get the shared roundtrip oracle. `BoundedMediaPart::new` gets an
//! exact-outcome oracle: it has precisely three documented rejection reasons,
//! evaluated in a fixed order, so the harness can predict the *specific* error
//! rather than settling for `is_err()`. A constructor that rejects for a
//! fourth reason, reports the wrong reason, or silently accepts an
//! over-length part is a real defect in the size-limit enforcement that guards
//! the media spool.

use libfuzzer_sys::fuzz_target;
use olp_engine::domain::canonical::requests::{ImageOperation, MediaHandle, Operation};
use olp_engine::protocols::openai::{
    audio::{self, TranscriptionRequest},
    images::{self, ImageCodecError, OpenAiImageEditRequest, OpenAiImageVariationRequest},
    media::{BoundedMediaPart, Error as MediaError},
};

mod oracle;
use oracle::roundtrip;

const UPSTREAM_MODEL: &str = "fuzz-provider-model";

fuzz_target!(|data: &[u8]| {
    roundtrip(
        data,
        "openai::image_edit",
        |value: OpenAiImageEditRequest| match images::decode_image_edit(value) {
            Ok(Operation::Images(ImageOperation::Edit(canonical))) => Some(canonical),
            _ => None,
        },
        |canonical| images::encode_image_edit(canonical, UPSTREAM_MODEL, bounded_image_part),
    );
    roundtrip(
        data,
        "openai::image_variation",
        |value: OpenAiImageVariationRequest| match images::decode_image_variation(value) {
            Ok(Operation::Images(ImageOperation::Variation(canonical))) => Some(canonical),
            _ => None,
        },
        |canonical| images::encode_image_variation(canonical, UPSTREAM_MODEL, bounded_image_part),
    );
    roundtrip(
        data,
        "openai::transcription",
        |value: TranscriptionRequest| match audio::decode_transcription(value) {
            Ok(Operation::Transcription(canonical)) => Some(canonical),
            _ => None,
        },
        |canonical| {
            audio::encode_transcription(canonical, UPSTREAM_MODEL, |handle| {
                BoundedMediaPart::new(
                    handle.clone(),
                    "fuzz.wav",
                    Some("audio/wav".into()),
                    1,
                    audio::DEFAULT_AUDIO_UPLOAD_LIMIT,
                )
                .map_err(|_| audio::Error::InvalidMediaPart)
            })
        },
    );

    bounded_part_contract(data);
});

/// `BoundedMediaPart::new` rejects for exactly three reasons, checked in this
/// order: a blank filename, a zero limit, then a length above the limit.
/// Anything else must be accepted and must preserve its inputs verbatim.
fn bounded_part_contract(data: &[u8]) {
    let selector = data.first().copied().unwrap_or(0);
    let payload = data.get(1..).unwrap_or(&[]);

    // Steer the inputs onto each branch and, crucially, onto the boundary
    // itself. Random lengths almost never land on `content_length == maximum`,
    // which is the one case an off-by-one would get wrong.
    let filename = match selector % 4 {
        0 => String::new(),
        1 => "   \t\n".to_owned(),
        _ => String::from_utf8_lossy(payload).into_owned(),
    };
    let maximum = match (selector / 4) % 3 {
        0 => 0,
        1 => u64::try_from(payload.len()).unwrap_or(u64::MAX),
        _ => u64::MAX,
    };
    let content_length = match (selector / 12) % 3 {
        0 => maximum.saturating_sub(1),
        1 => maximum,
        _ => maximum.saturating_add(1),
    };
    let content_type = if selector % 2 == 0 {
        None
    } else {
        Some("application/octet-stream".to_owned())
    };
    let handle = MediaHandle::new("fuzz-handle");

    let expected = if filename.trim().is_empty() {
        Some(MediaError::EmptyFilename)
    } else if maximum == 0 {
        Some(MediaError::ZeroLimit)
    } else if content_length > maximum {
        Some(MediaError::TooLarge {
            actual: content_length,
            maximum,
        })
    } else {
        None
    };

    let result = BoundedMediaPart::new(
        handle.clone(),
        filename.clone(),
        content_type.clone(),
        content_length,
        maximum,
    );

    match (result, expected) {
        (Err(actual), Some(wanted)) => assert_eq!(
            actual, wanted,
            "BoundedMediaPart::new rejected for the wrong reason \
             (filename {filename:?}, length {content_length}, maximum {maximum})"
        ),
        (Ok(_), Some(wanted)) => panic!(
            "BoundedMediaPart::new accepted a part it must reject with {wanted:?} \
             (filename {filename:?}, length {content_length}, maximum {maximum})"
        ),
        (Err(actual), None) => panic!(
            "BoundedMediaPart::new rejected a valid part with {actual:?} \
             (filename {filename:?}, length {content_length}, maximum {maximum})"
        ),
        (Ok(part), None) => {
            assert_eq!(part.filename, filename, "constructor altered the filename");
            assert_eq!(
                part.content_type, content_type,
                "constructor altered the content type"
            );
            assert_eq!(
                part.content_length, content_length,
                "constructor altered the content length"
            );
            assert_eq!(
                part.maximum_length, maximum,
                "constructor altered the maximum length"
            );

            // The artifact projection is what downstream spooling trusts for
            // its own limit checks; losing the length here would let an
            // unbounded body through a later stage.
            let artifact = part.artifact();
            assert_eq!(
                artifact.content_length,
                Some(content_length),
                "artifact() dropped or altered the content length"
            );
            assert_eq!(
                artifact.content_type, content_type,
                "artifact() dropped or altered the content type"
            );
        }
    }
}

fn bounded_image_part(handle: &MediaHandle) -> Result<BoundedMediaPart, ImageCodecError> {
    BoundedMediaPart::new(
        handle.clone(),
        "fuzz.png",
        Some("image/png".into()),
        1,
        50 * 1024 * 1024,
    )
    .map_err(|error| ImageCodecError::InvalidMediaPart(error.to_string()))
}

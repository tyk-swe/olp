#![no_main]

//! Oracles for the SSE codec.
//!
//! Three invariants, all of which are properties the product owes its clients:
//!
//! 1. **Fragmentation invariance.** A decoder fed the same bytes must produce
//!    the same events regardless of how the network split them. A decoder that
//!    only works when frames arrive whole is broken in production and fine in
//!    every fixture test.
//! 2. **The encoder's rejection contract.** `encode_frame` must refuse exactly
//!    those frames that could break out of their own field: a multiline
//!    `event` or `id`, or a NUL in `id`. Accepting one lets an upstream value
//!    forge event boundaries in a client's parser.
//! 3. **Encode/decode fidelity.** Encoding one frame must decode back to
//!    exactly one frame, carrying the original fields with `data`
//!    line endings normalised, which is the documented CR handling.

use std::mem::{Discriminant, discriminant};

use libfuzzer_sys::fuzz_target;
use olp_engine::protocols::{
    anthropic::stream::Decoder as AnthropicDecoder,
    gemini::stream::Decoder as GeminiDecoder,
    openai::{
        audio::Decoder as TranscriptionDecoder, response::Decoder as ChatDecoder,
        responses::stream::Decoder as ResponsesDecoder,
    },
    sse::{DecodeError, Decoder as SseDecoder, Frame, encode_frame},
};

/// Generous enough that the fidelity oracle is never confounded by the size
/// limit; the invariance oracle uses the fuzzer's own smaller bound instead.
const ROOMY_EVENT_LIMIT: usize = 1 << 20;

/// Decodes `data` in `width`-sized chunks.
///
/// Errors are reduced to their variant rather than their message. The
/// *decision* to reject must not depend on fragmentation, but the byte count
/// inside `EventTooLarge` legitimately does: the limit is checked before the
/// buffer grows, so the reported figure is how much *would* have been
/// buffered, which is larger when a whole event arrives in one chunk. Pinning
/// the message would assert a diagnostic detail that is not part of the
/// contract.
fn decode_chunked(
    data: &[u8],
    width: usize,
    max_event_bytes: usize,
) -> Result<Vec<Frame>, Discriminant<DecodeError>> {
    let mut decoder = SseDecoder::new(max_event_bytes);
    let mut frames = Vec::new();
    for chunk in data.chunks(width.max(1)) {
        frames.extend(decoder.push(chunk).map_err(|error| discriminant(&error))?);
    }
    frames.extend(decoder.finish().map_err(|error| discriminant(&error))?);
    Ok(frames)
}

fn frame_from(data: &[u8]) -> Frame {
    let third = data.len() / 3;
    let (event_bytes, rest) = data.split_at(third);
    let (id_bytes, data_bytes) = rest.split_at(third.min(rest.len()));
    Frame {
        event: (!event_bytes.is_empty()).then(|| String::from_utf8_lossy(event_bytes).into_owned()),
        id: (!id_bytes.is_empty()).then(|| String::from_utf8_lossy(id_bytes).into_owned()),
        data: String::from_utf8_lossy(data_bytes).into_owned(),
        retry_ms: data
            .first()
            .filter(|value| **value != u8::MAX)
            .map(|value| u64::from(*value)),
    }
}

/// `encode_frame` rejects exactly three things. Anything else must encode.
fn assert_rejection_contract(frame: &Frame) {
    let splits_stream = |value: &String| value.contains(['\r', '\n']);
    let must_reject = frame.event.as_ref().is_some_and(splits_stream)
        || frame.id.as_ref().is_some_and(splits_stream)
        || frame.id.as_ref().is_some_and(|value| value.contains('\0'));

    match encode_frame(frame) {
        Ok(_) if must_reject => panic!(
            "encode_frame accepted a frame whose event or id can forge an event \
             boundary: {frame:?}"
        ),
        Err(error) if !must_reject => {
            panic!("encode_frame rejected a well-formed frame with {error:?}: {frame:?}")
        }
        _ => {}
    }
}

/// A frame that encodes must decode back to itself, with `data` line endings
/// normalised the way `encode_frame` documents.
fn assert_encode_decode_fidelity(frame: &Frame) {
    let Ok(encoded) = encode_frame(frame) else {
        return;
    };
    let decoded = decode_chunked(&encoded, encoded.len().max(1), ROOMY_EVENT_LIMIT)
        .expect("the decoder must accept the encoder's own output");

    assert_eq!(
        decoded.len(),
        1,
        "encoding one frame must decode exactly once: {frame:?}"
    );
    let expected = Frame {
        event: frame.event.clone(),
        id: frame.id.clone(),
        retry_ms: frame.retry_ms,
        data: frame.data.replace("\r\n", "\n").replace('\r', "\n"),
    };
    assert_eq!(
        decoded[0], expected,
        "a frame did not survive an encode/decode round trip"
    );
}

fuzz_target!(|data: &[u8]| {
    let maximum = data
        .first()
        .map_or(1_024, |value| usize::from(*value).saturating_add(1));

    // Fragmentation invariance: chunk boundaries must not change the outcome.
    let baseline = decode_chunked(data, data.len().max(1), maximum);
    for width in [1, 2, 3, 5, 8, 13, 64] {
        let fragmented = decode_chunked(data, width, maximum);
        assert_eq!(
            baseline, fragmented,
            "decoding depended on chunk boundaries (width {width}, limit {maximum})"
        );
    }

    let frame = frame_from(data);
    assert_rejection_contract(&frame);
    assert_encode_decode_fidelity(&frame);

    // The same frame with the stream-splitting characters removed, so the
    // fidelity oracle still gets exercised when the raw one is rejected.
    let sanitized = Frame {
        event: frame
            .event
            .as_ref()
            .map(|value| value.replace(['\r', '\n'], "")),
        id: frame
            .id
            .as_ref()
            .map(|value| value.replace(['\r', '\n', '\0'], "")),
        data: frame.data.clone(),
        retry_ms: frame.retry_ms,
    };
    assert_rejection_contract(&sanitized);
    assert_encode_decode_fidelity(&sanitized);

    // Vendor decoders stay a smoke path: their event types are not comparable,
    // so only "must not panic" is asserted here.
    let fragment_width = data.first().map_or(1, |value| usize::from(*value % 31) + 1);
    macro_rules! drive_vendor_decoder {
        ($decoder:expr) => {{
            let mut decoder = $decoder;
            let mut rejected = false;
            for chunk in data.chunks(fragment_width) {
                if decoder.push(chunk).is_err() {
                    rejected = true;
                    break;
                }
            }
            if !rejected {
                let _ = decoder.finish();
            }
        }};
    }
    drive_vendor_decoder!(ChatDecoder::new());
    drive_vendor_decoder!(ResponsesDecoder::new());
    drive_vendor_decoder!(TranscriptionDecoder::new());
    drive_vendor_decoder!(AnthropicDecoder::new());
    drive_vendor_decoder!(GeminiDecoder::new());
});

#![no_main]

use libfuzzer_sys::fuzz_target;
use olp_protocols::{
    anthropic::AnthropicMessagesStreamDecoder,
    gemini::GeminiGenerateContentStreamDecoder,
    openai::{
        OpenAiChatStreamDecoder, OpenAiResponsesStreamDecoder, OpenAiTranscriptionStreamDecoder,
    },
    sse::SseDecoder,
};

fuzz_target!(|data: &[u8]| {
    let maximum = data
        .first()
        .map_or(1_024, |value| usize::from(*value).saturating_add(1));
    let mut decoder = SseDecoder::new(maximum);
    let mut position = 1_usize.min(data.len());
    while position < data.len() {
        let width = usize::from(data[position]).saturating_add(1);
        let end = position.saturating_add(width).min(data.len());
        if decoder.push(&data[position..end]).is_err() {
            return;
        }
        position = end;
    }
    let _ = decoder.finish();

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
    drive_vendor_decoder!(OpenAiChatStreamDecoder::new());
    drive_vendor_decoder!(OpenAiResponsesStreamDecoder::new());
    drive_vendor_decoder!(OpenAiTranscriptionStreamDecoder::new());
    drive_vendor_decoder!(AnthropicMessagesStreamDecoder::new());
    drive_vendor_decoder!(GeminiGenerateContentStreamDecoder::new());
});

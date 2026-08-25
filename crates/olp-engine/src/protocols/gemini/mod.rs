pub mod client;
pub mod client_stream;
pub mod count;
pub mod dto;
pub mod stream;
pub mod translate;

use crate::domain::canonical::events::FinishReason;

/// Gemini's declared `finishReason` values. A provider value outside this set
/// would fail a typed client, so it is clamped to `OTHER`.
const GEMINI_FINISH_REASONS: [&str; 17] = [
    "FINISH_REASON_UNSPECIFIED",
    "STOP",
    "MAX_TOKENS",
    "SAFETY",
    "RECITATION",
    "LANGUAGE",
    "OTHER",
    "BLOCKLIST",
    "PROHIBITED_CONTENT",
    "SPII",
    "MALFORMED_FUNCTION_CALL",
    "IMAGE_SAFETY",
    "UNEXPECTED_TOOL_CALL",
    "IMAGE_PROHIBITED_CONTENT",
    "NO_IMAGE",
    "IMAGE_RECITATION",
    "TOO_MANY_TOOL_CALLS",
];

fn finish_reason(reason: &FinishReason) -> &str {
    match reason {
        FinishReason::Stop | FinishReason::ToolCalls => "STOP",
        FinishReason::Length => "MAX_TOKENS",
        FinishReason::ContentFilter => "SAFETY",
        FinishReason::Error => "OTHER",
        FinishReason::Other(value) if GEMINI_FINISH_REASONS.contains(&value.as_str()) => value,
        FinishReason::Other(_) => "OTHER",
    }
}

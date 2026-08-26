pub mod client;
pub mod client_stream;
pub mod count;
pub mod dto;
pub mod stream;
pub mod translate;

use crate::domain::canonical::events::FinishReason;

/// Anthropic's declared `stop_reason` values. `pause_turn` in particular is a
/// real value that server-tool loops depend on, so provider values inside this
/// set pass through untouched; anything else would fail a typed client and is
/// clamped to `end_turn`.
const ANTHROPIC_STOP_REASONS: [&str; 7] = [
    "end_turn",
    "max_tokens",
    "stop_sequence",
    "tool_use",
    "pause_turn",
    "refusal",
    "model_context_window_exceeded",
];

fn finish_reason(reason: &FinishReason) -> &str {
    match reason {
        FinishReason::Stop => "end_turn",
        FinishReason::Length => "max_tokens",
        FinishReason::ToolCalls => "tool_use",
        FinishReason::ContentFilter | FinishReason::Error => "refusal",
        FinishReason::Other(value) if ANTHROPIC_STOP_REASONS.contains(&value.as_str()) => value,
        FinishReason::Other(_) => "end_turn",
    }
}

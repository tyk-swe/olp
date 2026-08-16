pub mod client;
pub mod client_stream;
pub mod count;
pub mod dto;
pub mod stream;
pub mod translate;

use crate::domain::canonical::events::FinishReason;

fn finish_reason(reason: &FinishReason) -> &str {
    match reason {
        FinishReason::Stop => "end_turn",
        FinishReason::Length => "max_tokens",
        FinishReason::ToolCalls => "tool_use",
        FinishReason::ContentFilter => "refusal",
        FinishReason::Error => "error",
        FinishReason::Other(value) => value,
    }
}

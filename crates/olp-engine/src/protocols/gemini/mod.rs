pub mod client;
pub mod client_stream;
pub mod count;
pub mod dto;
pub mod stream;
pub mod translate;

use crate::domain::canonical::events::FinishReason;

fn finish_reason(reason: &FinishReason) -> &str {
    match reason {
        FinishReason::Stop | FinishReason::ToolCalls => "STOP",
        FinishReason::Length => "MAX_TOKENS",
        FinishReason::ContentFilter => "SAFETY",
        FinishReason::Error => "OTHER",
        FinishReason::Other(value) => value,
    }
}

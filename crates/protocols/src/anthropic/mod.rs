mod client;
mod client_stream;
mod count;
mod dto;
mod stream;
mod translate;

use olp_domain::FinishReason;

pub use client::{ClientEncodeError, encode_messages_response};
pub use client_stream::{AnthropicMessagesClientStreamEncoder, ClientStreamEncodeError};
pub use count::{
    ANTHROPIC_COUNT_REQUEST_EXTENSION, CountDecodeError, CountEncodeError,
    decode_count_tokens_request, encode_count_tokens_result,
};
pub use dto::{
    ContentBlock, CountTokensRequest, CountTokensResponse, ImageBlock, MediaSource, Message,
    MessageContent, MessagesRequest, MessagesResponse, RedactedThinkingBlock, Role, SystemPrompt,
    TextBlock, ThinkingBlock, Tool, ToolChoice, ToolResultBlock, ToolResultContent, ToolUseBlock,
    Usage,
};
pub use stream::{AnthropicMessagesStreamDecoder, StreamError};
pub use translate::{
    DecodeError, EncodeError, ResponseError, decode_messages_request, decode_messages_response,
    encode_messages_request,
};

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

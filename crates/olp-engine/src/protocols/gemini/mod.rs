mod client;
mod client_stream;
mod count;
mod dto;
mod stream;
mod translate;

use crate::domain::FinishReason;

pub use client::{ClientEncodeError, encode_generate_content_response};
pub use client_stream::{ClientStreamEncodeError, GeminiGenerateContentClientStreamEncoder};
pub use count::{
    CountDecodeError, CountEncodeError, GEMINI_COUNT_REQUEST_EXTENSION,
    decode_count_tokens_request, encode_count_tokens_result,
};
pub use dto::{
    Blob, Candidate, Content, CountTokensRequest, CountTokensResponse, FileData, FileDataPart,
    FunctionCall, FunctionCallPart, FunctionCallingConfig, FunctionDeclaration, FunctionResponse,
    FunctionResponsePart, GenerateContentRequest, GenerateContentResponse, GenerationConfig,
    InlineDataPart, Part, TextPart, Tool, ToolConfig, UsageMetadata,
};
pub use stream::{GeminiGenerateContentStreamDecoder, StreamError};
pub use translate::{
    CountTokensError, DecodeError, EncodeError, ResponseError, decode_generate_content_request,
    decode_generate_content_response, encode_generate_content_request,
    validate_count_tokens_request,
};

fn finish_reason(reason: &FinishReason) -> &str {
    match reason {
        FinishReason::Stop | FinishReason::ToolCalls => "STOP",
        FinishReason::Length => "MAX_TOKENS",
        FinishReason::ContentFilter => "SAFETY",
        FinishReason::Error => "OTHER",
        FinishReason::Other(value) => value,
    }
}

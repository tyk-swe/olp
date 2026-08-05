mod errors;
mod helpers;
mod request;
mod response;
mod stream;
mod token_count;

pub use errors::ResponsesCodecError;
pub use request::{
    ResponseCreateRequest, ResponseInput, ResponseNamedToolChoice, ResponseTextConfig,
    ResponseTextFormat, ResponseTool, ResponseToolChoice, decode_response_create,
    encode_response_create,
};
pub use response::{
    ResponseErrorBody, ResponseInputTokenDetails, ResponseObject, ResponseOutputTokenDetails,
    ResponseUsage, decode_response_object,
};
pub use stream::OpenAiResponsesStreamDecoder;
pub use token_count::{
    OPENAI_RESPONSES_INPUT_TOKENS_REQUEST_EXTENSION, ResponseInputTokensRequest,
    ResponseInputTokensResponse, decode_response_input_tokens, decode_response_input_tokens_result,
    encode_response_input_tokens, encode_response_input_tokens_result,
};

pub(crate) const OPENAI_RESPONSES_RAW_OUTPUT_PREFIX: &str = "/__olp/openai_responses_raw_output";

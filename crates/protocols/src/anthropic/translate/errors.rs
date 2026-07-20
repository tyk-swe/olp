use olp_domain::RouteSlugError;
use thiserror::Error;

#[derive(Debug, Error)]
pub enum DecodeError {
    #[error(transparent)]
    InvalidRoute(#[from] RouteSlugError),
    #[error("messages must contain at least one non-system message")]
    EmptyMessages,
    #[error("message content cannot be empty")]
    EmptyMessage,
    #[error("{field} {reason}")]
    InvalidParameter {
        field: &'static str,
        reason: &'static str,
    },
    #[error("expected Anthropic content type {expected}, got {actual}")]
    UnexpectedType {
        expected: &'static str,
        actual: String,
    },
    #[error("Anthropic tool_use blocks are valid only in assistant messages")]
    ToolUseRole,
    #[error("Anthropic tool_result blocks are valid only in user messages")]
    ToolResultRole,
    #[error("text or image content after tool_use cannot be reordered canonically")]
    InterleavedToolUse,
    #[error("inline base64 media must be replaced by a bounded media handle before translation")]
    InlineMediaRequiresBoundedHandle,
    #[error("unsupported Anthropic media source type {0}")]
    UnsupportedMediaSource(String),
    #[error("Anthropic URL media source is missing url")]
    MissingMediaUrl,
    #[error("unsupported Anthropic content block {0}")]
    UnsupportedContentBlock(String),
    #[error("unsupported Anthropic tool choice {0}")]
    UnsupportedToolChoice(String),
    #[error("Anthropic named tool choice is missing name")]
    MissingToolName,
    #[error("Anthropic JSON value is invalid: {0}")]
    Json(serde_json::Error),
}

#[derive(Debug, Error)]
pub enum EncodeError {
    #[error(transparent)]
    Extensions(#[from] olp_domain::ExtensionError),
    #[error("Anthropic Messages requires max_output_tokens")]
    MissingMaxOutputTokens,
    #[error("Anthropic Messages requires at least one conversation message")]
    EmptyMessages,
    #[error("system or developer messages cannot appear after conversation content")]
    SystemMessageAfterConversation,
    #[error("Anthropic system prompts support text content only")]
    UnsupportedSystemContent,
    #[error("canonical message name is not representable by Anthropic Messages")]
    MessageNameUnsupported,
    #[error("tool_call_id is valid only on a canonical tool result message")]
    UnexpectedToolCallId,
    #[error("tool result message is missing tool_call_id")]
    MissingToolCallId,
    #[error("tool calls are valid only in assistant messages")]
    ToolUseRole,
    #[error("media handle cannot be encoded as an Anthropic URL source")]
    MediaHandleCannotBeEncoded,
    #[error("input audio is not representable by the launch Anthropic Messages surface")]
    InputAudioUnsupported,
    #[error("input files are not representable by the launch Anthropic Messages surface")]
    InputFileUnsupported,
    #[error("OpenAI-style image detail is not representable by Anthropic Messages")]
    ImageDetailUnsupported,
    #[error("a canonical refusal marker is not representable by Anthropic request content")]
    RefusalUnsupported,
    #[error("Anthropic Messages supports exactly one candidate")]
    CandidateCountUnsupported,
    #[error("Anthropic Messages does not support a deterministic seed")]
    SeedUnsupported,
    #[error("canonical response format requires an explicit Anthropic output-config translation")]
    ResponseFormatUnsupported,
    #[error("tool {tool} arguments are not valid JSON: {source}")]
    InvalidToolArguments {
        tool: String,
        source: serde_json::Error,
    },
    #[error("source extension path cannot be applied: {0}")]
    InvalidExtensionPath(String),
    #[error("Anthropic JSON value is invalid: {0}")]
    Json(serde_json::Error),
}

#[derive(Debug, Error)]
pub enum ResponseError {
    #[error("Anthropic response role is not assistant")]
    UnexpectedRole,
    #[error("unexpected Anthropic response content type {0}")]
    UnexpectedType(String),
    #[error("Anthropic response is missing stop_reason")]
    MissingStopReason,
    #[error("Anthropic response has too many content blocks")]
    TooManyContentBlocks,
    #[error("Anthropic response JSON is invalid: {0}")]
    Json(serde_json::Error),
}

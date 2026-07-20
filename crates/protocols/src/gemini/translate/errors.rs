use olp_domain::RouteSlugError;
use thiserror::Error;

#[derive(Debug, Error)]
pub enum DecodeError {
    #[error(transparent)]
    InvalidRoute(#[from] RouteSlugError),
    #[error("body model {0} conflicts with the model route")]
    ConflictingModel(String),
    #[error("generateContent requires at least one non-system content")]
    EmptyContents,
    #[error("Gemini content parts cannot be empty")]
    EmptyContent,
    #[error("unsupported Gemini content role {0}")]
    UnsupportedRole(String),
    #[error("Gemini functionCall parts are valid only for model content")]
    FunctionCallRole,
    #[error("Gemini functionResponse parts are valid only for user content")]
    FunctionResponseRole,
    #[error("content after functionCall cannot be reordered canonically")]
    InterleavedFunctionCall,
    #[error("inline base64 media must be replaced by a bounded media handle before translation")]
    InlineMediaRequiresBoundedHandle,
    #[error("Gemini fileData media type {0} is not an image generation input")]
    UnsupportedFileMediaType(String),
    #[error("Gemini thought parts require source-protocol passthrough")]
    ThoughtPartUnsupported,
    #[error("unsupported Gemini part {0}")]
    UnsupportedPart(String),
    #[error("unsupported Gemini system part {0}")]
    UnsupportedSystemPart(String),
    #[error("{field} {reason}")]
    InvalidParameter {
        field: &'static str,
        reason: &'static str,
    },
    #[error("responseSchema requires responseMimeType application/json")]
    SchemaWithoutJsonMimeType,
    #[error("Gemini JSON value is invalid: {0}")]
    Json(serde_json::Error),
}

#[derive(Debug, Error)]
pub enum EncodeError {
    #[error(transparent)]
    Extensions(#[from] olp_domain::ExtensionError),
    #[error("Gemini tool configuration cannot represent parallel_tool_calls")]
    ParallelToolCallsUnsupported,
    #[error("system or developer messages cannot appear after conversation content")]
    SystemMessageAfterConversation,
    #[error("Gemini system instructions support text content only")]
    UnsupportedSystemContent,
    #[error("generateContent requires at least one conversation content")]
    EmptyContents,
    #[error("canonical message name is not representable on regular Gemini content")]
    MessageNameUnsupported,
    #[error("tool_call_id is valid only on a canonical tool result message")]
    UnexpectedToolCallId,
    #[error("Gemini function calls are valid only in model content")]
    FunctionCallRole,
    #[error("Gemini function response is missing the function name")]
    MissingToolName,
    #[error("Gemini function response is missing tool_call_id")]
    MissingToolCallId,
    #[error("Gemini function response supports one JSON-compatible text result")]
    UnsupportedToolResultContent,
    #[error("media handle cannot be encoded as Gemini fileData")]
    MediaHandleCannotBeEncoded,
    #[error("input audio requires a bounded Gemini media adapter")]
    InputAudioUnsupported,
    #[error("input files are not supported by the launch Gemini surface")]
    InputFileUnsupported,
    #[error("Gemini inline audio requires an audio MIME type")]
    InvalidInputAudioMimeType,
    #[error("canonical refusal marker is not representable by Gemini request content")]
    RefusalUnsupported,
    #[error("OpenAI-style image detail is not representable by Gemini")]
    ImageDetailUnsupported,
    #[error("Gemini image MIME type extension is required at {0}")]
    ImageMimeTypeRequired(String),
    #[error("tool {tool} arguments are not valid JSON: {source}")]
    InvalidToolArguments {
        tool: String,
        source: serde_json::Error,
    },
    #[error("source extension path cannot be applied: {0}")]
    InvalidExtensionPath(String),
    #[error("Gemini JSON value is invalid: {0}")]
    Json(serde_json::Error),
}

#[derive(Debug, Error)]
pub enum ResponseError {
    #[error("Gemini response has no candidates or prompt feedback")]
    EmptyResponse,
    #[error("Gemini unary response candidate is missing finishReason")]
    MissingFinishReason,
    #[error("Gemini response contains too many candidates")]
    TooManyCandidates,
    #[error("Gemini response repeats candidate index {0}")]
    DuplicateCandidateIndex(u32),
    #[error("Gemini candidate role is not model: {0}")]
    UnexpectedRole(String),
    #[error("Gemini response has too many tool calls")]
    TooManyToolCalls,
    #[error("Gemini response JSON is invalid: {0}")]
    Json(serde_json::Error),
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
pub enum CountTokensError {
    #[error("countTokens requires exactly one of contents or generateContentRequest")]
    ExactlyOneInput,
}

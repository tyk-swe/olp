use olp_domain::RouteSlugError;
use thiserror::Error;

use crate::sse::SseDecodeError;

#[derive(Debug, Error)]
pub enum ResponsesCodecError {
    #[error(transparent)]
    InvalidRoute(#[from] RouteSlugError),
    #[error(transparent)]
    Extensions(#[from] olp_domain::ExtensionError),
    #[error("Responses background mode is outside the OLP contract")]
    BackgroundUnsupported,
    #[error("stateful Responses field {field} is unsupported")]
    StatefulField { field: &'static str, value: String },
    #[error("Responses input cannot be empty")]
    EmptyInput,
    #[error("invalid Responses input item at index {0}")]
    InvalidInputItem(usize),
    #[error("invalid Responses content part {part} in item {item}")]
    InvalidContentPart { item: usize, part: usize },
    #[error("missing field {field} in Responses input item {index}")]
    MissingInputField { index: usize, field: &'static str },
    #[error("invalid field {field} in Responses input item {index}")]
    InvalidInputField { index: usize, field: &'static str },
    #[error("unsupported Responses input item type: {0}")]
    UnsupportedInputItem(String),
    #[error("unsupported Responses content part type: {0}")]
    UnsupportedContentPart(String),
    #[error("unsupported Responses message role: {0}")]
    UnsupportedRole(String),
    #[error("unsupported Responses tool type: {0}")]
    UnsupportedTool(String),
    #[error("Responses function tool is missing {0}")]
    MissingToolField(&'static str),
    #[error("unsupported Responses tool choice: {0}")]
    UnsupportedToolChoice(String),
    #[error("unsupported Responses text format: {0}")]
    UnsupportedResponseFormat(String),
    #[error("Responses JSON schema is missing {0}")]
    MissingJsonSchemaField(&'static str),
    #[error("{0} must be within the supported range")]
    InvalidSampling(&'static str),
    #[error("OpenAI file_id input requires adapter-side bounded resolution")]
    FileIdNeedsResolution,
    #[error("Responses image input must contain exactly one source")]
    AmbiguousImageSource,
    #[error("Responses input_file cannot combine inline data with file_id or file_url")]
    AmbiguousFileSource,
    #[error("{0} input must be admitted through a bounded media spool")]
    InlineMediaNeedsBoundedSpool(String),
    #[error("invalid source extension path: {0}")]
    InvalidExtension(String),
    #[error("canonical field cannot be represented by the Responses API: {0}")]
    UnrepresentableCanonicalField(&'static str),
    #[error("canonical tool output is missing tool_call_id")]
    MissingCanonicalToolCallId,
    #[error("Responses input-token counting supports only one stateless user input")]
    TokenCountSemanticsUnsupported,
    #[error("invalid Responses response object: {0}")]
    InvalidResponse(String),
    #[error("unsupported Responses output item type: {0}")]
    UnsupportedOutputItem(String),
    #[error("Responses response contains too many output items")]
    TooManyOutputItems,
    #[error(transparent)]
    Json(#[from] serde_json::Error),
    #[error(transparent)]
    Sse(#[from] SseDecodeError),
    #[error("Responses stream ended before a terminal event")]
    UnexpectedEof,
    #[error("Responses stream contained data after its terminal event")]
    DataAfterDone,
}

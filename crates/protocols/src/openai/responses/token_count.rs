use std::collections::BTreeMap;

use olp_domain::{
    ContentPart, Operation, RouteSlug, SourceExtensions, Surface, TokenCountRequest,
    TokenCountResult,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;

use super::super::extensions::{apply_pointer_extensions, collect_extra};
use super::errors::ResponsesCodecError;
use super::request::{ResponseInput, decode_response_input, encode_response_content_part};

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseInputTokensRequest {
    pub model: String,
    pub input: ResponseInput,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

/// Exact, source-scoped request retained for same-protocol forwarding. The
/// canonical token-count operation intentionally carries only countable media
/// and text parts; it cannot faithfully represent Responses roles, function
/// calls/results, or future stateless input item fields on its own.
pub const OPENAI_RESPONSES_INPUT_TOKENS_REQUEST_EXTENSION: &str =
    "/__olp/openai_responses_input_tokens_request";

pub fn decode_response_input_tokens(
    request: ResponseInputTokensRequest,
) -> Result<Operation, ResponsesCodecError> {
    let plain_text = request.extra.is_empty() && matches!(&request.input, ResponseInput::Text(_));
    let preserved = serde_json::to_value(&request)?;
    let route = RouteSlug::parse(request.model)?;
    reject_stateful_token_count_fields(&request.extra)?;

    // Reuse the complete Responses input validator so the count endpoint and
    // response creation cannot disagree about supported stateless item forms.
    // Granular unknown fields are deliberately discarded here because the
    // exact body below is the sole lossless source-scoped representation.
    let mut validation_extensions = BTreeMap::new();
    let messages = decode_response_input(request.input, &mut validation_extensions)?;
    let mut input = Vec::new();
    let mut has_tool_semantics = false;
    for message in messages {
        input.extend(message.content);
        for call in message.tool_calls {
            has_tool_semantics = true;
            // Include arguments in the conservative local TPM estimate while
            // the preserved body remains authoritative for upstream counting.
            input.push(ContentPart::Text {
                text: call.arguments,
            });
        }
        has_tool_semantics |= message.tool_call_id.is_some();
    }
    if input.is_empty() && !has_tool_semantics {
        return Err(ResponsesCodecError::EmptyInput);
    }
    let extensions = if plain_text && validation_extensions.is_empty() && !has_tool_semantics {
        SourceExtensions::new(Surface::OpenAi, BTreeMap::new())
    } else {
        SourceExtensions::new(
            Surface::OpenAi,
            BTreeMap::from([(
                OPENAI_RESPONSES_INPUT_TOKENS_REQUEST_EXTENSION.to_owned(),
                preserved,
            )]),
        )
    };
    Ok(Operation::TokenCount(TokenCountRequest {
        route,
        input,
        extensions,
    }))
}

pub fn encode_response_input_tokens(
    request: &TokenCountRequest,
    upstream_model: &str,
) -> Result<ResponseInputTokensRequest, ResponsesCodecError> {
    request
        .extensions
        .ensure_representable_on(Surface::OpenAi)?;
    if let Some(preserved) = request
        .extensions
        .values
        .get(OPENAI_RESPONSES_INPUT_TOKENS_REQUEST_EXTENSION)
    {
        if request.extensions.values.len() != 1 {
            return Err(ResponsesCodecError::InvalidExtension(
                "Responses input-token preservation collided with another extension".into(),
            ));
        }
        let mut wire: ResponseInputTokensRequest = serde_json::from_value(preserved.clone())?;
        wire.model = upstream_model.to_owned();
        return Ok(wire);
    }
    let parts = request
        .input
        .iter()
        .map(encode_response_content_part)
        .collect::<Result<Vec<_>, _>>()?;
    apply_pointer_extensions(
        ResponseInputTokensRequest {
            model: upstream_model.into(),
            input: ResponseInput::Items(vec![serde_json::json!({
                "type": "message",
                "role": "user",
                "content": parts,
            })]),
            extra: BTreeMap::new(),
        },
        &request.extensions.values,
    )
    .map_err(ResponsesCodecError::InvalidExtension)
}

fn reject_stateful_token_count_fields(
    extra: &BTreeMap<String, Value>,
) -> Result<(), ResponsesCodecError> {
    for field in ["previous_response_id", "conversation"] {
        if let Some(value) = extra.get(field) {
            return Err(ResponsesCodecError::StatefulField {
                field,
                value: value.to_string(),
            });
        }
    }
    if extra.get("background") == Some(&Value::Bool(true)) {
        return Err(ResponsesCodecError::BackgroundUnsupported);
    }
    Ok(())
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseInputTokensResponse {
    pub input_tokens: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub object: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_response_input_tokens_result(
    response: ResponseInputTokensResponse,
) -> TokenCountResult {
    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);
    if let Some(object) = response.object {
        extensions.insert("/object".into(), Value::String(object));
    }
    TokenCountResult {
        input_tokens: response.input_tokens,
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    }
}

pub fn encode_response_input_tokens_result(
    result: &TokenCountResult,
) -> Result<ResponseInputTokensResponse, ResponsesCodecError> {
    result.extensions.ensure_representable_on(Surface::OpenAi)?;
    let mut extensions = result.extensions.values.clone();
    let object = extensions
        .remove("/object")
        .and_then(|value| value.as_str().map(str::to_owned))
        .or_else(|| Some("response.input_tokens".into()));
    apply_pointer_extensions(
        ResponseInputTokensResponse {
            input_tokens: result.input_tokens,
            object,
            extra: BTreeMap::new(),
        },
        &extensions,
    )
    .map_err(ResponsesCodecError::InvalidExtension)
}

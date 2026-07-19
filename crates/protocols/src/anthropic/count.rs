use std::collections::BTreeMap;

use olp_domain::{
    ContentPart, Operation, SourceExtensions, Surface, TokenCountRequest, TokenCountResult,
};
use serde_json::Value;
use thiserror::Error;

use super::{
    CountTokensRequest, CountTokensResponse, MessageContent, MessagesRequest, Role,
    decode_messages_request,
};

/// Private, source-scoped extension used to retain every Anthropic count-token
/// field that the deliberately small canonical token-count operation cannot
/// represent (roles, system blocks, tools, and future vendor fields).
pub const ANTHROPIC_COUNT_REQUEST_EXTENSION: &str = "/__olp/anthropic_count_tokens_request";

#[derive(Debug, Error)]
pub enum CountDecodeError {
    #[error("Anthropic countTokens request is invalid: {0}")]
    Messages(#[from] super::DecodeError),
    #[error("Anthropic countTokens request could not be preserved")]
    Json(#[from] serde_json::Error),
    #[error("Anthropic countTokens request contains no countable input")]
    Empty,
}

#[derive(Debug, Error)]
pub enum CountEncodeError {
    #[error("countTokens response extensions came from a different protocol")]
    CrossProtocol,
    #[error("countTokens response contains an invalid or colliding extension path")]
    Extension,
    #[error("Anthropic countTokens response could not be encoded")]
    Json(#[from] serde_json::Error),
}

pub fn decode_count_tokens_request(
    request: CountTokensRequest,
) -> Result<Operation, CountDecodeError> {
    let plain_text = is_plain_text_request(&request);
    let preserved = serde_json::to_value(&request)?;
    // Reuse the full Messages validator/translator so media boundaries, roles,
    // tool semantics, and source extension handling cannot drift between the
    // two Anthropic request surfaces.
    let generation = MessagesRequest {
        model: request.model,
        messages: request.messages,
        max_tokens: 1,
        system: request.system,
        stop_sequences: Vec::new(),
        temperature: None,
        top_p: None,
        tools: request.tools,
        tool_choice: request.tool_choice,
        stream: false,
        extra: request.extra,
    };
    let Operation::Generation(generation) = decode_messages_request(generation)? else {
        unreachable!("Anthropic Messages decoding always returns generation")
    };
    let input = generation
        .messages
        .into_iter()
        .flat_map(|message| message.content)
        .collect::<Vec<ContentPart>>();
    if input.is_empty() && generation.tools.is_empty() {
        return Err(CountDecodeError::Empty);
    }
    let extensions = if plain_text && generation.extensions.values.is_empty() {
        SourceExtensions::new(Surface::Anthropic, BTreeMap::new())
    } else {
        SourceExtensions::new(
            Surface::Anthropic,
            BTreeMap::from([(ANTHROPIC_COUNT_REQUEST_EXTENSION.to_owned(), preserved)]),
        )
    };
    Ok(Operation::TokenCount(TokenCountRequest {
        route: generation.route,
        input,
        extensions,
    }))
}

fn is_plain_text_request(request: &CountTokensRequest) -> bool {
    request.system.is_none()
        && request.tools.is_empty()
        && request.tool_choice.is_none()
        && request.extra.is_empty()
        && matches!(
            request.messages.as_slice(),
            [message]
                if message.role == Role::User
                    && message.extra.is_empty()
                    && matches!(&message.content, MessageContent::Text(_))
        )
}

pub fn encode_count_tokens_result(
    result: &TokenCountResult,
) -> Result<CountTokensResponse, CountEncodeError> {
    if !result.extensions.values.is_empty() && result.extensions.source != Some(Surface::Anthropic)
    {
        return Err(CountEncodeError::CrossProtocol);
    }
    let mut value = serde_json::json!({ "input_tokens": result.input_tokens });
    for (pointer, extension) in &result.extensions.values {
        insert_extension(&mut value, pointer, extension.clone())?;
    }
    serde_json::from_value(value).map_err(CountEncodeError::Json)
}

fn insert_extension(root: &mut Value, pointer: &str, value: Value) -> Result<(), CountEncodeError> {
    let key = pointer
        .strip_prefix('/')
        .filter(|key| !key.is_empty() && !key.contains('/'))
        .map(|key| key.replace("~1", "/").replace("~0", "~"))
        .ok_or(CountEncodeError::Extension)?;
    let object = root.as_object_mut().ok_or(CountEncodeError::Extension)?;
    if object.contains_key(&key) {
        return Err(CountEncodeError::Extension);
    }
    object.insert(key, value);
    Ok(())
}

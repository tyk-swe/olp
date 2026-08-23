use std::collections::BTreeMap;

use crate::domain::canonical::{
    identity::Surface,
    requests::{ContentPart, Operation, SourceExtensions, TokenCountRequest},
    results::TokenCountResult,
};
use thiserror::Error;

use super::{
    dto::{CountTokensRequest, CountTokensResponse, MessageContent, MessagesRequest, Role},
    translate::decode::request as decode_request,
};

/// Private, source-scoped extension used to retain every Anthropic count-token
/// field that the deliberately small canonical token-count operation cannot
/// represent (roles, system blocks, tools, and future vendor fields).
pub const ANTHROPIC_COUNT_REQUEST_EXTENSION: &str = "/__olp/anthropic_count_tokens_request";

#[derive(Debug, Error)]
pub enum DecodeError {
    #[error("Anthropic countTokens request is invalid: {0}")]
    Messages(#[from] super::translate::errors::DecodeError),
    #[error("Anthropic countTokens request could not be preserved")]
    Json(#[from] serde_json::Error),
    #[error("Anthropic countTokens request contains no countable input")]
    Empty,
}

pub type EncodeError = crate::protocols::extensions::TokenCountEncodeError;

pub fn decode_count_tokens_request(request: CountTokensRequest) -> Result<Operation, DecodeError> {
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
    // The Messages decoder's public contract always produces generation.
    let Operation::Generation(generation) = decode_request(generation)? else {
        unreachable!("Anthropic Messages decoding always returns generation")
    };
    let input = generation
        .messages
        .into_iter()
        .flat_map(|message| message.content)
        .collect::<Vec<ContentPart>>();
    if input.is_empty() && generation.tools.is_empty() {
        return Err(DecodeError::Empty);
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
) -> Result<CountTokensResponse, EncodeError> {
    crate::protocols::extensions::encode_token_count(result, Surface::Anthropic, "input_tokens")
}

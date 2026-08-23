use std::collections::BTreeMap;

use crate::domain::canonical::{
    identity::Surface,
    requests::{ContentPart, Operation, SourceExtensions, TokenCountRequest},
    results::TokenCountResult,
};
use thiserror::Error;

use super::{
    dto::{CountTokensRequest, CountTokensResponse, GenerateContentRequest, Part},
    translate::decode::request as decode_request,
};

/// Source-scoped exact Gemini body retained because canonical token counting
/// intentionally does not pretend that roles, tools, safety configuration, or
/// nested generateContentRequest semantics are interchangeable across APIs.
pub const GEMINI_COUNT_REQUEST_EXTENSION: &str = "/__olp/gemini_count_tokens_request";

#[derive(Debug, Error)]
pub enum DecodeError {
    #[error("Gemini countTokens request is invalid: {0}")]
    Count(#[from] super::translate::errors::CountTokensError),
    #[error("Gemini countTokens generation input is invalid: {0}")]
    Generation(#[from] super::translate::errors::DecodeError),
    #[error("Gemini countTokens request could not be preserved")]
    Json(#[from] serde_json::Error),
    #[error("Gemini countTokens request contains no countable input")]
    Empty,
}

pub type EncodeError = crate::protocols::extensions::TokenCountEncodeError;

pub fn decode_count_tokens_request(
    route_model: &str,
    request: CountTokensRequest,
) -> Result<Operation, DecodeError> {
    validate_count_tokens_request(&request)?;
    let plain_text = is_plain_text_request(&request);
    let preserved = serde_json::to_value(&request)?;
    let generation = match request.generate_content_request {
        Some(generation) => generation,
        None => GenerateContentRequest {
            contents: request.contents,
            extra: request.extra,
            ..GenerateContentRequest::default()
        },
    };
    // The generateContent decoder's public contract always produces generation.
    let Operation::Generation(generation) = decode_request(route_model, generation, false)? else {
        unreachable!("Gemini generation decoding always returns generation")
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
        SourceExtensions::new(Surface::Gemini, BTreeMap::new())
    } else {
        SourceExtensions::new(
            Surface::Gemini,
            BTreeMap::from([(GEMINI_COUNT_REQUEST_EXTENSION.to_owned(), preserved)]),
        )
    };
    Ok(Operation::TokenCount(TokenCountRequest {
        route: generation.route,
        input,
        extensions,
    }))
}

fn is_plain_text_request(request: &CountTokensRequest) -> bool {
    request.generate_content_request.is_none()
        && request.extra.is_empty()
        && matches!(
            request.contents.as_slice(),
            [content]
                if content.role.as_deref().is_none_or(|role| role == "user")
                    && content.extra.is_empty()
                    && !content.parts.is_empty()
                    && content.parts.iter().all(|part| matches!(
                        part,
                        Part::Text(text)
                            if text.thought.is_none()
                                && text.thought_signature.is_none()
                                && text.extra.is_empty()
                    ))
        )
}

pub fn encode_count_tokens_result(
    result: &TokenCountResult,
) -> Result<CountTokensResponse, EncodeError> {
    crate::protocols::extensions::encode_token_count(result, Surface::Gemini, "totalTokens")
}

pub fn validate_count_tokens_request(
    request: &CountTokensRequest,
) -> Result<(), super::translate::errors::CountTokensError> {
    let has_contents = !request.contents.is_empty();
    let has_generate_request = request.generate_content_request.is_some();
    if has_contents == has_generate_request {
        return Err(super::translate::errors::CountTokensError::ExactlyOneInput);
    }
    Ok(())
}

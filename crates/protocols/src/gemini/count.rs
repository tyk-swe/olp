use std::collections::BTreeMap;

use olp_domain::{
    ContentPart, Operation, SourceExtensions, Surface, TokenCountRequest, TokenCountResult,
};
use serde_json::Value;
use thiserror::Error;

use super::{
    CountTokensRequest, CountTokensResponse, GenerateContentRequest, Part,
    decode_generate_content_request, validate_count_tokens_request,
};

/// Source-scoped exact Gemini body retained because canonical token counting
/// intentionally does not pretend that roles, tools, safety configuration, or
/// nested generateContentRequest semantics are interchangeable across APIs.
pub const GEMINI_COUNT_REQUEST_EXTENSION: &str = "/__olp/gemini_count_tokens_request";

#[derive(Debug, Error)]
pub enum CountDecodeError {
    #[error("Gemini countTokens request is invalid: {0}")]
    Count(#[from] super::CountTokensError),
    #[error("Gemini countTokens generation input is invalid: {0}")]
    Generation(#[from] super::DecodeError),
    #[error("Gemini countTokens request could not be preserved")]
    Json(#[from] serde_json::Error),
    #[error("Gemini countTokens request contains no countable input")]
    Empty,
}

#[derive(Debug, Error)]
pub enum CountEncodeError {
    #[error("countTokens response extensions came from a different protocol")]
    CrossProtocol,
    #[error("countTokens response contains an invalid or colliding extension path")]
    Extension,
    #[error("Gemini countTokens response could not be encoded")]
    Json(#[from] serde_json::Error),
}

pub fn decode_count_tokens_request(
    route_model: &str,
    request: CountTokensRequest,
) -> Result<Operation, CountDecodeError> {
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
    let Operation::Generation(generation) =
        decode_generate_content_request(route_model, generation, false)?
    else {
        unreachable!("Gemini generation decoding always returns generation")
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
) -> Result<CountTokensResponse, CountEncodeError> {
    if !result.extensions.values.is_empty() && result.extensions.source != Some(Surface::Gemini) {
        return Err(CountEncodeError::CrossProtocol);
    }
    let mut value = serde_json::json!({ "totalTokens": result.input_tokens });
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

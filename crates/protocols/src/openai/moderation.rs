use std::collections::BTreeMap;

use olp_domain::{
    ContentPart, MediaSource, ModerationItem, ModerationRequest as CanonicalModerationRequest,
    ModerationResult, Operation, RouteSlug, RouteSlugError, SourceExtensions, Surface,
};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};
use thiserror::Error;

use super::extensions::{apply_pointer_extensions, collect_extra, escape_json_pointer};

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiModerationRequest {
    pub model: String,
    pub input: Value,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_moderation(
    request: OpenAiModerationRequest,
) -> Result<Operation, ModerationCodecError> {
    let route = RouteSlug::parse(request.model)?;
    let mut extensions = BTreeMap::new();
    collect_extra("", &request.extra, &mut extensions);
    let values = match request.input {
        Value::Array(values) => values,
        value => vec![value],
    };
    if values.is_empty() {
        return Err(ModerationCodecError::EmptyInput);
    }
    let input = values
        .into_iter()
        .enumerate()
        .map(|(index, value)| decode_moderation_input(index, value, &mut extensions))
        .collect::<Result<Vec<_>, _>>()?;
    Ok(Operation::Moderation(CanonicalModerationRequest {
        route,
        input,
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    }))
}

fn decode_moderation_input(
    index: usize,
    value: Value,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<ContentPart, ModerationCodecError> {
    match value {
        Value::String(text) if !text.is_empty() => Ok(ContentPart::Text { text }),
        Value::String(_) => Err(ModerationCodecError::EmptyInputItem(index)),
        Value::Object(mut object) => {
            let kind = take_string(&mut object, "type", index)?;
            let part = match kind.as_str() {
                "text" | "input_text" => ContentPart::Text {
                    text: take_string(&mut object, "text", index)?,
                },
                "image_url" => {
                    let image = object
                        .remove("image_url")
                        .and_then(|value| value.as_object().cloned())
                        .ok_or(ModerationCodecError::InvalidInputItem(index))?;
                    let mut image = image;
                    let url = take_string(&mut image, "url", index)?;
                    collect_object_extra(&format!("/input/{index}/image_url"), image, extensions);
                    ContentPart::Image {
                        source: MediaSource::Uri(url),
                        detail: None,
                    }
                }
                _ => return Err(ModerationCodecError::UnsupportedInputType(kind)),
            };
            collect_object_extra(&format!("/input/{index}"), object, extensions);
            Ok(part)
        }
        _ => Err(ModerationCodecError::InvalidInputItem(index)),
    }
}

pub fn encode_moderation(
    request: &CanonicalModerationRequest,
    upstream_model: &str,
) -> Result<OpenAiModerationRequest, ModerationCodecError> {
    request
        .extensions
        .ensure_representable_on(Surface::OpenAi)?;
    let input = request
        .input
        .iter()
        .map(|part| match part {
            ContentPart::Text { text } => Ok(Value::String(text.clone())),
            ContentPart::Image {
                source: MediaSource::Uri(url),
                ..
            } => Ok(serde_json::json!({
                "type": "image_url",
                "image_url": {"url": url},
            })),
            ContentPart::Image {
                source: MediaSource::Handle(_),
                ..
            }
            | ContentPart::InputAudio { .. }
            | ContentPart::InputFile { .. }
            | ContentPart::Refusal { .. } => Err(ModerationCodecError::UnrepresentableInput),
        })
        .collect::<Result<Vec<_>, _>>()?;
    apply_pointer_extensions(
        OpenAiModerationRequest {
            model: upstream_model.into(),
            input: Value::Array(input),
            extra: BTreeMap::new(),
        },
        &request.extensions.values,
    )
    .map_err(ModerationCodecError::InvalidExtension)
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiModerationResponse {
    pub id: String,
    pub model: String,
    pub results: Vec<OpenAiModerationResult>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiModerationResult {
    pub flagged: bool,
    pub categories: BTreeMap<String, bool>,
    pub category_scores: BTreeMap<String, f64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub category_applied_input_types: Option<BTreeMap<String, Vec<String>>>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_moderation_response(response: OpenAiModerationResponse) -> ModerationResult {
    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);
    let results = response
        .results
        .into_iter()
        .enumerate()
        .map(|(index, result)| {
            collect_extra(&format!("/results/{index}"), &result.extra, &mut extensions);
            if let Some(types) = result.category_applied_input_types {
                extensions.insert(
                    format!("/results/{index}/category_applied_input_types"),
                    serde_json::to_value(types).unwrap_or(Value::Null),
                );
            }
            ModerationItem {
                flagged: result.flagged,
                categories: result.categories,
                category_scores: result.category_scores,
            }
        })
        .collect();
    ModerationResult {
        id: Some(response.id),
        model: Some(response.model),
        results,
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    }
}

pub fn encode_moderation_response(
    result: &ModerationResult,
    client_model: &str,
    fallback_id: &str,
) -> Result<OpenAiModerationResponse, ModerationCodecError> {
    result.extensions.ensure_representable_on(Surface::OpenAi)?;
    let results = result
        .results
        .iter()
        .map(|item| OpenAiModerationResult {
            flagged: item.flagged,
            categories: item.categories.clone(),
            category_scores: item.category_scores.clone(),
            category_applied_input_types: None,
            extra: BTreeMap::new(),
        })
        .collect();
    apply_pointer_extensions(
        OpenAiModerationResponse {
            id: result.id.clone().unwrap_or_else(|| fallback_id.into()),
            // Client-facing model identifiers are always public route slugs;
            // never leak the selected provider model into the response.
            model: client_model.into(),
            results,
            extra: BTreeMap::new(),
        },
        &result.extensions.values,
    )
    .map_err(ModerationCodecError::InvalidExtension)
}

fn take_string(
    object: &mut Map<String, Value>,
    field: &'static str,
    index: usize,
) -> Result<String, ModerationCodecError> {
    object
        .remove(field)
        .and_then(|value| value.as_str().map(str::to_owned))
        .ok_or(ModerationCodecError::InvalidInputItem(index))
}

fn collect_object_extra(
    prefix: &str,
    object: Map<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) {
    for (field, value) in object {
        extensions.insert(format!("{prefix}/{}", escape_json_pointer(&field)), value);
    }
}

#[derive(Debug, Error)]
pub enum ModerationCodecError {
    #[error(transparent)]
    InvalidRoute(#[from] RouteSlugError),
    #[error(transparent)]
    Extensions(#[from] olp_domain::ExtensionError),
    #[error("moderation input cannot be empty")]
    EmptyInput,
    #[error("moderation input item {0} cannot be empty")]
    EmptyInputItem(usize),
    #[error("invalid moderation input item {0}")]
    InvalidInputItem(usize),
    #[error("unsupported moderation input type: {0}")]
    UnsupportedInputType(String),
    #[error("canonical moderation input cannot be represented by OpenAI")]
    UnrepresentableInput,
    #[error("invalid source extension path: {0}")]
    InvalidExtension(String),
}

use std::collections::BTreeMap;

use olp_domain::{
    ModelDescriptor, ModelListResult, ModelOperation, Operation, RouteSlug, RouteSlugError,
    SourceExtensions, Surface,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;

use super::extensions::{apply_pointer_extensions, collect_extra};

pub fn decode_model_list() -> Operation {
    Operation::Models(ModelOperation::List {
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    })
}

pub fn decode_model_get(model: &str) -> Result<Operation, ModelCodecError> {
    Ok(Operation::Models(ModelOperation::Get {
        route: RouteSlug::parse(model)?,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    }))
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiModelObject {
    pub id: String,
    pub object: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub created: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub owned_by: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OpenAiModelListResponse {
    pub object: String,
    pub data: Vec<OpenAiModelObject>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_model_object(model: OpenAiModelObject) -> Result<ModelDescriptor, ModelCodecError> {
    if model.object != "model" {
        return Err(ModelCodecError::UnexpectedObject(model.object));
    }
    let mut extensions = BTreeMap::new();
    collect_extra("", &model.extra, &mut extensions);
    Ok(ModelDescriptor {
        id: model.id,
        created_at: model.created,
        owned_by: model.owned_by,
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    })
}

pub fn encode_model_object(model: &ModelDescriptor) -> Result<OpenAiModelObject, ModelCodecError> {
    model.extensions.ensure_representable_on(Surface::OpenAi)?;
    apply_pointer_extensions(
        OpenAiModelObject {
            id: model.id.clone(),
            object: "model".into(),
            created: model.created_at,
            owned_by: model.owned_by.clone(),
            extra: BTreeMap::new(),
        },
        &model.extensions.values,
    )
    .map_err(ModelCodecError::InvalidExtension)
}

pub fn decode_model_list_response(
    response: OpenAiModelListResponse,
) -> Result<ModelListResult, ModelCodecError> {
    if response.object != "list" {
        return Err(ModelCodecError::UnexpectedObject(response.object));
    }
    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);
    let models = response
        .data
        .into_iter()
        .map(decode_model_object)
        .collect::<Result<_, _>>()?;
    Ok(ModelListResult {
        models,
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    })
}

pub fn encode_model_list_response(
    result: &ModelListResult,
) -> Result<OpenAiModelListResponse, ModelCodecError> {
    result.extensions.ensure_representable_on(Surface::OpenAi)?;
    let data = result
        .models
        .iter()
        .map(encode_model_object)
        .collect::<Result<_, _>>()?;
    apply_pointer_extensions(
        OpenAiModelListResponse {
            object: "list".into(),
            data,
            extra: BTreeMap::new(),
        },
        &result.extensions.values,
    )
    .map_err(ModelCodecError::InvalidExtension)
}

#[derive(Debug, Error)]
pub enum ModelCodecError {
    #[error(transparent)]
    InvalidRoute(#[from] RouteSlugError),
    #[error(transparent)]
    Extensions(#[from] olp_domain::ExtensionError),
    #[error("unexpected OpenAI object type: {0}")]
    UnexpectedObject(String),
    #[error("invalid source extension path: {0}")]
    InvalidExtension(String),
}

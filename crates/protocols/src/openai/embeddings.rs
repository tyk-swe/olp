use std::collections::BTreeMap;

use base64::{Engine as _, engine::general_purpose::STANDARD};
use olp_domain::{
    EmbeddingInput, EmbeddingVector, EmbeddingsRequest, EmbeddingsResult, Operation, RouteSlug,
    RouteSlugError, SourceExtensions, Surface, Usage,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;

use super::extensions::{apply_flat_extensions, collect_extra};

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct EmbeddingRequest {
    pub model: String,
    pub input: EmbeddingWireInput,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub dimensions: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub encoding_format: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub user: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(untagged)]
pub enum EmbeddingWireInput {
    Text(String),
    Texts(Vec<String>),
    Tokens(Vec<u32>),
    TokenArrays(Vec<Vec<u32>>),
}

pub fn decode_embedding_request(
    request: EmbeddingRequest,
) -> Result<Operation, EmbeddingCodecError> {
    let route = RouteSlug::parse(request.model)?;
    let input = match request.input {
        EmbeddingWireInput::Text(text) => vec![EmbeddingInput::Text(text)],
        EmbeddingWireInput::Texts(values) => values.into_iter().map(EmbeddingInput::Text).collect(),
        EmbeddingWireInput::Tokens(values) => vec![EmbeddingInput::Tokens(values)],
        EmbeddingWireInput::TokenArrays(values) => {
            values.into_iter().map(EmbeddingInput::Tokens).collect()
        }
    };
    if input.is_empty() {
        return Err(EmbeddingCodecError::EmptyInput);
    }
    if input.iter().any(|value| match value {
        EmbeddingInput::Text(text) => text.is_empty(),
        EmbeddingInput::Tokens(tokens) => tokens.is_empty(),
    }) {
        return Err(EmbeddingCodecError::EmptyInputItem);
    }
    if request.dimensions == Some(0) {
        return Err(EmbeddingCodecError::ZeroDimensions);
    }

    let mut extensions = BTreeMap::new();
    collect_extra("", &request.extra, &mut extensions);
    if let Some(value) = request.encoding_format {
        extensions.insert("/encoding_format".into(), Value::String(value));
    }
    if let Some(value) = request.user {
        extensions.insert("/user".into(), Value::String(value));
    }

    Ok(Operation::Embeddings(EmbeddingsRequest {
        route,
        input,
        dimensions: request.dimensions,
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    }))
}

pub fn encode_embedding_request(
    request: &EmbeddingsRequest,
    provider_model: &str,
) -> Result<EmbeddingRequest, EmbeddingCodecError> {
    request
        .extensions
        .ensure_representable_on(Surface::OpenAi)?;
    let input = match request.input.as_slice() {
        [EmbeddingInput::Text(text)] => EmbeddingWireInput::Text(text.clone()),
        [EmbeddingInput::Tokens(tokens)] => EmbeddingWireInput::Tokens(tokens.clone()),
        values
            if values
                .iter()
                .all(|value| matches!(value, EmbeddingInput::Text(_))) =>
        {
            EmbeddingWireInput::Texts(
                values
                    .iter()
                    .filter_map(|value| match value {
                        EmbeddingInput::Text(text) => Some(text.clone()),
                        EmbeddingInput::Tokens(_) => None,
                    })
                    .collect(),
            )
        }
        values
            if values
                .iter()
                .all(|value| matches!(value, EmbeddingInput::Tokens(_))) =>
        {
            EmbeddingWireInput::TokenArrays(
                values
                    .iter()
                    .filter_map(|value| match value {
                        EmbeddingInput::Tokens(tokens) => Some(tokens.clone()),
                        EmbeddingInput::Text(_) => None,
                    })
                    .collect(),
            )
        }
        _ => return Err(EmbeddingCodecError::MixedInputKinds),
    };
    let mut wire = EmbeddingRequest {
        model: provider_model.into(),
        input,
        dimensions: request.dimensions,
        encoding_format: None,
        user: None,
        extra: BTreeMap::new(),
    };
    for (path, value) in &request.extensions.values {
        match path.as_str() {
            "/encoding_format" => {
                wire.encoding_format = value.as_str().map(str::to_owned);
                if wire.encoding_format.is_none() {
                    return Err(EmbeddingCodecError::InvalidExtension(path.clone()));
                }
            }
            "/user" => {
                wire.user = value.as_str().map(str::to_owned);
                if wire.user.is_none() {
                    return Err(EmbeddingCodecError::InvalidExtension(path.clone()));
                }
            }
            _ => apply_flat_extensions(
                &mut wire.extra,
                &BTreeMap::from([(path.clone(), value.clone())]),
            )
            .map_err(EmbeddingCodecError::InvalidExtension)?,
        }
    }
    Ok(wire)
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct EmbeddingResponse {
    pub object: String,
    pub data: Vec<EmbeddingData>,
    pub model: String,
    pub usage: EmbeddingUsage,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct EmbeddingData {
    pub object: String,
    pub embedding: EmbeddingWireVector,
    pub index: u32,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(untagged)]
pub enum EmbeddingWireVector {
    Floats(Vec<f32>),
    Base64(String),
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct EmbeddingUsage {
    pub prompt_tokens: u64,
    pub total_tokens: u64,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_embedding_response(
    response: EmbeddingResponse,
) -> Result<EmbeddingsResult, EmbeddingCodecError> {
    if response.object != "list" {
        return Err(EmbeddingCodecError::UnexpectedObject(response.object));
    }
    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);
    collect_extra("/usage", &response.usage.extra, &mut extensions);
    let mut data = Vec::with_capacity(response.data.len());
    for item in response.data {
        if item.object != "embedding" {
            return Err(EmbeddingCodecError::UnexpectedObject(item.object));
        }
        collect_extra(
            &format!("/data/{}", item.index),
            &item.extra,
            &mut extensions,
        );
        let values = match item.embedding {
            EmbeddingWireVector::Floats(values) => values,
            EmbeddingWireVector::Base64(encoded) => decode_base64_vector(&encoded)?,
        };
        data.push(EmbeddingVector {
            index: item.index,
            values,
        });
    }
    data.sort_by_key(|item| item.index);
    Ok(EmbeddingsResult {
        model: Some(response.model),
        data,
        usage: Some(Usage {
            input_tokens: response.usage.prompt_tokens,
            output_tokens: 0,
            total_tokens: response.usage.total_tokens,
            cached_input_tokens: None,
            reasoning_tokens: None,
        }),
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    })
}

pub fn encode_embedding_response(
    result: &EmbeddingsResult,
    client_model: &str,
    encoding_format: Option<&str>,
) -> Result<EmbeddingResponse, EmbeddingCodecError> {
    result.extensions.ensure_representable_on(Surface::OpenAi)?;
    let data = result
        .data
        .iter()
        .map(|item| {
            let embedding = match encoding_format.unwrap_or("float") {
                "float" => EmbeddingWireVector::Floats(item.values.clone()),
                "base64" => {
                    let bytes = item
                        .values
                        .iter()
                        .flat_map(|value| value.to_le_bytes())
                        .collect::<Vec<_>>();
                    EmbeddingWireVector::Base64(STANDARD.encode(bytes))
                }
                value => return Err(EmbeddingCodecError::UnsupportedEncoding(value.into())),
            };
            Ok(EmbeddingData {
                object: "embedding".into(),
                embedding,
                index: item.index,
                extra: BTreeMap::new(),
            })
        })
        .collect::<Result<Vec<_>, _>>()?;
    let usage = result.usage.unwrap_or_default();
    super::extensions::apply_pointer_extensions(
        EmbeddingResponse {
            object: "list".into(),
            data,
            model: client_model.into(),
            usage: EmbeddingUsage {
                prompt_tokens: usage.input_tokens,
                total_tokens: usage.total_tokens,
                extra: BTreeMap::new(),
            },
            extra: BTreeMap::new(),
        },
        &result.extensions.values,
    )
    .map_err(EmbeddingCodecError::InvalidExtension)
}

fn decode_base64_vector(encoded: &str) -> Result<Vec<f32>, EmbeddingCodecError> {
    const MAX_EMBEDDING_BYTES: usize = 16 * 1024 * 1024;
    if encoded.len() > MAX_EMBEDDING_BYTES.saturating_mul(2) {
        return Err(EmbeddingCodecError::EmbeddingTooLarge);
    }
    let bytes = STANDARD
        .decode(encoded)
        .map_err(|_| EmbeddingCodecError::InvalidBase64Embedding)?;
    if bytes.len() > MAX_EMBEDDING_BYTES {
        return Err(EmbeddingCodecError::EmbeddingTooLarge);
    }
    let chunks = bytes.chunks_exact(4);
    if !chunks.remainder().is_empty() {
        return Err(EmbeddingCodecError::InvalidBase64Embedding);
    }
    let values = chunks
        .map(|chunk| f32::from_le_bytes([chunk[0], chunk[1], chunk[2], chunk[3]]))
        .collect::<Vec<_>>();
    if values.iter().any(|value| !value.is_finite()) {
        return Err(EmbeddingCodecError::InvalidBase64Embedding);
    }
    Ok(values)
}

#[derive(Debug, Error)]
pub enum EmbeddingCodecError {
    #[error(transparent)]
    InvalidRoute(#[from] RouteSlugError),
    #[error(transparent)]
    Extensions(#[from] olp_domain::ExtensionError),
    #[error("embedding input cannot be empty")]
    EmptyInput,
    #[error("embedding input items cannot be empty")]
    EmptyInputItem,
    #[error("dimensions must be greater than zero")]
    ZeroDimensions,
    #[error("text and token-array inputs cannot be mixed in one canonical request")]
    MixedInputKinds,
    #[error("invalid source extension path or value: {0}")]
    InvalidExtension(String),
    #[error("unexpected OpenAI object type: {0}")]
    UnexpectedObject(String),
    #[error("base64 embedding payload is invalid")]
    InvalidBase64Embedding,
    #[error("embedding payload exceeds the bounded decoder limit")]
    EmbeddingTooLarge,
    #[error("unsupported embedding encoding_format: {0}")]
    UnsupportedEncoding(String),
}

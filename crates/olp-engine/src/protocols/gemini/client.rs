use std::collections::BTreeMap;

use crate::domain::{CanonicalEvent, Surface};
use serde_json::Value;
use thiserror::Error;

use crate::protocols::client::{AggregateError, aggregate_generation};
use crate::protocols::extensions::{PointerExtensionError, apply_response_extensions};

use super::{
    Candidate, Content, FunctionCall, FunctionCallPart, GenerateContentResponse, Part, TextPart,
    UsageMetadata, finish_reason,
};

#[derive(Debug, Error)]
pub enum ClientEncodeError {
    #[error(transparent)]
    Aggregate(#[from] AggregateError),
    #[error("canonical output is missing a finish reason")]
    MissingFinish,
    #[error("canonical tool call is missing a name")]
    IncompleteTool,
    #[error("canonical tool arguments are not valid JSON")]
    ToolJson(#[source] serde_json::Error),
    #[error("canonical usage is inconsistent")]
    InvalidUsage,
    #[error("source extension path cannot be represented on the Gemini response")]
    Extension,
    #[error("Gemini response encoding failed")]
    Json(#[source] serde_json::Error),
}

pub fn encode_generate_content_response(
    events: &[CanonicalEvent],
    public_model: &str,
    fallback_id: &str,
) -> Result<GenerateContentResponse, ClientEncodeError> {
    let mut aggregate = aggregate_generation(events, Surface::Gemini)?;
    let preserve_empty_candidates = matches!(
        aggregate.extensions.get("/candidates"),
        Some(Value::Array(candidates)) if candidates.is_empty()
    );
    if preserve_empty_candidates {
        aggregate.extensions.remove("/candidates");
    }
    let mut candidates = Vec::with_capacity(aggregate.outputs.len());
    for (index, output) in aggregate.outputs {
        if preserve_empty_candidates {
            continue;
        }
        let preserved_parts = take_preserved_parts(&mut aggregate.extensions, index)?;
        let mut parts = preserved_parts.unwrap_or_default();
        if parts.is_empty() {
            if !output.text.is_empty() {
                parts.push(Part::Text(TextPart {
                    text: output.text,
                    thought: None,
                    thought_signature: None,
                    extra: BTreeMap::new(),
                }));
            }
            for tool in output.tools.into_values() {
                let name = tool.name.ok_or(ClientEncodeError::IncompleteTool)?;
                let args =
                    serde_json::from_str(&tool.arguments).map_err(ClientEncodeError::ToolJson)?;
                parts.push(Part::FunctionCall(FunctionCallPart {
                    function_call: FunctionCall {
                        name,
                        args,
                        id: tool.id,
                        extra: BTreeMap::new(),
                    },
                    extra: BTreeMap::new(),
                }));
            }
        }
        let finish_reason = output
            .finish
            .as_ref()
            .map(finish_reason)
            .ok_or(ClientEncodeError::MissingFinish)?;
        candidates.push(Candidate {
            content: Some(Content {
                role: Some("model".to_owned()),
                parts,
                extra: BTreeMap::new(),
            }),
            finish_reason: Some(finish_reason.to_owned()),
            index: Some(index),
            extra: BTreeMap::new(),
        });
    }
    let usage_metadata = if let Some(usage) = aggregate.usage {
        let tool_use_prompt_token_count = aggregate
            .extensions
            .remove("/usageMetadata/toolUsePromptTokenCount")
            .map(|value| value.as_u64().ok_or(ClientEncodeError::InvalidUsage))
            .transpose()?;
        let prompt_token_count = usage
            .input_tokens
            .checked_sub(tool_use_prompt_token_count.unwrap_or(0))
            .ok_or(ClientEncodeError::InvalidUsage)?;
        let candidates_token_count = usage
            .output_tokens
            .checked_sub(usage.reasoning_tokens.unwrap_or(0))
            .ok_or(ClientEncodeError::InvalidUsage)?;
        Some(UsageMetadata {
            prompt_token_count: Some(prompt_token_count),
            candidates_token_count: Some(candidates_token_count),
            total_token_count: Some(usage.total_tokens),
            cached_content_token_count: usage.cached_input_tokens,
            thoughts_token_count: usage.reasoning_tokens,
            tool_use_prompt_token_count,
            extra: BTreeMap::new(),
        })
    } else {
        None
    };
    let response = GenerateContentResponse {
        candidates,
        usage_metadata,
        model_version: Some(public_model.to_owned()),
        response_id: Some(
            aggregate
                .response_id
                .unwrap_or_else(|| fallback_id.to_owned()),
        ),
        extra: BTreeMap::new(),
    };
    let partial_usage = aggregate.extensions.remove("/usageMetadata");
    let mut response = apply_extensions(response, &aggregate.extensions)?;
    if let Some(Value::Object(usage)) = partial_usage {
        response
            .extra
            .insert("usageMetadata".into(), Value::Object(usage));
    }
    Ok(response)
}

fn take_preserved_parts(
    extensions: &mut BTreeMap<String, Value>,
    output_index: u32,
) -> Result<Option<Vec<Part>>, ClientEncodeError> {
    let prefix = format!("/candidates/{output_index}/content/parts/");
    let mut entries = extensions
        .keys()
        .filter_map(|path| {
            path.strip_prefix(&prefix)
                .and_then(|index| index.parse::<usize>().ok())
                .map(|index| (index, path.clone()))
        })
        .collect::<Vec<_>>();
    if entries.is_empty() {
        return Ok(None);
    }
    entries.sort_by_key(|(index, _)| *index);
    let mut parts = Vec::with_capacity(entries.len());
    for (expected_index, (index, path)) in entries.into_iter().enumerate() {
        if index != expected_index {
            return Err(ClientEncodeError::Extension);
        }
        let value = extensions
            .remove(&path)
            .ok_or(ClientEncodeError::Extension)?;
        parts.push(serde_json::from_value(value).map_err(ClientEncodeError::Json)?);
    }
    Ok(Some(parts))
}

fn apply_extensions(
    response: GenerateContentResponse,
    extensions: &BTreeMap<String, Value>,
) -> Result<GenerateContentResponse, ClientEncodeError> {
    apply_response_extensions(response, extensions).map_err(|error| match error {
        PointerExtensionError::InvalidPath(_) => ClientEncodeError::Extension,
        PointerExtensionError::Json(error) => ClientEncodeError::Json(error),
    })
}

#[cfg(test)]
mod tests {
    use crate::domain::{
        CanonicalEventKind, FinishReason, MessageRole, SourceExtensions, Usage as CanonicalUsage,
    };

    use super::*;

    #[test]
    fn encodes_multiple_candidates_usage_and_safety_extensions() {
        let events = vec![
            CanonicalEvent::new(
                0,
                CanonicalEventKind::ResponseStart {
                    response_id: Some("response-1".into()),
                    provider_model: Some("provider-model".into()),
                },
            ),
            CanonicalEvent::new(
                1,
                CanonicalEventKind::MessageStart {
                    output_index: 0,
                    role: MessageRole::Assistant,
                },
            ),
            CanonicalEvent::new(
                2,
                CanonicalEventKind::TextDelta {
                    output_index: 0,
                    text: "hello".into(),
                },
            ),
            CanonicalEvent::new(
                3,
                CanonicalEventKind::Finish {
                    output_index: 0,
                    reason: FinishReason::Stop,
                },
            ),
            CanonicalEvent::new(
                4,
                CanonicalEventKind::Usage {
                    usage: CanonicalUsage {
                        input_tokens: 3,
                        output_tokens: 3,
                        total_tokens: 6,
                        cached_input_tokens: Some(1),
                        reasoning_tokens: Some(1),
                    },
                },
            ),
            CanonicalEvent::new(
                5,
                CanonicalEventKind::SourceExtension {
                    extensions: SourceExtensions::new(
                        Surface::Gemini,
                        [("/vendorFlag".into(), Value::Bool(true))].into(),
                    ),
                },
            ),
            CanonicalEvent::new(6, CanonicalEventKind::Done),
        ];
        let response = encode_generate_content_response(&events, "route", "fallback").unwrap();
        assert_eq!(response.model_version.as_deref(), Some("route"));
        assert_eq!(response.extra["vendorFlag"], true);
        let usage = response.usage_metadata.unwrap();
        assert_eq!(usage.candidates_token_count, Some(2));
        assert_eq!(usage.thoughts_token_count, Some(1));
        assert_eq!(usage.total_token_count, Some(6));
    }
}

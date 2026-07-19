use std::collections::BTreeMap;

use olp_domain::{CanonicalEvent, FinishReason, Surface};
use serde_json::Value;
use thiserror::Error;

use crate::client::{AggregateError, aggregate_generation};

use super::{
    Candidate, Content, FunctionCall, FunctionCallPart, GenerateContentResponse, Part, TextPart,
    UsageMetadata,
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
    let aggregate = aggregate_generation(events, Surface::Gemini)?;
    let mut candidates = Vec::with_capacity(aggregate.outputs.len());
    for (index, output) in aggregate.outputs {
        let mut parts = Vec::new();
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
        let finish_reason = output
            .finish
            .as_ref()
            .map(gemini_finish_reason)
            .ok_or(ClientEncodeError::MissingFinish)?;
        candidates.push(Candidate {
            content: Some(Content {
                role: Some("model".to_owned()),
                parts,
                extra: BTreeMap::new(),
            }),
            finish_reason: Some(finish_reason),
            index: Some(index),
            extra: BTreeMap::new(),
        });
    }
    let usage_metadata = aggregate.usage.map(|usage| UsageMetadata {
        prompt_token_count: usage.input_tokens,
        candidates_token_count: usage.output_tokens,
        total_token_count: usage.total_tokens,
        cached_content_token_count: usage.cached_input_tokens,
        thoughts_token_count: usage.reasoning_tokens,
        extra: BTreeMap::new(),
    });
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
    apply_extensions(response, &aggregate.extensions)
}

fn gemini_finish_reason(reason: &FinishReason) -> String {
    match reason {
        FinishReason::Stop | FinishReason::ToolCalls => "STOP".to_owned(),
        FinishReason::Length => "MAX_TOKENS".to_owned(),
        FinishReason::ContentFilter => "SAFETY".to_owned(),
        FinishReason::Error => "OTHER".to_owned(),
        FinishReason::Other(value) => value.clone(),
    }
}

fn apply_extensions(
    response: GenerateContentResponse,
    extensions: &BTreeMap<String, Value>,
) -> Result<GenerateContentResponse, ClientEncodeError> {
    let mut value = serde_json::to_value(response).map_err(ClientEncodeError::Json)?;
    let mut entries = extensions.iter().collect::<Vec<_>>();
    entries.sort_by_key(|(pointer, _)| pointer_depth(pointer));
    for (pointer, extension) in entries {
        insert_pointer(&mut value, pointer, extension.clone())?;
    }
    serde_json::from_value(value).map_err(ClientEncodeError::Json)
}

fn insert_pointer(root: &mut Value, pointer: &str, value: Value) -> Result<(), ClientEncodeError> {
    let segments = pointer
        .strip_prefix('/')
        .filter(|pointer| !pointer.is_empty())
        .ok_or(ClientEncodeError::Extension)?
        .split('/')
        .map(|segment| segment.replace("~1", "/").replace("~0", "~"))
        .collect::<Vec<_>>();
    if segments.len() > 24 {
        return Err(ClientEncodeError::Extension);
    }
    let (terminal, parents) = segments.split_last().ok_or(ClientEncodeError::Extension)?;
    let mut current = root;
    for segment in parents {
        current = match current {
            Value::Object(object) => object.get_mut(segment),
            Value::Array(array) => segment
                .parse::<usize>()
                .ok()
                .and_then(|index| array.get_mut(index)),
            _ => None,
        }
        .ok_or(ClientEncodeError::Extension)?;
    }
    match current {
        Value::Object(object) if !object.contains_key(terminal) => {
            object.insert(terminal.clone(), value);
            Ok(())
        }
        Value::Array(array) => {
            let index = terminal
                .parse::<usize>()
                .map_err(|_| ClientEncodeError::Extension)?;
            if index <= array.len() {
                array.insert(index, value);
                Ok(())
            } else {
                Err(ClientEncodeError::Extension)
            }
        }
        _ => Err(ClientEncodeError::Extension),
    }
}

fn pointer_depth(pointer: &str) -> usize {
    pointer.bytes().filter(|byte| *byte == b'/').count()
}

#[cfg(test)]
mod tests {
    use olp_domain::{CanonicalEventKind, MessageRole, SourceExtensions, Usage as CanonicalUsage};

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
                        output_tokens: 2,
                        total_tokens: 5,
                        cached_input_tokens: Some(1),
                        reasoning_tokens: None,
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
        assert_eq!(response.usage_metadata.unwrap().total_token_count, 5);
    }
}

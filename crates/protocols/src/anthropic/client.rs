use std::collections::BTreeMap;

use olp_domain::{CanonicalEvent, FinishReason, Surface};
use serde_json::Value;
use thiserror::Error;

use crate::client::{AggregateError, aggregate_generation};

use super::{ContentBlock, MessagesResponse, Role, TextBlock, ToolUseBlock, Usage};

#[derive(Debug, Error)]
pub enum ClientEncodeError {
    #[error(transparent)]
    Aggregate(#[from] AggregateError),
    #[error("Anthropic Messages supports one output candidate")]
    CandidateCount,
    #[error("canonical output is missing a finish reason")]
    MissingFinish,
    #[error("canonical tool call is missing an ID or name")]
    IncompleteTool,
    #[error("canonical tool arguments are not valid JSON")]
    ToolJson(#[source] serde_json::Error),
    #[error("canonical reasoning-token usage is not representable in Anthropic usage")]
    ReasoningUsage,
    #[error("source extension path cannot be represented on the Anthropic response")]
    Extension,
    #[error("Anthropic response encoding failed")]
    Json(#[source] serde_json::Error),
}

pub fn encode_messages_response(
    events: &[CanonicalEvent],
    public_model: &str,
    fallback_id: &str,
) -> Result<MessagesResponse, ClientEncodeError> {
    let aggregate = aggregate_generation(events, Surface::Anthropic)?;
    if aggregate.outputs.len() != 1 || !aggregate.outputs.contains_key(&0) {
        return Err(ClientEncodeError::CandidateCount);
    }
    let output = aggregate
        .outputs
        .get(&0)
        .expect("candidate count was checked");
    let mut content = Vec::new();
    if !output.text.is_empty() {
        content.push(ContentBlock::Text(TextBlock {
            kind: "text".to_owned(),
            text: output.text.clone(),
            extra: BTreeMap::new(),
        }));
    }
    for tool in output.tools.values() {
        let id = tool.id.clone().ok_or(ClientEncodeError::IncompleteTool)?;
        let name = tool.name.clone().ok_or(ClientEncodeError::IncompleteTool)?;
        let input = serde_json::from_str(&tool.arguments).map_err(ClientEncodeError::ToolJson)?;
        content.push(ContentBlock::ToolUse(ToolUseBlock {
            kind: "tool_use".to_owned(),
            id,
            name,
            input,
            extra: BTreeMap::new(),
        }));
    }
    let finish = output
        .finish
        .as_ref()
        .ok_or(ClientEncodeError::MissingFinish)?;
    let stop_reason = match finish {
        FinishReason::Stop => "end_turn".to_owned(),
        FinishReason::Length => "max_tokens".to_owned(),
        FinishReason::ToolCalls => "tool_use".to_owned(),
        FinishReason::ContentFilter => "refusal".to_owned(),
        FinishReason::Error => "error".to_owned(),
        FinishReason::Other(value) => value.clone(),
    };
    let usage = aggregate.usage.unwrap_or_default();
    if usage.reasoning_tokens.is_some() {
        return Err(ClientEncodeError::ReasoningUsage);
    }
    let response = MessagesResponse {
        id: aggregate
            .response_id
            .unwrap_or_else(|| fallback_id.to_owned()),
        kind: "message".to_owned(),
        role: Role::Assistant,
        content,
        model: public_model.to_owned(),
        stop_reason: Some(stop_reason),
        stop_sequence: None,
        usage: Usage {
            input_tokens: usage
                .input_tokens
                .saturating_sub(usage.cached_input_tokens.unwrap_or(0)),
            output_tokens: usage.output_tokens,
            cache_creation_input_tokens: None,
            cache_read_input_tokens: usage.cached_input_tokens,
            extra: BTreeMap::new(),
        },
        extra: BTreeMap::new(),
    };
    apply_extensions(response, &aggregate.extensions)
}

fn apply_extensions(
    response: MessagesResponse,
    extensions: &BTreeMap<String, Value>,
) -> Result<MessagesResponse, ClientEncodeError> {
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
    fn encodes_messages_response_and_restores_same_surface_extensions() {
        let events = vec![
            CanonicalEvent::new(
                0,
                CanonicalEventKind::ResponseStart {
                    response_id: Some("msg_1".into()),
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
                CanonicalEventKind::SourceExtension {
                    extensions: SourceExtensions::new(
                        Surface::Anthropic,
                        [("/vendor_flag".into(), Value::Bool(true))].into(),
                    ),
                },
            ),
            CanonicalEvent::new(
                4,
                CanonicalEventKind::Usage {
                    usage: CanonicalUsage {
                        input_tokens: 5,
                        output_tokens: 1,
                        total_tokens: 6,
                        cached_input_tokens: None,
                        reasoning_tokens: None,
                    },
                },
            ),
            CanonicalEvent::new(
                5,
                CanonicalEventKind::Finish {
                    output_index: 0,
                    reason: FinishReason::Stop,
                },
            ),
            CanonicalEvent::new(6, CanonicalEventKind::Done),
        ];
        let response = encode_messages_response(&events, "route", "fallback").unwrap();
        assert_eq!(response.model, "route");
        assert_eq!(response.extra["vendor_flag"], true);
        assert_eq!(response.usage.input_tokens, 5);
    }

    #[test]
    fn rejects_cross_protocol_extensions() {
        let events = vec![
            CanonicalEvent::new(
                0,
                CanonicalEventKind::SourceExtension {
                    extensions: SourceExtensions::new(
                        Surface::OpenAi,
                        [("/field".into(), Value::Bool(true))].into(),
                    ),
                },
            ),
            CanonicalEvent::new(1, CanonicalEventKind::Done),
        ];
        assert!(matches!(
            encode_messages_response(&events, "route", "fallback"),
            Err(ClientEncodeError::Aggregate(
                AggregateError::CrossProtocolExtensions
            ))
        ));
    }
}

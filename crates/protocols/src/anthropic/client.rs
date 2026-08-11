use std::collections::BTreeMap;

use olp_domain::{CanonicalEvent, Surface};
use serde_json::Value;
use thiserror::Error;

use crate::client::{AggregateError, aggregate_generation};
use crate::extensions::{PointerExtensionError, apply_response_extensions};

use super::{ContentBlock, MessagesResponse, Role, TextBlock, ToolUseBlock, Usage, finish_reason};

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
    let stop_reason = finish_reason(finish).to_owned();
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
    apply_response_extensions(response, extensions).map_err(|error| match error {
        PointerExtensionError::InvalidPath(_) => ClientEncodeError::Extension,
        PointerExtensionError::Json(error) => ClientEncodeError::Json(error),
    })
}

#[cfg(test)]
mod tests {
    use olp_domain::{
        CanonicalEventKind, FinishReason, MessageRole, SourceExtensions, Usage as CanonicalUsage,
    };

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

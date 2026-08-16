use std::collections::BTreeMap;

use crate::domain::canonical::{events::Event, identity::Surface};
use serde_json::Value;
use thiserror::Error;

use crate::protocols::client::{AggregateError, aggregate_generation};
use crate::protocols::extensions::{PointerExtensionError, apply_response_extensions};

use super::{
    dto::{ContentBlock, MessagesResponse, Role, TextBlock, ToolUseBlock, Usage},
    finish_reason,
};

#[derive(Debug, Error)]
pub enum Error {
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
    events: &[Event],
    public_model: &str,
    fallback_id: &str,
) -> Result<MessagesResponse, Error> {
    let aggregate = aggregate_generation(events, Surface::Anthropic)?;
    if aggregate.outputs.len() != 1 || !aggregate.outputs.contains_key(&0) {
        return Err(Error::CandidateCount);
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
        let id = tool.id.clone().ok_or(Error::IncompleteTool)?;
        let name = tool.name.clone().ok_or(Error::IncompleteTool)?;
        let input = serde_json::from_str(&tool.arguments).map_err(Error::ToolJson)?;
        content.push(ContentBlock::ToolUse(ToolUseBlock {
            kind: "tool_use".to_owned(),
            id,
            name,
            input,
            extra: BTreeMap::new(),
        }));
    }
    let finish = output.finish.as_ref().ok_or(Error::MissingFinish)?;
    let stop_reason = finish_reason(finish).to_owned();
    let usage = aggregate.usage.unwrap_or_default();
    if usage.reasoning_tokens.is_some() {
        return Err(Error::ReasoningUsage);
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
) -> Result<MessagesResponse, Error> {
    apply_response_extensions(response, extensions).map_err(|error| match error {
        PointerExtensionError::InvalidPath(_) => Error::Extension,
        PointerExtensionError::Json(error) => Error::Json(error),
    })
}

#[cfg(test)]
mod tests {
    use crate::domain::canonical::{
        events::{FinishReason, Kind, Usage as CanonicalUsage},
        requests::{MessageRole, SourceExtensions},
    };

    use super::*;

    #[test]
    fn encodes_messages_response_and_restores_same_surface_extensions() {
        let events = vec![
            Event::new(
                0,
                Kind::ResponseStart {
                    response_id: Some("msg_1".into()),
                    provider_model: Some("provider-model".into()),
                },
            ),
            Event::new(
                1,
                Kind::MessageStart {
                    output_index: 0,
                    role: MessageRole::Assistant,
                },
            ),
            Event::new(
                2,
                Kind::TextDelta {
                    output_index: 0,
                    text: "hello".into(),
                },
            ),
            Event::new(
                3,
                Kind::SourceExtension {
                    extensions: SourceExtensions::new(
                        Surface::Anthropic,
                        [("/vendor_flag".into(), Value::Bool(true))].into(),
                    ),
                },
            ),
            Event::new(
                4,
                Kind::Usage {
                    usage: CanonicalUsage {
                        input_tokens: 5,
                        output_tokens: 1,
                        total_tokens: 6,
                        cached_input_tokens: None,
                        reasoning_tokens: None,
                    },
                },
            ),
            Event::new(
                5,
                Kind::Finish {
                    output_index: 0,
                    reason: FinishReason::Stop,
                },
            ),
            Event::new(6, Kind::Done),
        ];
        let response = encode_messages_response(&events, "route", "fallback").unwrap();
        assert_eq!(response.model, "route");
        assert_eq!(response.extra["vendor_flag"], true);
        assert_eq!(response.usage.input_tokens, 5);
    }

    #[test]
    fn rejects_cross_protocol_extensions() {
        let events = vec![
            Event::new(
                0,
                Kind::SourceExtension {
                    extensions: SourceExtensions::new(
                        Surface::OpenAi,
                        [("/field".into(), Value::Bool(true))].into(),
                    ),
                },
            ),
            Event::new(1, Kind::Done),
        ];
        assert!(matches!(
            encode_messages_response(&events, "route", "fallback"),
            Err(Error::Aggregate(AggregateError::CrossProtocolExtensions))
        ));
    }
}

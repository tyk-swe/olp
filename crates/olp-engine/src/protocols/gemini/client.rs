use std::collections::BTreeMap;

use crate::domain::{canonical::events::Event, canonical::identity::Surface};
use serde_json::Value;
use thiserror::Error;

use crate::protocols::client::{AggregateError, aggregate_generation};
use crate::protocols::extensions::{PointerExtensionError, apply_response_extensions};

use super::{
    dto::{
        Candidate, Content, FunctionCall, FunctionCallPart, GenerateContentResponse, Part,
        TextPart, UsageMetadata,
    },
    finish_reason,
};

#[derive(Debug, Error)]
pub enum Error {
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
    events: &[Event],
    public_model: &str,
    fallback_id: &str,
) -> Result<GenerateContentResponse, Error> {
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
            let name = tool.name.ok_or(Error::IncompleteTool)?;
            let args = serde_json::from_str(&tool.arguments).map_err(Error::ToolJson)?;
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
            .map(finish_reason)
            .ok_or(Error::MissingFinish)?;
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

fn apply_extensions(
    response: GenerateContentResponse,
    extensions: &BTreeMap<String, Value>,
) -> Result<GenerateContentResponse, Error> {
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
    fn encodes_multiple_candidates_usage_and_safety_extensions() {
        let events = vec![
            Event::new(
                0,
                Kind::ResponseStart {
                    response_id: Some("response-1".into()),
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
                Kind::Finish {
                    output_index: 0,
                    reason: FinishReason::Stop,
                },
            ),
            Event::new(
                4,
                Kind::Usage {
                    usage: CanonicalUsage {
                        input_tokens: 3,
                        output_tokens: 2,
                        total_tokens: 5,
                        cached_input_tokens: Some(1),
                        reasoning_tokens: None,
                    },
                },
            ),
            Event::new(
                5,
                Kind::SourceExtension {
                    extensions: SourceExtensions::new(
                        Surface::Gemini,
                        [("/vendorFlag".into(), Value::Bool(true))].into(),
                    ),
                },
            ),
            Event::new(6, Kind::Done),
        ];
        let response = encode_generate_content_response(&events, "route", "fallback").unwrap();
        assert_eq!(response.model_version.as_deref(), Some("route"));
        assert_eq!(response.extra["vendorFlag"], true);
        assert_eq!(response.usage_metadata.unwrap().total_token_count, 5);
    }
}

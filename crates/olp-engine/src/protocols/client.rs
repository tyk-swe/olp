use std::collections::BTreeMap;

use crate::domain::canonical::{
    events::{Event, FinishReason, Kind, Usage, validate_event_sequence},
    identity::Surface,
    requests::MessageRole,
};
use serde_json::Value;
use thiserror::Error;

pub(in crate::protocols) struct AggregatedGeneration {
    pub response_id: Option<String>,
    pub provider_model: Option<String>,
    pub outputs: BTreeMap<u32, AggregatedOutput>,
    pub usage: Option<Usage>,
    pub extensions: BTreeMap<String, Value>,
}

#[derive(Default)]
pub(in crate::protocols) struct AggregatedOutput {
    pub text: String,
    pub refusal: String,
    pub tools: BTreeMap<u32, AggregatedTool>,
    pub finish: Option<FinishReason>,
}

#[derive(Default)]
pub(in crate::protocols) struct AggregatedTool {
    pub id: Option<String>,
    pub name: Option<String>,
    pub arguments: String,
}

#[derive(Debug, Error)]
pub enum AggregateError {
    #[error("canonical event sequence is invalid")]
    Sequence,
    #[error("canonical stream did not terminate")]
    MissingDone,
    #[error("canonical stream contains an upstream error")]
    Upstream,
    #[error("canonical output role is not assistant")]
    Role,
    #[error("canonical refusal output is not representable")]
    Refusal,
    #[error("canonical source extensions came from a different protocol")]
    CrossProtocolExtensions,
    #[error("canonical source extension paths collide")]
    ExtensionCollision,
}

pub(in crate::protocols) fn aggregate_generation(
    events: &[Event],
    target: Surface,
) -> Result<AggregatedGeneration, AggregateError> {
    validate_event_sequence(events).map_err(|_| AggregateError::Sequence)?;
    if !matches!(events.last().map(|event| &event.kind), Some(Kind::Done)) {
        return Err(AggregateError::MissingDone);
    }
    let mut aggregate = AggregatedGeneration {
        response_id: None,
        provider_model: None,
        outputs: BTreeMap::new(),
        usage: None,
        extensions: BTreeMap::new(),
    };
    for event in events {
        match &event.kind {
            Kind::ResponseStart {
                response_id,
                provider_model,
            } => {
                aggregate.response_id.clone_from(response_id);
                aggregate.provider_model.clone_from(provider_model);
            }
            Kind::MessageStart { output_index, role } => {
                if *role != MessageRole::Assistant {
                    return Err(AggregateError::Role);
                }
                aggregate.outputs.entry(*output_index).or_default();
            }
            Kind::TextDelta { output_index, text } => {
                aggregate
                    .outputs
                    .entry(*output_index)
                    .or_default()
                    .text
                    .push_str(text);
            }
            Kind::RefusalDelta { output_index, text } => {
                if target != Surface::OpenAi {
                    return Err(AggregateError::Refusal);
                }
                aggregate
                    .outputs
                    .entry(*output_index)
                    .or_default()
                    .refusal
                    .push_str(text);
            }
            Kind::ToolCallDelta {
                output_index,
                tool_index,
                id,
                name,
                arguments_delta,
            } => {
                let tool = aggregate
                    .outputs
                    .entry(*output_index)
                    .or_default()
                    .tools
                    .entry(*tool_index)
                    .or_default();
                if id.is_some() {
                    tool.id.clone_from(id);
                }
                if name.is_some() {
                    tool.name.clone_from(name);
                }
                tool.arguments.push_str(arguments_delta);
            }
            Kind::Usage { usage } => aggregate.usage = Some(*usage),
            Kind::Finish {
                output_index,
                reason,
            } => {
                aggregate.outputs.entry(*output_index).or_default().finish = Some(reason.clone());
            }
            Kind::Error { .. } => return Err(AggregateError::Upstream),
            Kind::SourceExtension { extensions } => {
                if extensions.source != Some(target) {
                    return Err(AggregateError::CrossProtocolExtensions);
                }
                for (path, value) in &extensions.values {
                    if aggregate
                        .extensions
                        .insert(path.clone(), value.clone())
                        .is_some()
                    {
                        return Err(AggregateError::ExtensionCollision);
                    }
                }
            }
            Kind::Done => {}
        }
    }
    Ok(aggregate)
}

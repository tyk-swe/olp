use std::collections::BTreeMap;

use crate::domain::{
    CanonicalEvent, CanonicalEventKind, FinishReason, MessageRole, SourceExtensions, Surface,
    Usage as CanonicalUsage, UsageObservation,
};
use serde_json::Value;

use super::super::dto::{ContentBlock, MessagesResponse, Role};
use super::errors::ResponseError;
use super::extensions::{collect_extra, require_response_kind};
use crate::protocols::CanonicalEventBuilder as EventBuilder;

pub fn decode_messages_response(
    response: MessagesResponse,
) -> Result<Vec<CanonicalEvent>, ResponseError> {
    if response.role != Role::Assistant {
        return Err(ResponseError::UnexpectedRole);
    }
    if response.kind != "message" {
        return Err(ResponseError::UnexpectedType(response.kind));
    }
    let mut builder = EventBuilder::default();
    builder.push(CanonicalEventKind::ResponseStart {
        response_id: Some(response.id),
        provider_model: Some(response.model),
    });
    builder.push(CanonicalEventKind::MessageStart {
        output_index: 0,
        role: MessageRole::Assistant,
    });
    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);
    let mut tool_index = 0_u32;
    for (index, block) in response.content.into_iter().enumerate() {
        match block {
            ContentBlock::Text(block) => {
                require_response_kind(&block.kind, "text")?;
                collect_extra(&format!("/content/{index}"), &block.extra, &mut extensions);
                builder.push(CanonicalEventKind::TextDelta {
                    output_index: 0,
                    text: block.text,
                });
            }
            ContentBlock::ToolUse(block) => {
                require_response_kind(&block.kind, "tool_use")?;
                collect_extra(&format!("/content/{index}"), &block.extra, &mut extensions);
                builder.push(CanonicalEventKind::ToolCallDelta {
                    output_index: 0,
                    tool_index,
                    id: Some(block.id),
                    name: Some(block.name),
                    arguments_delta: serde_json::to_string(&block.input)
                        .map_err(ResponseError::Json)?,
                });
                tool_index = tool_index
                    .checked_add(1)
                    .ok_or(ResponseError::TooManyContentBlocks)?;
            }
            other => {
                extensions.insert(format!("/content/{index}"), other.as_value());
            }
        }
    }
    collect_usage_extensions(&response.usage, &mut extensions);
    if let Some(stop_sequence) = response.stop_sequence {
        extensions.insert("/stop_sequence".into(), Value::String(stop_sequence));
    }
    if !extensions.is_empty() {
        builder.push(CanonicalEventKind::SourceExtension {
            extensions: SourceExtensions::new(Surface::Anthropic, extensions),
        });
    }
    let usage_observation = if let Some(usage) = canonical_usage(&response.usage)? {
        builder.push(CanonicalEventKind::Usage { usage });
        None
    } else {
        Some(usage_observation_for(&response.usage)?)
    };
    let stop_reason = response
        .stop_reason
        .ok_or(ResponseError::MissingStopReason)?;
    builder.push(CanonicalEventKind::Finish {
        output_index: 0,
        reason: anthropic_finish_reason(&stop_reason),
    });
    if let Some(observation) = usage_observation {
        builder.push_with_usage_observation(CanonicalEventKind::Done, observation);
    } else {
        builder.push(CanonicalEventKind::Done);
    }
    Ok(builder.events)
}

pub(in crate::protocols) fn canonical_usage(
    usage: &super::super::dto::Usage,
) -> Result<Option<CanonicalUsage>, ResponseError> {
    for counter in [
        usage.input_tokens,
        usage.output_tokens,
        usage.cache_creation_input_tokens,
        usage.cache_read_input_tokens,
    ] {
        crate::protocols::usage::validate_counter(counter)
            .map_err(|_| ResponseError::InvalidUsage)?;
    }
    let (Some(base_input), Some(output_tokens)) = (usage.input_tokens, usage.output_tokens) else {
        return Ok(None);
    };
    let input_tokens = base_input
        .checked_add(usage.cache_creation_input_tokens.unwrap_or(0))
        .and_then(|tokens| tokens.checked_add(usage.cache_read_input_tokens.unwrap_or(0)))
        .ok_or(ResponseError::InvalidUsage)?;
    let total_tokens = input_tokens
        .checked_add(output_tokens)
        .ok_or(ResponseError::InvalidUsage)?;
    Ok(Some(CanonicalUsage {
        input_tokens,
        output_tokens,
        total_tokens,
        cached_input_tokens: usage.cache_read_input_tokens,
        reasoning_tokens: None,
    }))
}

fn usage_observation_for(
    usage: &super::super::dto::Usage,
) -> Result<UsageObservation, ResponseError> {
    for counter in [
        usage.input_tokens,
        usage.output_tokens,
        usage.cache_creation_input_tokens,
        usage.cache_read_input_tokens,
    ] {
        crate::protocols::usage::validate_counter(counter)
            .map_err(|_| ResponseError::InvalidUsage)?;
    }
    let input_tokens = usage
        .input_tokens
        .map(|base_input| {
            base_input
                .checked_add(usage.cache_creation_input_tokens.unwrap_or(0))
                .and_then(|tokens| tokens.checked_add(usage.cache_read_input_tokens.unwrap_or(0)))
                .ok_or(ResponseError::InvalidUsage)
        })
        .transpose()?;
    crate::protocols::usage::validate_counter(input_tokens)
        .map_err(|_| ResponseError::InvalidUsage)?;
    Ok(UsageObservation {
        input_tokens,
        output_tokens: usage.output_tokens,
        total_tokens: None,
        cached_input_tokens: usage.cache_read_input_tokens,
        reasoning_tokens: None,
    })
}

pub(in crate::protocols) fn collect_usage_extensions(
    usage: &super::super::dto::Usage,
    extensions: &mut BTreeMap<String, Value>,
) {
    collect_extra("/usage", &usage.extra, extensions);
    if let Some(tokens) = usage.cache_creation_input_tokens {
        extensions.insert(
            "/usage/cache_creation_input_tokens".into(),
            Value::from(tokens),
        );
    }
}

pub(in crate::protocols) fn anthropic_finish_reason(reason: &str) -> FinishReason {
    match reason {
        "end_turn" | "stop_sequence" => FinishReason::Stop,
        "max_tokens" | "model_context_window_exceeded" => FinishReason::Length,
        "tool_use" => FinishReason::ToolCalls,
        "refusal" => FinishReason::ContentFilter,
        other => FinishReason::Other(other.to_owned()),
    }
}

use std::collections::BTreeMap;

use olp_domain::{
    CanonicalEvent, CanonicalEventKind, FinishReason, MessageRole, SourceExtensions, Surface,
    Usage as CanonicalUsage,
};
use serde_json::Value;

use super::super::dto::{ContentBlock, MessagesResponse, Role};
use super::errors::ResponseError;
use super::extensions::{collect_extra, require_response_kind};

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
    builder.push(CanonicalEventKind::Usage {
        usage: canonical_usage(&response.usage),
    });
    let stop_reason = response
        .stop_reason
        .ok_or(ResponseError::MissingStopReason)?;
    builder.push(CanonicalEventKind::Finish {
        output_index: 0,
        reason: anthropic_finish_reason(&stop_reason),
    });
    builder.push(CanonicalEventKind::Done);
    Ok(builder.events)
}

pub(crate) fn canonical_usage(usage: &super::super::dto::Usage) -> CanonicalUsage {
    let input_tokens = usage
        .input_tokens
        .saturating_add(usage.cache_creation_input_tokens.unwrap_or(0))
        .saturating_add(usage.cache_read_input_tokens.unwrap_or(0));
    CanonicalUsage {
        input_tokens,
        output_tokens: usage.output_tokens,
        total_tokens: input_tokens.saturating_add(usage.output_tokens),
        cached_input_tokens: usage.cache_read_input_tokens,
        reasoning_tokens: None,
    }
}

pub(crate) fn collect_usage_extensions(
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

pub(crate) fn anthropic_finish_reason(reason: &str) -> FinishReason {
    match reason {
        "end_turn" | "stop_sequence" => FinishReason::Stop,
        "max_tokens" | "model_context_window_exceeded" => FinishReason::Length,
        "tool_use" => FinishReason::ToolCalls,
        "refusal" => FinishReason::ContentFilter,
        other => FinishReason::Other(other.to_owned()),
    }
}

#[derive(Default)]
pub(crate) struct EventBuilder {
    pub(crate) events: Vec<CanonicalEvent>,
}

impl EventBuilder {
    pub(crate) fn push(&mut self, kind: CanonicalEventKind) {
        let sequence = self.events.len().try_into().unwrap_or(u64::MAX);
        self.events.push(CanonicalEvent::new(sequence, kind));
    }
}

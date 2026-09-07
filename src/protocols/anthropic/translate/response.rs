use std::collections::BTreeMap;

use crate::protocols::canonical::events::Event;
use crate::protocols::canonical::events::FinishReason;
use crate::protocols::canonical::events::Kind;
use crate::protocols::canonical::events::Usage as CanonicalUsage;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::requests::MessageRole;
use crate::protocols::canonical::requests::SourceExtensions;
use serde_json::Value;

use crate::protocols::CanonicalEventBuilder as EventBuilder;
use crate::protocols::anthropic::dto::ContentBlock;
use crate::protocols::anthropic::dto::MessagesResponse;
use crate::protocols::anthropic::dto::Role;
use crate::protocols::anthropic::translate::errors::ResponseError;
use crate::protocols::anthropic::translate::extensions::require_response_kind;
use crate::protocols::extensions::collect_extra;

pub fn decode(response: MessagesResponse) -> Result<Vec<Event>, ResponseError> {
    if response.role != Role::Assistant {
        return Err(ResponseError::UnexpectedRole);
    }
    if response.kind != "message" {
        return Err(ResponseError::UnexpectedType(response.kind));
    }
    let mut builder = EventBuilder::default();
    builder.push(Kind::ResponseStart {
        response_id: Some(response.id),
        provider_model: Some(response.model),
    });
    builder.push(Kind::MessageStart {
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
                builder.push(Kind::TextDelta {
                    output_index: 0,
                    text: block.text,
                });
            }
            ContentBlock::ToolUse(block) => {
                require_response_kind(&block.kind, "tool_use")?;
                collect_extra(&format!("/content/{index}"), &block.extra, &mut extensions);
                builder.push(Kind::ToolCallDelta {
                    output_index: 0,
                    tool_index,
                    id: Some(block.id),
                    name: Some(block.name),
                    arguments_delta: serde_json::to_string(&block.input)?,
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
        builder.push(Kind::SourceExtension {
            extensions: SourceExtensions::new(Surface::Anthropic, extensions),
        });
    }
    builder.push(Kind::Usage {
        usage: canonical_usage(&response.usage),
    });
    let stop_reason = response
        .stop_reason
        .ok_or(ResponseError::MissingStopReason)?;
    builder.push(Kind::Finish {
        output_index: 0,
        reason: anthropic_finish_reason(&stop_reason),
    });
    builder.push(Kind::Done);
    Ok(builder.events)
}

pub(crate) fn canonical_usage(usage: &crate::protocols::anthropic::dto::Usage) -> CanonicalUsage {
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
    usage: &crate::protocols::anthropic::dto::Usage,
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

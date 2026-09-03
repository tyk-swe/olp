use std::collections::BTreeMap;

use axum::body::Bytes;
use olp_engine::{
    domain::canonical::{
        events::{Event, Kind},
        identity::Surface,
    },
    protocols::{
        materialize_response_pointer,
        openai::chat::client::{ChatCompletionStreamEncoder, finish_name},
        sse::encode_frame,
    },
};
use serde_json::{Value, json};

use super::{error::InferenceError, openai_http::unix_seconds};

pub(crate) struct OpenAiChatCompletionStreamEncoder {
    inner: ChatCompletionStreamEncoder,
}

impl OpenAiChatCompletionStreamEncoder {
    pub(crate) fn new(request_id: uuid::Uuid, model: &str, include_usage: bool) -> Self {
        Self {
            inner: ChatCompletionStreamEncoder::new(
                request_id,
                model,
                include_usage,
                unix_seconds(),
            ),
        }
    }

    pub(crate) fn encode(&mut self, event: Event) -> Result<Vec<Bytes>, InferenceError> {
        self.inner
            .push(event)
            .map_err(|error| {
                InferenceError::bad_gateway("provider_protocol_error", error.to_string())
            })?
            .iter()
            .map(|frame| {
                encode_frame(frame).map(Bytes::from).map_err(|error| {
                    InferenceError::bad_gateway("provider_protocol_error", error.to_string())
                })
            })
            .collect()
    }
}

#[derive(Default)]
struct UnaryChoice {
    content: String,
    refusal: String,
    tools: BTreeMap<u32, UnaryTool>,
    finish_reason: Option<String>,
}

#[derive(Default)]
struct UnaryTool {
    id: String,
    name: String,
    arguments: String,
}

pub(crate) fn aggregate_chat_completion_response(
    request_id: uuid::Uuid,
    model: &str,
    events: &[Event],
) -> Result<Value, InferenceError> {
    let mut id = format!("chatcmpl-{request_id}");
    let mut choices: BTreeMap<u32, UnaryChoice> = BTreeMap::new();
    let mut usage = None;
    let mut extensions = Vec::new();
    for event in events {
        match &event.kind {
            Kind::ResponseStart { response_id, .. } => {
                if let Some(response_id) = response_id {
                    id.clone_from(response_id);
                }
            }
            Kind::MessageStart { output_index, .. } => {
                choices.entry(*output_index).or_default();
            }
            Kind::TextDelta { output_index, text } => {
                choices
                    .entry(*output_index)
                    .or_default()
                    .content
                    .push_str(text);
            }
            Kind::RefusalDelta { output_index, text } => {
                choices
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
                let tool = choices
                    .entry(*output_index)
                    .or_default()
                    .tools
                    .entry(*tool_index)
                    .or_default();
                if let Some(id) = id {
                    tool.id.clone_from(id);
                }
                if let Some(name) = name {
                    tool.name.clone_from(name);
                }
                tool.arguments.push_str(arguments_delta);
            }
            Kind::Finish {
                output_index,
                reason,
            } => {
                choices.entry(*output_index).or_default().finish_reason =
                    Some(finish_name(reason.clone()).into_owned());
            }
            Kind::Usage { usage: value } => {
                usage = Some(json!({
                    "prompt_tokens": value.input_tokens,
                    "completion_tokens": value
                        .output_tokens
                        .saturating_add(value.reasoning_tokens.unwrap_or(0)),
                    "total_tokens": value.total_tokens,
                    "prompt_tokens_details": { "cached_tokens": value.cached_input_tokens },
                    "completion_tokens_details": { "reasoning_tokens": value.reasoning_tokens }
                }));
            }
            Kind::SourceExtension { extensions: values } => {
                if values.source != Some(Surface::OpenAi) {
                    continue;
                }
                extensions.extend(
                    values
                        .values
                        .iter()
                        .map(|(key, value)| (key.clone(), value.clone())),
                );
            }
            Kind::Error { error } => {
                return Err(InferenceError::from_canonical(error));
            }
            Kind::Done => {}
        }
    }
    let choices = choices
        .into_iter()
        .map(|(index, choice)| {
            let tools = choice
                .tools
                .into_values()
                .map(|tool| {
                    json!({
                        "id": tool.id,
                        "type": "function",
                        "function": { "name": tool.name, "arguments": tool.arguments }
                    })
                })
                .collect::<Vec<_>>();
            let mut message = json!({
                "role": "assistant",
                "content": (!choice.content.is_empty()).then_some(choice.content),
                "refusal": (!choice.refusal.is_empty()).then_some(choice.refusal),
            });
            if !tools.is_empty()
                && let Some(object) = message.as_object_mut()
            {
                object.insert("tool_calls".to_owned(), Value::from(tools));
            }
            json!({
                "index": index,
                "message": message,
                "finish_reason": choice.finish_reason
            })
        })
        .collect::<Vec<_>>();
    let mut response = json!({
        "id": id,
        "object": "chat.completion",
        "created": unix_seconds(),
        "model": model,
        "choices": choices,
        "usage": usage
    });
    for (pointer, value) in extensions {
        materialize_response_pointer(&mut response, &pointer, value).map_err(|_| {
            InferenceError::bad_gateway(
                "provider_protocol_error",
                format!("The provider extension path {pointer} is not representable."),
            )
        })?;
    }
    Ok(response)
}

#[cfg(test)]
mod tests;

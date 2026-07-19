use std::collections::{BTreeMap, BTreeSet};

use olp_domain::{CanonicalEvent, CanonicalEventKind, FinishReason, Surface};
use serde_json::{Value, json};
use thiserror::Error;

use crate::client::{AggregateError, aggregate_generation};
use crate::sse::SseFrame;

use super::extensions::apply_pointer_extensions;
use super::responses::OPENAI_RESPONSES_RAW_OUTPUT_PREFIX;
use super::{
    ChatCompletionChoice, ChatCompletionResponse, ChatFunctionCall, ChatResponseMessage, ChatRole,
    ChatToolCall, ChatUsage, CompletionTokenDetails, PromptTokenDetails, ResponseErrorBody,
    ResponseInputTokenDetails, ResponseObject, ResponseOutputTokenDetails, ResponseUsage,
};

pub fn encode_response_object(
    events: &[CanonicalEvent],
    client_model: &str,
    fallback_id: &str,
) -> Result<ResponseObject, OpenAiClientEncodeError> {
    let mut aggregate = aggregate_generation(events, Surface::OpenAi)?;
    let raw_output = take_raw_response_output(&mut aggregate.extensions)?;
    let mut output = Vec::new();
    for (output_index, item) in aggregate.outputs {
        if !item.text.is_empty() || !item.refusal.is_empty() || item.tools.is_empty() {
            let mut content = Vec::new();
            if !item.text.is_empty() {
                let annotations = aggregate
                    .extensions
                    .remove(&format!("/output/{output_index}/content/0/annotations"))
                    .unwrap_or_else(|| json!([]));
                content.push(json!({
                    "type": "output_text",
                    "text": item.text,
                    "annotations": annotations,
                }));
            }
            if !item.refusal.is_empty() {
                content.push(json!({"type": "refusal", "refusal": item.refusal}));
            }
            let id = take_string_extension(
                &mut aggregate.extensions,
                &format!("/output/{output_index}/id"),
            )
            .unwrap_or_else(|| format!("msg_{output_index}"));
            let status = take_string_extension(
                &mut aggregate.extensions,
                &format!("/output/{output_index}/status"),
            )
            .unwrap_or_else(|| "completed".into());
            output.push(json!({
                "id": id,
                "type": "message",
                "role": "assistant",
                "status": status,
                "content": content,
            }));
        }
        for (_, tool) in item.tools {
            let id = tool
                .id
                .ok_or(OpenAiClientEncodeError::IncompleteToolCall("id"))?;
            let name = tool
                .name
                .ok_or(OpenAiClientEncodeError::IncompleteToolCall("name"))?;
            let wire_id = take_string_extension(
                &mut aggregate.extensions,
                &format!("/output/{output_index}/id"),
            )
            .unwrap_or_else(|| format!("fc_{output_index}"));
            let status = take_string_extension(
                &mut aggregate.extensions,
                &format!("/output/{output_index}/status"),
            )
            .unwrap_or_else(|| "completed".into());
            output.push(json!({
                "id": wire_id,
                "type": "function_call",
                "call_id": id,
                "name": name,
                "arguments": tool.arguments,
                "status": status,
            }));
        }
    }
    for (index, item) in raw_output {
        if index > output.len() {
            return Err(OpenAiClientEncodeError::InvalidExtension(format!(
                "{OPENAI_RESPONSES_RAW_OUTPUT_PREFIX}/{index}"
            )));
        }
        output.insert(index, item);
    }
    let created_at = take_i64_extension(&mut aggregate.extensions, "/created_at").unwrap_or(0);
    let status = take_string_extension(&mut aggregate.extensions, "/status")
        .unwrap_or_else(|| "completed".into());
    let incomplete_details = aggregate.extensions.remove("/incomplete_details");
    let usage = aggregate.usage.map(|usage| ResponseUsage {
        input_tokens: usage.input_tokens,
        output_tokens: usage.output_tokens,
        total_tokens: usage.total_tokens,
        input_tokens_details: usage.cached_input_tokens.map(|cached_tokens| {
            ResponseInputTokenDetails {
                cached_tokens,
                extra: BTreeMap::new(),
            }
        }),
        output_tokens_details: usage.reasoning_tokens.map(|reasoning_tokens| {
            ResponseOutputTokenDetails {
                reasoning_tokens,
                extra: BTreeMap::new(),
            }
        }),
        extra: BTreeMap::new(),
    });
    apply_pointer_extensions(
        ResponseObject {
            id: aggregate.response_id.unwrap_or_else(|| fallback_id.into()),
            object: "response".into(),
            created_at,
            status,
            model: client_model.into(),
            output,
            usage,
            error: None::<ResponseErrorBody>,
            incomplete_details,
            extra: BTreeMap::new(),
        },
        &aggregate.extensions,
    )
    .map_err(OpenAiClientEncodeError::InvalidExtension)
}

pub fn encode_chat_completion_client_response(
    events: &[CanonicalEvent],
    client_model: &str,
    fallback_id: &str,
    created_at: i64,
) -> Result<ChatCompletionResponse, OpenAiClientEncodeError> {
    let aggregate = aggregate_generation(events, Surface::OpenAi)?;
    let choices = aggregate
        .outputs
        .into_iter()
        .map(|(index, item)| {
            let tool_calls = item
                .tools
                .into_values()
                .map(|tool| {
                    Ok::<_, OpenAiClientEncodeError>(ChatToolCall {
                        id: tool
                            .id
                            .ok_or(OpenAiClientEncodeError::IncompleteToolCall("id"))?,
                        kind: "function".into(),
                        function: ChatFunctionCall {
                            name: tool
                                .name
                                .ok_or(OpenAiClientEncodeError::IncompleteToolCall("name"))?,
                            arguments: tool.arguments,
                            extra: BTreeMap::new(),
                        },
                        extra: BTreeMap::new(),
                    })
                })
                .collect::<Result<Vec<_>, _>>()?;
            let finish_reason = item.finish.as_ref().map(|reason| match reason {
                FinishReason::Stop => "stop".into(),
                FinishReason::Length => "length".into(),
                FinishReason::ToolCalls => "tool_calls".into(),
                FinishReason::ContentFilter => "content_filter".into(),
                FinishReason::Error => "stop".into(),
                FinishReason::Other(value) => value.clone(),
            });
            Ok(ChatCompletionChoice {
                index,
                message: ChatResponseMessage {
                    role: ChatRole::Assistant,
                    content: (!item.text.is_empty()).then_some(item.text),
                    refusal: (!item.refusal.is_empty()).then_some(item.refusal),
                    tool_calls,
                    extra: BTreeMap::new(),
                },
                finish_reason,
                extra: BTreeMap::new(),
            })
        })
        .collect::<Result<Vec<_>, OpenAiClientEncodeError>>()?;
    let usage = aggregate.usage.map(|usage| ChatUsage {
        prompt_tokens: usage.input_tokens,
        completion_tokens: usage.output_tokens,
        total_tokens: usage.total_tokens,
        prompt_tokens_details: usage
            .cached_input_tokens
            .map(|cached_tokens| PromptTokenDetails {
                cached_tokens: Some(cached_tokens),
                extra: BTreeMap::new(),
            }),
        completion_tokens_details: usage.reasoning_tokens.map(|reasoning_tokens| {
            CompletionTokenDetails {
                reasoning_tokens: Some(reasoning_tokens),
                extra: BTreeMap::new(),
            }
        }),
        extra: BTreeMap::new(),
    });
    apply_pointer_extensions(
        ChatCompletionResponse {
            id: aggregate.response_id.unwrap_or_else(|| fallback_id.into()),
            object: "chat.completion".into(),
            created: created_at,
            model: client_model.into(),
            choices,
            usage,
            extra: BTreeMap::new(),
        },
        &aggregate.extensions,
    )
    .map_err(OpenAiClientEncodeError::InvalidExtension)
}

pub struct OpenAiResponsesStreamEncoder {
    client_model: String,
    fallback_id: String,
    created_at: i64,
    next_sequence: u64,
    events: Vec<CanonicalEvent>,
    emitted_outputs: BTreeSet<u32>,
    tool_outputs: BTreeSet<u32>,
    done: bool,
}

impl std::fmt::Debug for OpenAiResponsesStreamEncoder {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("OpenAiResponsesStreamEncoder")
            .field("next_sequence", &self.next_sequence)
            .field("emitted_output_count", &self.emitted_outputs.len())
            .field("done", &self.done)
            .finish_non_exhaustive()
    }
}

impl OpenAiResponsesStreamEncoder {
    #[must_use]
    pub fn new(
        client_model: impl Into<String>,
        fallback_id: impl Into<String>,
        created_at: i64,
    ) -> Self {
        Self {
            client_model: client_model.into(),
            fallback_id: fallback_id.into(),
            created_at,
            next_sequence: 0,
            events: Vec::new(),
            emitted_outputs: BTreeSet::new(),
            tool_outputs: BTreeSet::new(),
            done: false,
        }
    }

    pub fn push(
        &mut self,
        event: CanonicalEvent,
    ) -> Result<Vec<SseFrame>, OpenAiClientEncodeError> {
        if self.done {
            return Err(OpenAiClientEncodeError::DataAfterDone);
        }
        if event.sequence != self.next_sequence {
            return Err(OpenAiClientEncodeError::OutOfOrder {
                expected: self.next_sequence,
                actual: event.sequence,
            });
        }
        self.next_sequence = self.next_sequence.saturating_add(1);
        let mut frames = Vec::new();
        match &event.kind {
            CanonicalEventKind::ResponseStart {
                response_id,
                provider_model: _,
            } => {
                let id = response_id.as_deref().unwrap_or(&self.fallback_id);
                frames.push(response_sse_frame(
                    "response.created",
                    json!({
                        "response": {
                            "id": id,
                            "object": "response",
                            "created_at": self.created_at,
                            "status": "in_progress",
                            "model": self.client_model,
                            "output": []
                        }
                    }),
                )?);
            }
            CanonicalEventKind::MessageStart { .. } => {}
            CanonicalEventKind::TextDelta { output_index, text } => {
                self.ensure_stream_output(*output_index, false, &mut frames)?;
                frames.push(response_sse_frame(
                    "response.output_text.delta",
                    json!({"output_index": output_index, "content_index": 0, "delta": text}),
                )?);
            }
            CanonicalEventKind::RefusalDelta { output_index, text } => {
                self.ensure_stream_output(*output_index, false, &mut frames)?;
                frames.push(response_sse_frame(
                    "response.refusal.delta",
                    json!({"output_index": output_index, "content_index": 0, "delta": text}),
                )?);
            }
            CanonicalEventKind::ToolCallDelta {
                output_index,
                id,
                name,
                arguments_delta,
                ..
            } => {
                self.ensure_stream_output(*output_index, true, &mut frames)?;
                frames.push(response_sse_frame(
                    "response.function_call_arguments.delta",
                    json!({
                        "output_index": output_index,
                        "item_id": id,
                        "name": name,
                        "delta": arguments_delta
                    }),
                )?);
            }
            CanonicalEventKind::Finish { output_index, .. } => {
                self.ensure_stream_output(
                    *output_index,
                    self.tool_outputs.contains(output_index),
                    &mut frames,
                )?;
                frames.push(response_sse_frame(
                    "response.output_item.done",
                    json!({
                        "output_index": output_index,
                        "item": {"type": if self.tool_outputs.contains(output_index) {"function_call"} else {"message"}}
                    }),
                )?);
            }
            CanonicalEventKind::Usage { .. } => {}
            CanonicalEventKind::SourceExtension { extensions } => {
                if extensions.source != Some(Surface::OpenAi) {
                    return Err(OpenAiClientEncodeError::CrossProtocolExtensions);
                }
                for (path, value) in &extensions.values {
                    if path.starts_with("/stream/") {
                        let kind = value.get("type").and_then(Value::as_str).ok_or_else(|| {
                            OpenAiClientEncodeError::InvalidExtension(path.clone())
                        })?;
                        frames.push(response_sse_frame(kind, value.clone())?);
                    }
                }
            }
            CanonicalEventKind::Error { error } => {
                frames.push(response_sse_frame(
                    "response.failed",
                    json!({
                        "response": {
                            "id": self.fallback_id,
                            "object": "response",
                            "status": "failed",
                            "model": self.client_model,
                            "error": {"code": error.provider_code, "message": error.message}
                        }
                    }),
                )?);
            }
            CanonicalEventKind::Done => {
                let normalized = self.normalized_events_with(event.clone());
                let response =
                    encode_response_object(&normalized, &self.client_model, &self.fallback_id)?;
                frames.push(response_sse_frame(
                    "response.completed",
                    json!({"response": response}),
                )?);
                self.done = true;
            }
        }
        self.events.push(event);
        Ok(frames)
    }

    fn ensure_stream_output(
        &mut self,
        output_index: u32,
        tool: bool,
        frames: &mut Vec<SseFrame>,
    ) -> Result<(), OpenAiClientEncodeError> {
        if tool {
            self.tool_outputs.insert(output_index);
        }
        if self.emitted_outputs.insert(output_index) {
            let item = if tool {
                json!({"type": "function_call", "call_id": format!("call_{output_index}"), "name": "function", "arguments": ""})
            } else {
                json!({"type": "message", "role": "assistant", "status": "in_progress", "content": []})
            };
            frames.push(response_sse_frame(
                "response.output_item.added",
                json!({"output_index": output_index, "item": item}),
            )?);
        }
        Ok(())
    }

    fn normalized_events_with(&self, terminal: CanonicalEvent) -> Vec<CanonicalEvent> {
        self.events
            .iter()
            .chain(std::iter::once(&terminal))
            .filter(|event| {
                !matches!(
                    &event.kind,
                    CanonicalEventKind::SourceExtension { extensions }
                        if extensions.values.keys().all(|path| path.starts_with("/stream/"))
                )
            })
            .enumerate()
            .map(|(sequence, event)| {
                CanonicalEvent::new(sequence.try_into().unwrap_or(u64::MAX), event.kind.clone())
            })
            .collect()
    }
}

fn response_sse_frame(kind: &str, mut payload: Value) -> Result<SseFrame, OpenAiClientEncodeError> {
    let Value::Object(object) = &mut payload else {
        return Err(OpenAiClientEncodeError::InvalidStreamPayload);
    };
    object.insert("type".into(), Value::String(kind.into()));
    Ok(SseFrame {
        event: Some(kind.into()),
        data: serde_json::to_string(&payload)?,
        id: None,
        retry_ms: None,
    })
}

fn take_i64_extension(extensions: &mut BTreeMap<String, Value>, path: &str) -> Option<i64> {
    extensions.remove(path).and_then(|value| value.as_i64())
}

fn take_string_extension(extensions: &mut BTreeMap<String, Value>, path: &str) -> Option<String> {
    extensions
        .remove(path)
        .and_then(|value| value.as_str().map(str::to_owned))
}

fn take_raw_response_output(
    extensions: &mut BTreeMap<String, Value>,
) -> Result<Vec<(usize, Value)>, OpenAiClientEncodeError> {
    let prefix = format!("{OPENAI_RESPONSES_RAW_OUTPUT_PREFIX}/");
    let mut keys = extensions
        .keys()
        .filter(|path| path.starts_with(&prefix))
        .cloned()
        .collect::<Vec<_>>();
    keys.sort_by_key(|path| {
        path.strip_prefix(&prefix)
            .and_then(|index| index.parse::<usize>().ok())
            .unwrap_or(usize::MAX)
    });
    let mut output = Vec::with_capacity(keys.len());
    for path in keys {
        let index = path
            .strip_prefix(&prefix)
            .and_then(|index| index.parse::<usize>().ok())
            .ok_or_else(|| OpenAiClientEncodeError::InvalidExtension(path.clone()))?;
        let value = extensions
            .remove(&path)
            .ok_or_else(|| OpenAiClientEncodeError::InvalidExtension(path.clone()))?;
        output.push((index, value));
    }
    Ok(output)
}

#[derive(Debug, Error)]
pub enum OpenAiClientEncodeError {
    #[error(transparent)]
    Aggregate(#[from] AggregateError),
    #[error("canonical tool call is missing {0}")]
    IncompleteToolCall(&'static str),
    #[error("invalid source extension path: {0}")]
    InvalidExtension(String),
    #[error("canonical source extensions came from a different protocol")]
    CrossProtocolExtensions,
    #[error("expected canonical event sequence {expected}, got {actual}")]
    OutOfOrder { expected: u64, actual: u64 },
    #[error("canonical event appeared after done")]
    DataAfterDone,
    #[error("OpenAI stream payload must be an object")]
    InvalidStreamPayload,
    #[error(transparent)]
    Json(#[from] serde_json::Error),
}

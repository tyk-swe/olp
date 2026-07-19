use std::collections::{BTreeMap, BTreeSet};

use olp_domain::{
    CanonicalError, CanonicalEvent, CanonicalEventKind, ErrorClass, FinishReason, MessageRole,
    SourceExtensions, Surface, Usage,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;

use super::chat::{ChatRole, ChatToolCall};
use crate::sse::{DEFAULT_MAX_EVENT_BYTES, SseDecodeError, SseDecoder, SseFrame};

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatCompletionResponse {
    pub id: String,
    pub object: String,
    pub created: i64,
    pub model: String,
    pub choices: Vec<ChatCompletionChoice>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub usage: Option<ChatUsage>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatCompletionChoice {
    pub index: u32,
    pub message: ChatResponseMessage,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub finish_reason: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatResponseMessage {
    pub role: ChatRole,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub content: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub refusal: Option<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tool_calls: Vec<ChatToolCall>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Serialize)]
pub struct ChatUsage {
    #[serde(default)]
    pub prompt_tokens: u64,
    #[serde(default)]
    pub completion_tokens: u64,
    #[serde(default)]
    pub total_tokens: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub prompt_tokens_details: Option<PromptTokenDetails>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub completion_tokens_details: Option<CompletionTokenDetails>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Serialize)]
pub struct PromptTokenDetails {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cached_tokens: Option<u64>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Serialize)]
pub struct CompletionTokenDetails {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reasoning_tokens: Option<u64>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_chat_completion_response(
    response: ChatCompletionResponse,
) -> Result<Vec<CanonicalEvent>, OpenAiResponseError> {
    let mut builder = EventBuilder::default();
    builder.push(CanonicalEventKind::ResponseStart {
        response_id: Some(response.id),
        provider_model: Some(response.model),
    });

    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);

    for choice in response.choices {
        let prefix = format!("/choices/{}", choice.index);
        collect_extra(&prefix, &choice.extra, &mut extensions);
        collect_extra(
            &format!("{prefix}/message"),
            &choice.message.extra,
            &mut extensions,
        );
        builder.push(CanonicalEventKind::MessageStart {
            output_index: choice.index,
            role: canonical_role(choice.message.role),
        });
        if let Some(content) = choice.message.content {
            builder.push(CanonicalEventKind::TextDelta {
                output_index: choice.index,
                text: content,
            });
        }
        if let Some(refusal) = choice.message.refusal {
            builder.push(CanonicalEventKind::RefusalDelta {
                output_index: choice.index,
                text: refusal,
            });
        }
        for (tool_index, call) in choice.message.tool_calls.into_iter().enumerate() {
            if call.kind != "function" {
                return Err(OpenAiResponseError::UnsupportedToolType(call.kind));
            }
            let tool_prefix = format!("{prefix}/message/tool_calls/{tool_index}");
            collect_extra(&tool_prefix, &call.extra, &mut extensions);
            collect_extra(
                &format!("{tool_prefix}/function"),
                &call.function.extra,
                &mut extensions,
            );
            builder.push(CanonicalEventKind::ToolCallDelta {
                output_index: choice.index,
                tool_index: tool_index
                    .try_into()
                    .map_err(|_| OpenAiResponseError::TooManyToolCalls)?,
                id: Some(call.id),
                name: Some(call.function.name),
                arguments_delta: call.function.arguments,
            });
        }
        if let Some(reason) = choice.finish_reason {
            builder.push(CanonicalEventKind::Finish {
                output_index: choice.index,
                reason: finish_reason(&reason),
            });
        }
    }

    if let Some(usage) = response.usage {
        collect_usage_extensions("/usage", &usage, &mut extensions);
        builder.push(CanonicalEventKind::Usage {
            usage: canonical_usage(&usage),
        });
    }
    if !extensions.is_empty() {
        builder.push(CanonicalEventKind::SourceExtension {
            extensions: SourceExtensions::new(Surface::OpenAi, extensions),
        });
    }
    builder.push(CanonicalEventKind::Done);
    Ok(builder.events)
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatCompletionChunk {
    pub id: String,
    pub object: String,
    pub created: i64,
    pub model: String,
    #[serde(default)]
    pub choices: Vec<ChatChunkChoice>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub usage: Option<ChatUsage>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatChunkChoice {
    pub index: u32,
    #[serde(default)]
    pub delta: ChatDelta,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub finish_reason: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

impl ChatChunkChoice {
    fn is_extension_only(&self) -> bool {
        self.finish_reason.is_none()
            && self.delta.role.is_none()
            && self.delta.content.is_none()
            && self.delta.refusal.is_none()
            && self.delta.tool_calls.is_empty()
            && (!self.extra.is_empty() || !self.delta.extra.is_empty())
    }
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Serialize)]
pub struct ChatDelta {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub role: Option<ChatRole>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub content: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub refusal: Option<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tool_calls: Vec<ChatToolCallDelta>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatToolCallDelta {
    pub index: u32,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub id: Option<String>,
    #[serde(rename = "type", default, skip_serializing_if = "Option::is_none")]
    pub kind: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub function: Option<ChatFunctionCallDelta>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Serialize)]
pub struct ChatFunctionCallDelta {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub arguments: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize)]
struct OpenAiErrorEnvelope {
    error: OpenAiWireError,
}

#[derive(Clone, Debug, Deserialize)]
struct OpenAiWireError {
    message: String,
    #[serde(default)]
    code: Option<Value>,
    #[serde(rename = "type", default)]
    kind: Option<String>,
}

pub struct OpenAiChatStreamDecoder {
    sse: SseDecoder,
    sequence: u64,
    response_started: bool,
    started_choices: BTreeSet<u32>,
    finished_choices: BTreeSet<u32>,
    done: bool,
}

impl std::fmt::Debug for OpenAiChatStreamDecoder {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("OpenAiChatStreamDecoder")
            .field("next_sequence", &self.sequence)
            .field("response_started", &self.response_started)
            .field("started_choice_count", &self.started_choices.len())
            .field("finished_choice_count", &self.finished_choices.len())
            .field("done", &self.done)
            .finish_non_exhaustive()
    }
}

impl Default for OpenAiChatStreamDecoder {
    fn default() -> Self {
        Self::new()
    }
}

impl OpenAiChatStreamDecoder {
    #[must_use]
    pub fn new() -> Self {
        Self::with_max_event_bytes(DEFAULT_MAX_EVENT_BYTES)
    }

    #[must_use]
    pub fn with_max_event_bytes(max_event_bytes: usize) -> Self {
        Self {
            sse: SseDecoder::new(max_event_bytes),
            sequence: 0,
            response_started: false,
            started_choices: BTreeSet::new(),
            finished_choices: BTreeSet::new(),
            done: false,
        }
    }

    pub fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, OpenAiStreamError> {
        let frames = self.sse.push(bytes)?;
        self.decode_frames(frames)
    }

    pub fn finish(&mut self) -> Result<Vec<CanonicalEvent>, OpenAiStreamError> {
        let frames = self.sse.finish()?;
        let events = self.decode_frames(frames)?;
        if !self.done {
            return Err(OpenAiStreamError::UnexpectedEof);
        }
        Ok(events)
    }

    #[must_use]
    pub const fn is_done(&self) -> bool {
        self.done
    }

    fn decode_frames(
        &mut self,
        frames: Vec<SseFrame>,
    ) -> Result<Vec<CanonicalEvent>, OpenAiStreamError> {
        let mut events = Vec::new();
        for frame in frames {
            if self.done {
                return Err(OpenAiStreamError::DataAfterDone);
            }
            if frame.data.trim() == "[DONE]" {
                if self.started_choices.is_empty() || self.started_choices != self.finished_choices
                {
                    return Err(OpenAiStreamError::UnexpectedEof);
                }
                self.emit(&mut events, CanonicalEventKind::Done);
                self.done = true;
                continue;
            }

            let value: Value = serde_json::from_str(&frame.data)?;
            if value.get("error").is_some() {
                let envelope: OpenAiErrorEnvelope = serde_json::from_value(value)?;
                self.emit(
                    &mut events,
                    CanonicalEventKind::Error {
                        error: canonical_error(envelope.error),
                    },
                );
                self.emit(&mut events, CanonicalEventKind::Done);
                self.done = true;
                continue;
            }
            let chunk: ChatCompletionChunk = serde_json::from_value(value)?;
            self.decode_chunk(chunk, &mut events)?;
        }
        Ok(events)
    }

    fn decode_chunk(
        &mut self,
        chunk: ChatCompletionChunk,
        events: &mut Vec<CanonicalEvent>,
    ) -> Result<(), OpenAiStreamError> {
        if !self.response_started {
            self.emit(
                events,
                CanonicalEventKind::ResponseStart {
                    response_id: Some(chunk.id.clone()),
                    provider_model: Some(chunk.model.clone()),
                },
            );
            self.response_started = true;
        }

        let mut extensions = BTreeMap::new();
        collect_extra("", &chunk.extra, &mut extensions);

        for choice in chunk.choices {
            let choice_finished = self.finished_choices.contains(&choice.index);
            if choice_finished && !choice.is_extension_only() {
                return Err(OpenAiStreamError::DataAfterChoiceFinish(choice.index));
            }
            let prefix = format!("/choices/{}", choice.index);
            collect_extra(&prefix, &choice.extra, &mut extensions);
            collect_extra(
                &format!("{prefix}/delta"),
                &choice.delta.extra,
                &mut extensions,
            );
            if choice_finished {
                continue;
            }
            if self.started_choices.insert(choice.index) {
                self.emit(
                    events,
                    CanonicalEventKind::MessageStart {
                        output_index: choice.index,
                        role: choice
                            .delta
                            .role
                            .map_or(MessageRole::Assistant, canonical_role),
                    },
                );
            }
            if let Some(content) = choice.delta.content {
                self.emit(
                    events,
                    CanonicalEventKind::TextDelta {
                        output_index: choice.index,
                        text: content,
                    },
                );
            }
            if let Some(refusal) = choice.delta.refusal {
                self.emit(
                    events,
                    CanonicalEventKind::RefusalDelta {
                        output_index: choice.index,
                        text: refusal,
                    },
                );
            }
            for tool in choice.delta.tool_calls {
                if tool.kind.as_deref().is_some_and(|kind| kind != "function") {
                    return Err(OpenAiStreamError::UnsupportedToolType(
                        tool.kind.unwrap_or_default(),
                    ));
                }
                let tool_prefix = format!("{prefix}/delta/tool_calls/{}", tool.index);
                collect_extra(&tool_prefix, &tool.extra, &mut extensions);
                if let Some(function) = &tool.function {
                    collect_extra(
                        &format!("{tool_prefix}/function"),
                        &function.extra,
                        &mut extensions,
                    );
                }
                self.emit(
                    events,
                    CanonicalEventKind::ToolCallDelta {
                        output_index: choice.index,
                        tool_index: tool.index,
                        id: tool.id,
                        name: tool
                            .function
                            .as_ref()
                            .and_then(|function| function.name.clone()),
                        arguments_delta: tool
                            .function
                            .and_then(|function| function.arguments)
                            .unwrap_or_default(),
                    },
                );
            }
            if let Some(reason) = choice.finish_reason {
                self.finished_choices.insert(choice.index);
                self.emit(
                    events,
                    CanonicalEventKind::Finish {
                        output_index: choice.index,
                        reason: finish_reason(&reason),
                    },
                );
            }
        }

        if let Some(usage) = chunk.usage {
            collect_usage_extensions("/usage", &usage, &mut extensions);
            self.emit(
                events,
                CanonicalEventKind::Usage {
                    usage: canonical_usage(&usage),
                },
            );
        }
        if !extensions.is_empty() {
            self.emit(
                events,
                CanonicalEventKind::SourceExtension {
                    extensions: SourceExtensions::new(Surface::OpenAi, extensions),
                },
            );
        }
        Ok(())
    }

    fn emit(&mut self, events: &mut Vec<CanonicalEvent>, kind: CanonicalEventKind) {
        events.push(CanonicalEvent::new(self.sequence, kind));
        self.sequence = self.sequence.saturating_add(1);
    }
}

fn canonical_role(role: ChatRole) -> MessageRole {
    match role {
        ChatRole::System => MessageRole::System,
        ChatRole::Developer => MessageRole::Developer,
        ChatRole::User => MessageRole::User,
        ChatRole::Assistant => MessageRole::Assistant,
        ChatRole::Tool => MessageRole::Tool,
    }
}

fn finish_reason(reason: &str) -> FinishReason {
    match reason {
        "stop" => FinishReason::Stop,
        "length" => FinishReason::Length,
        "tool_calls" | "function_call" => FinishReason::ToolCalls,
        "content_filter" => FinishReason::ContentFilter,
        other => FinishReason::Other(other.to_owned()),
    }
}

fn canonical_usage(usage: &ChatUsage) -> Usage {
    Usage {
        input_tokens: usage.prompt_tokens,
        output_tokens: usage.completion_tokens,
        total_tokens: usage.total_tokens,
        cached_input_tokens: usage
            .prompt_tokens_details
            .as_ref()
            .and_then(|details| details.cached_tokens),
        reasoning_tokens: usage
            .completion_tokens_details
            .as_ref()
            .and_then(|details| details.reasoning_tokens),
    }
}

fn collect_usage_extensions(
    prefix: &str,
    usage: &ChatUsage,
    extensions: &mut BTreeMap<String, Value>,
) {
    collect_extra(prefix, &usage.extra, extensions);
    if let Some(details) = &usage.prompt_tokens_details {
        collect_extra(
            &format!("{prefix}/prompt_tokens_details"),
            &details.extra,
            extensions,
        );
    }
    if let Some(details) = &usage.completion_tokens_details {
        collect_extra(
            &format!("{prefix}/completion_tokens_details"),
            &details.extra,
            extensions,
        );
    }
}

fn collect_extra(
    prefix: &str,
    extra: &BTreeMap<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) {
    for (key, value) in extra {
        let key = key.replace('~', "~0").replace('/', "~1");
        extensions.insert(format!("{prefix}/{key}"), value.clone());
    }
}

fn canonical_error(error: OpenAiWireError) -> CanonicalError {
    let provider_code = error.code.map(|code| match code {
        Value::String(value) => value,
        value => value.to_string(),
    });
    let kind = error.kind.unwrap_or_default();
    let (class, retryable) = if kind.contains("rate_limit") {
        (ErrorClass::RateLimit, true)
    } else if kind.contains("authentication") {
        (ErrorClass::Authentication, false)
    } else if kind.contains("invalid_request") {
        (ErrorClass::InvalidRequest, false)
    } else {
        (ErrorClass::Upstream, false)
    };
    CanonicalError {
        class,
        message: error.message,
        provider_code,
        retryable,
    }
}

#[derive(Default)]
struct EventBuilder {
    events: Vec<CanonicalEvent>,
}

impl EventBuilder {
    fn push(&mut self, kind: CanonicalEventKind) {
        let sequence = self.events.len().try_into().unwrap_or(u64::MAX);
        self.events.push(CanonicalEvent::new(sequence, kind));
    }
}

#[derive(Debug, Error)]
pub enum OpenAiResponseError {
    #[error("unsupported OpenAI response tool type {0}")]
    UnsupportedToolType(String),
    #[error("OpenAI response contains more tool calls than the canonical index supports")]
    TooManyToolCalls,
}

#[derive(Debug, Error)]
pub enum OpenAiStreamError {
    #[error(transparent)]
    Sse(#[from] SseDecodeError),
    #[error("OpenAI stream frame did not contain valid JSON")]
    Json(#[from] serde_json::Error),
    #[error("OpenAI stream emitted data after [DONE]")]
    DataAfterDone,
    #[error("OpenAI stream emitted data after choice {0} finished")]
    DataAfterChoiceFinish(u32),
    #[error("OpenAI stream ended before terminal completion")]
    UnexpectedEof,
    #[error("unsupported OpenAI stream tool type {0}")]
    UnsupportedToolType(String),
}

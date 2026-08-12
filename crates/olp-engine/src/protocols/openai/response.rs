use std::collections::{BTreeMap, BTreeSet};

use crate::domain::{
    CanonicalError, CanonicalEvent, CanonicalEventKind, ErrorClass, FinishReason, MessageRole,
    SourceExtensions, Surface, UsageObservation,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;

use super::chat::{ChatRole, ChatToolCall};
use super::extensions::collect_extra;
use crate::protocols::CanonicalEventBuilder as EventBuilder;
use crate::protocols::sse::{DEFAULT_MAX_EVENT_BYTES, SseDecodeError, SseDecoder, SseFrame};
use crate::protocols::usage::ObservedUsage;

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

/// Normalizes only the wire omissions explicitly allowed for the internal
/// OpenAI-compatible decode profile. Native OpenAI and Azure never call this.
pub(crate) fn normalize_compatible_chat_response(
    value: &mut Value,
) -> Result<(), OpenAiResponseError> {
    let object = value
        .as_object_mut()
        .ok_or(OpenAiResponseError::InvalidCompatibleEnvelope)?;
    object
        .entry("id")
        .or_insert_with(|| Value::String(String::new()));
    object
        .entry("object")
        .or_insert_with(|| Value::String("chat.completion".into()));
    object.entry("created").or_insert_with(|| Value::from(0));
    object
        .entry("model")
        .or_insert_with(|| Value::String(String::new()));
    normalize_compatible_choices(object.get_mut("choices"), false, None, &BTreeMap::new())
}

pub(crate) fn decode_compatible_chat_completion_response(
    body: &[u8],
) -> Result<Vec<CanonicalEvent>, OpenAiCompatibleDecodeError> {
    let mut value: Value = serde_json::from_slice(body)?;
    normalize_compatible_chat_response(&mut value)?;
    let response: ChatCompletionResponse = serde_json::from_value(value)?;
    Ok(decode_chat_completion_response(response)?)
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
    #[serde(
        default,
        serialize_with = "crate::protocols::usage::serialize_required_option"
    )]
    pub prompt_tokens: Option<u64>,
    #[serde(
        default,
        serialize_with = "crate::protocols::usage::serialize_required_option"
    )]
    pub completion_tokens: Option<u64>,
    #[serde(
        default,
        serialize_with = "crate::protocols::usage::serialize_required_option"
    )]
    pub total_tokens: Option<u64>,
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
    if response.object != "chat.completion" {
        return Err(OpenAiResponseError::UnexpectedObject(response.object));
    }
    if response.choices.is_empty() {
        return Err(OpenAiResponseError::MissingChoices);
    }
    let mut builder = EventBuilder::default();
    builder.push(CanonicalEventKind::ResponseStart {
        response_id: Some(response.id),
        provider_model: Some(response.model),
    });

    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);

    let mut seen_choices = BTreeSet::new();
    for choice in response.choices {
        if !seen_choices.insert(choice.index) {
            return Err(OpenAiResponseError::DuplicateChoiceIndex(choice.index));
        }
        let reason = choice
            .finish_reason
            .as_deref()
            .map(finish_reason)
            .ok_or(OpenAiResponseError::MissingFinishReason(choice.index))?;
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
        builder.push(CanonicalEventKind::Finish {
            output_index: choice.index,
            reason,
        });
    }

    let mut usage_observation = None;
    if let Some(usage) = response.usage {
        collect_usage_extensions("/usage", &usage, &mut extensions);
        let observed = observed_usage(&usage);
        match observed
            .with_exact_total()
            .map_err(|_| OpenAiResponseError::InvalidUsage)?
        {
            Some(usage) => builder.push(CanonicalEventKind::Usage { usage }),
            None => usage_observation = Some(observed.observation()),
        }
    }
    if !extensions.is_empty() {
        builder.push(CanonicalEventKind::SourceExtension {
            extensions: SourceExtensions::new(Surface::OpenAi, extensions),
        });
    }
    if let Some(observation) = usage_observation {
        builder.push_with_usage_observation(CanonicalEventKind::Done, observation);
    } else {
        builder.push(CanonicalEventKind::Done);
    }
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

/// Compatible-only normalization for one decoded Chat Completions SSE chunk.
pub(crate) fn normalize_compatible_chat_chunk(
    value: &mut Value,
    known_choices: &BTreeSet<u32>,
    known_tools: &BTreeMap<u32, BTreeSet<u32>>,
) -> Result<(), OpenAiResponseError> {
    let object = value
        .as_object_mut()
        .ok_or(OpenAiResponseError::InvalidCompatibleEnvelope)?;
    object
        .entry("id")
        .or_insert_with(|| Value::String(String::new()));
    object
        .entry("object")
        .or_insert_with(|| Value::String("chat.completion.chunk".into()));
    object.entry("created").or_insert_with(|| Value::from(0));
    object
        .entry("model")
        .or_insert_with(|| Value::String(String::new()));
    if !object.contains_key("choices") {
        if object.get("usage").is_some_and(|usage| !usage.is_null()) {
            object.insert("choices".into(), Value::Array(Vec::new()));
        } else {
            return Err(OpenAiResponseError::InvalidCompatibleEnvelope);
        }
    }
    normalize_compatible_choices(
        object.get_mut("choices"),
        true,
        Some(known_choices),
        known_tools,
    )
}

fn normalize_compatible_choices(
    choices: Option<&mut Value>,
    streaming: bool,
    known_choices: Option<&BTreeSet<u32>>,
    known_tools: &BTreeMap<u32, BTreeSet<u32>>,
) -> Result<(), OpenAiResponseError> {
    let choices = choices
        .and_then(Value::as_array_mut)
        .ok_or(OpenAiResponseError::InvalidCompatibleEnvelope)?;
    let infer_choice = choices.len() == 1;
    let mut seen_choices = BTreeSet::new();
    for choice in choices {
        let choice = choice
            .as_object_mut()
            .ok_or(OpenAiResponseError::InvalidCompatibleEnvelope)?;
        if !choice.contains_key("index") {
            if !infer_choice {
                return Err(OpenAiResponseError::AmbiguousChoiceIndex);
            }
            let inferred = match known_choices {
                Some(known) if known.len() == 1 => known.iter().next().copied().unwrap_or_default(),
                Some(known) if known.is_empty() => 0,
                Some(_) => return Err(OpenAiResponseError::AmbiguousChoiceIndex),
                None => 0,
            };
            choice.insert("index".into(), Value::from(inferred));
        }
        let choice_index = choice
            .get("index")
            .and_then(Value::as_u64)
            .and_then(|value| u32::try_from(value).ok())
            .ok_or(OpenAiResponseError::InvalidCompatibleEnvelope)?;
        if !seen_choices.insert(choice_index) {
            return Err(OpenAiResponseError::DuplicateChoiceIndex(choice_index));
        }

        let tool_calls = if streaming {
            choice
                .get_mut("delta")
                .and_then(Value::as_object_mut)
                .and_then(|delta| delta.get_mut("tool_calls"))
        } else {
            choice
                .get_mut("message")
                .and_then(Value::as_object_mut)
                .and_then(|message| message.get_mut("tool_calls"))
        };
        let Some(tool_calls) = tool_calls else {
            continue;
        };
        let calls = tool_calls
            .as_array_mut()
            .ok_or(OpenAiResponseError::InvalidCompatibleEnvelope)?;
        let infer_tool = calls.len() == 1;
        let mut seen_tools = BTreeSet::new();
        for call in calls {
            let call = call
                .as_object_mut()
                .ok_or(OpenAiResponseError::InvalidCompatibleEnvelope)?;
            if !call.contains_key("index") && streaming {
                if !infer_tool {
                    return Err(OpenAiResponseError::AmbiguousToolIndex);
                }
                let known = known_tools.get(&choice_index);
                let inferred = match known {
                    Some(known) if known.len() == 1 => {
                        known.iter().next().copied().unwrap_or_default()
                    }
                    Some(known) if known.is_empty() => 0,
                    Some(_) => return Err(OpenAiResponseError::AmbiguousToolIndex),
                    None => 0,
                };
                call.insert("index".into(), Value::from(inferred));
            }
            if streaming {
                let tool_index = call
                    .get("index")
                    .and_then(Value::as_u64)
                    .and_then(|value| u32::try_from(value).ok())
                    .ok_or(OpenAiResponseError::InvalidCompatibleEnvelope)?;
                if !seen_tools.insert(tool_index) {
                    return Err(OpenAiResponseError::AmbiguousToolIndex);
                }
            }
        }
    }
    Ok(())
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
    tool_identities: BTreeMap<(u32, u32), StreamToolIdentity>,
    known_tools: BTreeMap<u32, BTreeSet<u32>>,
    usage_seen: bool,
    usage_observation: Option<UsageObservation>,
    compatible: bool,
    done: bool,
}

#[derive(Default)]
struct StreamToolIdentity {
    id: Option<String>,
    name: Option<String>,
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
            tool_identities: BTreeMap::new(),
            known_tools: BTreeMap::new(),
            usage_seen: false,
            usage_observation: None,
            compatible: false,
            done: false,
        }
    }

    #[must_use]
    pub(crate) fn with_compatible_profile(max_event_bytes: usize) -> Self {
        let mut decoder = Self::with_max_event_bytes(max_event_bytes);
        decoder.compatible = true;
        decoder
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
                self.emit_done(&mut events);
                self.done = true;
                continue;
            }

            let mut value: Value = serde_json::from_str(&frame.data)?;
            if value.get("error").is_some() {
                let envelope: OpenAiErrorEnvelope = serde_json::from_value(value)?;
                self.emit(
                    &mut events,
                    CanonicalEventKind::Error {
                        error: canonical_error(envelope.error),
                    },
                );
                self.emit_done(&mut events);
                self.done = true;
                continue;
            }
            if self.compatible {
                normalize_compatible_chat_chunk(
                    &mut value,
                    &self.started_choices,
                    &self.known_tools,
                )
                .map_err(OpenAiStreamError::Compatible)?;
            } else {
                validate_strict_chat_chunk_envelope(&value)?;
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
        if let Some(choice) = chunk.choices.iter().find(|choice| {
            self.finished_choices.contains(&choice.index) && !choice.is_extension_only()
        }) {
            return Err(OpenAiStreamError::DataAfterChoiceFinish(choice.index));
        }
        let azure_extension_only = chunk.object.is_empty()
            && chunk.usage.is_none()
            && !chunk.choices.is_empty()
            && chunk.choices.iter().all(|choice| {
                self.finished_choices.contains(&choice.index) && choice.is_extension_only()
            });
        if chunk.object != "chat.completion.chunk" && !azure_extension_only {
            return Err(OpenAiStreamError::UnexpectedObject(chunk.object));
        }
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

        let mut chunk_choices = BTreeSet::new();
        for choice in chunk.choices {
            if !chunk_choices.insert(choice.index) {
                return Err(OpenAiStreamError::DuplicateChoiceIndex(choice.index));
            }
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
            let mut chunk_tools = BTreeSet::new();
            for tool in choice.delta.tool_calls {
                if !chunk_tools.insert(tool.index) {
                    return Err(OpenAiStreamError::DuplicateToolIndex {
                        choice_index: choice.index,
                        tool_index: tool.index,
                    });
                }
                self.known_tools
                    .entry(choice.index)
                    .or_default()
                    .insert(tool.index);
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
                let id = tool.id;
                let name = tool
                    .function
                    .as_ref()
                    .and_then(|function| function.name.clone());
                {
                    let identity = self
                        .tool_identities
                        .entry((choice.index, tool.index))
                        .or_default();
                    if id
                        .as_ref()
                        .zip(identity.id.as_ref())
                        .is_some_and(|(new, existing)| new != existing)
                        || name
                            .as_ref()
                            .zip(identity.name.as_ref())
                            .is_some_and(|(new, existing)| new != existing)
                    {
                        return Err(OpenAiStreamError::ConflictingToolMetadata {
                            choice_index: choice.index,
                            tool_index: tool.index,
                        });
                    }
                    if identity.id.is_none() {
                        identity.id.clone_from(&id);
                    }
                    if identity.name.is_none() {
                        identity.name.clone_from(&name);
                    }
                }
                self.emit(
                    events,
                    CanonicalEventKind::ToolCallDelta {
                        output_index: choice.index,
                        tool_index: tool.index,
                        id,
                        name,
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
            if self.usage_seen {
                return Err(OpenAiStreamError::DuplicateUsage);
            }
            self.usage_seen = true;
            collect_usage_extensions("/usage", &usage, &mut extensions);
            let observed = observed_usage(&usage);
            match observed
                .with_exact_total()
                .map_err(|_| OpenAiStreamError::InvalidUsage)?
            {
                Some(usage) => self.emit(events, CanonicalEventKind::Usage { usage }),
                None => self.usage_observation = Some(observed.observation()),
            }
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

    fn emit_done(&mut self, events: &mut Vec<CanonicalEvent>) {
        let mut event = CanonicalEvent::new(self.sequence, CanonicalEventKind::Done);
        if let Some(observation) = self.usage_observation.take() {
            event = event.with_usage_observation(observation);
        }
        events.push(event);
        self.sequence = self.sequence.saturating_add(1);
    }
}

fn validate_strict_chat_chunk_envelope(value: &Value) -> Result<(), OpenAiStreamError> {
    let object = value
        .as_object()
        .ok_or(OpenAiStreamError::InvalidEnvelope)?;
    for field in ["id", "object", "created", "model", "choices"] {
        if !object.contains_key(field) {
            return Err(OpenAiStreamError::InvalidEnvelope);
        }
    }
    Ok(())
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

fn observed_usage(usage: &ChatUsage) -> ObservedUsage {
    ObservedUsage {
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

fn canonical_error(error: OpenAiWireError) -> CanonicalError {
    let provider_code = error.code.map(|code| match code {
        Value::String(value) => value,
        value => value.to_string(),
    });
    let kind = error.kind.unwrap_or_default();
    let (class, retryable) = if crate::protocols::openai::error_signals_rate_limit(
        provider_code.as_deref(),
        Some(&kind),
    ) {
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

#[derive(Debug, Error)]
pub enum OpenAiResponseError {
    #[error("unexpected OpenAI chat response object {0}")]
    UnexpectedObject(String),
    #[error("OpenAI chat response did not contain any choices")]
    MissingChoices,
    #[error("OpenAI chat response repeated choice index {0}")]
    DuplicateChoiceIndex(u32),
    #[error("OpenAI chat response choice {0} omitted its finish reason")]
    MissingFinishReason(u32),
    #[error("OpenAI-compatible response envelope is malformed")]
    InvalidCompatibleEnvelope,
    #[error("OpenAI-compatible response omitted an ambiguous choice index")]
    AmbiguousChoiceIndex,
    #[error("OpenAI-compatible response omitted or repeated an ambiguous tool index")]
    AmbiguousToolIndex,
    #[error("unsupported OpenAI response tool type {0}")]
    UnsupportedToolType(String),
    #[error("OpenAI response contains more tool calls than the canonical index supports")]
    TooManyToolCalls,
    #[error("OpenAI response usage is incomplete or internally inconsistent")]
    InvalidUsage,
}

#[derive(Debug, Error)]
pub(crate) enum OpenAiCompatibleDecodeError {
    #[error("OpenAI-compatible response did not contain valid JSON")]
    Json(#[from] serde_json::Error),
    #[error(transparent)]
    Response(#[from] OpenAiResponseError),
}

#[derive(Debug, Error)]
pub enum OpenAiStreamError {
    #[error(transparent)]
    Sse(#[from] SseDecodeError),
    #[error("OpenAI stream frame did not contain valid JSON")]
    Json(#[from] serde_json::Error),
    #[error("OpenAI stream chunk envelope is malformed")]
    InvalidEnvelope,
    #[error("unexpected OpenAI chat stream object {0}")]
    UnexpectedObject(String),
    #[error(transparent)]
    Compatible(#[from] OpenAiResponseError),
    #[error("OpenAI stream emitted data after [DONE]")]
    DataAfterDone,
    #[error("OpenAI stream emitted data after choice {0} finished")]
    DataAfterChoiceFinish(u32),
    #[error("OpenAI stream repeated choice index {0} in one chunk")]
    DuplicateChoiceIndex(u32),
    #[error(
        "OpenAI stream repeated tool index {tool_index} for choice {choice_index} in one chunk"
    )]
    DuplicateToolIndex { choice_index: u32, tool_index: u32 },
    #[error(
        "OpenAI stream emitted conflicting metadata for tool {tool_index} in choice {choice_index}"
    )]
    ConflictingToolMetadata { choice_index: u32, tool_index: u32 },
    #[error("OpenAI stream emitted more than one usage object")]
    DuplicateUsage,
    #[error("OpenAI stream ended before terminal completion")]
    UnexpectedEof,
    #[error("unsupported OpenAI stream tool type {0}")]
    UnsupportedToolType(String),
    #[error("OpenAI stream usage is internally inconsistent")]
    InvalidUsage,
}

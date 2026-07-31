use std::collections::BTreeMap;

use olp_domain::{CanonicalEvent, CanonicalEventKind, Surface};
use serde_json::{Value, json};
use thiserror::Error;

use crate::client::{AggregateError, aggregate_generation};
use crate::sse::SseFrame;

use super::extensions::apply_pointer_extensions;
use super::responses::OPENAI_RESPONSES_RAW_OUTPUT_PREFIX;
use super::{
    ResponseErrorBody, ResponseInputTokenDetails, ResponseObject, ResponseOutputTokenDetails,
    ResponseUsage,
};

pub fn encode_response_object(
    events: &[CanonicalEvent],
    client_model: &str,
    fallback_id: &str,
) -> Result<ResponseObject, OpenAiClientEncodeError> {
    let mut output_order = response_output_order(events);
    let mut aggregate = aggregate_generation(events, Surface::OpenAi)?;
    let raw_output = take_raw_response_output(&mut aggregate.extensions)?;
    let inferred_incomplete_details = aggregate.outputs.values().find_map(|output| {
        let reason = match output.finish.as_ref()? {
            olp_domain::FinishReason::Length => "max_output_tokens",
            olp_domain::FinishReason::ContentFilter => "content_filter",
            _ => return None,
        };
        Some(json!({"reason": reason}))
    });
    let response_incomplete = aggregate
        .extensions
        .get("/status")
        .and_then(Value::as_str)
        .map_or(inferred_incomplete_details.is_some(), |status| {
            status == "incomplete"
        });
    let mut output = Vec::new();
    for (output_index, mut item) in aggregate.outputs {
        let has_message =
            !item.text.is_empty() || !item.refusal.is_empty() || item.tools.is_empty();
        let components = output_order.entry(output_index).or_default();
        if has_message && !components.contains(&None) {
            components.insert(0, None);
        }
        for tool_index in item.tools.keys() {
            if !components.contains(&Some(*tool_index)) {
                components.push(Some(*tool_index));
            }
        }
        for component in components.iter().copied() {
            let status = take_string_extension(
                &mut aggregate.extensions,
                &format!("/output/{output_index}/status"),
            )
            .unwrap_or_else(|| {
                if response_incomplete {
                    "incomplete".into()
                } else {
                    "completed".into()
                }
            });
            if let Some(tool_index) = component {
                let Some(tool) = item.tools.remove(&tool_index) else {
                    continue;
                };
                output.push(json!({
                    "id": take_string_extension(&mut aggregate.extensions, &format!("/output/{output_index}/id"))
                        .map(Value::String),
                    "type": "function_call",
                    "call_id": tool.id.ok_or(OpenAiClientEncodeError::IncompleteToolCall("id"))?,
                    "name": tool.name.ok_or(OpenAiClientEncodeError::IncompleteToolCall("name"))?,
                    "arguments": tool.arguments,
                    "status": status,
                }));
            } else if has_message {
                let mut content = Vec::new();
                if !item.text.is_empty() {
                    content.push(json!({
                        "type": "output_text",
                        "text": std::mem::take(&mut item.text),
                        "annotations": aggregate.extensions.remove(
                            &format!("/output/{output_index}/content/0/annotations")
                        ).unwrap_or_else(|| json!([])),
                    }));
                }
                if !item.refusal.is_empty() {
                    content.push(
                        json!({"type": "refusal", "refusal": std::mem::take(&mut item.refusal)}),
                    );
                }
                output.push(json!({
                    "id": take_string_extension(&mut aggregate.extensions, &format!("/output/{output_index}/id"))
                        .map(Value::String),
                    "type": "message",
                    "role": "assistant",
                    "status": status,
                    "content": content,
                }));
            }
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
    for (index, item) in output.iter_mut().enumerate() {
        let prefix = match item.get("type").and_then(Value::as_str) {
            Some("message") => "msg",
            Some("function_call") => "fc",
            _ => continue,
        };
        if item.get("id").is_some_and(Value::is_null) {
            item["id"] = Value::String(format!("{prefix}_{index}"));
        }
    }
    let created_at = take_i64_extension(&mut aggregate.extensions, "/created_at").unwrap_or(0);
    let status = take_string_extension(&mut aggregate.extensions, "/status").unwrap_or_else(|| {
        if response_incomplete {
            "incomplete"
        } else {
            "completed"
        }
        .into()
    });
    let incomplete_details = aggregate
        .extensions
        .remove("/incomplete_details")
        .or(inferred_incomplete_details);
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

fn response_output_order(events: &[CanonicalEvent]) -> BTreeMap<u32, Vec<Option<u32>>> {
    let mut order = BTreeMap::<u32, Vec<Option<u32>>>::new();
    for event in events {
        let component = match &event.kind {
            CanonicalEventKind::TextDelta { output_index, .. }
            | CanonicalEventKind::RefusalDelta { output_index, .. } => Some((*output_index, None)),
            CanonicalEventKind::ToolCallDelta {
                output_index,
                tool_index,
                ..
            } => Some((*output_index, Some(*tool_index))),
            _ => None,
        };
        if let Some((output_index, component)) = component {
            let components = order.entry(output_index).or_default();
            if !components.contains(&component) {
                components.push(component);
            }
        }
    }
    order
}

pub struct OpenAiResponsesStreamEncoder {
    client_model: String,
    fallback_id: String,
    created_at: i64,
    next_sequence: u64,
    events: Vec<CanonicalEvent>,
    collected_event_bytes: usize,
    outputs: BTreeMap<(u32, Option<u32>), u32>,
    next_output_index: u32,
    done: bool,
}

impl std::fmt::Debug for OpenAiResponsesStreamEncoder {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("OpenAiResponsesStreamEncoder")
            .field("next_sequence", &self.next_sequence)
            .field("collected_event_bytes", &self.collected_event_bytes)
            .field("emitted_output_count", &self.outputs.len())
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
            collected_event_bytes: 0,
            outputs: BTreeMap::new(),
            next_output_index: 0,
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
        // Keep this aligned with the unary collection ceiling in
        // apps/olp/src/event_completion.rs.
        const MAX_COLLECTED_CANONICAL_EVENT_BYTES: usize = 16 * 1024 * 1024;
        let event_bytes = serde_json::to_vec(&event)?.len();
        self.collected_event_bytes = self
            .collected_event_bytes
            .checked_add(event_bytes)
            .filter(|total| *total <= MAX_COLLECTED_CANONICAL_EVENT_BYTES)
            .ok_or(OpenAiClientEncodeError::EventHistoryTooLarge)?;
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
                let wire_output_index = self.ensure_output(*output_index, None, &mut frames)?;
                frames.push(response_sse_frame(
                    "response.output_text.delta",
                    json!({"output_index": wire_output_index, "content_index": 0, "delta": text}),
                )?);
            }
            CanonicalEventKind::RefusalDelta { output_index, text } => {
                let wire_output_index = self.ensure_output(*output_index, None, &mut frames)?;
                frames.push(response_sse_frame(
                    "response.refusal.delta",
                    json!({"output_index": wire_output_index, "content_index": 0, "delta": text}),
                )?);
            }
            CanonicalEventKind::ToolCallDelta {
                output_index,
                tool_index,
                id,
                name,
                arguments_delta,
            } => {
                let wire_output_index = self.ensure_output(
                    *output_index,
                    Some((*tool_index, id.as_deref(), name.as_deref())),
                    &mut frames,
                )?;
                frames.push(response_sse_frame(
                    "response.function_call_arguments.delta",
                    json!({
                        "output_index": wire_output_index,
                        "item_id": format!("fc_{wire_output_index}"),
                        "delta": arguments_delta
                    }),
                )?);
            }
            CanonicalEventKind::Finish { output_index, .. } => {
                let mut finished = self
                    .outputs
                    .iter()
                    .filter(|((canonical_output_index, _), _)| {
                        canonical_output_index == output_index
                    })
                    .map(|((_, tool_index), wire_output_index)| {
                        (
                            *wire_output_index,
                            if tool_index.is_some() {
                                "function_call"
                            } else {
                                "message"
                            },
                        )
                    })
                    .collect::<Vec<_>>();
                if finished.is_empty() {
                    finished.push((
                        self.ensure_output(*output_index, None, &mut frames)?,
                        "message",
                    ));
                }
                finished.sort_unstable_by_key(|(wire_output_index, _)| *wire_output_index);
                for (wire_output_index, kind) in finished {
                    frames.push(response_sse_frame(
                        "response.output_item.done",
                        json!({
                            "output_index": wire_output_index,
                            "item": {"type": kind}
                        }),
                    )?);
                }
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
                let event = if response.status == "incomplete" {
                    "response.incomplete"
                } else {
                    "response.completed"
                };
                frames.push(response_sse_frame(event, json!({"response": response}))?);
                self.done = true;
            }
        }
        self.events.push(event);
        Ok(frames)
    }

    fn ensure_output(
        &mut self,
        canonical_output_index: u32,
        tool: Option<(u32, Option<&str>, Option<&str>)>,
        frames: &mut Vec<SseFrame>,
    ) -> Result<u32, OpenAiClientEncodeError> {
        let key = (canonical_output_index, tool.map(|(index, _, _)| index));
        if let Some(output_index) = self.outputs.get(&key) {
            if let Some((tool_index, id, name)) = tool {
                let previous = self.events.iter().find_map(|event| match &event.kind {
                    CanonicalEventKind::ToolCallDelta {
                        output_index,
                        tool_index: previous_tool_index,
                        id,
                        name,
                        ..
                    } if *output_index == canonical_output_index
                        && *previous_tool_index == tool_index =>
                    {
                        Some((id.as_deref(), name.as_deref()))
                    }
                    _ => None,
                });
                if previous.is_some_and(|(previous_id, previous_name)| {
                    id.is_some_and(|id| previous_id != Some(id))
                        || name.is_some_and(|name| previous_name != Some(name))
                }) {
                    return Err(OpenAiClientEncodeError::ConflictingToolCallIdentity);
                }
            }
            return Ok(*output_index);
        }
        let output_index = self.allocate_output_index(canonical_output_index)?;
        let item = if let Some((_, call_id, name)) = tool {
            json!({
                "id": format!("fc_{output_index}"),
                "type": "function_call",
                "status": "in_progress",
                "call_id": call_id.ok_or(OpenAiClientEncodeError::IncompleteToolCall("id"))?,
                "name": name.ok_or(OpenAiClientEncodeError::IncompleteToolCall("name"))?,
                "arguments": ""
            })
        } else {
            json!({
                "id": format!("msg_{output_index}"),
                "type": "message",
                "role": "assistant",
                "status": "in_progress",
                "content": []
            })
        };
        frames.push(response_sse_frame(
            "response.output_item.added",
            json!({"output_index": output_index, "item": item}),
        )?);
        self.outputs.insert(key, output_index);
        Ok(output_index)
    }

    fn allocate_output_index(&mut self, preferred: u32) -> Result<u32, OpenAiClientEncodeError> {
        let output_index = if self.outputs.values().any(|index| *index == preferred) {
            while self
                .outputs
                .values()
                .any(|index| *index == self.next_output_index)
            {
                self.next_output_index = self
                    .next_output_index
                    .checked_add(1)
                    .ok_or(OpenAiClientEncodeError::TooManyOutputItems)?;
            }
            self.next_output_index
        } else {
            preferred
        };
        if let Some(next) = output_index.checked_add(1) {
            self.next_output_index = next.max(self.next_output_index);
        }
        Ok(output_index)
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
    #[error("canonical tool call identity changed while streaming")]
    ConflictingToolCallIdentity,
    #[error("canonical response contains too many output items")]
    TooManyOutputItems,
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
    #[error("canonical event history exceeded the Responses stream encoder limit")]
    EventHistoryTooLarge,
    #[error(transparent)]
    Json(#[from] serde_json::Error),
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn responses_stream_encoder_rejects_oversized_event_history() {
        let mut encoder = OpenAiResponsesStreamEncoder::new("route", "response", 0);
        let event = CanonicalEvent::new(
            0,
            CanonicalEventKind::TextDelta {
                output_index: 0,
                text: "x".repeat(16 * 1024 * 1024),
            },
        );

        assert!(matches!(
            encoder.push(event),
            Err(OpenAiClientEncodeError::EventHistoryTooLarge)
        ));
    }

    #[test]
    fn responses_stream_encoder_rejects_changed_tool_identity() {
        let mut encoder = OpenAiResponsesStreamEncoder::new("route", "response", 0);
        let delta = |sequence, id: &str| {
            CanonicalEvent::new(
                sequence,
                CanonicalEventKind::ToolCallDelta {
                    output_index: 0,
                    tool_index: 0,
                    id: Some(id.into()),
                    name: Some("lookup".into()),
                    arguments_delta: String::new(),
                },
            )
        };
        encoder.push(delta(0, "call_1")).unwrap();
        assert!(matches!(
            encoder.push(delta(1, "call_2")),
            Err(OpenAiClientEncodeError::ConflictingToolCallIdentity)
        ));
    }
}

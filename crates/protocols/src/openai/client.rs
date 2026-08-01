use std::collections::{BTreeMap, BTreeSet, VecDeque};

use olp_domain::{CanonicalEvent, CanonicalEventKind, FinishReason, Surface};
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
    encode_response_object_with_order(events, client_model, fallback_id, None)
}

fn encode_response_object_with_order(
    events: &[CanonicalEvent],
    client_model: &str,
    fallback_id: &str,
    wire_output_indices: Option<&BTreeMap<(u32, Option<u32>), u32>>,
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
    let response_incomplete = aggregate.outputs.values().any(|output| {
        output
            .finish
            .as_ref()
            .is_some_and(response_output_is_incomplete)
    }) || aggregate
        .extensions
        .get("/status")
        .and_then(Value::as_str)
        .is_some_and(|status| status == "incomplete");
    let mut semantic_output = Vec::new();
    for (output_index, mut item) in aggregate.outputs {
        let output_incomplete = item
            .finish
            .as_ref()
            .map_or(response_incomplete, response_output_is_incomplete);
        let has_message =
            !item.text.is_empty() || !item.refusal.is_empty() || item.tools.is_empty();
        let components = output_order.entry(output_index).or_default();
        if has_message && !components.contains(&None) {
            components.insert(0, None);
        }
        for component in components.iter().copied() {
            let status = take_string_extension(
                &mut aggregate.extensions,
                &format!("/output/{output_index}/status"),
            )
            .unwrap_or_else(|| {
                if output_incomplete {
                    "incomplete".into()
                } else {
                    "completed".into()
                }
            });
            if let Some(tool_index) = component {
                let Some(tool) = item.tools.remove(&tool_index) else {
                    continue;
                };
                semantic_output.push((
                    (output_index, Some(tool_index)),
                    json!({
                        "id": take_string_extension(&mut aggregate.extensions, &format!("/output/{output_index}/id"))
                            .map(Value::String),
                        "type": "function_call",
                        "call_id": tool.id.ok_or(OpenAiClientEncodeError::IncompleteToolCall("id"))?,
                        "name": tool.name.ok_or(OpenAiClientEncodeError::IncompleteToolCall("name"))?,
                        "arguments": tool.arguments,
                        "status": status,
                    }),
                ));
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
                semantic_output.push((
                    (output_index, None),
                    json!({
                        "id": take_string_extension(&mut aggregate.extensions, &format!("/output/{output_index}/id"))
                            .map(Value::String),
                        "type": "message",
                        "role": "assistant",
                        "status": status,
                        "content": content,
                    }),
                ));
            }
        }
    }
    if let Some(wire_output_indices) = wire_output_indices {
        let mut indices = Vec::with_capacity(semantic_output.len() + raw_output.len());
        for (key, _) in &semantic_output {
            let index = wire_output_indices
                .get(key)
                .ok_or(OpenAiClientEncodeError::InvalidOutputOrder)?;
            indices.push(
                usize::try_from(*index).map_err(|_| OpenAiClientEncodeError::TooManyOutputItems)?,
            );
        }
        indices.extend(raw_output.iter().map(|(index, _)| *index));
        indices.sort_unstable();
        if indices
            .iter()
            .enumerate()
            .any(|(expected, actual)| expected != *actual)
        {
            return Err(OpenAiClientEncodeError::InvalidOutputOrder);
        }
        semantic_output
            .sort_by_key(|(key, _)| wire_output_indices.get(key).copied().unwrap_or(u32::MAX));
    }
    let mut output = semantic_output
        .into_iter()
        .map(|(_, item)| item)
        .collect::<Vec<_>>();
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

fn response_output_is_incomplete(reason: &FinishReason) -> bool {
    !matches!(reason, FinishReason::Stop | FinishReason::ToolCalls)
}

fn response_output_order(events: &[CanonicalEvent]) -> BTreeMap<u32, Vec<Option<u32>>> {
    let mut order = BTreeMap::<u32, Vec<Option<u32>>>::new();
    let mut seen = BTreeSet::new();
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
        if let Some((output_index, component)) = component
            && seen.insert((output_index, component))
        {
            order.entry(output_index).or_default().push(component);
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
    output_keys: Vec<Option<(u32, Option<u32>)>>,
    pending_output_starts: BTreeSet<u32>,
    finished_outputs: BTreeMap<(u32, Option<u32>), FinishReason>,
    pending_frames: VecDeque<PendingResponseFrame>,
    tools: BTreeMap<(u32, u32), StreamingTool>,
    next_output_to_emit: usize,
    done: bool,
}

struct PendingResponseFrame {
    output_index: Option<usize>,
    adds_output: bool,
    frame: SseFrame,
}

#[derive(Default)]
struct StreamingTool {
    id: Option<String>,
    name: Option<String>,
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
            output_keys: Vec::new(),
            pending_output_starts: BTreeSet::new(),
            finished_outputs: BTreeMap::new(),
            pending_frames: VecDeque::new(),
            tools: BTreeMap::new(),
            next_output_to_emit: 0,
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
            CanonicalEventKind::MessageStart { output_index, .. } => {
                let key = (*output_index, None);
                if self
                    .outputs
                    .range((*output_index, None)..=(*output_index, Some(u32::MAX)))
                    .next()
                    .is_none()
                {
                    self.reserve_output(key)?;
                    self.pending_output_starts.insert(*output_index);
                }
            }
            CanonicalEventKind::TextDelta { text, .. } if text.is_empty() => {}
            CanonicalEventKind::TextDelta { output_index, .. } => {
                let key = (*output_index, None);
                self.pending_output_starts.remove(output_index);
                let wire_output_index = self.reserve_output(key)?;
                self.queue_output_delta(&event, key, wire_output_index)?;
            }
            CanonicalEventKind::RefusalDelta { text, .. } if text.is_empty() => {}
            CanonicalEventKind::RefusalDelta { output_index, .. } => {
                let key = (*output_index, None);
                self.pending_output_starts.remove(output_index);
                let wire_output_index = self.reserve_output(key)?;
                self.queue_output_delta(&event, key, wire_output_index)?;
            }
            CanonicalEventKind::ToolCallDelta {
                output_index,
                tool_index,
                id,
                name,
                arguments_delta: _,
            } => {
                let key = (*output_index, Some(*tool_index));
                let wire_output_index = if self.pending_output_starts.remove(output_index) {
                    let placeholder = (*output_index, None);
                    let wire_output_index = self
                        .outputs
                        .remove(&placeholder)
                        .ok_or(OpenAiClientEncodeError::InvalidOutputOrder)?;
                    let slot = self
                        .output_keys
                        .get_mut(
                            usize::try_from(wire_output_index)
                                .map_err(|_| OpenAiClientEncodeError::TooManyOutputItems)?,
                        )
                        .ok_or(OpenAiClientEncodeError::InvalidOutputOrder)?;
                    *slot = Some(key);
                    self.outputs.insert(key, wire_output_index);
                    wire_output_index
                } else {
                    self.reserve_output(key)?
                };
                let tool = self.tools.entry((*output_index, *tool_index)).or_default();
                if let Some(id) = id {
                    if tool.id.as_ref().is_some_and(|previous| previous != id) {
                        return Err(OpenAiClientEncodeError::ConflictingToolCallIdentity);
                    }
                    tool.id.get_or_insert_with(|| id.clone());
                }
                if let Some(name) = name {
                    if tool.name.as_ref().is_some_and(|previous| previous != name) {
                        return Err(OpenAiClientEncodeError::ConflictingToolCallIdentity);
                    }
                    tool.name.get_or_insert_with(|| name.clone());
                }
                self.queue_output_delta(&event, key, wire_output_index)?;
            }
            CanonicalEventKind::Finish {
                output_index,
                reason,
            } => {
                self.pending_output_starts.remove(output_index);
                let mut finished = self
                    .outputs
                    .range((*output_index, None)..=(*output_index, Some(u32::MAX)))
                    .map(|(key, wire_output_index)| (*wire_output_index, *key))
                    .collect::<Vec<_>>();
                if finished.is_empty() {
                    let key = (*output_index, None);
                    finished.push((self.reserve_output(key)?, key));
                }
                finished.sort_unstable_by_key(|(wire_output_index, _)| *wire_output_index);
                if finished.iter().any(|(_, key)| !self.output_ready(*key)) {
                    return Err(OpenAiClientEncodeError::IncompleteToolCall("identity"));
                }
                for (wire_output_index, key) in finished {
                    if let std::collections::btree_map::Entry::Vacant(entry) =
                        self.finished_outputs.entry(key)
                    {
                        let output_index = usize::try_from(wire_output_index)
                            .map_err(|_| OpenAiClientEncodeError::TooManyOutputItems)?;
                        let frame = Self::output_done_frame(key, wire_output_index, reason)?;
                        entry.insert(reason.clone());
                        self.pending_frames.push_back(PendingResponseFrame {
                            output_index: Some(output_index),
                            adds_output: false,
                            frame,
                        });
                    }
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
                        let output_index = value
                            .get("output_index")
                            .and_then(Value::as_u64)
                            .and_then(|index| usize::try_from(index).ok());
                        if kind == "response.output_item.added" {
                            let output_index =
                                output_index.ok_or(OpenAiClientEncodeError::InvalidOutputOrder)?;
                            if output_index != self.output_keys.len() {
                                return Err(OpenAiClientEncodeError::InvalidOutputOrder);
                            }
                            self.output_keys.push(None);
                        }
                        let frame = response_sse_frame(kind, value.clone())?;
                        self.pending_frames.push_back(PendingResponseFrame {
                            output_index,
                            adds_output: kind == "response.output_item.added",
                            frame,
                        });
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
                self.emit_ready_outputs(&mut frames)?;
                if self.next_output_to_emit != self.output_keys.len()
                    || !self.pending_frames.is_empty()
                    || self
                        .outputs
                        .keys()
                        .any(|key| !self.finished_outputs.contains_key(key))
                {
                    return Err(OpenAiClientEncodeError::InvalidOutputOrder);
                }
                let normalized = self.normalized_events_with(event.clone());
                let response = encode_response_object_with_order(
                    &normalized,
                    &self.client_model,
                    &self.fallback_id,
                    Some(&self.outputs),
                )?;
                let event = if response.status == "incomplete" {
                    "response.incomplete"
                } else {
                    "response.completed"
                };
                frames.push(response_sse_frame(event, json!({"response": response}))?);
                self.done = true;
            }
        }
        self.emit_ready_outputs(&mut frames)?;
        self.events.push(event);
        Ok(frames)
    }

    fn reserve_output(&mut self, key: (u32, Option<u32>)) -> Result<u32, OpenAiClientEncodeError> {
        if let Some(output_index) = self.outputs.get(&key) {
            return Ok(*output_index);
        }
        let output_index = u32::try_from(self.output_keys.len())
            .map_err(|_| OpenAiClientEncodeError::TooManyOutputItems)?;
        self.output_keys.push(Some(key));
        self.outputs.insert(key, output_index);
        Ok(output_index)
    }

    fn output_ready(&self, key: (u32, Option<u32>)) -> bool {
        if key.1.is_none() && self.pending_output_starts.contains(&key.0) {
            return false;
        }
        key.1.is_none_or(|tool_index| {
            self.tools
                .get(&(key.0, tool_index))
                .is_some_and(|tool| tool.id.is_some() && tool.name.is_some())
        })
    }

    fn emit_ready_outputs(
        &mut self,
        frames: &mut Vec<SseFrame>,
    ) -> Result<(), OpenAiClientEncodeError> {
        loop {
            if self.pending_frames.front().is_some_and(|pending| {
                !pending.adds_output
                    && pending
                        .output_index
                        .is_none_or(|index| index < self.next_output_to_emit)
            }) {
                let pending = self
                    .pending_frames
                    .pop_front()
                    .ok_or(OpenAiClientEncodeError::InvalidOutputOrder)?;
                frames.push(pending.frame);
                continue;
            }
            if self.pending_frames.front().is_some_and(|pending| {
                pending.adds_output
                    && pending.output_index == Some(self.next_output_to_emit)
                    && self.output_keys.get(self.next_output_to_emit) == Some(&None)
            }) {
                let pending = self
                    .pending_frames
                    .pop_front()
                    .ok_or(OpenAiClientEncodeError::InvalidOutputOrder)?;
                frames.push(pending.frame);
                self.next_output_to_emit = self
                    .next_output_to_emit
                    .checked_add(1)
                    .ok_or(OpenAiClientEncodeError::TooManyOutputItems)?;
                continue;
            }
            let Some(Some(key)) = self.output_keys.get(self.next_output_to_emit).copied() else {
                break;
            };
            if !self.output_ready(key) {
                break;
            }
            self.emit_output_added(key, frames)?;
        }
        Ok(())
    }

    fn queue_output_delta(
        &mut self,
        event: &CanonicalEvent,
        key: (u32, Option<u32>),
        output_index: u32,
    ) -> Result<(), OpenAiClientEncodeError> {
        self.pending_frames.push_back(PendingResponseFrame {
            output_index: Some(
                usize::try_from(output_index)
                    .map_err(|_| OpenAiClientEncodeError::TooManyOutputItems)?,
            ),
            adds_output: false,
            frame: Self::output_delta_frame(event, key, output_index)?,
        });
        Ok(())
    }

    fn emit_output_added(
        &mut self,
        key: (u32, Option<u32>),
        frames: &mut Vec<SseFrame>,
    ) -> Result<(), OpenAiClientEncodeError> {
        let output_index = *self
            .outputs
            .get(&key)
            .ok_or(OpenAiClientEncodeError::InvalidOutputOrder)?;
        if usize::try_from(output_index).ok() != Some(self.next_output_to_emit) {
            return Err(OpenAiClientEncodeError::InvalidOutputOrder);
        }
        let item = if let Some(tool_index) = key.1 {
            let tool = self
                .tools
                .get(&(key.0, tool_index))
                .ok_or(OpenAiClientEncodeError::IncompleteToolCall("identity"))?;
            json!({
                "id": format!("fc_{output_index}"),
                "type": "function_call",
                "status": "in_progress",
                "call_id": tool.id.as_deref().ok_or(OpenAiClientEncodeError::IncompleteToolCall("id"))?,
                "name": tool.name.as_deref().ok_or(OpenAiClientEncodeError::IncompleteToolCall("name"))?,
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
        self.next_output_to_emit = self
            .next_output_to_emit
            .checked_add(1)
            .ok_or(OpenAiClientEncodeError::TooManyOutputItems)?;
        Ok(())
    }

    fn output_delta_frame(
        event: &CanonicalEvent,
        key: (u32, Option<u32>),
        output_index: u32,
    ) -> Result<SseFrame, OpenAiClientEncodeError> {
        match &event.kind {
            CanonicalEventKind::TextDelta {
                output_index: canonical_output_index,
                text,
            } if key == (*canonical_output_index, None) => response_sse_frame(
                "response.output_text.delta",
                json!({"output_index": output_index, "content_index": 0, "delta": text}),
            ),
            CanonicalEventKind::RefusalDelta {
                output_index: canonical_output_index,
                text,
            } if key == (*canonical_output_index, None) => response_sse_frame(
                "response.refusal.delta",
                json!({"output_index": output_index, "content_index": 0, "delta": text}),
            ),
            CanonicalEventKind::ToolCallDelta {
                output_index: canonical_output_index,
                tool_index,
                arguments_delta,
                ..
            } if key == (*canonical_output_index, Some(*tool_index)) => response_sse_frame(
                "response.function_call_arguments.delta",
                json!({
                    "output_index": output_index,
                    "item_id": format!("fc_{output_index}"),
                    "delta": arguments_delta
                }),
            ),
            _ => Err(OpenAiClientEncodeError::InvalidOutputOrder),
        }
    }

    fn output_done_frame(
        key: (u32, Option<u32>),
        output_index: u32,
        reason: &FinishReason,
    ) -> Result<SseFrame, OpenAiClientEncodeError> {
        response_sse_frame(
            "response.output_item.done",
            json!({
                "output_index": output_index,
                "item": {
                    "type": if key.1.is_some() { "function_call" } else { "message" },
                    "status": if response_output_is_incomplete(reason) {
                        "incomplete"
                    } else {
                        "completed"
                    }
                }
            }),
        )
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
    #[error("canonical stream output indexes are inconsistent")]
    InvalidOutputOrder,
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

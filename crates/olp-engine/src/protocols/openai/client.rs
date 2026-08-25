use std::collections::{BTreeMap, BTreeSet};

use crate::domain::canonical::{
    events::{Event, FinishReason, Kind},
    identity::Surface,
};
use serde_json::{Value, json};
use thiserror::Error;

use crate::protocols::client::{AggregateError, aggregate_generation};
use crate::protocols::sse::Frame;

use super::extensions::apply_pointer_extensions;
use super::responses::OPENAI_RESPONSES_RAW_OUTPUT_PREFIX;
use super::responses::response::{ErrorBody, InputTokenDetails, Object, OutputTokenDetails, Usage};

/// `created_at` is the fallback used when the canonical events did not come
/// from a Responses upstream, so a client never sees `created_at: 0`.
pub fn encode_response_object(
    events: &[Event],
    client_model: &str,
    fallback_id: &str,
    created_at: i64,
) -> Result<Object, OpenAiClientEncodeError> {
    let mut aggregate = aggregate_generation(events, Surface::OpenAi)?;
    let raw_output = take_raw_response_output(&mut aggregate.extensions)?;
    // Raw items are re-inserted at their original wire indices below, so the
    // items rebuilt here have to claim the positions those leave free. Each
    // gets its own index: two tool calls under one canonical output otherwise
    // reused the same `/output/{n}/id` extension and shipped duplicate ids.
    let raw_indices = raw_output
        .iter()
        .map(|(index, _)| *index)
        .collect::<BTreeSet<_>>();
    let mut next_index = 0_usize;
    let mut claim_wire_index = move || {
        while raw_indices.contains(&next_index) {
            next_index += 1;
        }
        let index = next_index;
        next_index += 1;
        index
    };
    let mut output = Vec::new();
    let mut finish = None;
    for (_, item) in aggregate.outputs {
        finish = finish.or_else(|| item.finish.clone());
        if !item.text.is_empty() || !item.refusal.is_empty() || item.tools.is_empty() {
            let wire_index = claim_wire_index();
            let mut content = Vec::new();
            if !item.text.is_empty() {
                let annotations = aggregate
                    .extensions
                    .remove(&format!("/output/{wire_index}/content/0/annotations"))
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
                &format!("/output/{wire_index}/id"),
            )
            .unwrap_or_else(|| format!("msg_{wire_index}"));
            let status = take_string_extension(
                &mut aggregate.extensions,
                &format!("/output/{wire_index}/status"),
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
            let wire_index = claim_wire_index();
            let id = tool
                .id
                .ok_or(OpenAiClientEncodeError::IncompleteToolCall("id"))?;
            let name = tool
                .name
                .ok_or(OpenAiClientEncodeError::IncompleteToolCall("name"))?;
            let wire_id = take_string_extension(
                &mut aggregate.extensions,
                &format!("/output/{wire_index}/id"),
            )
            .unwrap_or_else(|| format!("fc_{wire_index}"));
            let status = take_string_extension(
                &mut aggregate.extensions,
                &format!("/output/{wire_index}/status"),
            )
            .unwrap_or_else(|| "completed".into());
            output.push(json!({
                "id": wire_id,
                "type": "function_call",
                "call_id": id,
                "name": name,
                // A tool invoked with no parameters aggregates to an empty
                // string, which is not valid JSON for the client to parse.
                "arguments": if tool.arguments.trim().is_empty() {
                    "{}".to_owned()
                } else {
                    tool.arguments
                },
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
    let observed_created_at =
        take_i64_extension(&mut aggregate.extensions, "/created_at").unwrap_or(0);
    let created_at = if observed_created_at == 0 {
        created_at
    } else {
        observed_created_at
    };
    let mut incomplete_details = aggregate.extensions.remove("/incomplete_details");
    let status = take_string_extension(&mut aggregate.extensions, "/status").unwrap_or_else(|| {
        // A non-Responses upstream reports truncation only through the finish
        // reason, so the terminal status is derived from it rather than always
        // claiming the response completed.
        match finish {
            Some(FinishReason::Length) => {
                incomplete_details.get_or_insert_with(|| json!({"reason": "max_output_tokens"}));
                "incomplete".to_owned()
            }
            Some(FinishReason::ContentFilter) => {
                incomplete_details.get_or_insert_with(|| json!({"reason": "content_filter"}));
                "incomplete".to_owned()
            }
            _ => "completed".to_owned(),
        }
    });
    let usage = aggregate.usage.map(|usage| Usage {
        input_tokens: usage.input_tokens,
        // The Responses API reports reasoning inside `output_tokens`.
        output_tokens: usage
            .output_tokens
            .saturating_add(usage.reasoning_tokens.unwrap_or(0)),
        total_tokens: usage.total_tokens,
        input_tokens_details: usage
            .cached_input_tokens
            .map(|cached_tokens| InputTokenDetails {
                cached_tokens,
                extra: BTreeMap::new(),
            }),
        output_tokens_details: usage
            .reasoning_tokens
            .map(|reasoning_tokens| OutputTokenDetails {
                reasoning_tokens,
                extra: BTreeMap::new(),
            }),
        extra: BTreeMap::new(),
    });
    apply_pointer_extensions(
        Object {
            id: aggregate.response_id.unwrap_or_else(|| fallback_id.into()),
            object: "response".into(),
            created_at,
            status,
            model: client_model.into(),
            output,
            usage,
            error: None::<ErrorBody>,
            incomplete_details,
            extra: BTreeMap::new(),
        },
        &aggregate.extensions,
    )
    .map_err(OpenAiClientEncodeError::InvalidExtension)
}

pub struct Encoder {
    client_model: String,
    fallback_id: String,
    created_at: i64,
    next_sequence: u64,
    sequence_number: u64,
    events: Vec<Event>,
    collected_event_bytes: usize,
    outputs: BTreeMap<u32, StreamOutput>,
    done: bool,

    incomplete_reason: Option<&'static str>,
}

/// The lifecycle the Responses API promises per output item. Clients built on
/// the item events (the OpenAI Agents SDK among them) read the tool name and
/// call id off `response.output_item.added`, so the frame is held back until a
/// delta actually carries them.
#[derive(Default)]
struct StreamOutput {
    item_id: String,
    tool: bool,
    call_id: Option<String>,
    name: Option<String>,
    text: String,
    refusal: String,
    arguments: String,
}

impl std::fmt::Debug for Encoder {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("Encoder")
            .field("next_sequence", &self.next_sequence)
            .field("collected_event_bytes", &self.collected_event_bytes)
            .field("emitted_output_count", &self.outputs.len())
            .field("done", &self.done)
            .finish_non_exhaustive()
    }
}

impl Encoder {
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
            sequence_number: 0,
            events: Vec::new(),
            collected_event_bytes: 0,
            outputs: BTreeMap::new(),
            done: false,
            incomplete_reason: None,
        }
    }

    pub fn push(&mut self, event: Event) -> Result<Vec<Frame>, OpenAiClientEncodeError> {
        if self.done {
            return Err(OpenAiClientEncodeError::DataAfterDone);
        }
        if event.sequence != self.next_sequence {
            return Err(OpenAiClientEncodeError::OutOfOrder {
                expected: self.next_sequence,
                actual: event.sequence,
            });
        }
        // Keep this aligned with the transport-neutral collection ceiling in
        // crates/olp-engine/src/inference/events.rs.
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
            Kind::ResponseStart {
                response_id,
                provider_model: _,
            } => {
                let id = response_id
                    .clone()
                    .unwrap_or_else(|| self.fallback_id.clone());
                let response = json!({
                    "id": id,
                    "object": "response",
                    "created_at": self.created_at,
                    "status": "in_progress",
                    "model": self.client_model,
                    "output": []
                });
                frames.push(self.frame("response.created", json!({"response": response}))?);
                frames.push(self.frame("response.in_progress", json!({"response": response}))?);
            }
            Kind::MessageStart { .. } => {}
            Kind::TextDelta { output_index, text } => {
                let item_id = self.ensure_message_output(*output_index, &mut frames)?;
                if let Some(output) = self.outputs.get_mut(output_index) {
                    output.text.push_str(text);
                }
                if !text.is_empty() {
                    frames.push(self.frame(
                        "response.output_text.delta",
                        json!({
                            "item_id": item_id,
                            "output_index": output_index,
                            "content_index": 0,
                            "delta": text
                        }),
                    )?);
                }
            }
            Kind::RefusalDelta { output_index, text } => {
                let item_id = self.ensure_message_output(*output_index, &mut frames)?;
                if let Some(output) = self.outputs.get_mut(output_index) {
                    output.refusal.push_str(text);
                }
                frames.push(self.frame(
                    "response.refusal.delta",
                    json!({
                        "item_id": item_id,
                        "output_index": output_index,
                        "content_index": 0,
                        "delta": text
                    }),
                )?);
            }
            Kind::ToolCallDelta {
                output_index,
                id,
                name,
                arguments_delta,
                ..
            } => {
                let item_id = self.ensure_tool_output(
                    *output_index,
                    id.as_deref(),
                    name.as_deref(),
                    &mut frames,
                )?;
                if let Some(output) = self.outputs.get_mut(output_index) {
                    output.arguments.push_str(arguments_delta);
                }
                // The first frame of a zero-argument call carries no
                // arguments; real OpenAI emits no empty delta for it.
                if !arguments_delta.is_empty() {
                    frames.push(self.frame(
                        "response.function_call_arguments.delta",
                        json!({
                            "item_id": item_id,
                            "output_index": output_index,
                            "delta": arguments_delta
                        }),
                    )?);
                }
            }
            Kind::Finish {
                output_index,
                reason,
            } => {
                self.incomplete_reason = if matches!(&reason, FinishReason::Length) {
                    Some("max_output_tokens")
                } else if matches!(&reason, FinishReason::ContentFilter) {
                    Some("content_filter")
                } else {
                    None
                };
                if !self.outputs.contains_key(output_index) {
                    self.ensure_message_output(*output_index, &mut frames)?;
                }
                self.close_output(*output_index, &mut frames)?;
            }
            Kind::Usage { .. } => {}
            Kind::SourceExtension { extensions } => {
                // Extensions from another surface have no Responses
                // representation; dropping them keeps the stream alive.
                if extensions.source != Some(Surface::OpenAi) {
                    self.events.push(event);
                    return Ok(frames);
                }
                for (path, value) in extensions.values.clone() {
                    if path.starts_with("/stream/") {
                        let kind = value
                            .get("type")
                            .and_then(Value::as_str)
                            .ok_or_else(|| OpenAiClientEncodeError::InvalidExtension(path.clone()))?
                            .to_owned();
                        frames.push(self.frame(&kind, value)?);
                    }
                }
            }
            Kind::Error { error } => {
                let payload = json!({
                    "response": {
                        "id": self.fallback_id,
                        "object": "response",
                        "status": "failed",
                        "model": self.client_model,
                        "error": {"code": error.provider_code, "message": error.message}
                    }
                });
                frames.push(self.frame("response.failed", payload)?);
            }
            Kind::Done => {
                let terminal_reason = self.incomplete_reason.take();
                let normalized = self.normalized_events_with(event.clone());
                let mut response = encode_response_object(
                    &normalized,
                    &self.client_model,
                    &self.fallback_id,
                    self.created_at,
                )?;
                if let Some(reason) = terminal_reason {
                    response.status = "incomplete".to_owned();
                    response.incomplete_details = Some(serde_json::json!({ "reason": reason }));
                }
                let terminal_event_type = if response.status == "incomplete" {
                    "response.incomplete"
                } else {
                    "response.completed"
                };
                frames.push(self.frame(terminal_event_type, json!({"response": response}))?);
                self.done = true;
            }
        }
        self.events.push(event);
        Ok(frames)
    }

    fn ensure_message_output(
        &mut self,
        output_index: u32,
        frames: &mut Vec<Frame>,
    ) -> Result<String, OpenAiClientEncodeError> {
        if let Some(output) = self.outputs.get(&output_index) {
            return Ok(output.item_id.clone());
        }
        let item_id = format!("msg_{output_index}");
        self.outputs.insert(
            output_index,
            StreamOutput {
                item_id: item_id.clone(),
                ..StreamOutput::default()
            },
        );
        frames.push(self.frame(
            "response.output_item.added",
            json!({
                "output_index": output_index,
                "item": {
                    "id": item_id,
                    "type": "message",
                    "role": "assistant",
                    "status": "in_progress",
                    "content": []
                }
            }),
        )?);
        frames.push(self.frame(
            "response.content_part.added",
            json!({
                "item_id": item_id,
                "output_index": output_index,
                "content_index": 0,
                "part": {"type": "output_text", "text": "", "annotations": []}
            }),
        )?);
        Ok(item_id)
    }

    fn ensure_tool_output(
        &mut self,
        output_index: u32,
        call_id: Option<&str>,
        name: Option<&str>,
        frames: &mut Vec<Frame>,
    ) -> Result<String, OpenAiClientEncodeError> {
        if let Some(output) = self.outputs.get(&output_index) {
            return Ok(output.item_id.clone());
        }
        let item_id = format!("fc_{output_index}");
        let call_id = call_id
            .map(str::to_owned)
            .unwrap_or_else(|| format!("call_{output_index}"));
        let name = name.map(str::to_owned).unwrap_or_else(|| "function".into());
        self.outputs.insert(
            output_index,
            StreamOutput {
                item_id: item_id.clone(),
                tool: true,
                call_id: Some(call_id.clone()),
                name: Some(name.clone()),
                ..StreamOutput::default()
            },
        );
        frames.push(self.frame(
            "response.output_item.added",
            json!({
                "output_index": output_index,
                "item": {
                    "id": item_id,
                    "type": "function_call",
                    "call_id": call_id,
                    "name": name,
                    "arguments": "",
                    "status": "in_progress"
                }
            }),
        )?);
        Ok(item_id)
    }

    fn close_output(
        &mut self,
        output_index: u32,
        frames: &mut Vec<Frame>,
    ) -> Result<(), OpenAiClientEncodeError> {
        let Some(output) = self.outputs.get(&output_index) else {
            return Ok(());
        };
        let item_id = output.item_id.clone();
        if output.tool {
            let arguments = output.arguments.clone();
            let item = json!({
                "id": item_id,
                "type": "function_call",
                "call_id": output.call_id.clone(),
                "name": output.name.clone(),
                "arguments": arguments,
                "status": "completed"
            });
            frames.push(self.frame(
                "response.function_call_arguments.done",
                json!({
                    "item_id": item_id,
                    "output_index": output_index,
                    "arguments": arguments
                }),
            )?);
            frames.push(self.frame(
                "response.output_item.done",
                json!({"output_index": output_index, "item": item}),
            )?);
            return Ok(());
        }
        let text = output.text.clone();
        let refusal = output.refusal.clone();
        let mut content = Vec::new();
        if !text.is_empty() {
            content.push(json!({"type": "output_text", "text": text, "annotations": []}));
        }
        if !refusal.is_empty() {
            content.push(json!({"type": "refusal", "refusal": refusal}));
        }
        let part = if refusal.is_empty() {
            json!({"type": "output_text", "text": text, "annotations": []})
        } else {
            json!({"type": "refusal", "refusal": refusal})
        };
        if refusal.is_empty() {
            frames.push(self.frame(
                "response.output_text.done",
                json!({
                    "item_id": item_id,
                    "output_index": output_index,
                    "content_index": 0,
                    "text": text
                }),
            )?);
        }
        frames.push(self.frame(
            "response.content_part.done",
            json!({
                "item_id": item_id,
                "output_index": output_index,
                "content_index": 0,
                "part": part
            }),
        )?);
        frames.push(self.frame(
            "response.output_item.done",
            json!({
                "output_index": output_index,
                "item": {
                    "id": item_id,
                    "type": "message",
                    "role": "assistant",
                    "status": "completed",
                    "content": content
                }
            }),
        )?);
        Ok(())
    }

    /// Every Responses stream event carries a monotonic `sequence_number`;
    /// clients use it to detect gaps and to order reconnects.
    fn frame(&mut self, kind: &str, mut payload: Value) -> Result<Frame, OpenAiClientEncodeError> {
        let Value::Object(object) = &mut payload else {
            return Err(OpenAiClientEncodeError::InvalidStreamPayload);
        };
        object.insert("type".into(), Value::String(kind.into()));
        object
            .entry("sequence_number")
            .or_insert_with(|| Value::from(self.sequence_number));
        self.sequence_number = self.sequence_number.saturating_add(1);
        Ok(Frame {
            event: Some(kind.into()),
            data: serde_json::to_string(&payload)?,
            id: None,
            retry_ms: None,
        })
    }

    fn normalized_events_with(&self, terminal: Event) -> Vec<Event> {
        self.events
            .iter()
            .chain(std::iter::once(&terminal))
            .filter(|event| {
                !matches!(
                    &event.kind,
                    Kind::SourceExtension { extensions }
                        if extensions.values.keys().all(|path| path.starts_with("/stream/"))
                )
            })
            .enumerate()
            .map(|(sequence, event)| {
                Event::new(sequence.try_into().unwrap_or(u64::MAX), event.kind.clone())
            })
            .collect()
    }
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
        let mut encoder = Encoder::new("route", "response", 0);
        let event = Event::new(
            0,
            Kind::TextDelta {
                output_index: 0,
                text: "x".repeat(16 * 1024 * 1024),
            },
        );

        assert!(matches!(
            encoder.push(event),
            Err(OpenAiClientEncodeError::EventHistoryTooLarge)
        ));
    }
}

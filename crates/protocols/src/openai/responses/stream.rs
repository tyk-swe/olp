use std::collections::{BTreeMap, BTreeSet};

use olp_domain::{
    CanonicalError, CanonicalEvent, CanonicalEventKind, ErrorClass, FinishReason, MessageRole,
    SourceExtensions, Surface,
};
use serde_json::Value;

use crate::sse::{DEFAULT_MAX_EVENT_BYTES, SseDecoder, SseFrame};

use super::super::extensions::escape_json_pointer;
use super::OPENAI_RESPONSES_RAW_OUTPUT_PREFIX;
use super::errors::ResponsesCodecError;
use super::response::{ResponseUsage, canonical_response_usage};

pub struct OpenAiResponsesStreamDecoder {
    sse: SseDecoder,
    sequence: u64,
    response_started: bool,
    started_outputs: BTreeSet<u32>,
    finished_outputs: BTreeSet<u32>,
    done: bool,
}

impl std::fmt::Debug for OpenAiResponsesStreamDecoder {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("OpenAiResponsesStreamDecoder")
            .field("next_sequence", &self.sequence)
            .field("response_started", &self.response_started)
            .field("started_output_count", &self.started_outputs.len())
            .field("finished_output_count", &self.finished_outputs.len())
            .field("done", &self.done)
            .finish_non_exhaustive()
    }
}

impl Default for OpenAiResponsesStreamDecoder {
    fn default() -> Self {
        Self::new()
    }
}

impl OpenAiResponsesStreamDecoder {
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
            started_outputs: BTreeSet::new(),
            finished_outputs: BTreeSet::new(),
            done: false,
        }
    }

    pub fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, ResponsesCodecError> {
        let frames = self.sse.push(bytes)?;
        self.decode_frames(frames)
    }

    pub fn finish(&mut self) -> Result<Vec<CanonicalEvent>, ResponsesCodecError> {
        let frames = self.sse.finish()?;
        let events = self.decode_frames(frames)?;
        if !self.done {
            return Err(ResponsesCodecError::UnexpectedEof);
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
    ) -> Result<Vec<CanonicalEvent>, ResponsesCodecError> {
        let mut events = Vec::new();
        for frame in frames {
            if self.done {
                return Err(ResponsesCodecError::DataAfterDone);
            }
            if frame.data.trim() == "[DONE]" {
                self.finish_open_outputs(&mut events);
                self.emit(&mut events, CanonicalEventKind::Done);
                self.done = true;
                continue;
            }
            let mut value: Value = serde_json::from_str(&frame.data)?;
            let kind = value
                .get("type")
                .and_then(Value::as_str)
                .or(frame.event.as_deref())
                .ok_or_else(|| ResponsesCodecError::InvalidResponse("stream event type".into()))?
                .to_owned();
            self.decode_stream_event(&kind, &mut value, &mut events)?;
        }
        Ok(events)
    }

    fn decode_stream_event(
        &mut self,
        kind: &str,
        value: &mut Value,
        events: &mut Vec<CanonicalEvent>,
    ) -> Result<(), ResponsesCodecError> {
        match kind {
            "response.created" | "response.in_progress" => {
                let response = value.get("response").unwrap_or(value);
                self.ensure_response_started(response, events);
            }
            "response.output_item.added" => {
                self.ensure_response_started(value, events);
                let output_index = stream_index(value, "output_index")?;
                let item = value
                    .get("item")
                    .ok_or_else(|| ResponsesCodecError::InvalidResponse("stream item".into()))?;
                let role = item.get("role").and_then(Value::as_str).map_or(
                    MessageRole::Assistant,
                    |role| match role {
                        "assistant" => MessageRole::Assistant,
                        _ => MessageRole::Assistant,
                    },
                );
                self.ensure_output_started(output_index, role, events);
                if item.get("type").and_then(Value::as_str) == Some("function_call") {
                    self.emit(
                        events,
                        CanonicalEventKind::ToolCallDelta {
                            output_index,
                            tool_index: 0,
                            id: item
                                .get("call_id")
                                .or_else(|| item.get("id"))
                                .and_then(Value::as_str)
                                .map(str::to_owned),
                            name: item.get("name").and_then(Value::as_str).map(str::to_owned),
                            arguments_delta: item
                                .get("arguments")
                                .and_then(Value::as_str)
                                .unwrap_or_default()
                                .to_owned(),
                        },
                    );
                }
            }
            "response.output_text.delta" => {
                let output_index = stream_index(value, "output_index")?;
                self.ensure_output_started(output_index, MessageRole::Assistant, events);
                self.emit(
                    events,
                    CanonicalEventKind::TextDelta {
                        output_index,
                        text: stream_string(value, "delta")?,
                    },
                );
            }
            "response.refusal.delta" => {
                let output_index = stream_index(value, "output_index")?;
                self.ensure_output_started(output_index, MessageRole::Assistant, events);
                self.emit(
                    events,
                    CanonicalEventKind::RefusalDelta {
                        output_index,
                        text: stream_string(value, "delta")?,
                    },
                );
            }
            "response.function_call_arguments.delta" => {
                let output_index = stream_index(value, "output_index")?;
                self.ensure_output_started(output_index, MessageRole::Assistant, events);
                self.emit(
                    events,
                    CanonicalEventKind::ToolCallDelta {
                        output_index,
                        tool_index: 0,
                        id: value
                            .get("item_id")
                            .and_then(Value::as_str)
                            .map(str::to_owned),
                        name: None,
                        arguments_delta: stream_string(value, "delta")?,
                    },
                );
            }
            "response.output_item.done" => {
                let output_index = stream_index(value, "output_index")?;
                if self.finished_outputs.insert(output_index) {
                    let reason = if value
                        .get("item")
                        .and_then(|item| item.get("type"))
                        .and_then(Value::as_str)
                        == Some("function_call")
                    {
                        FinishReason::ToolCalls
                    } else {
                        FinishReason::Stop
                    };
                    self.emit(
                        events,
                        CanonicalEventKind::Finish {
                            output_index,
                            reason,
                        },
                    );
                }
            }
            "response.completed" | "response.incomplete" => {
                let response = value.get("response").unwrap_or(value);
                self.ensure_response_started(response, events);
                self.finish_open_outputs(events);
                let raw_output = raw_response_output_extensions(response)?;
                if !raw_output.is_empty() {
                    self.emit(
                        events,
                        CanonicalEventKind::SourceExtension {
                            extensions: SourceExtensions::new(Surface::OpenAi, raw_output),
                        },
                    );
                }
                if let Some(usage) = response.get("usage") {
                    let usage: ResponseUsage = serde_json::from_value(usage.clone())?;
                    self.emit(
                        events,
                        CanonicalEventKind::Usage {
                            usage: canonical_response_usage(&usage),
                        },
                    );
                }
                self.emit(events, CanonicalEventKind::Done);
                self.done = true;
            }
            "response.failed" | "error" => {
                let error = value
                    .get("response")
                    .and_then(|response| response.get("error"))
                    .or_else(|| value.get("error"))
                    .unwrap_or(value);
                self.emit(
                    events,
                    CanonicalEventKind::Error {
                        error: CanonicalError {
                            class: ErrorClass::Upstream,
                            message: error
                                .get("message")
                                .and_then(Value::as_str)
                                .unwrap_or("OpenAI Responses stream failed")
                                .to_owned(),
                            provider_code: error
                                .get("code")
                                .and_then(Value::as_str)
                                .map(str::to_owned),
                            retryable: false,
                        },
                    },
                );
                self.finish_open_outputs(events);
                self.emit(events, CanonicalEventKind::Done);
                self.done = true;
            }
            // Lifecycle events that contain no new semantic payload.
            "response.content_part.added"
            | "response.content_part.done"
            | "response.output_text.done"
            | "response.function_call_arguments.done" => {}
            _ => {
                self.emit(
                    events,
                    CanonicalEventKind::SourceExtension {
                        extensions: SourceExtensions::new(
                            Surface::OpenAi,
                            BTreeMap::from([(
                                format!("/stream/{}", escape_json_pointer(kind)),
                                value.clone(),
                            )]),
                        ),
                    },
                );
            }
        }
        Ok(())
    }

    fn ensure_response_started(&mut self, value: &Value, events: &mut Vec<CanonicalEvent>) {
        if self.response_started {
            return;
        }
        let response = value.get("response").unwrap_or(value);
        self.emit(
            events,
            CanonicalEventKind::ResponseStart {
                response_id: response
                    .get("id")
                    .and_then(Value::as_str)
                    .map(str::to_owned),
                provider_model: response
                    .get("model")
                    .and_then(Value::as_str)
                    .map(str::to_owned),
            },
        );
        self.response_started = true;
    }

    fn ensure_output_started(
        &mut self,
        output_index: u32,
        role: MessageRole,
        events: &mut Vec<CanonicalEvent>,
    ) {
        if self.started_outputs.insert(output_index) {
            self.emit(
                events,
                CanonicalEventKind::MessageStart { output_index, role },
            );
        }
    }

    fn finish_open_outputs(&mut self, events: &mut Vec<CanonicalEvent>) {
        let unfinished = self
            .started_outputs
            .difference(&self.finished_outputs)
            .copied()
            .collect::<Vec<_>>();
        for output_index in unfinished {
            self.finished_outputs.insert(output_index);
            self.emit(
                events,
                CanonicalEventKind::Finish {
                    output_index,
                    reason: FinishReason::Stop,
                },
            );
        }
    }

    fn emit(&mut self, events: &mut Vec<CanonicalEvent>, kind: CanonicalEventKind) {
        events.push(CanonicalEvent::new(self.sequence, kind));
        self.sequence = self.sequence.saturating_add(1);
    }
}

fn raw_response_output_extensions(
    response: &Value,
) -> Result<BTreeMap<String, Value>, ResponsesCodecError> {
    let Some(output) = response.get("output").and_then(Value::as_array) else {
        return Ok(BTreeMap::new());
    };
    let mut extensions = BTreeMap::new();
    for (index, item) in output.iter().enumerate() {
        let kind = item
            .get("type")
            .and_then(Value::as_str)
            .ok_or_else(|| ResponsesCodecError::InvalidResponse("output item type".to_owned()))?;
        if !matches!(kind, "message" | "function_call") {
            extensions.insert(
                format!("{OPENAI_RESPONSES_RAW_OUTPUT_PREFIX}/{index}"),
                item.clone(),
            );
        }
    }
    Ok(extensions)
}

fn stream_index(value: &Value, field: &'static str) -> Result<u32, ResponsesCodecError> {
    value
        .get(field)
        .and_then(Value::as_u64)
        .and_then(|value| value.try_into().ok())
        .ok_or_else(|| ResponsesCodecError::InvalidResponse(field.into()))
}

fn stream_string(value: &Value, field: &'static str) -> Result<String, ResponsesCodecError> {
    value
        .get(field)
        .and_then(Value::as_str)
        .map(str::to_owned)
        .ok_or_else(|| ResponsesCodecError::InvalidResponse(field.into()))
}

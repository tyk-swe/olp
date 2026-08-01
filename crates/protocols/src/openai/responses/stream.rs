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
use super::response::{ResponseUsage, canonical_response_usage, response_incomplete_reason};

pub struct OpenAiResponsesStreamDecoder {
    sse: SseDecoder,
    sequence: u64,
    response_started: bool,
    started_outputs: BTreeSet<u32>,
    finished_outputs: BTreeMap<u32, Option<FinishReason>>,
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
            finished_outputs: BTreeMap::new(),
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
                self.require_response_started()?;
                self.finish_open_outputs(None, &mut events);
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
            "response.created" => {
                if self.response_started {
                    return Err(ResponsesCodecError::InvalidResponse(
                        "duplicate response.created".into(),
                    ));
                }
                let response = value.get("response").unwrap_or(value);
                self.ensure_response_started(response, events);
            }
            "response.in_progress" => self.require_response_started()?,
            "response.output_item.added" => {
                self.require_response_started()?;
                let output_index = stream_index(value, "output_index")?;
                let item = value
                    .get("item")
                    .ok_or_else(|| ResponsesCodecError::InvalidResponse("stream item".into()))?;
                match stream_string(item, "type")?.as_str() {
                    "message" => {
                        if stream_string(item, "role")? != "assistant" {
                            return Err(ResponsesCodecError::InvalidResponse(
                                "stream message role".into(),
                            ));
                        }
                        self.start_output(output_index, events)?;
                    }
                    "function_call" => {
                        self.start_output(output_index, events)?;
                        self.emit(
                            events,
                            CanonicalEventKind::ToolCallDelta {
                                output_index,
                                tool_index: 0,
                                id: Some(stream_string(item, "call_id")?),
                                name: Some(stream_string(item, "name")?),
                                arguments_delta: item
                                    .get("arguments")
                                    .and_then(Value::as_str)
                                    .unwrap_or_default()
                                    .to_owned(),
                            },
                        );
                    }
                    _ => self.preserve_stream_event(kind, value, events),
                }
            }
            "response.output_text.delta" => {
                self.require_response_started()?;
                let output_index = stream_index(value, "output_index")?;
                self.require_output_open(output_index)?;
                self.emit(
                    events,
                    CanonicalEventKind::TextDelta {
                        output_index,
                        text: stream_string(value, "delta")?,
                    },
                );
            }
            "response.refusal.delta" => {
                self.require_response_started()?;
                let output_index = stream_index(value, "output_index")?;
                self.require_output_open(output_index)?;
                self.emit(
                    events,
                    CanonicalEventKind::RefusalDelta {
                        output_index,
                        text: stream_string(value, "delta")?,
                    },
                );
            }
            "response.function_call_arguments.delta" => {
                self.require_response_started()?;
                let output_index = stream_index(value, "output_index")?;
                self.require_output_open(output_index)?;
                self.emit(
                    events,
                    CanonicalEventKind::ToolCallDelta {
                        output_index,
                        tool_index: 0,
                        id: None,
                        name: None,
                        arguments_delta: stream_string(value, "delta")?,
                    },
                );
            }
            "response.output_item.done" => {
                self.require_response_started()?;
                let output_index = stream_index(value, "output_index")?;
                let item = value
                    .get("item")
                    .ok_or_else(|| ResponsesCodecError::InvalidResponse("stream item".into()))?;
                let reason = match stream_string(item, "type")?.as_str() {
                    "message" => Some(FinishReason::Stop),
                    "function_call" => Some(FinishReason::ToolCalls),
                    _ => None,
                };
                if let Some(reason) = reason {
                    let reason = match item.get("status") {
                        None => Some(reason),
                        Some(Value::String(status)) if status == "completed" => Some(reason),
                        Some(Value::String(status)) if status == "incomplete" => None,
                        _ => {
                            return Err(ResponsesCodecError::InvalidResponse(
                                "Responses output item status".into(),
                            ));
                        }
                    };
                    if !self.started_outputs.contains(&output_index)
                        || self.finished_outputs.insert(output_index, reason).is_some()
                    {
                        return Err(ResponsesCodecError::InvalidResponse(
                            "Responses output lifecycle".into(),
                        ));
                    }
                } else {
                    self.preserve_stream_event(kind, value, events);
                }
            }
            "response.completed" | "response.incomplete" => {
                self.require_response_started()?;
                let response = value.get("response").unwrap_or(value);
                let incomplete = kind == "response.incomplete";
                if let Some(status) = response.get("status") {
                    let status = status.as_str().ok_or_else(|| {
                        ResponsesCodecError::InvalidResponse("response status".into())
                    })?;
                    if (status == "incomplete") != incomplete {
                        return Err(ResponsesCodecError::InvalidResponse(
                            "response terminal status".into(),
                        ));
                    }
                }
                let terminal_reason = incomplete
                    .then(|| response_incomplete_reason(response.get("incomplete_details")));
                self.finish_open_outputs(terminal_reason.as_ref(), events);
                let mut extensions = raw_response_output_extensions(response)?;
                if incomplete {
                    extensions.insert("/status".into(), Value::String("incomplete".into()));
                    if let Some(details) = response.get("incomplete_details") {
                        extensions.insert("/incomplete_details".into(), details.clone());
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
                self.require_response_started()?;
                let error = value
                    .get("response")
                    .and_then(|response| response.get("error"))
                    .or_else(|| value.get("error"))
                    .unwrap_or(value);
                let provider_code = error.get("code").and_then(Value::as_str).map(str::to_owned);
                let retryable = crate::openai::error_signals_rate_limit(
                    provider_code.as_deref(),
                    error.get("type").and_then(Value::as_str),
                );
                self.emit(
                    events,
                    CanonicalEventKind::Error {
                        error: CanonicalError {
                            class: if retryable {
                                ErrorClass::RateLimit
                            } else {
                                ErrorClass::Upstream
                            },
                            message: error
                                .get("message")
                                .and_then(Value::as_str)
                                .unwrap_or("OpenAI Responses stream failed")
                                .to_owned(),
                            provider_code,
                            retryable,
                        },
                    },
                );
                self.finish_open_outputs(Some(&FinishReason::Error), events);
                self.emit(events, CanonicalEventKind::Done);
                self.done = true;
            }
            // Lifecycle events that contain no new semantic payload.
            "response.content_part.added"
            | "response.content_part.done"
            | "response.output_text.done"
            | "response.function_call_arguments.done" => {}
            _ => self.preserve_stream_event(kind, value, events),
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

    fn preserve_stream_event(
        &mut self,
        kind: &str,
        value: &Value,
        events: &mut Vec<CanonicalEvent>,
    ) {
        self.emit(
            events,
            CanonicalEventKind::SourceExtension {
                extensions: SourceExtensions::new(
                    Surface::OpenAi,
                    BTreeMap::from([(
                        format!("/stream/{}/{}", escape_json_pointer(kind), self.sequence),
                        value.clone(),
                    )]),
                ),
            },
        );
    }

    fn require_response_started(&self) -> Result<(), ResponsesCodecError> {
        if self.response_started {
            Ok(())
        } else {
            Err(ResponsesCodecError::InvalidResponse(
                "Responses event before response start".into(),
            ))
        }
    }

    fn start_output(
        &mut self,
        output_index: u32,
        events: &mut Vec<CanonicalEvent>,
    ) -> Result<(), ResponsesCodecError> {
        if !self.started_outputs.insert(output_index) {
            return Err(ResponsesCodecError::InvalidResponse(
                "duplicate Responses output item".into(),
            ));
        }
        self.emit(
            events,
            CanonicalEventKind::MessageStart {
                output_index,
                role: MessageRole::Assistant,
            },
        );
        Ok(())
    }

    fn require_output_open(&self, output_index: u32) -> Result<(), ResponsesCodecError> {
        if self.started_outputs.contains(&output_index)
            && !self.finished_outputs.contains_key(&output_index)
        {
            Ok(())
        } else {
            Err(ResponsesCodecError::InvalidResponse(
                "Responses delta outside an open output".into(),
            ))
        }
    }

    fn finish_open_outputs(
        &mut self,
        terminal_reason: Option<&FinishReason>,
        events: &mut Vec<CanonicalEvent>,
    ) {
        let outputs = self.started_outputs.iter().copied().collect::<Vec<_>>();
        for output_index in outputs {
            let reason = self
                .finished_outputs
                .get(&output_index)
                .cloned()
                .flatten()
                .or_else(|| terminal_reason.cloned())
                .unwrap_or(FinishReason::Stop);
            self.emit(
                events,
                CanonicalEventKind::Finish {
                    output_index,
                    reason,
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn streamed_rate_limit_error_is_retryable() {
        let mut decoder = OpenAiResponsesStreamDecoder::new();
        let events = decoder
            .push(
                concat!(
                    "event: response.created\n",
                    "data: {\"type\":\"response.created\"}\n\n",
                    "event: response.failed\n",
                    "data: {\"type\":\"response.failed\",\"response\":{\"error\":",
                    "{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",",
                    "\"message\":\"slow down\"}}}\n\n"
                )
                .as_bytes(),
            )
            .unwrap();

        let error = events
            .iter()
            .find_map(|event| match &event.kind {
                CanonicalEventKind::Error { error } => Some(error),
                _ => None,
            })
            .expect("failed response event must emit a canonical error");
        assert_eq!(error.class, ErrorClass::RateLimit);
        assert!(error.retryable);
    }

    #[test]
    fn rejects_invalid_output_lifecycle() {
        let frame = |value: Value| format!("data: {value}\n\n");
        let created = frame(serde_json::json!({"type": "response.created"}));
        let added = frame(serde_json::json!({
            "type": "response.output_item.added", "output_index": 0,
            "item": {"type": "message", "role": "assistant"}
        }));
        let delta = frame(serde_json::json!({
            "type": "response.output_text.delta", "output_index": 0, "delta": "x"
        }));
        let done = frame(serde_json::json!({
            "type": "response.output_item.done", "output_index": 0,
            "item": {"type": "message"}
        }));
        let cases = [
            delta.clone(),
            format!("{created}{delta}"),
            format!("{created}{done}"),
            format!("{created}{added}{delta}{done}{delta}"),
            format!("{created}{created}"),
        ];
        for wire in cases {
            assert!(
                OpenAiResponsesStreamDecoder::new()
                    .push(wire.as_bytes())
                    .is_err()
            );
        }
    }
}

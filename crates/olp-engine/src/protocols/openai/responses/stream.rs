use std::collections::{BTreeMap, BTreeSet};

use crate::domain::{
    CanonicalError, CanonicalEvent, CanonicalEventKind, ErrorClass, FinishReason, MessageRole,
    SourceExtensions, Surface, UsageObservation,
};
use serde_json::Value;

use crate::protocols::sse::{DEFAULT_MAX_EVENT_BYTES, SseDecoder, SseFrame};

use super::super::extensions::escape_json_pointer;
use super::OPENAI_RESPONSES_RAW_OUTPUT_PREFIX;
use super::errors::ResponsesCodecError;
use super::response::{
    ResponseUsage, collect_response_usage_extensions, decode_response_usage,
    incomplete_finish_reason,
};

pub struct OpenAiResponsesStreamDecoder {
    sse: SseDecoder,
    sequence: u64,
    response_started: bool,
    started_outputs: BTreeSet<u32>,
    finished_outputs: BTreeSet<u32>,
    incomplete_outputs: BTreeSet<u32>,
    output_kinds: BTreeMap<u32, StreamOutputKind>,
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
            incomplete_outputs: BTreeSet::new(),
            output_kinds: BTreeMap::new(),
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
            if frame.data.trim() == "[DONE]" {
                if !self.done {
                    return Err(ResponsesCodecError::InvalidStreamState);
                }
                continue;
            }
            if self.done {
                return Err(ResponsesCodecError::DataAfterDone);
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
                let response = value.get("response").unwrap_or(value);
                if self.response_started {
                    return Err(ResponsesCodecError::InvalidStreamState);
                }
                self.ensure_response_started(response, events);
            }
            "response.in_progress" => {
                if !self.response_started {
                    return Err(ResponsesCodecError::InvalidStreamState);
                }
            }
            "response.output_item.added" => {
                if !self.response_started {
                    return Err(ResponsesCodecError::InvalidStreamState);
                }
                let output_index = stream_index(value, "output_index")?;
                if self.started_outputs.contains(&output_index) {
                    return Err(ResponsesCodecError::InvalidStreamState);
                }
                let item = value
                    .get("item")
                    .ok_or_else(|| ResponsesCodecError::InvalidResponse("stream item".into()))?;
                if item.get("status").and_then(Value::as_str) != Some("in_progress") {
                    return Err(ResponsesCodecError::InvalidStreamState);
                }
                let output_kind = match item.get("type").and_then(Value::as_str) {
                    Some("message") => {
                        if item.get("role").and_then(Value::as_str) != Some("assistant") {
                            return Err(ResponsesCodecError::InvalidStreamState);
                        }
                        StreamOutputKind::Message
                    }
                    Some("function_call") => StreamOutputKind::FunctionCall,
                    Some(_) => return Ok(()),
                    None => {
                        return Err(ResponsesCodecError::InvalidResponse(
                            "output item type".into(),
                        ));
                    }
                };
                self.output_kinds.insert(output_index, output_kind);
                self.ensure_output_started(output_index, MessageRole::Assistant, events);
                if output_kind == StreamOutputKind::FunctionCall {
                    self.emit(
                        events,
                        CanonicalEventKind::ToolCallDelta {
                            output_index,
                            tool_index: 0,
                            id: Some(
                                item.get("call_id")
                                    .and_then(Value::as_str)
                                    .ok_or_else(|| {
                                        ResponsesCodecError::InvalidResponse("call_id".into())
                                    })?
                                    .to_owned(),
                            ),
                            name: Some(
                                item.get("name")
                                    .and_then(Value::as_str)
                                    .ok_or_else(|| {
                                        ResponsesCodecError::InvalidResponse("name".into())
                                    })?
                                    .to_owned(),
                            ),
                            arguments_delta: item
                                .get("arguments")
                                .and_then(Value::as_str)
                                .ok_or_else(|| {
                                    ResponsesCodecError::InvalidResponse("arguments".into())
                                })?
                                .to_owned(),
                        },
                    );
                }
            }
            "response.output_text.delta" => {
                let output_index = stream_index(value, "output_index")?;
                self.require_output_open(output_index, StreamOutputKind::Message)?;
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
                self.require_output_open(output_index, StreamOutputKind::Message)?;
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
                self.require_output_open(output_index, StreamOutputKind::FunctionCall)?;
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
                let item = value
                    .get("item")
                    .ok_or_else(|| ResponsesCodecError::InvalidResponse("stream item".into()))?;
                let status = item.get("status").and_then(Value::as_str);
                if !matches!(status, Some("completed" | "incomplete")) {
                    return Err(ResponsesCodecError::InvalidStreamState);
                }
                let item_kind = match item.get("type").and_then(Value::as_str) {
                    Some("message") => StreamOutputKind::Message,
                    Some("function_call") => StreamOutputKind::FunctionCall,
                    Some(_) => return Ok(()),
                    None => {
                        return Err(ResponsesCodecError::InvalidResponse(
                            "output item type".into(),
                        ));
                    }
                };
                if self.finished_outputs.contains(&output_index)
                    || self.output_kinds.get(&output_index) != Some(&item_kind)
                {
                    return Err(ResponsesCodecError::InvalidStreamState);
                }
                self.finished_outputs.insert(output_index);
                if status == Some("incomplete") {
                    self.incomplete_outputs.insert(output_index);
                } else {
                    let reason = match item_kind {
                        StreamOutputKind::FunctionCall => FinishReason::ToolCalls,
                        StreamOutputKind::Message => FinishReason::Stop,
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
            "response.completed" => {
                let response = value.get("response").unwrap_or(value);
                if response.get("status").and_then(Value::as_str) != Some("completed")
                    || present(response, "error")
                    || present(response, "incomplete_details")
                {
                    return Err(ResponsesCodecError::InvalidStreamState);
                }
                self.require_completed_terminal_state(response)?;
                validate_terminal_output(response, &self.output_kinds, &self.incomplete_outputs)?;
                let mut extensions = terminal_response_extensions(response)?;
                extensions.extend(raw_response_output_extensions(response)?);
                let observation = self.emit_terminal_usage(response, events, &mut extensions)?;
                if !extensions.is_empty() {
                    self.emit(
                        events,
                        CanonicalEventKind::SourceExtension {
                            extensions: SourceExtensions::new(Surface::OpenAi, extensions),
                        },
                    );
                }
                self.emit_done(events, observation);
                self.done = true;
            }
            "response.incomplete" => {
                let response = value.get("response").unwrap_or(value);
                if response.get("status").and_then(Value::as_str) != Some("incomplete")
                    || present(response, "error")
                {
                    return Err(ResponsesCodecError::InvalidStreamState);
                }
                self.require_terminal_state()?;
                validate_terminal_output(response, &self.output_kinds, &self.incomplete_outputs)?;
                let reason = incomplete_finish_reason(response.get("incomplete_details"))?;
                let incomplete = self.incomplete_outputs.iter().copied().collect::<Vec<_>>();
                for output_index in incomplete {
                    self.emit(
                        events,
                        CanonicalEventKind::Finish {
                            output_index,
                            reason: reason.clone(),
                        },
                    );
                }
                let mut extensions = terminal_response_extensions(response)?;
                extensions.extend(raw_response_output_extensions(response)?);
                let observation = self.emit_terminal_usage(response, events, &mut extensions)?;
                if !extensions.is_empty() {
                    self.emit(
                        events,
                        CanonicalEventKind::SourceExtension {
                            extensions: SourceExtensions::new(Surface::OpenAi, extensions),
                        },
                    );
                }
                self.emit_done(events, observation);
                self.done = true;
            }
            "response.failed" | "response.cancelled" | "error" => {
                let response = value.get("response").unwrap_or(value);
                if kind != "error"
                    && (response.get("status").and_then(Value::as_str)
                        != Some(if kind == "response.failed" {
                            "failed"
                        } else {
                            "cancelled"
                        })
                        || present(response, "incomplete_details")
                        || (kind == "response.failed" && !present(response, "error"))
                        || (kind == "response.cancelled" && present(response, "error")))
                {
                    return Err(ResponsesCodecError::InvalidStreamState);
                }
                let error = value
                    .get("response")
                    .and_then(|response| response.get("error"))
                    .or_else(|| value.get("error"))
                    .unwrap_or(value);
                let provider_code = error.get("code").and_then(Value::as_str).map(str::to_owned);
                let retryable = crate::protocols::openai::error_signals_rate_limit(
                    provider_code.as_deref(),
                    error.get("type").and_then(Value::as_str),
                );
                let mut extensions = if kind == "error" {
                    BTreeMap::new()
                } else {
                    terminal_response_extensions(response)?
                };
                let observation = self.emit_terminal_usage(response, events, &mut extensions)?;
                if !extensions.is_empty() {
                    self.emit(
                        events,
                        CanonicalEventKind::SourceExtension {
                            extensions: SourceExtensions::new(Surface::OpenAi, extensions),
                        },
                    );
                }
                let mut event = CanonicalEvent::new(
                    self.sequence,
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
                if let Some(observation) = observation {
                    event = event.with_usage_observation(observation);
                }
                events.push(event);
                self.sequence = self.sequence.saturating_add(1);
                self.emit_done(events, None);
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

    fn require_output_open(
        &self,
        output_index: u32,
        expected_kind: StreamOutputKind,
    ) -> Result<(), ResponsesCodecError> {
        if self.finished_outputs.contains(&output_index)
            || self.output_kinds.get(&output_index) != Some(&expected_kind)
        {
            return Err(ResponsesCodecError::InvalidStreamState);
        }
        Ok(())
    }

    fn require_terminal_state(&self) -> Result<(), ResponsesCodecError> {
        if !self.response_started || self.started_outputs != self.finished_outputs {
            return Err(ResponsesCodecError::InvalidStreamState);
        }
        Ok(())
    }

    fn require_completed_terminal_state(
        &self,
        response: &Value,
    ) -> Result<(), ResponsesCodecError> {
        self.require_terminal_state()?;
        if !self.incomplete_outputs.is_empty()
            || !matches!(
                response.get("output").and_then(Value::as_array),
                Some(output) if !output.is_empty()
            )
        {
            return Err(ResponsesCodecError::InvalidStreamState);
        }
        Ok(())
    }

    fn emit(&mut self, events: &mut Vec<CanonicalEvent>, kind: CanonicalEventKind) {
        events.push(CanonicalEvent::new(self.sequence, kind));
        self.sequence = self.sequence.saturating_add(1);
    }

    fn emit_terminal_usage(
        &mut self,
        response: &Value,
        events: &mut Vec<CanonicalEvent>,
        extensions: &mut BTreeMap<String, Value>,
    ) -> Result<Option<UsageObservation>, ResponsesCodecError> {
        let Some(usage) = response.get("usage").filter(|usage| !usage.is_null()) else {
            return Ok(None);
        };
        let usage: ResponseUsage = serde_json::from_value(usage.clone())?;
        let (canonical, observation) = decode_response_usage(&usage)?;
        if let Some(canonical) = canonical {
            collect_response_usage_extensions(&usage, extensions);
            self.emit(events, CanonicalEventKind::Usage { usage: canonical });
            Ok(None)
        } else {
            Ok(Some(observation))
        }
    }

    fn emit_done(
        &mut self,
        events: &mut Vec<CanonicalEvent>,
        observation: Option<UsageObservation>,
    ) {
        let mut event = CanonicalEvent::new(self.sequence, CanonicalEventKind::Done);
        if let Some(observation) = observation {
            event = event.with_usage_observation(observation);
        }
        events.push(event);
        self.sequence = self.sequence.saturating_add(1);
    }
}

fn terminal_response_extensions(
    response: &Value,
) -> Result<BTreeMap<String, Value>, ResponsesCodecError> {
    let object = response
        .as_object()
        .ok_or_else(|| ResponsesCodecError::InvalidResponse("terminal response".into()))?;
    let mut extensions = BTreeMap::new();
    for (key, value) in object {
        match key.as_str() {
            "status" => {
                extensions.insert("/status".into(), value.clone());
            }
            "incomplete_details" => {
                extensions.insert("/incomplete_details".into(), value.clone());
            }
            "type" | "id" | "object" | "created_at" | "model" | "output" | "usage" | "error" => {}
            _ => {
                extensions.insert(format!("/{}", escape_json_pointer(key)), value.clone());
            }
        }
    }
    Ok(extensions)
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum StreamOutputKind {
    Message,
    FunctionCall,
}

fn validate_terminal_output(
    response: &Value,
    output_kinds: &BTreeMap<u32, StreamOutputKind>,
    incomplete_outputs: &BTreeSet<u32>,
) -> Result<(), ResponsesCodecError> {
    let Some(output) = response.get("output").and_then(Value::as_array) else {
        return Ok(());
    };
    for (index, item) in output.iter().enumerate() {
        let kind = match item.get("type").and_then(Value::as_str) {
            Some("message") => Some(StreamOutputKind::Message),
            Some("function_call") => Some(StreamOutputKind::FunctionCall),
            Some(_) => None,
            None => {
                return Err(ResponsesCodecError::InvalidResponse(
                    "output item type".into(),
                ));
            }
        };
        if let Some(kind) = kind {
            let index =
                u32::try_from(index).map_err(|_| ResponsesCodecError::TooManyOutputItems)?;
            let expected_status = if incomplete_outputs.contains(&index) {
                "incomplete"
            } else {
                "completed"
            };
            if item.get("status").and_then(Value::as_str) != Some(expected_status)
                || output_kinds.get(&index) != Some(&kind)
            {
                return Err(ResponsesCodecError::InvalidStreamState);
            }
        }
    }
    Ok(())
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

fn present(value: &Value, field: &str) -> bool {
    value.get(field).is_some_and(|value| !value.is_null())
}

#[cfg(test)]
mod tests {
    use crate::domain::validate_event_sequence;
    use serde_json::json;

    use super::*;

    fn data(value: Value) -> String {
        format!("data: {value}\n\n")
    }

    #[test]
    fn streamed_rate_limit_error_is_retryable() {
        let mut decoder = OpenAiResponsesStreamDecoder::new();
        let events = decoder
            .push(
                concat!(
                    "event: response.created\n",
                    "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n",
                    "event: response.failed\n",
                    "data: {\"type\":\"response.failed\",\"response\":{\"error\":",
                    "{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",",
                    "\"message\":\"slow down\"},\"status\":\"failed\"}}\n\n"
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
    fn completed_stream_aggregates_tools_usage_and_raw_output_in_order() {
        let wire = concat!(
            "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"private-model\"}}\n\n",
            "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{\\\"city\\\":\"}}\n\n",
            "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"call_1\",\"delta\":\"\\\"Paris\\\"}\"}\n\n",
            "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"status\":\"completed\"}}\n\n",
            "data: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"reasoning\",\"status\":\"in_progress\"}}\n\n",
            "data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"reasoning\",\"status\":\"completed\",\"encrypted_content\":\"opaque\"}}\n\n",
            "data: {\"type\":\"future/event~name\",\"vendor\":{\"retained\":true}}\n\n",
            "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"status\":\"completed\"},{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\"}],\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7,\"input_tokens_details\":{\"cached_tokens\":3,\"vendor_cached\":true},\"output_tokens_details\":{\"vendor_reasoning\":true},\"vendor_usage\":true},\"vendor_terminal\":true}}\n\n"
        );
        let mut decoder = OpenAiResponsesStreamDecoder::new();
        let events = decoder.push(wire.as_bytes()).unwrap();

        validate_event_sequence(&events).unwrap();
        assert!(decoder.is_done());
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::ResponseStart {
                response_id: Some(id),
                provider_model: Some(model),
            } if id == "resp_1" && model == "private-model"
        )));
        let argument_fragments = events
            .iter()
            .filter_map(|event| match &event.kind {
                CanonicalEventKind::ToolCallDelta {
                    arguments_delta, ..
                } => Some(arguments_delta.as_str()),
                _ => None,
            })
            .collect::<String>();
        assert_eq!(argument_fragments, "{\"city\":\"Paris\"}");
        assert_eq!(
            events
                .iter()
                .filter(|event| matches!(event.kind, CanonicalEventKind::MessageStart { .. }))
                .count(),
            1
        );
        assert!(events.iter().any(|event| matches!(
            event.kind,
            CanonicalEventKind::Finish {
                reason: FinishReason::ToolCalls,
                ..
            }
        )));
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::Usage { usage }
                if usage.cached_input_tokens == Some(3)
                    && usage.reasoning_tokens.is_none()
        )));
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::SourceExtension { extensions }
                if extensions.values.contains_key("/stream/future~1event~0name")
        )));
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::SourceExtension { extensions }
                if extensions.values.contains_key(&format!(
                    "{OPENAI_RESPONSES_RAW_OUTPUT_PREFIX}/1"
                ))
        )));
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::SourceExtension { extensions }
                if extensions.values.get("/vendor_terminal") == Some(&json!(true))
                    && extensions.values.get("/usage/vendor_usage") == Some(&json!(true))
                    && extensions
                        .values
                        .get("/usage/input_tokens_details/vendor_cached")
                        == Some(&json!(true))
                    && extensions
                        .values
                        .get("/usage/output_tokens_details")
                        == Some(&json!({"vendor_reasoning": true}))
        )));
        let mut encoder = crate::protocols::openai::client::OpenAiResponsesStreamEncoder::new(
            "route", "fallback", 0,
        );
        let encoded = events
            .into_iter()
            .flat_map(|event| encoder.push(event).unwrap())
            .collect::<Vec<_>>();
        let terminal: Value = serde_json::from_str(&encoded.last().unwrap().data).unwrap();
        assert_eq!(terminal["response"]["vendor_terminal"], true);
        assert_eq!(terminal["response"]["usage"]["vendor_usage"], true);
        assert_eq!(
            terminal["response"]["usage"]["output_tokens_details"],
            json!({"vendor_reasoning": true})
        );
        assert!(decoder.finish().unwrap().is_empty());
    }

    #[test]
    fn native_incomplete_stream_round_trips_terminal_state() {
        let wire = [
            data(json!({"type": "response.created", "response": {"id": "r"}})),
            data(json!({
                "type": "response.output_item.added",
                "output_index": 0,
                "item": {"type": "message", "role": "assistant", "status": "in_progress"}
            })),
            data(json!({
                "type": "response.output_text.delta",
                "output_index": 0,
                "delta": "partial"
            })),
            data(json!({
                "type": "response.output_item.done",
                "output_index": 0,
                "item": {"type": "message", "status": "incomplete"}
            })),
            data(json!({
                "type": "response.incomplete",
                "response": {
                    "status": "incomplete",
                    "incomplete_details": {"reason": "content_filter", "vendor": true},
                    "output": [{"type": "message", "status": "incomplete"}],
                    "vendor_terminal": "kept"
                }
            })),
        ]
        .concat();
        let mut decoder = OpenAiResponsesStreamDecoder::new();
        let events = decoder.push(wire.as_bytes()).unwrap();

        let mut encoder = crate::protocols::openai::client::OpenAiResponsesStreamEncoder::new(
            "route", "fallback", 0,
        );
        let frames = events
            .into_iter()
            .flat_map(|event| encoder.push(event).unwrap())
            .collect::<Vec<_>>();
        assert_eq!(
            frames
                .iter()
                .find(|frame| frame.event.as_deref() == Some("response.output_item.done"))
                .map(
                    |frame| serde_json::from_str::<Value>(&frame.data).unwrap()["item"]["status"]
                        .clone()
                ),
            Some(json!("incomplete"))
        );
        let terminal = frames.last().unwrap();
        assert_eq!(terminal.event.as_deref(), Some("response.incomplete"));
        let terminal: Value = serde_json::from_str(&terminal.data).unwrap();
        assert_eq!(
            terminal["response"]["incomplete_details"],
            json!({"reason": "content_filter", "vendor": true})
        );
        assert_eq!(terminal["response"]["vendor_terminal"], "kept");
    }

    #[test]
    fn partial_stream_usage_is_accounting_only_and_drops_usage_extensions() {
        let wire = [
            data(json!({"type": "response.created", "response": {"id": "r"}})),
            data(json!({
                "type": "response.output_item.added",
                "output_index": 0,
                "item": {"type": "message", "role": "assistant", "status": "in_progress"}
            })),
            data(json!({
                "type": "response.output_item.done",
                "output_index": 0,
                "item": {"type": "message", "status": "completed"}
            })),
            data(json!({
                "type": "response.completed",
                "response": {
                    "status": "completed",
                    "output": [{"type": "message", "status": "completed"}],
                    "usage": {"input_tokens": 3, "vendor_usage": true},
                    "vendor_terminal": true
                }
            })),
        ]
        .concat();
        let mut decoder = OpenAiResponsesStreamDecoder::new();
        let events = decoder.push(wire.as_bytes()).unwrap();

        assert!(
            !events
                .iter()
                .any(|event| matches!(event.kind, CanonicalEventKind::Usage { .. }))
        );
        assert_eq!(
            events
                .last()
                .unwrap()
                .usage_observation
                .unwrap()
                .input_tokens,
            Some(3)
        );
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::SourceExtension { extensions }
                if extensions.values.get("/vendor_terminal") == Some(&json!(true))
                    && !extensions.values.keys().any(|path| path.starts_with("/usage"))
        )));
    }

    #[test]
    fn done_marker_is_framing_only_after_semantic_terminal() {
        let wire = concat!(
            "event: response.created\n",
            "data: {\"response\":{\"id\":\"resp_2\",\"model\":\"private\"}}\n\n",
            "data: {\"type\":\"response.output_item.added\",\"output_index\":0,",
            "\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\"}}\n\n",
            "data: {\"type\":\"response.refusal.delta\",\"output_index\":0,",
            "\"delta\":\"cannot\"}\n\n",
            "data: {\"type\":\"response.output_item.done\",\"output_index\":0,",
            "\"item\":{\"type\":\"message\",\"status\":\"completed\"}}\n\n",
            "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",",
            "\"output\":[{\"type\":\"message\",\"status\":\"completed\"}]}}\n\n",
            "data: [DONE]\n\n"
        );
        let mut decoder = OpenAiResponsesStreamDecoder::new();
        let events = decoder.push(wire.as_bytes()).unwrap();

        validate_event_sequence(&events).unwrap();
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::RefusalDelta { output_index: 0, text } if text == "cannot"
        )));
        assert!(events.iter().any(|event| matches!(
            event.kind,
            CanonicalEventKind::Finish {
                output_index: 0,
                reason: FinishReason::Stop,
            }
        )));
        assert!(matches!(
            decoder.push(data(json!({"type": "response.created"})).as_bytes()),
            Err(ResponsesCodecError::DataAfterDone)
        ));
    }

    #[test]
    fn failed_stream_reports_error_without_inventing_output_finish() {
        let wire = [
            data(json!({"type": "response.created", "response": {"id": "r"}})),
            data(json!({
                "type": "response.output_item.added",
                "output_index": 1,
                "item": {"type": "message", "role": "assistant", "status": "in_progress"}
            })),
            data(json!({
                "type": "response.output_text.delta",
                "output_index": 1,
                "delta": "partial"
            })),
            data(json!({"type": "error", "error": {"message": "failed"}})),
        ]
        .concat();
        let mut decoder = OpenAiResponsesStreamDecoder::new();
        let events = decoder.push(wire.as_bytes()).unwrap();

        validate_event_sequence(&events).unwrap();
        let error = events
            .iter()
            .find_map(|event| match &event.kind {
                CanonicalEventKind::Error { error } => Some(error),
                _ => None,
            })
            .unwrap();
        assert_eq!(error.class, ErrorClass::Upstream);
        assert_eq!(error.message, "failed");
        assert_eq!(error.provider_code, None);
        assert!(!error.retryable);
        assert!(
            !events
                .iter()
                .any(|event| matches!(event.kind, CanonicalEventKind::Finish { .. }))
        );
        assert!(matches!(
            events.last().unwrap().kind,
            CanonicalEventKind::Done
        ));

        let mut decoder = OpenAiResponsesStreamDecoder::new();
        let events = decoder
            .push(
                data(json!({
                    "type": "response.failed",
                    "response": {
                        "id": "r",
                        "object": "response",
                        "status": "failed",
                        "error": {"code": "upstream_error", "message": "failed"},
                        "usage": {
                            "input_tokens": 2,
                            "output_tokens": 1,
                            "total_tokens": 3,
                            "input_tokens_details": {"vendor_detail": true},
                            "vendor_usage": "kept"
                        },
                        "vendor_response": "kept"
                    }
                }))
                .as_bytes(),
            )
            .unwrap();
        let usage_index = events
            .iter()
            .position(|event| matches!(&event.kind, CanonicalEventKind::Usage { .. }))
            .unwrap();
        let error_index = events
            .iter()
            .position(|event| matches!(&event.kind, CanonicalEventKind::Error { .. }))
            .unwrap();
        assert!(usage_index < error_index);
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::SourceExtension { extensions }
                if extensions.values["/status"] == "failed"
                    && extensions.values["/vendor_response"] == "kept"
                    && extensions.values["/usage/vendor_usage"] == "kept"
                    && extensions.values["/usage/input_tokens_details"]["vendor_detail"] == true
        )));

        let mut decoder = OpenAiResponsesStreamDecoder::new();
        let events = decoder
            .push(
                data(json!({
                    "type": "response.failed",
                    "response": {
                        "status": "failed",
                        "error": {"code": "upstream_error", "message": "failed"},
                        "usage": {"input_tokens": 2, "vendor_usage": "dropped"},
                        "vendor_response": "kept"
                    }
                }))
                .as_bytes(),
            )
            .unwrap();
        let error = events
            .iter()
            .find(|event| matches!(&event.kind, CanonicalEventKind::Error { .. }))
            .unwrap();
        assert!(error.usage_observation.is_some());
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::SourceExtension { extensions }
                if extensions.values["/vendor_response"] == "kept"
                    && !extensions.values.keys().any(|path| path.starts_with("/usage"))
        )));
    }

    #[test]
    fn incomplete_stream_may_terminate_without_output() {
        let wire = [
            data(json!({"type": "response.created", "response": {"id": "r"}})),
            data(json!({
                "type": "response.incomplete",
                "response": {
                    "id": "r",
                    "object": "response",
                    "status": "incomplete",
                    "output": [],
                    "incomplete_details": {"reason": "max_output_tokens"},
                    "usage": null
                }
            })),
        ]
        .concat();
        let mut decoder = OpenAiResponsesStreamDecoder::new();
        let events = decoder.push(wire.as_bytes()).unwrap();

        validate_event_sequence(&events).unwrap();
        assert!(matches!(
            events.first().map(|event| &event.kind),
            Some(CanonicalEventKind::ResponseStart { .. })
        ));
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::SourceExtension { extensions }
                if extensions.values.get("/status") == Some(&json!("incomplete"))
                    && extensions.values.get("/incomplete_details")
                        == Some(&json!({"reason": "max_output_tokens"}))
        )));
        assert!(matches!(
            events.last().map(|event| &event.kind),
            Some(CanonicalEventKind::Done)
        ));
        assert!(decoder.finish().unwrap().is_empty());
    }

    #[test]
    fn completed_stream_preserves_output_when_only_raw_items_exist() {
        let terminal_item = json!({
            "id": "rs_1",
            "type": "reasoning",
            "status": "completed",
            "summary": []
        });
        let wire = [
            data(json!({"type": "response.created", "response": {"id": "r"}})),
            data(json!({
                "type": "response.output_item.added",
                "output_index": 0,
                "item": {
                    "id": "rs_1",
                    "type": "reasoning",
                    "status": "in_progress",
                    "summary": []
                }
            })),
            data(json!({
                "type": "response.output_item.done",
                "output_index": 0,
                "item": terminal_item.clone()
            })),
            data(json!({
                "type": "response.completed",
                "response": {
                    "status": "completed",
                    "output": [terminal_item.clone()]
                }
            })),
        ]
        .concat();
        let mut decoder = OpenAiResponsesStreamDecoder::new();
        let events = decoder.push(wire.as_bytes()).unwrap();

        validate_event_sequence(&events).unwrap();
        assert!(!events.iter().any(|event| matches!(
            event.kind,
            CanonicalEventKind::MessageStart { .. } | CanonicalEventKind::Finish { .. }
        )));
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::SourceExtension { extensions }
                if extensions.values.get(&format!(
                    "{OPENAI_RESPONSES_RAW_OUTPUT_PREFIX}/0"
                )) == Some(&terminal_item)
        )));
        assert!(matches!(
            events.last().map(|event| &event.kind),
            Some(CanonicalEventKind::Done)
        ));
        assert!(decoder.finish().unwrap().is_empty());
    }

    #[test]
    fn invalid_terminal_and_output_lifecycle_sequences_fail_closed() {
        for wire in [
            "data: [DONE]\n\n".to_owned(),
            data(json!({"type": "response.completed", "response": {}})),
            [
                data(json!({"type": "response.created", "response": {"id": "r"}})),
                data(json!({
                    "type": "response.completed",
                    "response": {"status": "completed", "output": []}
                })),
            ]
            .concat(),
            [
                data(json!({"type": "response.created", "response": {"id": "r"}})),
                data(json!({
                    "type": "response.output_item.added",
                    "output_index": 0,
                    "item": {"type": "message", "role": "assistant", "status": "in_progress"}
                })),
                data(json!({
                    "type": "response.output_item.done",
                    "output_index": 0,
                    "item": {"type": "message", "status": "completed"}
                })),
                data(json!({
                    "type": "response.completed",
                    "response": {
                        "status": "completed",
                        "incomplete_details": {"reason": "max_output_tokens"},
                        "output": [{"type": "message", "status": "completed"}]
                    }
                })),
            ]
            .concat(),
            [
                data(json!({"type": "response.created", "response": {"id": "r"}})),
                data(json!({
                    "type": "response.output_item.added",
                    "output_index": 0,
                    "item": {"type": "message", "role": "assistant", "status": "in_progress"}
                })),
                data(json!({
                    "type": "response.output_text.delta",
                    "output_index": 0,
                    "delta": "partial"
                })),
                data(json!({"type": "response.completed", "response": {"status": "completed"}})),
            ]
            .concat(),
            [
                data(json!({"type": "response.created", "response": {"id": "r"}})),
                data(json!({
                    "type": "response.output_item.added",
                    "output_index": 0,
                    "item": {"type": "message", "role": "assistant", "status": "in_progress"}
                })),
                data(json!({
                    "type": "response.output_text.delta",
                    "output_index": 0,
                    "delta": "first"
                })),
                data(json!({
                    "type": "response.output_item.done",
                    "output_index": 0,
                    "item": {"type": "message", "status": "completed"}
                })),
                data(json!({
                    "type": "response.output_item.done",
                    "output_index": 0,
                    "item": {"type": "message", "status": "completed"}
                })),
            ]
            .concat(),
            [
                data(json!({"type": "response.created", "response": {"id": "r"}})),
                data(json!({
                    "type": "response.output_item.added",
                    "output_index": 0,
                    "item": {"type": "message", "role": "assistant", "status": "in_progress"}
                })),
                data(json!({
                    "type": "response.output_text.delta",
                    "output_index": 0,
                    "delta": "first"
                })),
                data(json!({
                    "type": "response.output_item.done",
                    "output_index": 0,
                    "item": {"type": "message", "status": "completed"}
                })),
                data(json!({
                    "type": "response.output_text.delta",
                    "output_index": 0,
                    "delta": "late"
                })),
            ]
            .concat(),
        ] {
            let mut decoder = OpenAiResponsesStreamDecoder::new();
            assert!(matches!(
                decoder.push(wire.as_bytes()),
                Err(ResponsesCodecError::InvalidStreamState)
            ));
        }

        let mut incomplete_without_item_done = OpenAiResponsesStreamDecoder::new();
        let wire = [
            data(json!({"type": "response.created", "response": {"id": "r"}})),
            data(
                json!({"type": "response.output_item.added", "output_index": 0,
                "item": {"type": "message", "role": "assistant", "status": "in_progress"}}),
            ),
            data(json!({"type": "response.incomplete", "response": {
                "status": "incomplete", "incomplete_details": {"reason": "max_output_tokens"}
            }})),
        ]
        .concat();
        assert!(matches!(
            incomplete_without_item_done.push(wire.as_bytes()),
            Err(ResponsesCodecError::InvalidStreamState)
        ));

        for terminal in [
            json!({"type": "response.incomplete", "response": {
                "status": "incomplete",
                "incomplete_details": {"reason": "max_output_tokens"},
                "error": {"code": "unexpected", "message": "bad"}
            }}),
            json!({"type": "response.failed", "response": {"status": "failed"}}),
            json!({"type": "response.cancelled", "response": {
                "status": "cancelled", "error": {"message": "unexpected"}
            }}),
        ] {
            let mut decoder = OpenAiResponsesStreamDecoder::new();
            assert!(matches!(
                decoder.push(data(terminal).as_bytes()),
                Err(ResponsesCodecError::InvalidStreamState)
            ));
        }
    }

    #[test]
    fn incomplete_item_defers_finish_until_the_terminal_reason() {
        let mut decoder = OpenAiResponsesStreamDecoder::new();
        let prefix = [
            data(json!({"type": "response.created", "response": {"id": "r"}})),
            data(json!({
                "type": "response.output_item.added",
                "output_index": 0,
                "item": {"type": "message", "role": "assistant", "status": "in_progress"}
            })),
            data(json!({
                "type": "response.output_text.delta",
                "output_index": 0,
                "delta": "partial"
            })),
            data(json!({
                "type": "response.output_item.done",
                "output_index": 0,
                "item": {"type": "message", "status": "incomplete"}
            })),
        ]
        .concat();
        let mut events = decoder.push(prefix.as_bytes()).unwrap();
        assert!(
            !events
                .iter()
                .any(|event| matches!(event.kind, CanonicalEventKind::Finish { .. }))
        );

        let terminal = data(json!({
            "type": "response.incomplete",
            "response": {
                "status": "incomplete",
                "incomplete_details": {"reason": "max_output_tokens"},
                "output": [
                    {"type": "message", "status": "incomplete"},
                    {"type": "reasoning", "status": "incomplete", "summary": []}
                ]
            }
        }));
        events.extend(decoder.push(terminal.as_bytes()).unwrap());

        validate_event_sequence(&events).unwrap();
        assert_eq!(
            events
                .iter()
                .filter(|event| matches!(
                    event.kind,
                    CanonicalEventKind::Finish {
                        output_index: 0,
                        reason: FinishReason::Length,
                    }
                ))
                .count(),
            1
        );
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::SourceExtension { extensions }
                if extensions.values.contains_key(&format!(
                    "{OPENAI_RESPONSES_RAW_OUTPUT_PREFIX}/1"
                ))
        )));
        assert!(matches!(
            events.last().map(|event| &event.kind),
            Some(CanonicalEventKind::Done)
        ));
        assert!(decoder.finish().unwrap().is_empty());
    }

    #[test]
    fn incomplete_item_closure_rejects_incompatible_follow_up_events() {
        let prefix = [
            data(json!({"type": "response.created", "response": {"id": "r"}})),
            data(json!({
                "type": "response.output_item.added",
                "output_index": 0,
                "item": {"type": "message", "role": "assistant", "status": "in_progress"}
            })),
            data(json!({
                "type": "response.output_item.done",
                "output_index": 0,
                "item": {"type": "message", "status": "incomplete"}
            })),
        ]
        .concat();
        for follow_up in [
            data(json!({
                "type": "response.output_item.done",
                "output_index": 0,
                "item": {"type": "message", "status": "incomplete"}
            })),
            data(json!({
                "type": "response.output_text.delta",
                "output_index": 0,
                "delta": "late"
            })),
            data(json!({
                "type": "response.completed",
                "response": {"status": "completed"}
            })),
            data(json!({
                "type": "response.incomplete",
                "response": {
                    "status": "incomplete",
                    "incomplete_details": {"reason": "max_output_tokens"},
                    "output": [{"type": "message", "status": "completed"}]
                }
            })),
        ] {
            let mut decoder = OpenAiResponsesStreamDecoder::new();
            let wire = format!("{prefix}{follow_up}");
            assert!(matches!(
                decoder.push(wire.as_bytes()),
                Err(ResponsesCodecError::InvalidStreamState)
            ));
        }
    }

    #[test]
    fn malformed_stream_fields_and_raw_outputs_fail_closed() {
        let mut missing_type = OpenAiResponsesStreamDecoder::new();
        assert!(matches!(
            missing_type.push(data(json!({})).as_bytes()),
            Err(ResponsesCodecError::InvalidResponse(_))
        ));

        for value in [
            json!({"type": "response.output_text.delta", "output_index": -1, "delta": "x"}),
            json!({"type": "response.output_text.delta", "output_index": 0, "delta": 7}),
        ] {
            let wire = [
                data(json!({"type": "response.created", "response": {"id": "r"}})),
                data(
                    json!({"type": "response.output_item.added", "output_index": 0,
                    "item": {"type": "message", "role": "assistant", "status": "in_progress"}}),
                ),
                data(value),
            ]
            .concat();
            let mut decoder = OpenAiResponsesStreamDecoder::new();
            assert!(matches!(
                decoder.push(wire.as_bytes()),
                Err(ResponsesCodecError::InvalidResponse(_))
            ));
        }

        let wire = [
            data(json!({"type": "response.created", "response": {"id": "r"}})),
            data(json!({
                "type": "response.output_item.added",
                "output_index": 0,
                "item": {"type": "message", "role": "assistant", "status": "in_progress"}
            })),
            data(json!({
                "type": "response.output_item.done",
                "output_index": 0,
                "item": {"type": "message", "status": "completed"}
            })),
            data(json!({
                "type": "response.completed",
                "response": {"status": "completed", "output": [{"vendor": true}]}
            })),
        ]
        .concat();
        let mut decoder = OpenAiResponsesStreamDecoder::new();
        assert!(matches!(
            decoder.push(wire.as_bytes()),
            Err(ResponsesCodecError::InvalidResponse(_))
        ));

        let mut truncated = OpenAiResponsesStreamDecoder::new();
        truncated
            .push(data(json!({"type": "response.created"})).as_bytes())
            .unwrap();
        assert!(matches!(
            truncated.finish(),
            Err(ResponsesCodecError::UnexpectedEof)
        ));
    }
}

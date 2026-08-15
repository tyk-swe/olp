use std::collections::{BTreeMap, BTreeSet};

use crate::domain::{
    CanonicalError, CanonicalEvent, CanonicalEventKind, ErrorClass, FinishReason, MessageRole,
    SourceExtensions, Surface,
};
use serde_json::Value;

use crate::protocols::sse::{DEFAULT_MAX_EVENT_BYTES, SseDecoder, SseFrame};

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
                self.finish_open_outputs(&mut events, FinishReason::Stop);
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
                self.ensure_output_started(output_index, MessageRole::Assistant, events);
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
            terminal_type @ ("response.completed" | "response.incomplete") => {
                let response = value.get("response").unwrap_or(value);
                self.ensure_response_started(response, events);
                let finish_reason = if terminal_type == "response.incomplete" {
                    match response
                        .pointer("/incomplete_details/reason")
                        .and_then(|value| value.as_str())
                    {
                        Some("content_filter") => FinishReason::ContentFilter,
                        _ => FinishReason::Length,
                    }
                } else {
                    FinishReason::Stop
                };
                self.finish_open_outputs(events, finish_reason);
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
                let provider_code = error.get("code").and_then(Value::as_str).map(str::to_owned);
                let retryable = crate::protocols::openai::error_signals_rate_limit(
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
                self.finish_open_outputs(events, FinishReason::Stop);
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

    fn finish_open_outputs(
        &mut self,
        events: &mut Vec<CanonicalEvent>,
        finish_reason: FinishReason,
    ) {
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
                    reason: finish_reason.clone(),
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
    fn completed_stream_aggregates_tools_usage_and_raw_output_in_order() {
        let wire = concat!(
            "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"private-model\"}}\n\n",
            "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{\\\"city\\\":\"}}\n\n",
            "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"call_1\",\"delta\":\"\\\"Paris\\\"}\"}\n\n",
            "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\"}}\n\n",
            "data: {\"type\":\"future/event~name\",\"vendor\":{\"retained\":true}}\n\n",
            "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\"},{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\"}],\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7,\"input_tokens_details\":{\"cached_tokens\":3},\"output_tokens_details\":{\"reasoning_tokens\":1}}}}\n\n"
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
                    && usage.reasoning_tokens == Some(1)
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
        assert!(decoder.finish().unwrap().is_empty());
    }

    #[test]
    fn done_marker_finishes_open_outputs_and_rejects_later_data() {
        let wire = concat!(
            "event: response.in_progress\n",
            "data: {\"response\":{\"id\":\"resp_2\",\"model\":\"private\"}}\n\n",
            "data: {\"type\":\"response.refusal.delta\",\"output_index\":2,",
            "\"delta\":\"cannot\"}\n\n",
            "data: [DONE]\n\n"
        );
        let mut decoder = OpenAiResponsesStreamDecoder::new();
        let events = decoder.push(wire.as_bytes()).unwrap();

        validate_event_sequence(&events).unwrap();
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::RefusalDelta { output_index: 2, text } if text == "cannot"
        )));
        assert!(events.iter().any(|event| matches!(
            event.kind,
            CanonicalEventKind::Finish {
                output_index: 2,
                reason: FinishReason::Stop,
            }
        )));
        assert!(matches!(
            decoder.push(data(json!({"type": "response.created"})).as_bytes()),
            Err(ResponsesCodecError::DataAfterDone)
        ));
    }

    #[test]
    fn failed_stream_closes_started_outputs_and_uses_safe_fallbacks() {
        let wire = [
            data(json!({
                "type": "response.output_text.delta",
                "output_index": 1,
                "delta": "partial"
            })),
            data(json!({"type": "error"})),
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
        assert_eq!(error.message, "OpenAI Responses stream failed");
        assert_eq!(error.provider_code, None);
        assert!(!error.retryable);
        assert!(events.iter().any(|event| matches!(
            event.kind,
            CanonicalEventKind::Finish {
                output_index: 1,
                ..
            }
        )));
        assert!(matches!(
            events.last().unwrap().kind,
            CanonicalEventKind::Done
        ));
    }

    #[test]
    fn malformed_stream_fields_and_raw_outputs_fail_closed() {
        for value in [
            json!({}),
            json!({"type": "response.output_text.delta", "output_index": -1, "delta": "x"}),
            json!({"type": "response.output_text.delta", "output_index": 0, "delta": 7}),
            json!({
                "type": "response.completed",
                "response": {"output": [{"vendor": true}]}
            }),
        ] {
            let mut decoder = OpenAiResponsesStreamDecoder::new();
            assert!(matches!(
                decoder.push(data(value).as_bytes()),
                Err(ResponsesCodecError::InvalidResponse(_))
            ));
        }

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

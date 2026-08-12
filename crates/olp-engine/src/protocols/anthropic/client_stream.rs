use std::collections::BTreeMap;

use crate::domain::{CanonicalEvent, CanonicalEventKind, ErrorClass, MessageRole, Surface, Usage};
use serde_json::{Value, json};
use thiserror::Error;

use super::finish_reason;
use crate::protocols::sse::{RAW_SSE_FRAME_EXTENSION, SseFrame, decode_raw_sse_frame};

const MAX_BUFFERED_SEMANTIC_FRAME_BYTES: usize = 16 * 1024 * 1024;

#[derive(Debug, Error)]
pub enum ClientStreamEncodeError {
    #[error("Anthropic stream received events out of order")]
    Sequence,
    #[error("Anthropic Messages supports one assistant candidate")]
    Candidate,
    #[error("Anthropic stream is missing response metadata")]
    Response,
    #[error("Anthropic stream contains an incomplete or conflicting tool call")]
    Tool,
    #[error("canonical reasoning-token usage is not representable in Anthropic usage")]
    ReasoningUsage,
    #[error("canonical usage details are internally inconsistent")]
    InvalidUsage,
    #[error("source extensions cannot be represented in an Anthropic client stream")]
    Extension,
    #[error("Anthropic stream completed without a finish reason")]
    MissingFinish,
    #[error("Anthropic stream completed without complete usage")]
    MissingUsage,
    #[error("Anthropic stream exceeded the buffered semantic-frame limit")]
    BufferedFramesTooLarge,
}

#[derive(Debug)]
pub struct AnthropicMessagesClientStreamEncoder {
    public_model: String,
    fallback_id: String,
    expected_sequence: u64,
    response_id: Option<String>,
    response_started: bool,
    message_declared: bool,
    message_emitted: bool,
    message_pending: bool,
    usage: Option<Usage>,
    buffered_frames: Vec<SseFrame>,
    buffered_frame_bytes: usize,
    max_buffered_frame_bytes: usize,
    text_block: Option<u32>,
    tools: BTreeMap<u32, ToolState>,
    next_block: u32,
    finished: bool,
    finish_reason: Option<crate::domain::FinishReason>,
    terminal_usage_emitted: bool,
    errored: bool,
    done: bool,
    skip_native_events: usize,
}

#[derive(Debug)]
struct ToolState {
    block: u32,
    id: String,
    name: String,
}

impl AnthropicMessagesClientStreamEncoder {
    #[must_use]
    pub fn new(public_model: impl Into<String>, fallback_id: impl Into<String>) -> Self {
        Self {
            public_model: public_model.into(),
            fallback_id: fallback_id.into(),
            expected_sequence: 0,
            response_id: None,
            response_started: false,
            message_declared: false,
            message_emitted: false,
            message_pending: false,
            usage: None,
            buffered_frames: Vec::new(),
            buffered_frame_bytes: 0,
            max_buffered_frame_bytes: MAX_BUFFERED_SEMANTIC_FRAME_BYTES,
            text_block: None,
            tools: BTreeMap::new(),
            next_block: 0,
            finished: false,
            finish_reason: None,
            terminal_usage_emitted: false,
            errored: false,
            done: false,
            skip_native_events: 0,
        }
    }

    #[cfg(test)]
    fn with_max_buffered_frame_bytes(
        public_model: impl Into<String>,
        fallback_id: impl Into<String>,
        maximum: usize,
    ) -> Self {
        let mut encoder = Self::new(public_model, fallback_id);
        encoder.max_buffered_frame_bytes = maximum;
        encoder
    }

    pub fn push(
        &mut self,
        event: CanonicalEvent,
    ) -> Result<Vec<SseFrame>, ClientStreamEncodeError> {
        if self.done || event.sequence != self.expected_sequence {
            return Err(ClientStreamEncodeError::Sequence);
        }
        self.expected_sequence = self.expected_sequence.saturating_add(1);
        if self.skip_native_events > 0 {
            self.skip_native_events -= 1;
            if matches!(event.kind, CanonicalEventKind::Done) {
                self.done = true;
            }
            return Ok(Vec::new());
        }
        let mut frames = Vec::new();
        match event.kind {
            CanonicalEventKind::ResponseStart { response_id, .. } => {
                if self.response_started {
                    return Err(ClientStreamEncodeError::Sequence);
                }
                self.response_id = response_id;
                self.response_started = true;
            }
            CanonicalEventKind::MessageStart { output_index, role } => {
                require_candidate(output_index)?;
                if role != MessageRole::Assistant || self.message_declared {
                    return Err(ClientStreamEncodeError::Candidate);
                }
                self.message_declared = true;
            }
            CanonicalEventKind::TextDelta { output_index, text } => {
                require_candidate(output_index)?;
                self.ensure_message(&mut frames)?;
                let block = match self.text_block {
                    Some(block) => block,
                    None => {
                        let block = self.allocate_block()?;
                        self.text_block = Some(block);
                        frames.push(frame(
                            "content_block_start",
                            json!({
                                "type": "content_block_start",
                                "index": block,
                                "content_block": {"type": "text", "text": ""}
                            }),
                        ));
                        block
                    }
                };
                if !text.is_empty() {
                    frames.push(frame(
                        "content_block_delta",
                        json!({
                            "type": "content_block_delta",
                            "index": block,
                            "delta": {"type": "text_delta", "text": text}
                        }),
                    ));
                }
            }
            CanonicalEventKind::ToolCallDelta {
                output_index,
                tool_index,
                id,
                name,
                arguments_delta,
            } => {
                require_candidate(output_index)?;
                self.ensure_message(&mut frames)?;
                if let Some(tool) = self.tools.get(&tool_index) {
                    if id.as_ref().is_some_and(|id| id != &tool.id)
                        || name.as_ref().is_some_and(|name| name != &tool.name)
                    {
                        return Err(ClientStreamEncodeError::Tool);
                    }
                } else {
                    let id = id.ok_or(ClientStreamEncodeError::Tool)?;
                    let name = name.ok_or(ClientStreamEncodeError::Tool)?;
                    let block = self.allocate_block()?;
                    frames.push(frame(
                        "content_block_start",
                        json!({
                            "type": "content_block_start",
                            "index": block,
                            "content_block": {"type": "tool_use", "id": id, "name": name, "input": {}}
                        }),
                    ));
                    self.tools.insert(tool_index, ToolState { block, id, name });
                }
                let block = self
                    .tools
                    .get(&tool_index)
                    .ok_or(ClientStreamEncodeError::Tool)?
                    .block;
                if !arguments_delta.is_empty() {
                    frames.push(frame(
                        "content_block_delta",
                        json!({
                            "type": "content_block_delta",
                            "index": block,
                            "delta": {"type": "input_json_delta", "partial_json": arguments_delta}
                        }),
                    ));
                }
            }
            CanonicalEventKind::Usage { usage } => {
                if usage.reasoning_tokens.is_some() {
                    return Err(ClientStreamEncodeError::ReasoningUsage);
                }
                base_input_tokens(&usage)?;
                self.usage = Some(usage);
                if self.message_pending {
                    let pending = std::mem::take(&mut self.buffered_frames);
                    let mut buffered = Vec::with_capacity(pending.len().saturating_add(1));
                    self.emit_message_start(&mut buffered)?;
                    buffered.extend(pending);
                    self.buffered_frames = buffered;
                } else if self.message_declared && !self.message_emitted {
                    self.ensure_message(&mut frames)?;
                }
                if self.finish_reason.is_some() && !self.terminal_usage_emitted {
                    self.emit_terminal_delta(&mut frames)?;
                }
            }
            CanonicalEventKind::Finish {
                output_index,
                reason,
            } => {
                require_candidate(output_index)?;
                if self.finished {
                    return Err(ClientStreamEncodeError::Sequence);
                }
                self.ensure_message(&mut frames)?;
                let mut blocks = self
                    .tools
                    .values()
                    .map(|tool| tool.block)
                    .collect::<Vec<_>>();
                blocks.extend(self.text_block);
                blocks.sort_unstable();
                for block in blocks {
                    frames.push(frame(
                        "content_block_stop",
                        json!({"type": "content_block_stop", "index": block}),
                    ));
                }
                self.finished = true;
                self.finish_reason = Some(reason);
                if self.usage.is_some() {
                    self.emit_terminal_delta(&mut frames)?;
                }
            }
            CanonicalEventKind::Error { error } => {
                self.buffered_frames.clear();
                self.buffered_frame_bytes = 0;
                self.message_pending = false;
                frames.push(frame(
                    "error",
                    json!({
                        "type": "error",
                        "error": {"type": anthropic_error_type(error.class), "message": error.message}
                    }),
                ));
                self.finished = true;
                self.errored = true;
            }
            CanonicalEventKind::SourceExtension { extensions } => {
                if extensions.source != Some(Surface::Anthropic) {
                    return Err(ClientStreamEncodeError::Extension);
                }
                if let Some(value) = extensions.values.get(RAW_SSE_FRAME_EXTENSION) {
                    if extensions.values.len() != 1 {
                        return Err(ClientStreamEncodeError::Extension);
                    }
                    let (mut raw, semantic_events) =
                        decode_raw_sse_frame(value).ok_or(ClientStreamEncodeError::Extension)?;
                    rewrite_anthropic_model(&mut raw, &self.public_model)?;
                    self.skip_native_events = semantic_events;
                    frames.push(raw);
                } else if !extensions.values.is_empty() {
                    return Err(ClientStreamEncodeError::Extension);
                }
            }
            CanonicalEventKind::RefusalDelta { .. } => {
                return Err(ClientStreamEncodeError::Candidate);
            }
            CanonicalEventKind::Done => {
                if !self.finished {
                    return Err(ClientStreamEncodeError::MissingFinish);
                }
                if !self.errored {
                    if self.finish_reason.is_some() && !self.terminal_usage_emitted {
                        return Err(ClientStreamEncodeError::MissingUsage);
                    }
                    if self.message_emitted {
                        frames.push(frame("message_stop", json!({"type": "message_stop"})));
                    }
                }
                self.done = true;
            }
        }
        self.flush_or_buffer(frames)
    }

    fn ensure_message(
        &mut self,
        frames: &mut Vec<SseFrame>,
    ) -> Result<(), ClientStreamEncodeError> {
        if self.message_emitted || self.message_pending {
            return Ok(());
        }
        if !self.response_started || !self.message_declared {
            return Err(ClientStreamEncodeError::Response);
        }
        if self.usage.is_none() {
            self.message_pending = true;
            return Ok(());
        }
        self.emit_message_start(frames)
    }

    fn emit_message_start(
        &mut self,
        frames: &mut Vec<SseFrame>,
    ) -> Result<(), ClientStreamEncodeError> {
        let usage = self
            .usage
            .as_ref()
            .ok_or(ClientStreamEncodeError::MissingUsage)?;
        frames.push(frame(
            "message_start",
            json!({
                "type": "message_start",
                "message": {
                    "id": self.response_id.as_deref().unwrap_or(&self.fallback_id),
                    "type": "message",
                    "role": "assistant",
                    "content": [],
                    "model": self.public_model,
                    "stop_reason": null,
                    "stop_sequence": null,
                    "usage": message_start_usage(usage)?
                }
            }),
        ));
        self.message_pending = false;
        self.message_emitted = true;
        Ok(())
    }

    fn flush_or_buffer(
        &mut self,
        mut frames: Vec<SseFrame>,
    ) -> Result<Vec<SseFrame>, ClientStreamEncodeError> {
        if self.message_pending && self.usage.is_none() && !self.errored {
            let additional = frames.iter().try_fold(0_usize, |total, frame| {
                total.checked_add(buffered_frame_size(frame)?)
            });
            let total = additional
                .and_then(|additional| self.buffered_frame_bytes.checked_add(additional))
                .filter(|total| *total <= self.max_buffered_frame_bytes)
                .ok_or(ClientStreamEncodeError::BufferedFramesTooLarge)?;
            self.buffered_frame_bytes = total;
            self.buffered_frames.append(&mut frames);
            return Ok(Vec::new());
        }
        if self.buffered_frames.is_empty() {
            return Ok(frames);
        }
        self.buffered_frames.append(&mut frames);
        self.buffered_frame_bytes = 0;
        Ok(std::mem::take(&mut self.buffered_frames))
    }

    fn emit_terminal_delta(
        &mut self,
        frames: &mut Vec<SseFrame>,
    ) -> Result<(), ClientStreamEncodeError> {
        let reason = self
            .finish_reason
            .as_ref()
            .ok_or(ClientStreamEncodeError::MissingFinish)?;
        let usage = self
            .usage
            .as_ref()
            .ok_or(ClientStreamEncodeError::MissingUsage)?;
        frames.push(frame(
            "message_delta",
            json!({
                "type": "message_delta",
                "delta": {"stop_reason": finish_reason(reason), "stop_sequence": null},
                "usage": terminal_usage(usage)?
            }),
        ));
        self.terminal_usage_emitted = true;
        Ok(())
    }

    fn allocate_block(&mut self) -> Result<u32, ClientStreamEncodeError> {
        let block = self.next_block;
        self.next_block = self
            .next_block
            .checked_add(1)
            .ok_or(ClientStreamEncodeError::Candidate)?;
        Ok(block)
    }
}

fn buffered_frame_size(frame: &SseFrame) -> Option<usize> {
    // Semantic frames contain compact, single-line JSON. This allowance covers
    // every SSE field prefix, terminator, and a maximum-width retry value.
    const FRAMING_BYTES: usize = 64;
    frame
        .data
        .len()
        .checked_add(frame.event.as_ref().map_or(0, String::len))?
        .checked_add(frame.id.as_ref().map_or(0, String::len))?
        .checked_add(FRAMING_BYTES)
}

fn base_input_tokens(usage: &Usage) -> Result<u64, ClientStreamEncodeError> {
    usage
        .input_tokens
        .checked_sub(usage.cached_input_tokens.unwrap_or(0))
        .ok_or(ClientStreamEncodeError::InvalidUsage)
}

fn message_start_usage(usage: &Usage) -> Result<Value, ClientStreamEncodeError> {
    let mut value = json!({
        "input_tokens": base_input_tokens(usage)?,
        "output_tokens": 0
    });
    if let Some(cached) = usage.cached_input_tokens {
        value["cache_read_input_tokens"] = Value::from(cached);
    }
    Ok(value)
}

fn terminal_usage(usage: &Usage) -> Result<Value, ClientStreamEncodeError> {
    let mut value = json!({
        "input_tokens": base_input_tokens(usage)?,
        "output_tokens": usage.output_tokens
    });
    if let Some(cached) = usage.cached_input_tokens {
        value["cache_read_input_tokens"] = Value::from(cached);
    }
    Ok(value)
}

fn rewrite_anthropic_model(
    frame: &mut SseFrame,
    public_model: &str,
) -> Result<(), ClientStreamEncodeError> {
    let mut value: Value =
        serde_json::from_str(&frame.data).map_err(|_| ClientStreamEncodeError::Extension)?;
    if let Some(message) = value.get_mut("message").and_then(Value::as_object_mut)
        && message.contains_key("model")
    {
        message.insert("model".into(), Value::String(public_model.to_owned()));
    }
    frame.data = serde_json::to_string(&value).map_err(|_| ClientStreamEncodeError::Extension)?;
    Ok(())
}

fn require_candidate(index: u32) -> Result<(), ClientStreamEncodeError> {
    if index == 0 {
        Ok(())
    } else {
        Err(ClientStreamEncodeError::Candidate)
    }
}

fn anthropic_error_type(class: ErrorClass) -> &'static str {
    match class {
        ErrorClass::Authentication => "authentication_error",
        ErrorClass::Authorization => "permission_error",
        ErrorClass::InvalidRequest => "invalid_request_error",
        ErrorClass::RateLimit => "rate_limit_error",
        ErrorClass::Timeout
        | ErrorClass::Transport
        | ErrorClass::Upstream
        | ErrorClass::Internal => "api_error",
    }
}

fn frame(event: &'static str, value: Value) -> SseFrame {
    SseFrame {
        event: Some(event.to_owned()),
        data: value.to_string(),
        id: None,
        retry_ms: None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::{CanonicalEventKind, MessageRole};

    #[test]
    fn semantic_buffer_rejects_the_first_frame_past_its_byte_limit() {
        let expected = [
            frame(
                "content_block_start",
                json!({
                    "type": "content_block_start",
                    "index": 0,
                    "content_block": {"type": "text", "text": ""}
                }),
            ),
            frame(
                "content_block_delta",
                json!({
                    "type": "content_block_delta",
                    "index": 0,
                    "delta": {"type": "text_delta", "text": "a"}
                }),
            ),
        ];
        let maximum = expected
            .iter()
            .map(|frame| buffered_frame_size(frame).unwrap())
            .sum();
        let mut encoder = AnthropicMessagesClientStreamEncoder::with_max_buffered_frame_bytes(
            "route", "fallback", maximum,
        );
        for event in [
            CanonicalEvent::new(
                0,
                CanonicalEventKind::ResponseStart {
                    response_id: None,
                    provider_model: None,
                },
            ),
            CanonicalEvent::new(
                1,
                CanonicalEventKind::MessageStart {
                    output_index: 0,
                    role: MessageRole::Assistant,
                },
            ),
        ] {
            assert!(encoder.push(event).unwrap().is_empty());
        }
        assert!(
            encoder
                .push(CanonicalEvent::new(
                    2,
                    CanonicalEventKind::TextDelta {
                        output_index: 0,
                        text: "a".into(),
                    },
                ))
                .unwrap()
                .is_empty()
        );
        assert_eq!(encoder.buffered_frame_bytes, maximum);
        assert!(matches!(
            encoder.push(CanonicalEvent::new(
                3,
                CanonicalEventKind::TextDelta {
                    output_index: 0,
                    text: "b".into(),
                },
            )),
            Err(ClientStreamEncodeError::BufferedFramesTooLarge)
        ));
        assert_eq!(encoder.buffered_frame_bytes, maximum);
    }
}

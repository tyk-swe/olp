use std::collections::BTreeMap;

use olp_domain::{
    CanonicalEvent, CanonicalEventKind, ErrorClass, FinishReason, MessageRole, Surface, Usage,
};
use serde_json::{Value, json};
use thiserror::Error;

use crate::sse::{RAW_SSE_FRAME_EXTENSION, SseFrame, decode_raw_sse_frame};

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
    #[error("source extensions cannot be represented in an Anthropic client stream")]
    Extension,
    #[error("Anthropic stream completed without a finish reason")]
    MissingFinish,
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
    usage: Usage,
    text_block: Option<u32>,
    tools: BTreeMap<u32, ToolState>,
    next_block: u32,
    finished: bool,
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
            usage: Usage::default(),
            text_block: None,
            tools: BTreeMap::new(),
            next_block: 0,
            finished: false,
            done: false,
            skip_native_events: 0,
        }
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
                self.usage = usage;
                if self.message_declared && !self.message_emitted {
                    self.ensure_message(&mut frames)?;
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
                frames.push(frame(
                    "message_delta",
                    json!({
                        "type": "message_delta",
                        "delta": {"stop_reason": finish_reason(&reason), "stop_sequence": null},
                        "usage": {
                            "input_tokens": self.usage.input_tokens,
                            "output_tokens": self.usage.output_tokens,
                            "cache_read_input_tokens": self.usage.cached_input_tokens
                        }
                    }),
                ));
                self.finished = true;
            }
            CanonicalEventKind::Error { error } => {
                frames.push(frame(
                    "error",
                    json!({
                        "type": "error",
                        "error": {"type": anthropic_error_type(error.class), "message": error.message}
                    }),
                ));
                self.finished = true;
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
                if self.message_emitted {
                    frames.push(frame("message_stop", json!({"type": "message_stop"})));
                }
                self.done = true;
            }
        }
        Ok(frames)
    }

    fn ensure_message(
        &mut self,
        frames: &mut Vec<SseFrame>,
    ) -> Result<(), ClientStreamEncodeError> {
        if self.message_emitted {
            return Ok(());
        }
        if !self.response_started || !self.message_declared {
            return Err(ClientStreamEncodeError::Response);
        }
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
                    "usage": {
                        "input_tokens": self.usage.input_tokens,
                        "output_tokens": 0,
                        "cache_read_input_tokens": self.usage.cached_input_tokens
                    }
                }
            }),
        ));
        self.message_emitted = true;
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

fn finish_reason(reason: &FinishReason) -> String {
    match reason {
        FinishReason::Stop => "end_turn".to_owned(),
        FinishReason::Length => "max_tokens".to_owned(),
        FinishReason::ToolCalls => "tool_use".to_owned(),
        FinishReason::ContentFilter => "refusal".to_owned(),
        FinishReason::Error => "error".to_owned(),
        FinishReason::Other(value) => value.clone(),
    }
}

fn anthropic_error_type(class: ErrorClass) -> &'static str {
    match class {
        ErrorClass::Authentication => "authentication_error",
        ErrorClass::Authorization => "permission_error",
        ErrorClass::InvalidRequest => "invalid_request_error",
        ErrorClass::RateLimit => "rate_limit_error",
        ErrorClass::Timeout | ErrorClass::Transport | ErrorClass::Upstream => "api_error",
        ErrorClass::Internal => "api_error",
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

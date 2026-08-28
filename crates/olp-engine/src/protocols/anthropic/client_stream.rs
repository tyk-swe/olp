use std::collections::BTreeMap;

use crate::domain::canonical::{
    events::{Error as StreamError, ErrorClass, Event, FinishReason, Kind, Usage},
    identity::Surface,
    requests::{MessageRole, SourceExtensions},
};
use serde_json::{Value, json};
use thiserror::Error;

use super::finish_reason;
use crate::protocols::sse::{Frame, RAW_SSE_FRAME_EXTENSION, decode_raw_sse_frame};

#[derive(Debug, Error)]
pub enum Error {
    #[error("Anthropic stream received events out of order")]
    Sequence,
    #[error("Anthropic Messages supports one assistant candidate")]
    Candidate,
    #[error("Anthropic stream is missing response metadata")]
    Response,
    #[error("Anthropic stream contains an incomplete or conflicting tool call")]
    Tool,
    #[error("source extensions cannot be represented in an Anthropic client stream")]
    Extension,
    #[error("Anthropic stream completed without a finish reason")]
    MissingFinish,
}

#[derive(Debug)]
pub struct Encoder {
    public_model: String,
    fallback_id: String,
    expected_sequence: u64,
    response_id: Option<String>,
    response_started: bool,
    message_declared: bool,
    message_emitted: bool,
    usage: Usage,
    cache_creation_input_tokens: Option<u64>,
    stop_sequence: Option<String>,
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

impl Encoder {
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
            cache_creation_input_tokens: None,
            stop_sequence: None,
            text_block: None,
            tools: BTreeMap::new(),
            next_block: 0,
            finished: false,
            done: false,
            skip_native_events: 0,
        }
    }

    pub fn push(&mut self, event: Event) -> Result<Vec<Frame>, Error> {
        if self.done || event.sequence != self.expected_sequence {
            return Err(Error::Sequence);
        }
        self.expected_sequence = self.expected_sequence.saturating_add(1);
        if self.skip_native_events > 0 {
            self.skip_native_events -= 1;
            if matches!(event.kind, Kind::Done) {
                self.done = true;
            }
            return Ok(Vec::new());
        }
        let mut frames = Vec::new();
        match event.kind {
            Kind::ResponseStart { response_id, .. } => self.start_response(response_id)?,
            Kind::MessageStart { output_index, role } => {
                self.declare_message(output_index, role)?
            }
            Kind::TextDelta { output_index, text } => {
                self.push_text_delta(output_index, text, &mut frames)?;
            }
            Kind::ToolCallDelta {
                output_index,
                tool_index,
                id,
                name,
                arguments_delta,
            } => self.push_tool_call_delta(
                output_index,
                tool_index,
                id,
                name,
                arguments_delta,
                &mut frames,
            )?,
            Kind::Usage { usage } => self.record_usage(usage, &mut frames)?,
            Kind::Finish {
                output_index,
                reason,
            } => self.finish_message(output_index, &reason, &mut frames)?,
            Kind::Error { error } => self.push_error(&error, &mut frames),
            Kind::SourceExtension { extensions } => {
                self.apply_source_extension(extensions, &mut frames)?;
            }
            Kind::RefusalDelta { .. } => return Err(Error::Candidate),
            Kind::Done => self.stop_message(&mut frames)?,
        }
        Ok(frames)
    }

    fn start_response(&mut self, response_id: Option<String>) -> Result<(), Error> {
        if self.response_started {
            return Err(Error::Sequence);
        }
        self.response_id = response_id;
        self.response_started = true;
        Ok(())
    }

    fn declare_message(&mut self, output_index: u32, role: MessageRole) -> Result<(), Error> {
        require_candidate(output_index)?;
        if role != MessageRole::Assistant || self.message_declared {
            return Err(Error::Candidate);
        }
        self.message_declared = true;
        Ok(())
    }

    fn push_text_delta(
        &mut self,
        output_index: u32,
        text: String,
        frames: &mut Vec<Frame>,
    ) -> Result<(), Error> {
        require_candidate(output_index)?;
        self.ensure_message(frames)?;
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
        Ok(())
    }

    fn push_tool_call_delta(
        &mut self,
        output_index: u32,
        tool_index: u32,
        id: Option<String>,
        name: Option<String>,
        arguments_delta: String,
        frames: &mut Vec<Frame>,
    ) -> Result<(), Error> {
        require_candidate(output_index)?;
        self.ensure_message(frames)?;
        if let Some(tool) = self.tools.get(&tool_index) {
            if id.as_ref().is_some_and(|id| id != &tool.id)
                || name.as_ref().is_some_and(|name| name != &tool.name)
            {
                return Err(Error::Tool);
            }
        } else {
            let id = id.ok_or(Error::Tool)?;
            let name = name.ok_or(Error::Tool)?;
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
        let block = self.tools.get(&tool_index).ok_or(Error::Tool)?.block;
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
        Ok(())
    }

    fn record_usage(&mut self, usage: Usage, frames: &mut Vec<Frame>) -> Result<(), Error> {
        // Reasoning tokens have no Anthropic field. Dropping the count
        // is a reporting gap; failing the stream loses the response.
        self.usage = usage;
        if self.message_declared && !self.message_emitted {
            self.ensure_message(frames)?;
        }
        Ok(())
    }

    fn finish_message(
        &mut self,
        output_index: u32,
        reason: &FinishReason,
        frames: &mut Vec<Frame>,
    ) -> Result<(), Error> {
        require_candidate(output_index)?;
        if self.finished {
            return Err(Error::Sequence);
        }
        self.ensure_message(frames)?;
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
        let usage = super::client::anthropic_usage(&self.usage, self.cache_creation_input_tokens);
        // `stop_reason: "end_turn"` alongside a matched `stop_sequence`
        // is self-contradictory; the sequence decides the reason.
        let stop_reason = if matches!(reason, FinishReason::Stop) && self.stop_sequence.is_some() {
            "stop_sequence"
        } else {
            finish_reason(reason)
        };
        frames.push(frame(
            "message_delta",
            json!({
                "type": "message_delta",
                "delta": {
                    "stop_reason": stop_reason,
                    "stop_sequence": self.stop_sequence.clone()
                },
                "usage": {
                    "input_tokens": usage.input_tokens,
                    "output_tokens": usage.output_tokens,
                    "cache_creation_input_tokens": usage.cache_creation_input_tokens,
                    "cache_read_input_tokens": usage.cache_read_input_tokens
                }
            }),
        ));
        self.finished = true;
        Ok(())
    }

    fn push_error(&mut self, error: &StreamError, frames: &mut Vec<Frame>) {
        frames.push(frame(
            "error",
            json!({
                "type": "error",
                "error": {"type": anthropic_error_type(error.class), "message": error.message}
            }),
        ));
        self.finished = true;
    }

    fn apply_source_extension(
        &mut self,
        mut extensions: SourceExtensions,
        frames: &mut Vec<Frame>,
    ) -> Result<(), Error> {
        // Anything from another surface is unrepresentable here, and the
        // response is already in flight, so it is dropped rather than
        // failing a stream the client is reading.
        if extensions.source != Some(Surface::Anthropic) {
            return Ok(());
        }
        if let Some(value) = extensions.values.remove(RAW_SSE_FRAME_EXTENSION) {
            if !extensions.values.is_empty() {
                return Err(Error::Extension);
            }
            let (mut raw, semantic_events) = decode_raw_sse_frame(value).ok_or(Error::Extension)?;
            rewrite_anthropic_model(&mut raw, &self.public_model)?;
            self.skip_native_events = semantic_events;
            frames.push(raw);
        } else {
            // Only the fields the terminal frames still have to carry
            // are retained; the rest were already delivered upstream.
            if let Some(tokens) = extensions
                .values
                .get("/message/usage/cache_creation_input_tokens")
                .or_else(|| extensions.values.get("/usage/cache_creation_input_tokens"))
                .and_then(Value::as_u64)
            {
                self.cache_creation_input_tokens = Some(tokens);
            }
            if let Some(sequence) = extensions
                .values
                .get("/delta/stop_sequence")
                .and_then(Value::as_str)
            {
                self.stop_sequence = Some(sequence.to_owned());
            }
        }
        Ok(())
    }

    fn stop_message(&mut self, frames: &mut Vec<Frame>) -> Result<(), Error> {
        if !self.finished {
            return Err(Error::MissingFinish);
        }
        if self.message_emitted {
            frames.push(frame("message_stop", json!({"type": "message_stop"})));
        }
        self.done = true;
        Ok(())
    }

    fn ensure_message(&mut self, frames: &mut Vec<Frame>) -> Result<(), Error> {
        if self.message_emitted {
            return Ok(());
        }
        if !self.response_started || !self.message_declared {
            return Err(Error::Response);
        }
        // Preliminary counts: message_delta corrects them once the upstream
        // reports the final usage. They still have to split the cache tiers the
        // same way, so the two frames never contradict each other.
        let usage = super::client::anthropic_usage(&self.usage, self.cache_creation_input_tokens);
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
                        "input_tokens": usage.input_tokens,
                        "output_tokens": 0,
                        "cache_creation_input_tokens": usage.cache_creation_input_tokens,
                        "cache_read_input_tokens": usage.cache_read_input_tokens
                    }
                }
            }),
        ));
        self.message_emitted = true;
        Ok(())
    }

    fn allocate_block(&mut self) -> Result<u32, Error> {
        let block = self.next_block;
        self.next_block = self.next_block.checked_add(1).ok_or(Error::Candidate)?;
        Ok(block)
    }
}

fn rewrite_anthropic_model(frame: &mut Frame, public_model: &str) -> Result<(), Error> {
    // Only message_start carries the model. Every other frame is replayed
    // byte-for-byte; a false positive here merely takes the parse path.
    if !frame.data.contains("\"model\"") {
        return Ok(());
    }
    let mut value: Value = serde_json::from_str(&frame.data).map_err(|_| Error::Extension)?;
    if let Some(message) = value.get_mut("message").and_then(Value::as_object_mut)
        && message.contains_key("model")
    {
        message.insert("model".into(), Value::String(public_model.to_owned()));
    }
    frame.data = serde_json::to_string(&value).map_err(|_| Error::Extension)?;
    Ok(())
}

fn require_candidate(index: u32) -> Result<(), Error> {
    if index == 0 {
        Ok(())
    } else {
        Err(Error::Candidate)
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

fn frame(event: &'static str, value: Value) -> Frame {
    Frame {
        event: Some(event.to_owned()),
        data: value.to_string(),
        id: None,
        retry_ms: None,
    }
}

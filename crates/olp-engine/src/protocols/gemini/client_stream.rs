use std::collections::BTreeMap;

use crate::domain::canonical::{
    events::{ErrorClass, Event, Kind},
    identity::Surface,
    requests::MessageRole,
};
use serde_json::{Value, json};
use thiserror::Error;

use super::finish_reason;
use crate::protocols::sse::{Frame, RAW_SSE_FRAME_EXTENSION, decode_raw_sse_frame};

#[derive(Debug, Error)]
pub enum Error {
    #[error("Gemini stream received events out of order")]
    Sequence,
    #[error("Gemini output role is not model")]
    Role,
    #[error("Gemini function call is incomplete or has invalid JSON arguments")]
    Tool,
    #[error("source extensions cannot be represented in a Gemini client stream")]
    Extension,
    #[error("Gemini stream completed with unfinished function calls")]
    UnfinishedTools,
}

#[derive(Debug)]
pub struct Encoder {
    public_model: String,
    fallback_id: String,
    expected_sequence: u64,
    response_id: Option<String>,
    tools: BTreeMap<(u32, u32), ToolState>,
    done: bool,
    skip_native_events: usize,
}

#[derive(Debug, Default)]
struct ToolState {
    id: Option<String>,
    name: Option<String>,
    arguments: String,
}

impl Encoder {
    #[must_use]
    pub fn new(public_model: impl Into<String>, fallback_id: impl Into<String>) -> Self {
        Self {
            public_model: public_model.into(),
            fallback_id: fallback_id.into(),
            expected_sequence: 0,
            response_id: None,
            tools: BTreeMap::new(),
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
            Kind::ResponseStart { response_id, .. } => {
                self.response_id = response_id;
            }
            Kind::MessageStart { role, .. } => {
                if role != MessageRole::Assistant {
                    return Err(Error::Role);
                }
            }
            Kind::TextDelta { output_index, text } => {
                frames.push(self.response_frame(json!({
                    "candidates": [{
                        "index": output_index,
                        "content": {"role": "model", "parts": [{"text": text}]}
                    }]
                })));
            }
            Kind::ToolCallDelta {
                output_index,
                tool_index,
                id,
                name,
                arguments_delta,
            } => {
                let tool = self.tools.entry((output_index, tool_index)).or_default();
                if let Some(id) = id {
                    if tool.id.as_ref().is_some_and(|existing| existing != &id) {
                        return Err(Error::Tool);
                    }
                    tool.id = Some(id);
                }
                if let Some(name) = name {
                    if tool.name.as_ref().is_some_and(|existing| existing != &name) {
                        return Err(Error::Tool);
                    }
                    tool.name = Some(name);
                }
                tool.arguments.push_str(&arguments_delta);
            }
            Kind::Usage { usage } => {
                frames.push(self.response_frame(json!({
                    "usageMetadata": {
                        "promptTokenCount": usage.input_tokens,
                        "candidatesTokenCount": usage.output_tokens,
                        "totalTokenCount": usage.total_tokens,
                        "cachedContentTokenCount": usage.cached_input_tokens,
                        "thoughtsTokenCount": usage.reasoning_tokens
                    }
                })));
            }
            Kind::Finish {
                output_index,
                reason,
            } => {
                let keys = self
                    .tools
                    .keys()
                    .filter(|(candidate, _)| *candidate == output_index)
                    .copied()
                    .collect::<Vec<_>>();
                let mut parts = Vec::with_capacity(keys.len());
                for key in keys {
                    let tool = self.tools.remove(&key).ok_or(Error::Tool)?;
                    let name = tool.name.ok_or(Error::Tool)?;
                    let args =
                        serde_json::from_str::<Value>(&tool.arguments).map_err(|_| Error::Tool)?;
                    parts.push(json!({
                        "functionCall": {"id": tool.id, "name": name, "args": args}
                    }));
                }
                frames.push(self.response_frame(json!({
                    "candidates": [{
                        "index": output_index,
                        "content": {"role": "model", "parts": parts},
                        "finishReason": finish_reason(&reason)
                    }]
                })));
            }
            Kind::Error { error } => {
                frames.push(Frame {
                    event: None,
                    data: json!({
                        "error": {
                            "code": error_status(error.class),
                            "message": error.message,
                            "status": error_code(error.class)
                        }
                    })
                    .to_string(),
                    id: None,
                    retry_ms: None,
                });
            }
            Kind::SourceExtension { extensions } => {
                // A stream already in flight cannot renegotiate, so extensions
                // that this surface cannot carry are dropped, not fatal.
                if extensions.source != Some(Surface::Gemini) {
                    return Ok(frames);
                }
                if let Some(value) = extensions.values.get(RAW_SSE_FRAME_EXTENSION) {
                    if extensions.values.len() != 1 {
                        return Err(Error::Extension);
                    }
                    let (mut raw, semantic_events) =
                        decode_raw_sse_frame(value).ok_or(Error::Extension)?;
                    rewrite_gemini_model(&mut raw, &self.public_model)?;
                    self.skip_native_events = semantic_events;
                    frames.push(raw);
                } else if !extensions.values.is_empty() {
                    return Ok(frames);
                }
            }
            Kind::RefusalDelta { .. } => {
                return Err(Error::Role);
            }
            Kind::Done => {
                if !self.tools.is_empty() {
                    return Err(Error::UnfinishedTools);
                }
                self.done = true;
            }
        }
        Ok(frames)
    }

    fn response_frame(&self, mut value: Value) -> Frame {
        let object = value
            .as_object_mut()
            .expect("Gemini stream chunks are always objects");
        object.insert(
            "responseId".into(),
            Value::String(
                self.response_id
                    .clone()
                    .unwrap_or_else(|| self.fallback_id.clone()),
            ),
        );
        object.insert(
            "modelVersion".into(),
            Value::String(self.public_model.clone()),
        );
        Frame {
            event: None,
            data: value.to_string(),
            id: None,
            retry_ms: None,
        }
    }
}

fn rewrite_gemini_model(frame: &mut Frame, public_model: &str) -> Result<(), Error> {
    let mut value: Value = serde_json::from_str(&frame.data).map_err(|_| Error::Extension)?;
    let object = value.as_object_mut().ok_or(Error::Extension)?;
    if object.contains_key("modelVersion") {
        object.insert(
            "modelVersion".into(),
            Value::String(public_model.to_owned()),
        );
    }
    frame.data = serde_json::to_string(&value).map_err(|_| Error::Extension)?;
    Ok(())
}

const fn error_status(class: ErrorClass) -> u16 {
    match class {
        ErrorClass::Authentication => 401,
        ErrorClass::Authorization => 403,
        ErrorClass::InvalidRequest => 400,
        ErrorClass::RateLimit => 429,
        ErrorClass::Timeout => 504,
        ErrorClass::Transport | ErrorClass::Upstream | ErrorClass::Internal => 500,
    }
}

const fn error_code(class: ErrorClass) -> &'static str {
    match class {
        ErrorClass::Authentication => "UNAUTHENTICATED",
        ErrorClass::Authorization => "PERMISSION_DENIED",
        ErrorClass::InvalidRequest => "INVALID_ARGUMENT",
        ErrorClass::RateLimit => "RESOURCE_EXHAUSTED",
        ErrorClass::Timeout => "DEADLINE_EXCEEDED",
        ErrorClass::Transport | ErrorClass::Upstream | ErrorClass::Internal => "INTERNAL",
    }
}

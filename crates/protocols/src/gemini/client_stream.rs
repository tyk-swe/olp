use std::collections::BTreeMap;

use olp_domain::{
    CanonicalEvent, CanonicalEventKind, ErrorClass, FinishReason, MessageRole, Surface,
};
use serde_json::{Value, json};
use thiserror::Error;

use crate::sse::{RAW_SSE_FRAME_EXTENSION, SseFrame, decode_raw_sse_frame};

#[derive(Debug, Error)]
pub enum ClientStreamEncodeError {
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
pub struct GeminiGenerateContentClientStreamEncoder {
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

impl GeminiGenerateContentClientStreamEncoder {
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
                self.response_id = response_id;
            }
            CanonicalEventKind::MessageStart { role, .. } => {
                if role != MessageRole::Assistant {
                    return Err(ClientStreamEncodeError::Role);
                }
            }
            CanonicalEventKind::TextDelta { output_index, text } => {
                frames.push(self.response_frame(json!({
                    "candidates": [{
                        "index": output_index,
                        "content": {"role": "model", "parts": [{"text": text}]}
                    }]
                })));
            }
            CanonicalEventKind::ToolCallDelta {
                output_index,
                tool_index,
                id,
                name,
                arguments_delta,
            } => {
                let tool = self.tools.entry((output_index, tool_index)).or_default();
                if let Some(id) = id {
                    if tool.id.as_ref().is_some_and(|existing| existing != &id) {
                        return Err(ClientStreamEncodeError::Tool);
                    }
                    tool.id = Some(id);
                }
                if let Some(name) = name {
                    if tool.name.as_ref().is_some_and(|existing| existing != &name) {
                        return Err(ClientStreamEncodeError::Tool);
                    }
                    tool.name = Some(name);
                }
                tool.arguments.push_str(&arguments_delta);
            }
            CanonicalEventKind::Usage { usage } => {
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
            CanonicalEventKind::Finish {
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
                    let tool = self
                        .tools
                        .remove(&key)
                        .ok_or(ClientStreamEncodeError::Tool)?;
                    let name = tool.name.ok_or(ClientStreamEncodeError::Tool)?;
                    let args = serde_json::from_str::<Value>(&tool.arguments)
                        .map_err(|_| ClientStreamEncodeError::Tool)?;
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
            CanonicalEventKind::Error { error } => {
                frames.push(SseFrame {
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
            CanonicalEventKind::SourceExtension { extensions } => {
                if extensions.source != Some(Surface::Gemini) {
                    return Err(ClientStreamEncodeError::Extension);
                }
                if let Some(value) = extensions.values.get(RAW_SSE_FRAME_EXTENSION) {
                    if extensions.values.len() != 1 {
                        return Err(ClientStreamEncodeError::Extension);
                    }
                    let (mut raw, semantic_events) =
                        decode_raw_sse_frame(value).ok_or(ClientStreamEncodeError::Extension)?;
                    rewrite_gemini_model(&mut raw, &self.public_model)?;
                    self.skip_native_events = semantic_events;
                    frames.push(raw);
                } else if !extensions.values.is_empty() {
                    return Err(ClientStreamEncodeError::Extension);
                }
            }
            CanonicalEventKind::RefusalDelta { .. } => {
                return Err(ClientStreamEncodeError::Role);
            }
            CanonicalEventKind::Done => {
                if !self.tools.is_empty() {
                    return Err(ClientStreamEncodeError::UnfinishedTools);
                }
                self.done = true;
            }
        }
        Ok(frames)
    }

    fn response_frame(&self, mut value: Value) -> SseFrame {
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
        SseFrame {
            event: None,
            data: value.to_string(),
            id: None,
            retry_ms: None,
        }
    }
}

fn rewrite_gemini_model(
    frame: &mut SseFrame,
    public_model: &str,
) -> Result<(), ClientStreamEncodeError> {
    let mut value: Value =
        serde_json::from_str(&frame.data).map_err(|_| ClientStreamEncodeError::Extension)?;
    let object = value
        .as_object_mut()
        .ok_or(ClientStreamEncodeError::Extension)?;
    if object.contains_key("modelVersion") {
        object.insert(
            "modelVersion".into(),
            Value::String(public_model.to_owned()),
        );
    }
    frame.data = serde_json::to_string(&value).map_err(|_| ClientStreamEncodeError::Extension)?;
    Ok(())
}

fn finish_reason(reason: &FinishReason) -> String {
    match reason {
        FinishReason::Stop | FinishReason::ToolCalls => "STOP".to_owned(),
        FinishReason::Length => "MAX_TOKENS".to_owned(),
        FinishReason::ContentFilter => "SAFETY".to_owned(),
        FinishReason::Error => "OTHER".to_owned(),
        FinishReason::Other(value) => value.clone(),
    }
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

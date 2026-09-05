use std::borrow::Cow;

use serde_json::{Value, json};
use thiserror::Error;

use crate::{
    domain::canonical::{
        events::{Event, FinishReason, Kind},
        identity::Surface,
        requests::MessageRole,
    },
    protocols::{extensions::materialize_response_pointer, openai::error_type, sse::Frame},
};

pub struct ChatCompletionStreamEncoder {
    response_id: String,
    created: i64,
    model: String,
    include_usage: bool,
    usage: Option<Value>,
}

impl ChatCompletionStreamEncoder {
    #[must_use]
    pub fn new(request_id: uuid::Uuid, model: &str, include_usage: bool, created: i64) -> Self {
        Self {
            response_id: format!("chatcmpl-{request_id}"),
            created,
            model: model.to_owned(),
            include_usage,
            usage: None,
        }
    }

    pub fn push(&mut self, event: Event) -> Result<Vec<Frame>, ChatClientEncodeError> {
        let value = match event.kind {
            Kind::ResponseStart { response_id, .. } => {
                if let Some(response_id) = response_id {
                    self.response_id = response_id;
                }
                return Ok(Vec::new());
            }
            Kind::MessageStart { output_index, role } => self.chunk(
                vec![json!({ "index": output_index, "delta": { "role": role_name(role) }, "finish_reason": null })],
                None,
            ),
            Kind::TextDelta { output_index, text } => self.chunk(
                vec![json!({ "index": output_index, "delta": { "content": text }, "finish_reason": null })],
                None,
            ),
            Kind::RefusalDelta { output_index, text } => self.chunk(
                vec![json!({ "index": output_index, "delta": { "refusal": text }, "finish_reason": null })],
                None,
            ),
            Kind::ToolCallDelta {
                output_index,
                tool_index,
                id,
                name,
                arguments_delta,
            } => self.tool_delta(output_index, tool_index, id, name, arguments_delta),
            Kind::Finish {
                output_index,
                reason,
            } => self.chunk(
                vec![json!({ "index": output_index, "delta": {}, "finish_reason": finish_name(reason) })],
                None,
            ),
            Kind::Usage { usage } => {
                self.usage = Some(json!({
                    "prompt_tokens": usage.input_tokens,
                    "completion_tokens": usage.output_tokens
                        .saturating_add(usage.reasoning_tokens.unwrap_or(0)),
                    "total_tokens": usage.total_tokens,
                    "prompt_tokens_details": { "cached_tokens": usage.cached_input_tokens },
                    "completion_tokens_details": { "reasoning_tokens": usage.reasoning_tokens }
                }));
                return Ok(Vec::new());
            }
            Kind::SourceExtension { extensions } => {
                if extensions.source != Some(Surface::OpenAi) {
                    return Ok(Vec::new());
                }
                let mut value = self.chunk(Vec::new(), None);
                for (pointer, extension) in extensions.values {
                    materialize_response_pointer(&mut value, &pointer, extension)
                        .map_err(|_| ChatClientEncodeError::InvalidExtension(pointer))?;
                }
                value
            }
            Kind::Error { error } => {
                return Ok(vec![data_frame(json!({
                    "error": {
                        "message": error.message,
                        "type": error_type(error.class),
                        "code": error.provider_code
                    }
                }))]);
            }
            Kind::Done => {
                let mut frames = Vec::new();
                if let Some(usage) = self.usage.take()
                    && self.include_usage
                {
                    frames.push(data_frame(self.chunk(Vec::new(), Some(usage))));
                }
                frames.push(Frame {
                    event: None,
                    data: "[DONE]".to_owned(),
                    id: None,
                    retry_ms: None,
                });
                return Ok(frames);
            }
        };
        Ok(vec![data_frame(value)])
    }

    fn tool_delta(
        &self,
        output_index: u32,
        tool_index: u32,
        id: Option<String>,
        name: Option<String>,
        arguments_delta: String,
    ) -> Value {
        let mut function = serde_json::Map::new();
        if let Some(name) = name {
            function.insert("name".to_owned(), Value::String(name));
        }
        function.insert("arguments".to_owned(), Value::String(arguments_delta));
        let mut call = serde_json::Map::new();
        call.insert("index".to_owned(), Value::from(tool_index));
        if let Some(id) = id {
            call.insert("id".to_owned(), Value::String(id));
            call.insert("type".to_owned(), Value::String("function".to_owned()));
        }
        call.insert("function".to_owned(), Value::Object(function));
        self.chunk(
            vec![json!({
                "index": output_index,
                "delta": { "tool_calls": [Value::Object(call)] },
                "finish_reason": null
            })],
            None,
        )
    }

    fn chunk(&self, choices: Vec<Value>, usage: Option<Value>) -> Value {
        let mut value = json!({
            "id": self.response_id,
            "object": "chat.completion.chunk",
            "created": self.created,
            "model": self.model,
            "choices": choices
        });
        if let Some(usage) = usage {
            value["usage"] = usage;
        }
        value
    }
}

fn data_frame(value: Value) -> Frame {
    Frame {
        event: None,
        data: value.to_string(),
        id: None,
        retry_ms: None,
    }
}

fn role_name(role: MessageRole) -> &'static str {
    match role {
        MessageRole::System => "system",
        MessageRole::Developer => "developer",
        MessageRole::User => "user",
        MessageRole::Assistant => "assistant",
        MessageRole::Tool => "tool",
    }
}

/// The `finish_reason` literals the OpenAI SDKs declare. A value outside this
/// set (Gemini `OTHER`, Anthropic `pause_turn`) fails a strictly typed client
/// on an otherwise successful response, so it is clamped to `stop`; the raw
/// value still reaches same-surface clients as a source extension.
const OPENAI_FINISH_REASONS: [&str; 5] = [
    "stop",
    "length",
    "tool_calls",
    "content_filter",
    "function_call",
];

#[must_use]
pub fn finish_name(reason: FinishReason) -> Cow<'static, str> {
    match reason {
        FinishReason::Stop => Cow::Borrowed("stop"),
        FinishReason::Length => Cow::Borrowed("length"),
        FinishReason::ToolCalls => Cow::Borrowed("tool_calls"),
        FinishReason::ContentFilter => Cow::Borrowed("content_filter"),
        FinishReason::Error => Cow::Borrowed("error"),
        FinishReason::Other(value) if OPENAI_FINISH_REASONS.contains(&value.as_str()) => {
            Cow::Owned(value)
        }
        FinishReason::Other(_) => Cow::Borrowed("stop"),
    }
}

#[derive(Debug, Error)]
pub enum ChatClientEncodeError {
    #[error("invalid source extension path: {0}")]
    InvalidExtension(String),
}

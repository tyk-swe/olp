use std::{borrow::Cow, collections::BTreeMap};

use axum::body::Bytes;
use olp_engine::domain::canonical::{
    events::{Event, FinishReason, Kind},
    identity::Surface,
    requests::MessageRole,
};
use serde_json::{Value, json};

use super::{
    error::InferenceError,
    openai_http::{error_type, sse_json, unix_seconds},
};

pub(crate) struct OpenAiChatCompletionStreamEncoder {
    response_id: String,
    created: i64,
    model: String,
    include_usage: bool,
    usage: Option<Value>,
}

impl OpenAiChatCompletionStreamEncoder {
    pub(crate) fn new(request_id: uuid::Uuid, model: &str, include_usage: bool) -> Self {
        Self {
            response_id: format!("chatcmpl-{request_id}"),
            created: unix_seconds(),
            model: model.to_owned(),
            include_usage,
            usage: None,
        }
    }

    pub(crate) fn encode(&mut self, event: Event) -> Result<Vec<Bytes>, InferenceError> {
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
            } => {
                // Real OpenAI omits `id`, `type`, and `function.name` on
                // continuation chunks. Emitting explicit nulls clobbers the id
                // in accumulators that assign unconditionally, and makes
                // name-concatenating accumulators raise a TypeError.
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
            Kind::Finish {
                output_index,
                reason,
            } => self.chunk(
                vec![json!({ "index": output_index, "delta": {}, "finish_reason": finish_name(reason) })],
                None,
            ),
            // A provider may report running totals on every chunk (Gemini
            // does). OpenAI sends exactly one usage-only chunk, immediately
            // before `[DONE]`, so only the last total is kept and emitted
            // there — and only when the client asked for it.
            Kind::Usage { usage } => {
                self.usage = Some(json!({
                    "prompt_tokens": usage.input_tokens,
                    "completion_tokens": usage
                        .output_tokens
                        .saturating_add(usage.reasoning_tokens.unwrap_or(0)),
                    "total_tokens": usage.total_tokens,
                    "prompt_tokens_details": { "cached_tokens": usage.cached_input_tokens },
                    "completion_tokens_details": { "reasoning_tokens": usage.reasoning_tokens }
                }));
                return Ok(Vec::new());
            }
            Kind::SourceExtension { extensions } => {
                // Extensions from another surface have no representation here.
                // Dropping them keeps a Gemini or Anthropic upstream usable from
                // an OpenAI client instead of failing the response mid-stream.
                if extensions.source != Some(Surface::OpenAi) {
                    return Ok(Vec::new());
                }
                let mut value = self.chunk(Vec::new(), None);
                for (pointer, extension) in extensions.values {
                    set_json_pointer(&mut value, &pointer, extension).map_err(|()| {
                        InferenceError::bad_gateway(
                            "provider_protocol_error",
                            format!("The provider extension path {pointer} is not representable."),
                        )
                    })?;
                }
                value
            }
            Kind::Error { error } => {
                return Ok(vec![sse_json(&json!({
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
                    frames.push(sse_json(&self.chunk(Vec::new(), Some(usage))));
                }
                frames.push(Bytes::from_static(b"data: [DONE]\n\n"));
                return Ok(frames);
            }
        };
        Ok(vec![sse_json(&value)])
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

#[derive(Default)]
struct UnaryChoice {
    content: String,
    refusal: String,
    tools: BTreeMap<u32, UnaryTool>,
    finish_reason: Option<String>,
}

#[derive(Default)]
struct UnaryTool {
    id: String,
    name: String,
    arguments: String,
}

pub(crate) fn aggregate_chat_completion_response(
    request_id: uuid::Uuid,
    model: &str,
    events: &[Event],
) -> Result<Value, InferenceError> {
    let mut id = format!("chatcmpl-{request_id}");
    let mut choices: BTreeMap<u32, UnaryChoice> = BTreeMap::new();
    let mut usage = None;
    let mut extensions = Vec::new();
    for event in events {
        match &event.kind {
            Kind::ResponseStart { response_id, .. } => {
                if let Some(response_id) = response_id {
                    id.clone_from(response_id);
                }
            }
            Kind::MessageStart { output_index, .. } => {
                choices.entry(*output_index).or_default();
            }
            Kind::TextDelta { output_index, text } => {
                choices
                    .entry(*output_index)
                    .or_default()
                    .content
                    .push_str(text);
            }
            Kind::RefusalDelta { output_index, text } => {
                choices
                    .entry(*output_index)
                    .or_default()
                    .refusal
                    .push_str(text);
            }
            Kind::ToolCallDelta {
                output_index,
                tool_index,
                id,
                name,
                arguments_delta,
            } => {
                let tool = choices
                    .entry(*output_index)
                    .or_default()
                    .tools
                    .entry(*tool_index)
                    .or_default();
                if let Some(id) = id {
                    tool.id.clone_from(id);
                }
                if let Some(name) = name {
                    tool.name.clone_from(name);
                }
                tool.arguments.push_str(arguments_delta);
            }
            Kind::Finish {
                output_index,
                reason,
            } => {
                choices.entry(*output_index).or_default().finish_reason =
                    Some(finish_name(reason.clone()).into_owned());
            }
            Kind::Usage { usage: value } => {
                usage = Some(json!({
                    "prompt_tokens": value.input_tokens,
                    "completion_tokens": value
                        .output_tokens
                        .saturating_add(value.reasoning_tokens.unwrap_or(0)),
                    "total_tokens": value.total_tokens,
                    "prompt_tokens_details": { "cached_tokens": value.cached_input_tokens },
                    "completion_tokens_details": { "reasoning_tokens": value.reasoning_tokens }
                }));
            }
            Kind::SourceExtension { extensions: values } => {
                if values.source != Some(Surface::OpenAi) {
                    continue;
                }
                extensions.extend(
                    values
                        .values
                        .iter()
                        .map(|(key, value)| (key.clone(), value.clone())),
                );
            }
            Kind::Error { error } => {
                return Err(InferenceError::from_canonical(error));
            }
            Kind::Done => {}
        }
    }
    let choices = choices
        .into_iter()
        .map(|(index, choice)| {
            let tools = choice
                .tools
                .into_values()
                .map(|tool| {
                    json!({
                        "id": tool.id,
                        "type": "function",
                        "function": { "name": tool.name, "arguments": tool.arguments }
                    })
                })
                .collect::<Vec<_>>();
            let mut message = json!({
                "role": "assistant",
                "content": (!choice.content.is_empty()).then_some(choice.content),
                "refusal": (!choice.refusal.is_empty()).then_some(choice.refusal),
            });
            if !tools.is_empty()
                && let Some(object) = message.as_object_mut()
            {
                object.insert("tool_calls".to_owned(), Value::from(tools));
            }
            json!({
                "index": index,
                "message": message,
                "finish_reason": choice.finish_reason
            })
        })
        .collect::<Vec<_>>();
    let mut response = json!({
        "id": id,
        "object": "chat.completion",
        "created": unix_seconds(),
        "model": model,
        "choices": choices,
        "usage": usage
    });
    for (pointer, value) in extensions {
        set_json_pointer(&mut response, &pointer, value).map_err(|()| {
            InferenceError::bad_gateway(
                "provider_protocol_error",
                format!("The provider extension path {pointer} is not representable."),
            )
        })?;
    }
    Ok(response)
}

fn set_json_pointer(root: &mut Value, pointer: &str, value: Value) -> Result<(), ()> {
    if !pointer.starts_with('/') || pointer.len() > 1_024 {
        return Err(());
    }
    let segments = pointer[1..]
        .split('/')
        .map(|segment| segment.replace("~1", "/").replace("~0", "~"))
        .collect::<Vec<_>>();
    if segments.len() > 16 {
        return Err(());
    }
    let mut current = root;
    for (index, segment) in segments.iter().enumerate() {
        if index + 1 == segments.len() {
            match current {
                Value::Object(object) => {
                    object.insert(segment.clone(), value);
                    return Ok(());
                }
                Value::Array(array) => {
                    let position: usize = segment.parse().map_err(|_| ())?;
                    while array.len() <= position {
                        array.push(Value::Null);
                    }
                    array[position] = value;
                    return Ok(());
                }
                _ => return Err(()),
            }
        }
        let next_is_index = segments
            .get(index + 1)
            .is_some_and(|next| next.parse::<usize>().is_ok());
        current = match current {
            Value::Object(object) => object.entry(segment.clone()).or_insert_with(|| {
                if next_is_index {
                    Value::Array(Vec::new())
                } else {
                    Value::Object(Default::default())
                }
            }),
            Value::Array(array) => {
                let position: usize = segment.parse().map_err(|_| ())?;
                while array.len() <= position {
                    let mut next_value = if next_is_index {
                        Value::Array(Vec::new())
                    } else {
                        Value::Object(Default::default())
                    };
                    if let Value::Object(object) = &mut next_value {
                        object.insert("index".to_owned(), Value::from(array.len()));
                    }
                    array.push(next_value);
                }
                &mut array[position]
            }
            _ => return Err(()),
        };
    }
    Err(())
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

fn finish_name(reason: FinishReason) -> Cow<'static, str> {
    match reason {
        FinishReason::Stop => Cow::Borrowed("stop"),
        FinishReason::Length => Cow::Borrowed("length"),
        FinishReason::ToolCalls => Cow::Borrowed("tool_calls"),
        FinishReason::ContentFilter => Cow::Borrowed("content_filter"),
        FinishReason::Error => Cow::Borrowed("stop"),
        FinishReason::Other(value) if OPENAI_FINISH_REASONS.contains(&value.as_str()) => {
            Cow::Owned(value)
        }
        FinishReason::Other(_) => Cow::Borrowed("stop"),
    }
}

#[cfg(test)]
mod tests;

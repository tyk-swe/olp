use std::{
    borrow::Cow,
    collections::BTreeMap,
    time::{SystemTime, UNIX_EPOCH},
};

use axum::body::Bytes;
use olp_domain::{
    CanonicalEvent, CanonicalEventKind, ErrorClass, FinishReason, MessageRole, Surface,
};
use olp_protocols::sse::{SseFrame, encode_frame};
use serde_json::{Value, json};

use crate::gateway::InferenceError;

pub(crate) struct OpenAiStreamEncoder {
    response_id: String,
    created: i64,
    model: String,
}

impl OpenAiStreamEncoder {
    pub(crate) fn new(request_id: uuid::Uuid, model: &str) -> Self {
        Self {
            response_id: format!("chatcmpl-{request_id}"),
            created: unix_seconds(),
            model: model.to_owned(),
        }
    }

    pub(crate) fn encode(&mut self, event: CanonicalEvent) -> Result<Vec<Bytes>, InferenceError> {
        let value = match event.kind {
            CanonicalEventKind::ResponseStart { response_id, .. } => {
                if let Some(response_id) = response_id {
                    self.response_id = response_id;
                }
                return Ok(Vec::new());
            }
            CanonicalEventKind::MessageStart { output_index, role } => self.chunk(
                vec![json!({ "index": output_index, "delta": { "role": role_name(role) }, "finish_reason": null })],
                None,
            ),
            CanonicalEventKind::TextDelta { output_index, text } => self.chunk(
                vec![json!({ "index": output_index, "delta": { "content": text }, "finish_reason": null })],
                None,
            ),
            CanonicalEventKind::RefusalDelta { output_index, text } => self.chunk(
                vec![json!({ "index": output_index, "delta": { "refusal": text }, "finish_reason": null })],
                None,
            ),
            CanonicalEventKind::ToolCallDelta {
                output_index,
                tool_index,
                id,
                name,
                arguments_delta,
            } => self.chunk(
                vec![json!({
                    "index": output_index,
                    "delta": { "tool_calls": [{
                        "index": tool_index,
                        "id": id,
                        "type": "function",
                        "function": { "name": name, "arguments": arguments_delta }
                    }]},
                    "finish_reason": null
                })],
                None,
            ),
            CanonicalEventKind::Finish {
                output_index,
                reason,
            } => self.chunk(
                vec![json!({ "index": output_index, "delta": {}, "finish_reason": finish_name(reason) })],
                None,
            ),
            CanonicalEventKind::Usage { usage } => self.chunk(
                Vec::new(),
                Some(json!({
                    "prompt_tokens": usage.input_tokens,
                    "completion_tokens": usage.output_tokens,
                    "total_tokens": usage.total_tokens,
                    "prompt_tokens_details": { "cached_tokens": usage.cached_input_tokens },
                    "completion_tokens_details": { "reasoning_tokens": usage.reasoning_tokens }
                })),
            ),
            CanonicalEventKind::SourceExtension { extensions } => {
                if extensions.source != Some(Surface::OpenAi) {
                    return Err(InferenceError::bad_gateway(
                        "provider_protocol_error",
                        "A provider emitted extensions for a different client protocol.",
                    ));
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
            CanonicalEventKind::Error { error } => {
                return Ok(vec![sse_json(&json!({
                    "error": {
                        "message": error.message,
                        "type": error_type(error.class),
                        "code": error.provider_code
                    }
                }))]);
            }
            CanonicalEventKind::Done => {
                return Ok(vec![Bytes::from_static(b"data: [DONE]\n\n")]);
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

pub(crate) fn aggregate_openai_response(
    request_id: uuid::Uuid,
    model: &str,
    events: &[CanonicalEvent],
) -> Result<Value, InferenceError> {
    let mut id = format!("chatcmpl-{request_id}");
    let mut choices: BTreeMap<u32, UnaryChoice> = BTreeMap::new();
    let mut usage = None;
    let mut extensions = Vec::new();
    for event in events {
        match &event.kind {
            CanonicalEventKind::ResponseStart { response_id, .. } => {
                if let Some(response_id) = response_id {
                    id.clone_from(response_id);
                }
            }
            CanonicalEventKind::MessageStart { output_index, .. } => {
                choices.entry(*output_index).or_default();
            }
            CanonicalEventKind::TextDelta { output_index, text } => {
                choices
                    .entry(*output_index)
                    .or_default()
                    .content
                    .push_str(text);
            }
            CanonicalEventKind::RefusalDelta { output_index, text } => {
                choices
                    .entry(*output_index)
                    .or_default()
                    .refusal
                    .push_str(text);
            }
            CanonicalEventKind::ToolCallDelta {
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
            CanonicalEventKind::Finish {
                output_index,
                reason,
            } => {
                choices.entry(*output_index).or_default().finish_reason =
                    Some(finish_name(reason.clone()).into_owned());
            }
            CanonicalEventKind::Usage { usage: value } => {
                usage = Some(json!({
                    "prompt_tokens": value.input_tokens,
                    "completion_tokens": value.output_tokens,
                    "total_tokens": value.total_tokens,
                    "prompt_tokens_details": { "cached_tokens": value.cached_input_tokens },
                    "completion_tokens_details": { "reasoning_tokens": value.reasoning_tokens }
                }));
            }
            CanonicalEventKind::SourceExtension { extensions: values } => {
                if values.source != Some(Surface::OpenAi) {
                    return Err(InferenceError::bad_gateway(
                        "provider_protocol_error",
                        "A provider emitted extensions for a different client protocol.",
                    ));
                }
                extensions.extend(
                    values
                        .values
                        .iter()
                        .map(|(key, value)| (key.clone(), value.clone())),
                );
            }
            CanonicalEventKind::Error { error } => {
                return Err(InferenceError::from_canonical(error));
            }
            CanonicalEventKind::Done => {}
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
            json!({
                "index": index,
                "message": {
                    "role": "assistant",
                    "content": choice.content,
                    "refusal": (!choice.refusal.is_empty()).then_some(choice.refusal),
                    "tool_calls": tools
                },
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

pub(crate) fn error_sse(error: &InferenceError) -> Bytes {
    sse_json(&json!({ "error": {
        "message": error.message(),
        "type": error.kind(),
        "param": null,
        "code": error.code()
    }}))
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

fn finish_name(reason: FinishReason) -> Cow<'static, str> {
    match reason {
        FinishReason::Stop => Cow::Borrowed("stop"),
        FinishReason::Length => Cow::Borrowed("length"),
        FinishReason::ToolCalls => Cow::Borrowed("tool_calls"),
        FinishReason::ContentFilter => Cow::Borrowed("content_filter"),
        FinishReason::Error => Cow::Borrowed("error"),
        FinishReason::Other(value) => Cow::Owned(value),
    }
}

pub(crate) fn error_type(class: ErrorClass) -> &'static str {
    match class {
        ErrorClass::Authentication => "authentication_error",
        ErrorClass::Authorization => "permission_error",
        ErrorClass::InvalidRequest => "invalid_request_error",
        ErrorClass::RateLimit => "rate_limit_error",
        ErrorClass::Timeout => "timeout_error",
        ErrorClass::Transport | ErrorClass::Upstream => "upstream_error",
        ErrorClass::Internal => "internal_error",
    }
}

fn sse_json(value: &Value) -> Bytes {
    Bytes::from(
        encode_frame(&SseFrame {
            event: None,
            data: value.to_string(),
            id: None,
            retry_ms: None,
        })
        .expect("data-only SSE frame is valid"),
    )
}

pub(crate) fn unix_seconds() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .ok()
        .and_then(|duration| i64::try_from(duration.as_secs()).ok())
        .unwrap_or_default()
}

#[cfg(test)]
mod tests;

use std::collections::BTreeMap;

use crate::domain::canonical::{
    events::{Error, ErrorClass, Event, FinishReason, Kind, Usage as CanonicalUsage},
    identity::Surface,
    requests::{MessageRole, SourceExtensions},
};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

use super::super::extensions::collect_extra;
use super::OPENAI_RESPONSES_RAW_OUTPUT_PREFIX;
use super::errors::ResponsesCodecError;
use super::helpers::collect_object_extra;
use crate::protocols::CanonicalEventBuilder as ResponsesEventBuilder;

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct Object {
    pub id: String,
    pub object: String,
    pub created_at: i64,
    pub status: String,
    pub model: String,
    #[serde(default)]
    pub output: Vec<Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub usage: Option<Usage>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub error: Option<ErrorBody>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub incomplete_details: Option<Value>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct Usage {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub total_tokens: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub input_tokens_details: Option<InputTokenDetails>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub output_tokens_details: Option<OutputTokenDetails>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct InputTokenDetails {
    #[serde(default)]
    pub cached_tokens: u64,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct OutputTokenDetails {
    #[serde(default)]
    pub reasoning_tokens: u64,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ErrorBody {
    pub code: String,
    pub message: String,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_response_object(response: Object) -> Result<Vec<Event>, ResponsesCodecError> {
    if response.object != "response" {
        return Err(ResponsesCodecError::InvalidResponse(response.object));
    }
    let mut builder = ResponsesEventBuilder::default();
    builder.push(Kind::ResponseStart {
        response_id: Some(response.id),
        provider_model: Some(response.model),
    });
    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);
    extensions.insert("/created_at".into(), Value::from(response.created_at));
    extensions.insert("/status".into(), Value::String(response.status.clone()));
    let response_incomplete_reason = response
        .incomplete_details
        .as_ref()
        .and_then(|details| details.get("reason"))
        .and_then(Value::as_str)
        .map(str::to_owned);
    if let Some(details) = response.incomplete_details {
        extensions.insert("/incomplete_details".into(), details);
    }

    // Responses output items are parts of one assistant turn, not separate
    // candidates: a parallel tool call must not become `choices[1]`, where a
    // client reading `choices[0]` silently loses it. They all fold into
    // canonical output 0; extension paths keep the wire item index so the
    // encoder can rebuild the original array.
    let mut turn = OutputTurn::default();
    for (item_index, item) in response.output.into_iter().enumerate() {
        let item_index: u32 = item_index
            .try_into()
            .map_err(|_| ResponsesCodecError::TooManyOutputItems)?;
        decode_response_output_item(item_index, item, &mut extensions, &mut builder, &mut turn)?;
    }
    if let Some(usage) = response.usage {
        collect_response_usage_extensions(&usage, &mut extensions);
        builder.push(Kind::Usage {
            usage: canonical_response_usage(&usage),
        });
    }
    if let Some(error) = response.error {
        collect_extra("/error", &error.extra, &mut extensions);
        let retryable = crate::protocols::openai::error_signals_rate_limit(
            Some(&error.code),
            error.extra.get("type").and_then(Value::as_str),
        );
        builder.push(Kind::Error {
            error: Error {
                class: if retryable {
                    ErrorClass::RateLimit
                } else {
                    ErrorClass::Upstream
                },
                message: error.message,
                provider_code: Some(error.code),
                retryable,
            },
        });
    }
    if !extensions.is_empty() {
        builder.push(Kind::SourceExtension {
            extensions: SourceExtensions::new(Surface::OpenAi, extensions),
        });
    }
    if turn.started {
        builder.push(Kind::Finish {
            output_index: CANONICAL_OUTPUT_INDEX,
            reason: turn.finish_reason(&response.status, response_incomplete_reason.as_deref()),
        });
    }
    builder.push(Kind::Done);
    Ok(builder.events)
}

const CANONICAL_OUTPUT_INDEX: u32 = 0;

#[derive(Default)]
struct OutputTurn {
    started: bool,
    tool_index: u32,
    saw_tool_call: bool,
}

impl OutputTurn {
    fn start(&mut self, builder: &mut ResponsesEventBuilder, role: MessageRole) {
        if !self.started {
            self.started = true;
            builder.push(Kind::MessageStart {
                output_index: CANONICAL_OUTPUT_INDEX,
                role,
            });
        }
    }

    /// The streaming decoder derives the same reasons from the same fields; the
    /// unary path used to hardcode `Stop`, so a truncated response reached the
    /// client indistinguishable from a completed one.
    fn finish_reason(&self, status: &str, incomplete_reason: Option<&str>) -> FinishReason {
        if self.saw_tool_call {
            return FinishReason::ToolCalls;
        }
        if status == "incomplete" {
            return match incomplete_reason {
                Some("content_filter") => FinishReason::ContentFilter,
                _ => FinishReason::Length,
            };
        }
        FinishReason::Stop
    }
}

fn decode_response_output_item(
    item_index: u32,
    item: Value,
    extensions: &mut BTreeMap<String, Value>,
    builder: &mut ResponsesEventBuilder,
    turn: &mut OutputTurn,
) -> Result<(), ResponsesCodecError> {
    let Value::Object(mut object) = item else {
        return Err(ResponsesCodecError::InvalidResponse(
            "output item is not an object".into(),
        ));
    };
    let kind = take_required_output_string(&mut object, "type")?;
    match kind.as_str() {
        "message" => {
            let role = match take_required_output_string(&mut object, "role")?.as_str() {
                "assistant" => MessageRole::Assistant,
                value => return Err(ResponsesCodecError::UnsupportedRole(value.into())),
            };
            let content = object
                .remove("content")
                .and_then(|value| value.as_array().cloned())
                .ok_or_else(|| ResponsesCodecError::InvalidResponse("message content".into()))?;
            turn.start(builder, role);
            for (part_index, part) in content.into_iter().enumerate() {
                let Value::Object(mut part) = part else {
                    return Err(ResponsesCodecError::InvalidResponse(
                        "output content part".into(),
                    ));
                };
                let part_kind = take_required_output_string(&mut part, "type")?;
                match part_kind.as_str() {
                    "output_text" => builder.push(Kind::TextDelta {
                        output_index: CANONICAL_OUTPUT_INDEX,
                        text: take_required_output_string(&mut part, "text")?,
                    }),
                    "refusal" => builder.push(Kind::RefusalDelta {
                        output_index: CANONICAL_OUTPUT_INDEX,
                        text: take_required_output_string(&mut part, "refusal")?,
                    }),
                    _ => return Err(ResponsesCodecError::UnsupportedOutputItem(part_kind)),
                }
                collect_object_extra(
                    &format!("/output/{item_index}/content/{part_index}"),
                    part,
                    extensions,
                );
            }
            collect_object_extra(&format!("/output/{item_index}"), object, extensions);
        }
        "function_call" => {
            let id = object
                .remove("call_id")
                .or_else(|| object.remove("id"))
                .and_then(|value| value.as_str().map(str::to_owned));
            let name = Some(take_required_output_string(&mut object, "name")?);
            let arguments_delta = take_required_output_string(&mut object, "arguments")?;
            turn.start(builder, MessageRole::Assistant);
            builder.push(Kind::ToolCallDelta {
                output_index: CANONICAL_OUTPUT_INDEX,
                tool_index: turn.tool_index,
                id,
                name,
                arguments_delta,
            });
            turn.tool_index = turn
                .tool_index
                .checked_add(1)
                .ok_or(ResponsesCodecError::TooManyOutputItems)?;
            turn.saw_tool_call = true;
            collect_object_extra(&format!("/output/{item_index}"), object, extensions);
        }
        _ => {
            object.insert("type".into(), Value::String(kind));
            extensions.insert(
                format!("{OPENAI_RESPONSES_RAW_OUTPUT_PREFIX}/{item_index}"),
                Value::Object(object),
            );
        }
    }
    Ok(())
}

fn take_required_output_string(
    object: &mut Map<String, Value>,
    field: &'static str,
) -> Result<String, ResponsesCodecError> {
    object
        .remove(field)
        .and_then(|value| value.as_str().map(str::to_owned))
        .ok_or_else(|| ResponsesCodecError::InvalidResponse(field.into()))
}

pub(super) fn canonical_response_usage(usage: &Usage) -> CanonicalUsage {
    let reasoning_tokens = usage
        .output_tokens_details
        .as_ref()
        .map(|details| details.reasoning_tokens);
    CanonicalUsage {
        input_tokens: usage.input_tokens,
        // The Responses API counts reasoning inside `output_tokens`; canonical
        // `output_tokens` is disjoint from `reasoning_tokens`.
        output_tokens: usage
            .output_tokens
            .saturating_sub(reasoning_tokens.unwrap_or(0)),
        total_tokens: usage.total_tokens,
        cached_input_tokens: usage
            .input_tokens_details
            .as_ref()
            .map(|details| details.cached_tokens),
        reasoning_tokens,
    }
}

fn collect_response_usage_extensions(usage: &Usage, extensions: &mut BTreeMap<String, Value>) {
    collect_extra("/usage", &usage.extra, extensions);
    if let Some(details) = &usage.input_tokens_details {
        collect_extra("/usage/input_tokens_details", &details.extra, extensions);
    }
    if let Some(details) = &usage.output_tokens_details {
        collect_extra("/usage/output_tokens_details", &details.extra, extensions);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn response_rate_limit_error_is_retryable() {
        let events = decode_response_object(Object {
            id: "resp_test".to_owned(),
            object: "response".to_owned(),
            created_at: 1,
            status: "failed".to_owned(),
            model: "gpt-test".to_owned(),
            output: Vec::new(),
            usage: None,
            error: Some(ErrorBody {
                code: "rate_limit_exceeded".to_owned(),
                message: "slow down".to_owned(),
                extra: BTreeMap::new(),
            }),
            incomplete_details: None,
            extra: BTreeMap::new(),
        })
        .unwrap();

        let error = events
            .iter()
            .find_map(|event| match &event.kind {
                Kind::Error { error } => Some(error),
                _ => None,
            })
            .expect("failed response must emit a canonical error");
        assert_eq!(error.class, ErrorClass::RateLimit);
        assert!(error.retryable);
    }
}

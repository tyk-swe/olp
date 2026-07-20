use std::collections::BTreeMap;

use olp_domain::{
    CanonicalError, CanonicalEvent, CanonicalEventKind, ErrorClass, FinishReason, MessageRole,
    SourceExtensions, Surface, Usage,
};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

use super::super::extensions::collect_extra;
use super::OPENAI_RESPONSES_RAW_OUTPUT_PREFIX;
use super::errors::ResponsesCodecError;
use super::helpers::collect_object_extra;

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseObject {
    pub id: String,
    pub object: String,
    pub created_at: i64,
    pub status: String,
    pub model: String,
    #[serde(default)]
    pub output: Vec<Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub usage: Option<ResponseUsage>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub error: Option<ResponseErrorBody>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub incomplete_details: Option<Value>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseUsage {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub total_tokens: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub input_tokens_details: Option<ResponseInputTokenDetails>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub output_tokens_details: Option<ResponseOutputTokenDetails>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseInputTokenDetails {
    #[serde(default)]
    pub cached_tokens: u64,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseOutputTokenDetails {
    #[serde(default)]
    pub reasoning_tokens: u64,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseErrorBody {
    pub code: String,
    pub message: String,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_response_object(
    response: ResponseObject,
) -> Result<Vec<CanonicalEvent>, ResponsesCodecError> {
    if response.object != "response" {
        return Err(ResponsesCodecError::InvalidResponse(response.object));
    }
    let mut builder = ResponsesEventBuilder::default();
    builder.push(CanonicalEventKind::ResponseStart {
        response_id: Some(response.id),
        provider_model: Some(response.model),
    });
    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);
    extensions.insert("/created_at".into(), Value::from(response.created_at));
    extensions.insert("/status".into(), Value::String(response.status.clone()));
    if let Some(details) = response.incomplete_details {
        extensions.insert("/incomplete_details".into(), details);
    }

    for (output_index, item) in response.output.into_iter().enumerate() {
        decode_response_output_item(
            output_index
                .try_into()
                .map_err(|_| ResponsesCodecError::TooManyOutputItems)?,
            item,
            &mut extensions,
            &mut builder,
        )?;
    }
    if let Some(usage) = response.usage {
        collect_response_usage_extensions(&usage, &mut extensions);
        builder.push(CanonicalEventKind::Usage {
            usage: canonical_response_usage(&usage),
        });
    }
    if let Some(error) = response.error {
        collect_extra("/error", &error.extra, &mut extensions);
        builder.push(CanonicalEventKind::Error {
            error: CanonicalError {
                class: ErrorClass::Upstream,
                message: error.message,
                provider_code: Some(error.code),
                retryable: false,
            },
        });
    }
    if !extensions.is_empty() {
        builder.push(CanonicalEventKind::SourceExtension {
            extensions: SourceExtensions::new(Surface::OpenAi, extensions),
        });
    }
    builder.push(CanonicalEventKind::Done);
    Ok(builder.events)
}

fn decode_response_output_item(
    output_index: u32,
    item: Value,
    extensions: &mut BTreeMap<String, Value>,
    builder: &mut ResponsesEventBuilder,
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
            builder.push(CanonicalEventKind::MessageStart { output_index, role });
            for (part_index, part) in content.into_iter().enumerate() {
                let Value::Object(mut part) = part else {
                    return Err(ResponsesCodecError::InvalidResponse(
                        "output content part".into(),
                    ));
                };
                let part_kind = take_required_output_string(&mut part, "type")?;
                match part_kind.as_str() {
                    "output_text" => builder.push(CanonicalEventKind::TextDelta {
                        output_index,
                        text: take_required_output_string(&mut part, "text")?,
                    }),
                    "refusal" => builder.push(CanonicalEventKind::RefusalDelta {
                        output_index,
                        text: take_required_output_string(&mut part, "refusal")?,
                    }),
                    _ => return Err(ResponsesCodecError::UnsupportedOutputItem(part_kind)),
                }
                collect_object_extra(
                    &format!("/output/{output_index}/content/{part_index}"),
                    part,
                    extensions,
                );
            }
            collect_object_extra(&format!("/output/{output_index}"), object, extensions);
            builder.push(CanonicalEventKind::Finish {
                output_index,
                reason: FinishReason::Stop,
            });
        }
        "function_call" => {
            let id = object
                .remove("call_id")
                .or_else(|| object.remove("id"))
                .and_then(|value| value.as_str().map(str::to_owned));
            let name = Some(take_required_output_string(&mut object, "name")?);
            let arguments_delta = take_required_output_string(&mut object, "arguments")?;
            builder.push(CanonicalEventKind::MessageStart {
                output_index,
                role: MessageRole::Assistant,
            });
            builder.push(CanonicalEventKind::ToolCallDelta {
                output_index,
                tool_index: 0,
                id,
                name,
                arguments_delta,
            });
            collect_object_extra(&format!("/output/{output_index}"), object, extensions);
            builder.push(CanonicalEventKind::Finish {
                output_index,
                reason: FinishReason::ToolCalls,
            });
        }
        _ => {
            object.insert("type".into(), Value::String(kind));
            extensions.insert(
                format!("{OPENAI_RESPONSES_RAW_OUTPUT_PREFIX}/{output_index}"),
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

pub(super) fn canonical_response_usage(usage: &ResponseUsage) -> Usage {
    Usage {
        input_tokens: usage.input_tokens,
        output_tokens: usage.output_tokens,
        total_tokens: usage.total_tokens,
        cached_input_tokens: usage
            .input_tokens_details
            .as_ref()
            .map(|details| details.cached_tokens),
        reasoning_tokens: usage
            .output_tokens_details
            .as_ref()
            .map(|details| details.reasoning_tokens),
    }
}

fn collect_response_usage_extensions(
    usage: &ResponseUsage,
    extensions: &mut BTreeMap<String, Value>,
) {
    collect_extra("/usage", &usage.extra, extensions);
    if let Some(details) = &usage.input_tokens_details {
        collect_extra("/usage/input_tokens_details", &details.extra, extensions);
    }
    if let Some(details) = &usage.output_tokens_details {
        collect_extra("/usage/output_tokens_details", &details.extra, extensions);
    }
}

#[derive(Default)]
struct ResponsesEventBuilder {
    events: Vec<CanonicalEvent>,
}

impl ResponsesEventBuilder {
    fn push(&mut self, kind: CanonicalEventKind) {
        let sequence = self.events.len().try_into().unwrap_or(u64::MAX);
        self.events.push(CanonicalEvent::new(sequence, kind));
    }
}

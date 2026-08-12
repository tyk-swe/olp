use std::collections::BTreeMap;

use crate::domain::{
    CanonicalError, CanonicalEvent, CanonicalEventKind, ErrorClass, FinishReason, MessageRole,
    SourceExtensions, Surface, Usage, UsageObservation,
};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

use super::super::extensions::collect_extra;
use super::OPENAI_RESPONSES_RAW_OUTPUT_PREFIX;
use super::errors::ResponsesCodecError;
use super::helpers::collect_object_extra;
use crate::protocols::CanonicalEventBuilder as ResponsesEventBuilder;
use crate::protocols::usage::ObservedUsage;

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
    #[serde(
        default,
        serialize_with = "crate::protocols::usage::serialize_required_option"
    )]
    pub input_tokens: Option<u64>,
    #[serde(
        default,
        serialize_with = "crate::protocols::usage::serialize_required_option"
    )]
    pub output_tokens: Option<u64>,
    #[serde(
        default,
        serialize_with = "crate::protocols::usage::serialize_required_option"
    )]
    pub total_tokens: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub input_tokens_details: Option<ResponseInputTokenDetails>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub output_tokens_details: Option<ResponseOutputTokenDetails>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseInputTokenDetails {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cached_tokens: Option<u64>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseOutputTokenDetails {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reasoning_tokens: Option<u64>,
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
    let cancelled = response.status == "cancelled";
    let policy = match response.status.as_str() {
        "completed" => {
            if response.error.is_some() || response.incomplete_details.is_some() {
                return Err(ResponsesCodecError::InvalidResponse(
                    "completed response has terminal error details".into(),
                ));
            }
            if response.output.is_empty() {
                return Err(ResponsesCodecError::InvalidResponse(
                    "completed response has no output".into(),
                ));
            }
            OutputPolicy::Completed
        }
        "incomplete" => {
            if response.error.is_some() {
                return Err(ResponsesCodecError::InvalidResponse(
                    "incomplete response has an error".into(),
                ));
            }
            OutputPolicy::Incomplete(incomplete_finish_reason(
                response.incomplete_details.as_ref(),
            )?)
        }
        "failed" => {
            if response.error.is_none() || response.incomplete_details.is_some() {
                return Err(ResponsesCodecError::InvalidResponse(
                    "failed response has incoherent terminal details".into(),
                ));
            }
            OutputPolicy::Failed
        }
        "cancelled" => {
            if response.error.is_some() || response.incomplete_details.is_some() {
                return Err(ResponsesCodecError::InvalidResponse(
                    "cancelled response has terminal details".into(),
                ));
            }
            OutputPolicy::Failed
        }
        "queued" | "in_progress" => {
            return Err(ResponsesCodecError::InvalidResponse(
                "response is not terminal".into(),
            ));
        }
        status => {
            return Err(ResponsesCodecError::InvalidResponse(format!(
                "unknown response status {status}"
            )));
        }
    };
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
            &policy,
            &mut extensions,
            &mut builder,
        )?;
    }
    let mut usage_observation = None;
    if let Some(usage) = response.usage {
        collect_response_usage_extensions(&usage, &mut extensions);
        let (canonical, observation) = decode_response_usage(&usage)?;
        if let Some(usage) = canonical {
            builder.push(CanonicalEventKind::Usage { usage });
        } else {
            usage_observation = Some(observation);
        }
    }
    if let Some(error) = response.error {
        collect_extra("/error", &error.extra, &mut extensions);
        let retryable = crate::protocols::openai::error_signals_rate_limit(
            Some(&error.code),
            error.extra.get("type").and_then(Value::as_str),
        );
        builder.push(CanonicalEventKind::Error {
            error: CanonicalError {
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
    } else if cancelled {
        builder.push(CanonicalEventKind::Error {
            error: CanonicalError {
                class: ErrorClass::Upstream,
                message: "OpenAI response was cancelled".into(),
                provider_code: Some("response_cancelled".into()),
                retryable: false,
            },
        });
    }
    if !extensions.is_empty() {
        builder.push(CanonicalEventKind::SourceExtension {
            extensions: SourceExtensions::new(Surface::OpenAi, extensions),
        });
    }
    if let Some(observation) = usage_observation {
        builder.push_with_usage_observation(CanonicalEventKind::Done, observation);
    } else {
        builder.push(CanonicalEventKind::Done);
    }
    Ok(builder.events)
}

fn decode_response_output_item(
    output_index: u32,
    item: Value,
    policy: &OutputPolicy,
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
            let finish = output_finish_reason(&mut object, policy, FinishReason::Stop)?;
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
            if let Some(reason) = finish {
                builder.push(CanonicalEventKind::Finish {
                    output_index,
                    reason,
                });
            }
        }
        "function_call" => {
            let finish = output_finish_reason(&mut object, policy, FinishReason::ToolCalls)?;
            let id = Some(take_required_output_string(&mut object, "call_id")?);
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
            if let Some(reason) = finish {
                builder.push(CanonicalEventKind::Finish {
                    output_index,
                    reason,
                });
            }
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

enum OutputPolicy {
    Completed,
    Incomplete(FinishReason),
    Failed,
}

fn output_finish_reason(
    object: &mut Map<String, Value>,
    policy: &OutputPolicy,
    completed_reason: FinishReason,
) -> Result<Option<FinishReason>, ResponsesCodecError> {
    let status = take_required_output_string(object, "status")?;
    match (policy, status.as_str()) {
        (OutputPolicy::Completed, "completed") => Ok(Some(completed_reason)),
        (OutputPolicy::Incomplete(_), "completed") => Ok(Some(completed_reason)),
        (OutputPolicy::Incomplete(reason), "incomplete") => Ok(Some(reason.clone())),
        (OutputPolicy::Failed, "in_progress" | "incomplete" | "completed") => Ok(None),
        _ => Err(ResponsesCodecError::InvalidResponse(
            "output item status contradicts response status".into(),
        )),
    }
}

pub(super) fn incomplete_finish_reason(
    details: Option<&Value>,
) -> Result<FinishReason, ResponsesCodecError> {
    match details
        .and_then(|details| details.get("reason"))
        .and_then(Value::as_str)
    {
        Some("max_output_tokens") => Ok(FinishReason::Length),
        Some("content_filter") => Ok(FinishReason::ContentFilter),
        _ => Err(ResponsesCodecError::InvalidResponse(
            "unsupported incomplete response reason".into(),
        )),
    }
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

pub(super) fn decode_response_usage(
    usage: &ResponseUsage,
) -> Result<(Option<Usage>, UsageObservation), ResponsesCodecError> {
    let observed = ObservedUsage {
        input_tokens: usage.input_tokens,
        output_tokens: usage.output_tokens,
        total_tokens: usage.total_tokens,
        cached_input_tokens: usage
            .input_tokens_details
            .as_ref()
            .and_then(|details| details.cached_tokens),
        reasoning_tokens: usage
            .output_tokens_details
            .as_ref()
            .and_then(|details| details.reasoning_tokens),
    };
    observed
        .with_exact_total()
        .map(|usage| (usage, observed.observation()))
        .map_err(|_| ResponsesCodecError::InvalidUsage)
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

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn response_rate_limit_error_is_retryable() {
        let events = decode_response_object(ResponseObject {
            id: "resp_test".to_owned(),
            object: "response".to_owned(),
            created_at: 1,
            status: "failed".to_owned(),
            model: "gpt-test".to_owned(),
            output: Vec::new(),
            usage: None,
            error: Some(ResponseErrorBody {
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
                CanonicalEventKind::Error { error } => Some(error),
                _ => None,
            })
            .expect("failed response must emit a canonical error");
        assert_eq!(error.class, ErrorClass::RateLimit);
        assert!(error.retryable);
    }

    #[test]
    fn unary_terminal_status_and_output_status_are_authoritative() {
        let base = json!({
            "id":"r","object":"response","created_at":1,"model":"m",
            "status":"completed","output":[{
                "type":"message","status":"completed","role":"assistant",
                "content":[{"type":"output_text","text":"ok"}]
            }]
        });
        assert!(decode_response_object(serde_json::from_value(base.clone()).unwrap()).is_ok());

        for mutation in [
            json!({"status":"queued"}),
            json!({"status":"in_progress"}),
            json!({"status":"completed","output":[{"type":"message","status":"in_progress","role":"assistant","content":[]}]}),
            json!({"status":"failed","error":null}),
            json!({"status":"incomplete","incomplete_details":{"reason":"unknown"}}),
        ] {
            let mut invalid = base.clone();
            for (key, value) in mutation.as_object().unwrap() {
                invalid[key] = value.clone();
            }
            let response: ResponseObject = serde_json::from_value(invalid).unwrap();
            assert!(decode_response_object(response).is_err());
        }

        for (reason, expected) in [
            ("max_output_tokens", FinishReason::Length),
            ("content_filter", FinishReason::ContentFilter),
        ] {
            let response: ResponseObject = serde_json::from_value(json!({
                "id":"r","object":"response","created_at":1,"model":"m",
                "status":"incomplete","incomplete_details":{"reason":reason},
                "output":[{"type":"message","status":"incomplete","role":"assistant","content":[]}]
            }))
            .unwrap();
            let events = decode_response_object(response).unwrap();
            assert!(events.iter().any(|event| matches!(&event.kind,
                CanonicalEventKind::Finish { reason, .. } if reason == &expected)));
        }

        let cancelled: ResponseObject = serde_json::from_value(json!({
            "id":"r","object":"response","created_at":1,"model":"m",
            "status":"cancelled","output":[]
        }))
        .unwrap();
        let events = decode_response_object(cancelled).unwrap();
        assert!(events.iter().any(|event| matches!(&event.kind,
            CanonicalEventKind::Error { error }
                if error.provider_code.as_deref() == Some("response_cancelled"))));
        assert!(!events.iter().any(|event| matches!(
            event.kind,
            CanonicalEventKind::Finish {
                reason: FinishReason::Stop,
                ..
            }
        )));
    }

    #[test]
    fn partial_unary_usage_is_accounting_only() {
        let response: ResponseObject = serde_json::from_value(json!({
            "id":"r","object":"response","created_at":1,"model":"m",
            "status":"completed","output":[{
                "type":"message","status":"completed","role":"assistant","content":[]
            }],
            "usage":{"input_tokens":3}
        }))
        .unwrap();
        let events = decode_response_object(response).unwrap();
        assert!(
            !events
                .iter()
                .any(|event| matches!(event.kind, CanonicalEventKind::Usage { .. }))
        );
        let done = events.last().unwrap();
        assert_eq!(done.usage_observation.unwrap().input_tokens, Some(3));
        assert!(
            serde_json::to_value(done)
                .unwrap()
                .get("usage_observation")
                .is_none()
        );
    }
}

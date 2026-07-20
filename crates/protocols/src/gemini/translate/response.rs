use std::collections::{BTreeMap, BTreeSet};

use olp_domain::{
    CanonicalEvent, CanonicalEventKind, FinishReason, MessageRole, SourceExtensions, Surface, Usage,
};
use serde_json::Value;

use super::super::dto::{Candidate, GenerateContentResponse, Part, UsageMetadata};
use super::errors::ResponseError;
use super::extensions::collect_extra;

pub fn decode_generate_content_response(
    response: GenerateContentResponse,
) -> Result<Vec<CanonicalEvent>, ResponseError> {
    decode_response(response, true)
}

pub(crate) fn decode_generate_content_chunk(
    response: GenerateContentResponse,
) -> Result<Vec<CanonicalEvent>, ResponseError> {
    decode_response(response, false)
}

fn decode_response(
    response: GenerateContentResponse,
    require_finish: bool,
) -> Result<Vec<CanonicalEvent>, ResponseError> {
    let mut builder = EventBuilder::default();
    builder.push(CanonicalEventKind::ResponseStart {
        response_id: response.response_id,
        provider_model: response.model_version,
    });
    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);
    let metadata_only = !require_finish
        && response.candidates.is_empty()
        && (response.usage_metadata.is_some() || !extensions.is_empty());
    let prompt_blocked = response.candidates.is_empty()
        && (extensions.contains_key("/promptFeedback")
            || extensions.contains_key("/prompt_feedback"));
    if response.candidates.is_empty() && !prompt_blocked && !metadata_only {
        return Err(ResponseError::EmptyResponse);
    }
    let candidate_count = response.candidates.len();
    let mut finished_count = 0_usize;
    let mut candidate_indexes = BTreeSet::new();
    for (position, candidate) in response.candidates.iter().enumerate() {
        let index = candidate.index.unwrap_or(
            position
                .try_into()
                .map_err(|_| ResponseError::TooManyCandidates)?,
        );
        if !candidate_indexes.insert(index) {
            return Err(ResponseError::DuplicateCandidateIndex(index));
        }
    }
    for (position, candidate) in response.candidates.into_iter().enumerate() {
        if decode_candidate(candidate, position, &mut builder, &mut extensions)? {
            finished_count += 1;
        }
    }
    if require_finish && !prompt_blocked && finished_count != candidate_count {
        return Err(ResponseError::MissingFinishReason);
    }
    if let Some(usage) = response.usage_metadata {
        collect_extra("/usageMetadata", &usage.extra, &mut extensions);
        builder.push(CanonicalEventKind::Usage {
            usage: canonical_usage(&usage),
        });
    }
    if !extensions.is_empty() {
        builder.push(CanonicalEventKind::SourceExtension {
            extensions: SourceExtensions::new(Surface::Gemini, extensions),
        });
    }
    if prompt_blocked {
        builder.push(CanonicalEventKind::Finish {
            output_index: 0,
            reason: FinishReason::ContentFilter,
        });
    }
    builder.push(CanonicalEventKind::Done);
    Ok(builder.events)
}

fn decode_candidate(
    candidate: Candidate,
    position: usize,
    builder: &mut EventBuilder,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<bool, ResponseError> {
    let output_index = candidate.index.unwrap_or(
        position
            .try_into()
            .map_err(|_| ResponseError::TooManyCandidates)?,
    );
    let prefix = format!("/candidates/{output_index}");
    collect_extra(&prefix, &candidate.extra, extensions);
    builder.push(CanonicalEventKind::MessageStart {
        output_index,
        role: MessageRole::Assistant,
    });
    let mut tool_index = 0_u32;
    if let Some(content) = candidate.content {
        if content.role.as_deref().is_some_and(|role| role != "model") {
            return Err(ResponseError::UnexpectedRole(
                content.role.unwrap_or_default(),
            ));
        }
        collect_extra(&format!("{prefix}/content"), &content.extra, extensions);
        for (part_index, part) in content.parts.into_iter().enumerate() {
            match part {
                Part::Text(part)
                    if part.thought != Some(true) && part.thought_signature.is_none() =>
                {
                    collect_extra(
                        &format!("{prefix}/content/parts/{part_index}"),
                        &part.extra,
                        extensions,
                    );
                    if let Some(thought) = part.thought {
                        extensions.insert(
                            format!("{prefix}/content/parts/{part_index}/thought"),
                            Value::Bool(thought),
                        );
                    }
                    builder.push(CanonicalEventKind::TextDelta {
                        output_index,
                        text: part.text,
                    });
                }
                Part::FunctionCall(part) => {
                    collect_extra(
                        &format!("{prefix}/content/parts/{part_index}"),
                        &part.extra,
                        extensions,
                    );
                    collect_extra(
                        &format!("{prefix}/content/parts/{part_index}/functionCall"),
                        &part.function_call.extra,
                        extensions,
                    );
                    builder.push(CanonicalEventKind::ToolCallDelta {
                        output_index,
                        tool_index,
                        id: part.function_call.id,
                        name: Some(part.function_call.name),
                        arguments_delta: serde_json::to_string(&part.function_call.args)
                            .map_err(ResponseError::Json)?,
                    });
                    tool_index = tool_index
                        .checked_add(1)
                        .ok_or(ResponseError::TooManyToolCalls)?;
                }
                part => {
                    extensions.insert(
                        format!("{prefix}/content/parts/{part_index}"),
                        part.as_value(),
                    );
                }
            }
        }
    }
    let finished = candidate.finish_reason.is_some();
    if let Some(reason) = candidate.finish_reason {
        let canonical = gemini_finish_reason(&reason);
        if !matches!(reason.as_str(), "STOP" | "MAX_TOKENS") {
            extensions.insert(format!("{prefix}/finishReason"), Value::String(reason));
        }
        builder.push(CanonicalEventKind::Finish {
            output_index,
            reason: canonical,
        });
    }
    Ok(finished)
}

pub(crate) fn canonical_usage(usage: &UsageMetadata) -> Usage {
    let total_tokens = if usage.total_token_count == 0 {
        usage
            .prompt_token_count
            .saturating_add(usage.candidates_token_count)
            .saturating_add(usage.thoughts_token_count.unwrap_or(0))
    } else {
        usage.total_token_count
    };
    Usage {
        input_tokens: usage.prompt_token_count,
        output_tokens: usage.candidates_token_count,
        total_tokens,
        cached_input_tokens: usage.cached_content_token_count,
        reasoning_tokens: usage.thoughts_token_count,
    }
}

pub(crate) fn gemini_finish_reason(reason: &str) -> FinishReason {
    match reason {
        "STOP" => FinishReason::Stop,
        "MAX_TOKENS" => FinishReason::Length,
        "SAFETY"
        | "RECITATION"
        | "BLOCKLIST"
        | "PROHIBITED_CONTENT"
        | "SPII"
        | "IMAGE_SAFETY"
        | "IMAGE_PROHIBITED_CONTENT" => FinishReason::ContentFilter,
        "MALFORMED_FUNCTION_CALL" | "UNEXPECTED_TOOL_CALL" => FinishReason::Error,
        other => FinishReason::Other(other.to_owned()),
    }
}

#[derive(Default)]
pub(crate) struct EventBuilder {
    pub(crate) events: Vec<CanonicalEvent>,
}

impl EventBuilder {
    pub(crate) fn push(&mut self, kind: CanonicalEventKind) {
        let sequence = self.events.len().try_into().unwrap_or(u64::MAX);
        self.events.push(CanonicalEvent::new(sequence, kind));
    }
}

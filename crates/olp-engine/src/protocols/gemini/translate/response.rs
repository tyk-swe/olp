use std::collections::{BTreeMap, BTreeSet};

use crate::domain::{
    CanonicalEvent, CanonicalEventKind, FinishReason, MessageRole, SourceExtensions, Surface,
    Usage, UsageObservation,
};
use serde_json::Value;

use super::super::dto::{Candidate, GenerateContentResponse, Part, UsageMetadata};
use super::errors::ResponseError;
use super::extensions::collect_extra;
use crate::protocols::CanonicalEventBuilder as EventBuilder;

pub fn decode_generate_content_response(
    response: GenerateContentResponse,
) -> Result<Vec<CanonicalEvent>, ResponseError> {
    decode_response(response, true)
}

pub(in crate::protocols) fn decode_generate_content_chunk(
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
    let mut usage_observation = None;
    if let Some(usage) = response.usage_metadata {
        collect_extra("/usageMetadata", &usage.extra, &mut extensions);
        if let Some(usage) = canonical_usage(&usage)? {
            builder.push(CanonicalEventKind::Usage { usage });
        } else {
            usage_observation = Some(usage_observation_for(&usage)?);
        }
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
    if let Some(observation) = usage_observation {
        builder.push_with_usage_observation(CanonicalEventKind::Done, observation);
    } else {
        builder.push(CanonicalEventKind::Done);
    }
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

pub(in crate::protocols) fn canonical_usage(
    usage: &UsageMetadata,
) -> Result<Option<Usage>, ResponseError> {
    for counter in [
        usage.prompt_token_count,
        usage.candidates_token_count,
        usage.total_token_count,
        usage.cached_content_token_count,
        usage.thoughts_token_count,
        usage.tool_use_prompt_token_count,
    ] {
        crate::protocols::usage::validate_counter(counter)
            .map_err(|_| ResponseError::InvalidUsage)?;
    }
    let (Some(input_tokens), Some(output_tokens), Some(total_tokens)) = (
        usage.prompt_token_count,
        usage.candidates_token_count,
        usage.total_token_count,
    ) else {
        return Ok(None);
    };
    let canonical_input_tokens = input_tokens
        .checked_add(usage.tool_use_prompt_token_count.unwrap_or(0))
        .ok_or(ResponseError::InvalidUsage)?;
    let canonical_output_tokens = output_tokens
        .checked_add(usage.thoughts_token_count.unwrap_or(0))
        .ok_or(ResponseError::InvalidUsage)?;
    let expected_total = canonical_input_tokens
        .checked_add(canonical_output_tokens)
        .ok_or(ResponseError::InvalidUsage)?;
    if total_tokens != expected_total
        || usage
            .cached_content_token_count
            .is_some_and(|cached| cached > canonical_input_tokens)
        || usage
            .thoughts_token_count
            .is_some_and(|thoughts| thoughts > canonical_output_tokens)
    {
        return Err(ResponseError::InvalidUsage);
    }
    Ok(Some(Usage {
        input_tokens: canonical_input_tokens,
        output_tokens: canonical_output_tokens,
        total_tokens,
        cached_input_tokens: usage.cached_content_token_count,
        reasoning_tokens: usage.thoughts_token_count,
    }))
}

fn usage_observation_for(usage: &UsageMetadata) -> Result<UsageObservation, ResponseError> {
    for counter in [
        usage.prompt_token_count,
        usage.candidates_token_count,
        usage.total_token_count,
        usage.cached_content_token_count,
        usage.thoughts_token_count,
        usage.tool_use_prompt_token_count,
    ] {
        crate::protocols::usage::validate_counter(counter)
            .map_err(|_| ResponseError::InvalidUsage)?;
    }
    let input_tokens = usage
        .prompt_token_count
        .map(|prompt| {
            prompt
                .checked_add(usage.tool_use_prompt_token_count.unwrap_or(0))
                .ok_or(ResponseError::InvalidUsage)
        })
        .transpose()?;
    let output_tokens = usage
        .candidates_token_count
        .map(|candidates| {
            candidates
                .checked_add(usage.thoughts_token_count.unwrap_or(0))
                .ok_or(ResponseError::InvalidUsage)
        })
        .transpose()?;
    for counter in [input_tokens, output_tokens] {
        crate::protocols::usage::validate_counter(counter)
            .map_err(|_| ResponseError::InvalidUsage)?;
    }
    if usage
        .cached_content_token_count
        .zip(input_tokens)
        .is_some_and(|(cached, input)| cached > input)
        || usage
            .thoughts_token_count
            .zip(output_tokens)
            .is_some_and(|(thoughts, output)| thoughts > output)
    {
        return Err(ResponseError::InvalidUsage);
    }
    Ok(UsageObservation {
        input_tokens,
        output_tokens,
        total_tokens: usage.total_token_count,
        cached_input_tokens: usage.cached_content_token_count,
        reasoning_tokens: usage.thoughts_token_count,
    })
}

pub(in crate::protocols) fn gemini_finish_reason(reason: &str) -> FinishReason {
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

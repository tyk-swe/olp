use std::collections::{BTreeMap, BTreeSet};

use crate::domain::{
    CanonicalError, CanonicalEvent, CanonicalEventKind, ErrorClass, SourceExtensions, Surface,
    UsageObservation,
};
use serde::Deserialize;
use serde_json::Value;
use thiserror::Error;

use super::{
    GenerateContentResponse, UsageMetadata,
    translate::{ResponseError, canonical_usage, decode_generate_content_chunk},
};
use crate::protocols::sse::{
    DEFAULT_MAX_EVENT_BYTES, SseDecodeError, SseDecoder, SseFrame, raw_sse_frame_event,
};

pub struct GeminiGenerateContentStreamDecoder {
    sse: SseDecoder,
    sequence: u64,
    response_started: bool,
    started_candidates: BTreeSet<u32>,
    finished_candidates: BTreeSet<u32>,
    next_tool_indexes: BTreeMap<u32, u32>,
    prompt_blocked: bool,
    usage: Option<UsageMetadata>,
    complete_usage_frame_seen: bool,
    usage_emitted: bool,
    done: bool,
    preserve_raw_frames: bool,
}

impl std::fmt::Debug for GeminiGenerateContentStreamDecoder {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("GeminiGenerateContentStreamDecoder")
            .field("next_sequence", &self.sequence)
            .field("response_started", &self.response_started)
            .field("started_candidate_count", &self.started_candidates.len())
            .field("finished_candidate_count", &self.finished_candidates.len())
            .field("prompt_blocked", &self.prompt_blocked)
            .field("done", &self.done)
            .finish_non_exhaustive()
    }
}

impl Default for GeminiGenerateContentStreamDecoder {
    fn default() -> Self {
        Self::new()
    }
}

impl GeminiGenerateContentStreamDecoder {
    #[must_use]
    pub fn new() -> Self {
        Self::with_max_event_bytes(DEFAULT_MAX_EVENT_BYTES)
    }

    #[must_use]
    pub fn with_max_event_bytes(max_event_bytes: usize) -> Self {
        Self::with_max_event_bytes_and_raw_passthrough(max_event_bytes, false)
    }

    #[must_use]
    pub fn with_max_event_bytes_and_raw_passthrough(
        max_event_bytes: usize,
        preserve_raw_frames: bool,
    ) -> Self {
        Self {
            sse: SseDecoder::new(max_event_bytes),
            sequence: 0,
            response_started: false,
            started_candidates: BTreeSet::new(),
            finished_candidates: BTreeSet::new(),
            next_tool_indexes: BTreeMap::new(),
            prompt_blocked: false,
            usage: None,
            complete_usage_frame_seen: false,
            usage_emitted: false,
            done: false,
            preserve_raw_frames,
        }
    }

    pub fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, StreamError> {
        let frames = self.sse.push(bytes)?;
        self.decode_frames(frames)
    }

    pub fn finish(&mut self) -> Result<Vec<CanonicalEvent>, StreamError> {
        let frames = self.sse.finish()?;
        let mut events = self.decode_frames(frames)?;
        if self.done {
            return Ok(events);
        }
        let candidates_complete = !self.started_candidates.is_empty()
            && self.started_candidates == self.finished_candidates;
        let prompt_only = self.prompt_blocked && self.started_candidates.is_empty();
        if !prompt_only && !candidates_complete {
            return Err(StreamError::UnexpectedEof);
        }
        self.emit_terminal_usage_if_ready(&mut events)?;
        self.emit_done(&mut events, false)?;
        self.done = true;
        Ok(events)
    }

    #[must_use]
    pub const fn is_done(&self) -> bool {
        self.done
    }

    fn decode_frames(&mut self, frames: Vec<SseFrame>) -> Result<Vec<CanonicalEvent>, StreamError> {
        let mut events = Vec::new();
        for frame in frames {
            if self.done {
                return Err(StreamError::DataAfterDone);
            }
            let raw_frame = self.preserve_raw_frames.then(|| frame.clone());
            let event_start = events.len();
            let sequence_start = self.sequence;
            let value: Value = serde_json::from_str(&frame.data)?;
            if value.get("error").is_some() {
                self.decode_error(value, &mut events)?;
            } else {
                if let Some(event_name) = frame.event.clone()
                    && event_name != "message"
                {
                    self.emit(
                        &mut events,
                        CanonicalEventKind::SourceExtension {
                            extensions: SourceExtensions::new(
                                Surface::Gemini,
                                BTreeMap::from([("/sse/event".into(), Value::String(event_name))]),
                            ),
                        },
                    );
                }
                let response: GenerateContentResponse = serde_json::from_value(value)?;
                if let Some(usage) = &response.usage_metadata {
                    self.observe_usage(usage)?;
                }
                for (position, candidate) in response.candidates.iter().enumerate() {
                    let output_index = candidate.index.unwrap_or(
                        position
                            .try_into()
                            .map_err(|_| ResponseError::TooManyCandidates)?,
                    );
                    if self.finished_candidates.contains(&output_index)
                        && (candidate.content.is_some() || candidate.finish_reason.is_some())
                    {
                        return Err(StreamError::CandidateDataAfterFinish(output_index));
                    }
                }
                let prompt_blocked = response.candidates.is_empty()
                    && (response.extra.contains_key("promptFeedback")
                        || response.extra.contains_key("prompt_feedback"));
                if prompt_blocked
                    && !self.started_candidates.is_empty()
                    && self.started_candidates != self.finished_candidates
                {
                    return Err(StreamError::PromptFeedbackAfterCandidateStart);
                }
                self.prompt_blocked |= prompt_blocked;
                let canonical = decode_generate_content_chunk(response)?;
                for event in canonical {
                    match event.kind {
                        CanonicalEventKind::ResponseStart { .. } if self.response_started => {}
                        CanonicalEventKind::ResponseStart { .. } => {
                            self.response_started = true;
                            self.emit(&mut events, event.kind);
                        }
                        CanonicalEventKind::MessageStart { output_index, .. } => {
                            if self.started_candidates.insert(output_index) {
                                self.emit(&mut events, event.kind);
                            }
                        }
                        CanonicalEventKind::Finish { output_index, .. } => {
                            if !self.finished_candidates.insert(output_index) {
                                return Err(StreamError::DuplicateCandidateFinish(output_index));
                            }
                            self.emit(&mut events, event.kind);
                        }
                        CanonicalEventKind::ToolCallDelta {
                            output_index,
                            id,
                            name,
                            arguments_delta,
                            ..
                        } => {
                            let tool_index =
                                self.next_tool_indexes.entry(output_index).or_default();
                            let current = *tool_index;
                            *tool_index = tool_index
                                .checked_add(1)
                                .ok_or(StreamError::TooManyToolCalls)?;
                            self.emit(
                                &mut events,
                                CanonicalEventKind::ToolCallDelta {
                                    output_index,
                                    tool_index: current,
                                    id,
                                    name,
                                    arguments_delta,
                                },
                            );
                        }
                        CanonicalEventKind::Usage { .. } | CanonicalEventKind::Done => {}
                        kind => self.emit(&mut events, kind),
                    }
                }
                self.emit_terminal_usage_if_ready(&mut events)?;
            }
            if let Some(raw_frame) = raw_frame {
                let semantic_events = events.len().saturating_sub(event_start);
                for event in &mut events[event_start..] {
                    event.sequence = event.sequence.saturating_add(1);
                }
                events.insert(
                    event_start,
                    raw_sse_frame_event(
                        sequence_start,
                        Surface::Gemini,
                        &raw_frame,
                        semantic_events,
                    ),
                );
                self.sequence = self.sequence.saturating_add(1);
            }
        }
        Ok(events)
    }

    fn decode_error(
        &mut self,
        value: Value,
        events: &mut Vec<CanonicalEvent>,
    ) -> Result<(), StreamError> {
        let envelope: ErrorEnvelope = serde_json::from_value(value)?;
        let status = envelope.error.status.unwrap_or_default();
        let code = envelope.error.code;
        let (class, retryable) = if code == Some(429) || status == "RESOURCE_EXHAUSTED" {
            (ErrorClass::RateLimit, true)
        } else if code.is_some_and(|code| code >= 500) || status == "UNAVAILABLE" {
            (ErrorClass::Upstream, true)
        } else if code == Some(401) || status == "UNAUTHENTICATED" {
            (ErrorClass::Authentication, false)
        } else if code == Some(403) || status == "PERMISSION_DENIED" {
            (ErrorClass::Authorization, false)
        } else if code.is_some_and(|code| (400..500).contains(&code)) {
            (ErrorClass::InvalidRequest, false)
        } else {
            (ErrorClass::Upstream, false)
        };
        self.emit(
            events,
            CanonicalEventKind::Error {
                error: CanonicalError {
                    class,
                    message: envelope.error.message,
                    provider_code: (!status.is_empty()).then_some(status),
                    retryable,
                },
            },
        );
        self.emit_done(events, true)?;
        self.done = true;
        Ok(())
    }

    fn observe_usage(&mut self, usage: &UsageMetadata) -> Result<(), StreamError> {
        let frame_is_complete = canonical_usage(usage)?.is_some();
        if frame_is_complete && self.complete_usage_frame_seen {
            return Err(StreamError::DuplicateCompleteUsage);
        }
        let mut merged = self.usage.take().unwrap_or_default();
        merged.prompt_token_count = merge_usage_counter(
            "promptTokenCount",
            merged.prompt_token_count,
            usage.prompt_token_count,
        )?;
        merged.candidates_token_count = merge_usage_counter(
            "candidatesTokenCount",
            merged.candidates_token_count,
            usage.candidates_token_count,
        )?;
        merged.total_token_count = merge_usage_counter(
            "totalTokenCount",
            merged.total_token_count,
            usage.total_token_count,
        )?;
        merged.cached_content_token_count = merge_usage_counter(
            "cachedContentTokenCount",
            merged.cached_content_token_count,
            usage.cached_content_token_count,
        )?;
        merged.thoughts_token_count = merge_usage_counter(
            "thoughtsTokenCount",
            merged.thoughts_token_count,
            usage.thoughts_token_count,
        )?;
        merged.tool_use_prompt_token_count = merge_usage_counter(
            "toolUsePromptTokenCount",
            merged.tool_use_prompt_token_count,
            usage.tool_use_prompt_token_count,
        )?;
        usage_observation(&merged, false)?;
        let merged_is_complete = canonical_usage(&merged)?.is_some();
        self.complete_usage_frame_seen |= frame_is_complete || merged_is_complete;
        self.usage = Some(merged);
        Ok(())
    }

    fn canonical_usage(&self) -> Result<Option<crate::domain::Usage>, StreamError> {
        self.usage
            .as_ref()
            .map(canonical_usage)
            .transpose()
            .map(Option::flatten)
            .map_err(StreamError::from)
    }

    fn emit_terminal_usage_if_ready(
        &mut self,
        events: &mut Vec<CanonicalEvent>,
    ) -> Result<(), StreamError> {
        if self.usage_emitted {
            return Ok(());
        }
        let candidates_complete = !self.started_candidates.is_empty()
            && self.started_candidates == self.finished_candidates;
        let prompt_only = self.prompt_blocked && self.started_candidates.is_empty();
        if (candidates_complete || prompt_only)
            && let Some(usage) = self.canonical_usage()?
        {
            self.emit(events, CanonicalEventKind::Usage { usage });
            self.usage_emitted = true;
        }
        Ok(())
    }

    fn emit_done(
        &mut self,
        events: &mut Vec<CanonicalEvent>,
        force_incomplete: bool,
    ) -> Result<(), StreamError> {
        let mut event = CanonicalEvent::new(self.sequence, CanonicalEventKind::Done);
        if !self.usage_emitted
            && let Some(usage) = &self.usage
        {
            event = event.with_usage_observation(usage_observation(usage, force_incomplete)?);
        }
        events.push(event);
        self.sequence = self.sequence.saturating_add(1);
        Ok(())
    }

    fn emit(&mut self, events: &mut Vec<CanonicalEvent>, kind: CanonicalEventKind) {
        events.push(CanonicalEvent::new(self.sequence, kind));
        self.sequence = self.sequence.saturating_add(1);
    }
}

#[derive(Deserialize)]
struct ErrorEnvelope {
    error: WireError,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct WireError {
    #[serde(default)]
    code: Option<u16>,
    message: String,
    #[serde(default)]
    status: Option<String>,
}

fn merge_usage_counter(
    name: &'static str,
    current: Option<u64>,
    newer: Option<u64>,
) -> Result<Option<u64>, StreamError> {
    match (current, newer) {
        (Some(current), Some(newer)) if current != newer => {
            Err(StreamError::ConflictingUsageCounter(name))
        }
        (current, newer) => Ok(newer.or(current)),
    }
}

fn usage_observation(
    usage: &UsageMetadata,
    force_incomplete: bool,
) -> Result<UsageObservation, StreamError> {
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
        return Err(ResponseError::InvalidUsage.into());
    }
    Ok(UsageObservation {
        input_tokens,
        output_tokens,
        total_tokens: (!force_incomplete)
            .then_some(usage.total_token_count)
            .flatten(),
        cached_input_tokens: usage.cached_content_token_count,
        reasoning_tokens: usage.thoughts_token_count,
    })
}

#[derive(Debug, Error)]
pub enum StreamError {
    #[error(transparent)]
    Sse(#[from] SseDecodeError),
    #[error("Gemini stream frame is not valid JSON: {0}")]
    Json(#[from] serde_json::Error),
    #[error("Gemini response chunk is invalid: {0}")]
    Response(#[from] ResponseError),
    #[error("Gemini stream emitted data after completion")]
    DataAfterDone,
    #[error("Gemini stream ended before every candidate emitted finishReason")]
    UnexpectedEof,
    #[error("Gemini stream has too many tool calls")]
    TooManyToolCalls,
    #[error("Gemini candidate {0} emitted data after finishReason")]
    CandidateDataAfterFinish(u32),
    #[error("Gemini candidate {0} emitted finishReason more than once")]
    DuplicateCandidateFinish(u32),
    #[error("Gemini promptFeedback cannot terminate candidates that already started")]
    PromptFeedbackAfterCandidateStart,
    #[error("Gemini stream emitted complete usage metadata more than once")]
    DuplicateCompleteUsage,
    #[error("Gemini stream changed the observed {0} usage counter")]
    ConflictingUsageCounter(&'static str),
}

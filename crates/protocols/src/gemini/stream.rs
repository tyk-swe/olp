use std::collections::{BTreeMap, BTreeSet};

use olp_domain::{
    CanonicalError, CanonicalEvent, CanonicalEventKind, ErrorClass, SourceExtensions, Surface,
};
use serde::Deserialize;
use serde_json::Value;
use thiserror::Error;

use super::{
    GenerateContentResponse,
    translate::{ResponseError, decode_generate_content_chunk},
};
use crate::sse::{
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
        if !self.prompt_blocked && !candidates_complete {
            return Err(StreamError::UnexpectedEof);
        }
        self.emit(&mut events, CanonicalEventKind::Done);
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
                self.prompt_blocked |= response.candidates.is_empty()
                    && (response.extra.contains_key("promptFeedback")
                        || response.extra.contains_key("prompt_feedback"));
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
                        CanonicalEventKind::Done => {}
                        kind => self.emit(&mut events, kind),
                    }
                }
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
        self.emit(events, CanonicalEventKind::Done);
        self.done = true;
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
}

use serde::{Deserialize, Serialize};
use thiserror::Error;

use super::requests::{MessageRole, SourceExtensions};

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct Event {
    pub sequence: u64,
    #[serde(flatten)]
    pub kind: Kind,
}

impl Event {
    #[must_use]
    pub const fn new(sequence: u64, kind: Kind) -> Self {
        Self { sequence, kind }
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(tag = "event", rename_all = "snake_case")]
pub enum Kind {
    ResponseStart {
        response_id: Option<String>,
        provider_model: Option<String>,
    },
    MessageStart {
        output_index: u32,
        role: MessageRole,
    },
    TextDelta {
        output_index: u32,
        text: String,
    },
    RefusalDelta {
        output_index: u32,
        text: String,
    },
    ToolCallDelta {
        output_index: u32,
        tool_index: u32,
        id: Option<String>,
        name: Option<String>,
        arguments_delta: String,
    },
    Usage {
        usage: Usage,
    },
    Finish {
        output_index: u32,
        reason: FinishReason,
    },
    Error {
        error: Error,
    },
    SourceExtension {
        extensions: SourceExtensions,
    },
    Done,
}

/// Token counts with one fixed meaning regardless of the provider that
/// reported them. Every decoder normalises into this contract and every
/// encoder denormalises out of it, so cost, limits, and client encoders can
/// read the fields verbatim.
#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct Usage {
    /// Prompt tokens, **inclusive** of `cached_input_tokens` and of any
    /// cache-creation tokens the provider bills separately.
    pub input_tokens: u64,
    /// Generated tokens, **exclusive** of `reasoning_tokens`. The two are
    /// disjoint so a consumer can add them without double counting.
    pub output_tokens: u64,
    /// `input_tokens + output_tokens + reasoning_tokens`.
    pub total_tokens: u64,
    /// The subset of `input_tokens` the provider served from its prompt cache.
    pub cached_input_tokens: Option<u64>,
    /// Reasoning / thinking tokens, disjoint from `output_tokens`. `None` means
    /// the provider does not report them, not that there were zero.
    pub reasoning_tokens: Option<u64>,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum FinishReason {
    Stop,
    Length,
    ToolCalls,
    ContentFilter,
    Error,
    Other(String),
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct Error {
    pub class: ErrorClass,
    pub message: String,
    pub provider_code: Option<String>,
    pub retryable: bool,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ErrorClass {
    Authentication,
    Authorization,
    InvalidRequest,
    RateLimit,
    Timeout,
    Transport,
    Upstream,
    Internal,
}

pub fn validate_event_sequence(events: &[Event]) -> Result<(), EventSequenceError> {
    let mut validator = EventSequenceValidator::new();
    for event in events {
        validator.push(event)?;
    }
    validator.finish()
}

/// Incrementally validates the ordering and terminal invariant of a canonical
/// event stream before protocol-specific encoders observe it.
#[derive(Clone, Copy, Debug, Default)]
pub struct EventSequenceValidator {
    expected: u64,
    done: bool,
}

impl EventSequenceValidator {
    #[must_use]
    pub const fn new() -> Self {
        Self {
            expected: 0,
            done: false,
        }
    }

    pub fn push(&mut self, event: &Event) -> Result<(), EventSequenceError> {
        if self.done {
            return Err(EventSequenceError::AfterDone {
                sequence: event.sequence,
            });
        }
        if event.sequence != self.expected {
            return Err(EventSequenceError::OutOfOrder {
                expected: self.expected,
                actual: event.sequence,
            });
        }
        self.done = matches!(event.kind, Kind::Done);
        self.expected = self.expected.saturating_add(1);
        Ok(())
    }

    pub fn finish(self) -> Result<(), EventSequenceError> {
        if self.done {
            Ok(())
        } else {
            Err(EventSequenceError::MissingDone {
                next_sequence: self.expected,
            })
        }
    }

    #[must_use]
    pub const fn is_complete(self) -> bool {
        self.done
    }
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
pub enum EventSequenceError {
    #[error("expected canonical event sequence {expected}, got {actual}")]
    OutOfOrder { expected: u64, actual: u64 },
    #[error("canonical event {sequence} appeared after the terminal done event")]
    AfterDone { sequence: u64 },
    #[error("canonical event stream ended before done; next sequence would be {next_sequence}")]
    MissingDone { next_sequence: u64 },
}

use serde::{Deserialize, Serialize};
use thiserror::Error;

use super::{MessageRole, SourceExtensions};

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct CanonicalEvent {
    pub sequence: u64,
    #[serde(flatten)]
    pub kind: CanonicalEventKind,
    /// Content-free provider usage observed during decoding but not complete
    /// enough to emit as canonical [`Usage`]. This is accounting-only state
    /// and must never cross a protocol boundary.
    #[serde(skip)]
    pub usage_observation: Option<UsageObservation>,
}

impl CanonicalEvent {
    #[must_use]
    pub const fn new(sequence: u64, kind: CanonicalEventKind) -> Self {
        Self {
            sequence,
            kind,
            usage_observation: None,
        }
    }

    #[must_use]
    pub const fn with_usage_observation(mut self, observation: UsageObservation) -> Self {
        self.usage_observation = Some(observation);
        self
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(tag = "event", rename_all = "snake_case")]
pub enum CanonicalEventKind {
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
        error: CanonicalError,
    },
    SourceExtension {
        extensions: SourceExtensions,
    },
    Done,
}

#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct Usage {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub total_tokens: u64,
    pub cached_input_tokens: Option<u64>,
    pub reasoning_tokens: Option<u64>,
}

/// Presence-aware usage counters retained only for internal accounting.
///
/// `Some(UsageObservation::default())` means the provider emitted a usage
/// object whose counters were all absent or null.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct UsageObservation {
    pub input_tokens: Option<u64>,
    pub output_tokens: Option<u64>,
    pub total_tokens: Option<u64>,
    pub cached_input_tokens: Option<u64>,
    pub reasoning_tokens: Option<u64>,
}

impl UsageObservation {
    /// Merges a later observation without turning an omitted counter into zero.
    #[must_use]
    pub fn merge_latest(self, newer: Self) -> Self {
        Self {
            input_tokens: newer.input_tokens.or(self.input_tokens),
            output_tokens: newer.output_tokens.or(self.output_tokens),
            total_tokens: newer.total_tokens.or(self.total_tokens),
            cached_input_tokens: newer.cached_input_tokens.or(self.cached_input_tokens),
            reasoning_tokens: newer.reasoning_tokens.or(self.reasoning_tokens),
        }
    }
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
pub struct CanonicalError {
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

pub fn validate_event_sequence(events: &[CanonicalEvent]) -> Result<(), EventSequenceError> {
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

    pub fn push(&mut self, event: &CanonicalEvent) -> Result<(), EventSequenceError> {
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
        self.done = matches!(event.kind, CanonicalEventKind::Done);
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accounting_observation_never_serializes() {
        let event = CanonicalEvent::new(0, CanonicalEventKind::Done).with_usage_observation(
            UsageObservation {
                input_tokens: Some(3),
                ..UsageObservation::default()
            },
        );
        let value = serde_json::to_value(&event).unwrap();
        assert_eq!(value, serde_json::json!({"sequence": 0, "event": "done"}));
        let decoded: CanonicalEvent = serde_json::from_value(value).unwrap();
        assert_eq!(decoded.usage_observation, None);
    }

    #[test]
    fn usage_observations_merge_without_fabricating_absent_counters() {
        let merged = UsageObservation {
            input_tokens: Some(3),
            ..UsageObservation::default()
        }
        .merge_latest(UsageObservation {
            output_tokens: Some(2),
            ..UsageObservation::default()
        });
        assert_eq!(merged.input_tokens, Some(3));
        assert_eq!(merged.output_tokens, Some(2));
        assert_eq!(merged.total_tokens, None);
    }
}

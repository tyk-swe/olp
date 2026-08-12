//! Vendor DTOs and loss-aware translations to and from `olp-engine::domain`.

use crate::domain::{CanonicalEvent, CanonicalEventKind, UsageObservation};

pub mod anthropic;
mod client;
mod extensions;
pub mod gemini;
pub mod openai;
pub mod sse;
mod usage;

#[derive(Default)]
pub(in crate::protocols) struct CanonicalEventBuilder {
    pub(in crate::protocols) events: Vec<CanonicalEvent>,
}

impl CanonicalEventBuilder {
    pub(in crate::protocols) fn push(&mut self, kind: CanonicalEventKind) {
        let sequence = self.events.len().try_into().unwrap_or(u64::MAX);
        self.events.push(CanonicalEvent::new(sequence, kind));
    }

    pub(in crate::protocols) fn push_with_usage_observation(
        &mut self,
        kind: CanonicalEventKind,
        observation: UsageObservation,
    ) {
        let sequence = self.events.len().try_into().unwrap_or(u64::MAX);
        self.events
            .push(CanonicalEvent::new(sequence, kind).with_usage_observation(observation));
    }
}

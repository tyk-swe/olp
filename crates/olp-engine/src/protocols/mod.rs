//! Vendor DTOs and loss-aware translations to and from `olp-engine::domain`.

use crate::domain::canonical::events::{Event, Kind};

pub mod anthropic;
mod client;
mod client_sequence;
pub mod extensions;
pub mod gemini;
pub mod openai;
pub mod sse;

#[derive(Default)]
pub(in crate::protocols) struct CanonicalEventBuilder {
    pub(in crate::protocols) events: Vec<Event>,
}

impl CanonicalEventBuilder {
    pub(in crate::protocols) fn push(&mut self, kind: Kind) {
        let sequence = self.events.len().try_into().unwrap_or(u64::MAX);
        self.events.push(Event::new(sequence, kind));
    }
}

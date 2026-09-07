//! Vendor DTOs and loss-aware translations to and from `protocols::canonical`.

use crate::protocols::canonical::events::Event;
use crate::protocols::canonical::events::Kind;

#[derive(Default)]
pub(crate) struct CanonicalEventBuilder {
    pub(crate) events: Vec<Event>,
}

impl CanonicalEventBuilder {
    pub(crate) fn push(&mut self, kind: Kind) {
        let sequence = self.events.len().try_into().unwrap_or(u64::MAX);
        self.events.push(Event::new(sequence, kind));
    }
}

pub mod anthropic;

pub mod canonical;

pub mod client;

pub mod client_sequence;

pub mod extensions;

pub mod gemini;

pub mod openai;

pub mod sse;

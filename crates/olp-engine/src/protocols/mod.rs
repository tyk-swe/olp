//! Vendor DTOs and loss-aware translations to and from `olp-engine::domain`.

use crate::domain::{CanonicalEvent, CanonicalEventKind};

pub mod anthropic;
mod client;
mod extensions;
pub mod gemini;
pub mod openai;
pub mod sse;

#[derive(Default)]
pub(in crate::protocols) struct CanonicalEventBuilder {
    pub(in crate::protocols) events: Vec<CanonicalEvent>,
}

impl CanonicalEventBuilder {
    pub(in crate::protocols) fn push(&mut self, kind: CanonicalEventKind) {
        let sequence = self.events.len().try_into().unwrap_or(u64::MAX);
        self.events.push(CanonicalEvent::new(sequence, kind));
    }
}

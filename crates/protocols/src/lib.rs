//! Vendor DTOs and loss-aware translations to and from `olp-domain`.

use olp_domain::{CanonicalEvent, CanonicalEventKind};

pub mod anthropic;
mod client;
mod extensions;
pub mod gemini;
pub mod openai;
pub mod sse;

#[derive(Default)]
pub(crate) struct CanonicalEventBuilder {
    pub(crate) events: Vec<CanonicalEvent>,
}

impl CanonicalEventBuilder {
    pub(crate) fn push(&mut self, kind: CanonicalEventKind) {
        let sequence = self.events.len().try_into().unwrap_or(u64::MAX);
        self.events.push(CanonicalEvent::new(sequence, kind));
    }
}

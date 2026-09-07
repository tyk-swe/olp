use crate::protocols::canonical::events::Event;
use crate::protocols::canonical::events::Kind;

/// Why a client encoder refused an event.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SequenceRejection {
    AfterDone,
    OutOfOrder { expected: u64, actual: u64 },
}

/// What the encoder should do with an admitted event.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Admission {
    Handle,
    /// A native frame already replayed this event's content; only the
    /// sequence advances.
    Skipped,
}

/// The gate every client encoder runs before encoding: events must arrive in
/// sequence, nothing may follow `Done`, and after a raw frame was replayed the
/// semantic events it produced are consumed without being encoded again.
#[derive(Debug, Default)]
pub(crate) struct ClientSequence {
    expected: u64,
    done: bool,
    skip_native_events: usize,
}

impl ClientSequence {
    pub(crate) fn admit(&mut self, event: &Event) -> Result<Admission, SequenceRejection> {
        if self.done {
            return Err(SequenceRejection::AfterDone);
        }
        if event.sequence != self.expected {
            return Err(SequenceRejection::OutOfOrder {
                expected: self.expected,
                actual: event.sequence,
            });
        }
        self.expected = self.expected.saturating_add(1);
        if self.skip_native_events > 0 {
            self.skip_native_events -= 1;
            if matches!(event.kind, Kind::Done) {
                self.done = true;
            }
            return Ok(Admission::Skipped);
        }
        Ok(Admission::Handle)
    }

    pub(crate) fn skip_native(&mut self, count: usize) {
        self.skip_native_events = count;
    }

    pub(crate) fn finish(&mut self) {
        self.done = true;
    }

    pub(crate) fn is_done(&self) -> bool {
        self.done
    }

    pub(crate) fn expected(&self) -> u64 {
        self.expected
    }
}

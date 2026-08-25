use std::time::Duration;

use zeroize::Zeroizing;

/// Deadlines shared by every provider connector.
///
/// `first_byte` and `idle` are floors, not caps: whichever of them and the
/// attempt deadline is later governs, so a route configured with a long
/// `overall_timeout` is never cut short by a connector default.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Timeouts {
    /// DNS, TCP, and TLS connection deadline.
    pub connect: Duration,
    /// Minimum time allowed for the first response byte.
    pub first_byte: Duration,
    /// Minimum resetting gap allowed between response events or body chunks.
    pub idle: Duration,
}

impl Default for Timeouts {
    fn default() -> Self {
        Self {
            connect: Duration::from_secs(5),
            first_byte: Duration::from_secs(30),
            idle: Duration::from_secs(60),
        }
    }
}

impl Timeouts {
    pub(in crate::providers) fn validate(self) -> Result<Self, &'static str> {
        [
            ("connect", self.connect),
            ("first_byte", self.first_byte),
            ("idle", self.idle),
        ]
        .into_iter()
        .find_map(|(name, value)| value.is_zero().then_some(name))
        .map_or(Ok(self), Err)
    }
}

pub(in crate::providers) fn validate_response_limits(
    max_response_bytes: usize,
    max_event_bytes: usize,
) -> Result<(), &'static str> {
    [
        ("max_response_bytes", max_response_bytes),
        ("max_event_bytes", max_event_bytes),
    ]
    .into_iter()
    .find_map(|(name, value)| (value == 0).then_some(name))
    .map_or(Ok(()), Err)
}

pub(in crate::providers) fn visible_secret<E>(
    value: impl Into<String>,
    empty: E,
    invalid: E,
) -> Result<Zeroizing<String>, E> {
    let value = value.into();
    if value.trim().is_empty() {
        Err(empty)
    } else if value.bytes().all(|byte| byte.is_ascii_graphic()) {
        Ok(Zeroizing::new(value))
    } else {
        Err(invalid)
    }
}

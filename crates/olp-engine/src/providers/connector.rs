use std::time::Duration;

use zeroize::Zeroizing;

/// Deadlines shared by every provider connector.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Timeouts {
    /// DNS, TCP, and TLS connection deadline.
    pub connect: Duration,
    /// Deadline for receiving the first response byte.
    pub first_byte: Duration,
    /// Resetting deadline between response events or body chunks.
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

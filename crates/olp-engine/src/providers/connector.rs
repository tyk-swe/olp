use std::{fmt, time::Duration};

use zeroize::Zeroizing;

/// A provider API key. Zeroized on drop and never included in `Debug` output.
pub struct ApiKey(Zeroizing<String>);

impl ApiKey {
    pub fn new(value: impl Into<String>) -> Result<Self, ApiKeyError> {
        visible_secret(value, ApiKeyError::Empty, ApiKeyError::Invalid).map(Self)
    }

    pub(in crate::providers) fn expose(&self) -> &str {
        self.0.as_str()
    }
}

impl fmt::Debug for ApiKey {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("ApiKey([REDACTED])")
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
pub enum ApiKeyError {
    #[error("API key cannot be empty")]
    Empty,
    #[error("API key must contain visible ASCII characters only")]
    Invalid,
}

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

#[cfg(test)]
mod tests {
    use super::{ApiKey, ApiKeyError};

    #[test]
    fn api_key_is_debug_redacted_and_rejects_header_injection() {
        let key = ApiKey::new("sk-super-secret").unwrap();
        assert!(!format!("{key:?}").contains("super-secret"));
        assert_eq!(
            ApiKey::new("sk-key\nheader").unwrap_err(),
            ApiKeyError::Invalid
        );
        assert_eq!(ApiKey::new("  ").unwrap_err(), ApiKeyError::Empty);
    }
}

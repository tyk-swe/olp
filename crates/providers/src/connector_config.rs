//! Shared connector configuration invariants.
//!
//! Provider modules keep their provider-specific error vocabulary, while this
//! module owns the values and validation rules that must remain identical
//! across HTTP connectors.

use std::{fmt, time::Duration};

use zeroize::Zeroizing;

pub const DEFAULT_MAX_RESPONSE_BYTES: usize = 16 * 1024 * 1024;
pub const DEFAULT_MAX_EVENT_BYTES: usize = 1024 * 1024;

pub(crate) const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const DEFAULT_FIRST_BYTE_TIMEOUT: Duration = Duration::from_secs(30);
const DEFAULT_IDLE_TIMEOUT: Duration = Duration::from_secs(60);

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ConnectorTimeouts {
    /// DNS/TCP/TLS connection and endpoint-preparation bound.
    pub connect: Duration,
    /// Response setup and first response-body byte bound.
    pub first_byte: Duration,
    /// Resetting response-body or event-stream inactivity bound.
    pub idle: Duration,
}

impl Default for ConnectorTimeouts {
    fn default() -> Self {
        Self {
            connect: DEFAULT_CONNECT_TIMEOUT,
            first_byte: DEFAULT_FIRST_BYTE_TIMEOUT,
            idle: DEFAULT_IDLE_TIMEOUT,
        }
    }
}

impl ConnectorTimeouts {
    pub(crate) fn validate(self) -> Result<Self, &'static str> {
        for (name, value) in [
            ("connect", self.connect),
            ("first_byte", self.first_byte),
            ("idle", self.idle),
        ] {
            if value.is_zero() {
                return Err(name);
            }
        }
        Ok(self)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct ResponseLimits {
    pub(crate) response_bytes: usize,
    pub(crate) event_bytes: usize,
}

impl Default for ResponseLimits {
    fn default() -> Self {
        Self {
            response_bytes: DEFAULT_MAX_RESPONSE_BYTES,
            event_bytes: DEFAULT_MAX_EVENT_BYTES,
        }
    }
}

impl ResponseLimits {
    pub(crate) fn new(response_bytes: usize, event_bytes: usize) -> Result<Self, &'static str> {
        if response_bytes == 0 {
            return Err("max_response_bytes");
        }
        if event_bytes == 0 {
            return Err("max_event_bytes");
        }
        Ok(Self {
            response_bytes,
            event_bytes,
        })
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SecretValidationError {
    Empty,
    Invalid,
}

/// A non-empty visible-ASCII secret whose storage is zeroized on drop.
///
/// Header construction still performs its own validation at the HTTP boundary;
/// this type prevents all known control-character and whitespace injection
/// paths before a connector can be assembled.
pub(crate) struct SecretString(Zeroizing<String>);

impl SecretString {
    pub(crate) fn new(value: impl Into<String>) -> Result<Self, SecretValidationError> {
        let value = Zeroizing::new(value.into());
        if value.trim().is_empty() {
            return Err(SecretValidationError::Empty);
        }
        if !value.bytes().all(|byte| byte.is_ascii_graphic()) {
            return Err(SecretValidationError::Invalid);
        }
        Ok(Self(value))
    }

    pub(crate) fn expose(&self) -> &str {
        self.0.as_str()
    }
}

impl fmt::Debug for SecretString {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("SecretString([REDACTED])")
    }
}

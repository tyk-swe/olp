//! Direct Anthropic connector with a fail-closed custom-endpoint boundary.
//!
//! DNS is resolved, classified, and pinned into a redirect-free client whose
//! connection pool is reused only for an unchanged, periodically revalidated
//! DNS identity; idle sockets have a bounded lifetime. Credentials are
//! attached only after request encoding and endpoint validation complete. The
//! connector performs no hidden retries.

mod endpoint;
mod transport;

use std::{fmt, time::Duration};

pub use endpoint::EndpointError;
pub use transport::{AnthropicConnector, validate_operation};
use zeroize::Zeroizing;

use crate::anthropic::endpoint::Endpoint;

pub const DEFAULT_MAX_RESPONSE_BYTES: usize = 16 * 1024 * 1024;
pub const DEFAULT_MAX_EVENT_BYTES: usize = 1024 * 1024;
pub const DEFAULT_API_VERSION: &str = "2023-06-01";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ConnectorTimeouts {
    pub connect: Duration,
    pub first_byte: Duration,
    pub idle: Duration,
}

impl Default for ConnectorTimeouts {
    fn default() -> Self {
        Self {
            connect: Duration::from_secs(5),
            first_byte: Duration::from_secs(30),
            idle: Duration::from_secs(60),
        }
    }
}

impl ConnectorTimeouts {
    #[cfg(any(test, feature = "test-util"))]
    fn validate(self) -> Result<Self, ConnectorBuildError> {
        for (name, value) in [
            ("connect", self.connect),
            ("first_byte", self.first_byte),
            ("idle", self.idle),
        ] {
            if value.is_zero() {
                return Err(ConnectorBuildError::ZeroTimeout(name));
            }
        }
        Ok(self)
    }
}

#[derive(Clone, Debug)]
pub struct ConnectorConfig {
    endpoint: Endpoint,
    api_version: String,
    timeouts: ConnectorTimeouts,
    max_response_bytes: usize,
    max_event_bytes: usize,
}

impl Default for ConnectorConfig {
    fn default() -> Self {
        Self {
            endpoint: Endpoint::default(),
            api_version: DEFAULT_API_VERSION.to_owned(),
            timeouts: ConnectorTimeouts::default(),
            max_response_bytes: DEFAULT_MAX_RESPONSE_BYTES,
            max_event_bytes: DEFAULT_MAX_EVENT_BYTES,
        }
    }
}

impl ConnectorConfig {
    pub fn with_base_url(base_url: &str) -> Result<Self, ConnectorBuildError> {
        Ok(Self {
            endpoint: Endpoint::parse(base_url)?,
            ..Self::default()
        })
    }

    pub fn with_api_version(
        mut self,
        version: impl Into<String>,
    ) -> Result<Self, ConnectorBuildError> {
        let version = version.into();
        if version.is_empty() || !version.bytes().all(|byte| byte.is_ascii_graphic()) {
            return Err(ConnectorBuildError::InvalidApiVersion);
        }
        self.api_version = version;
        Ok(self)
    }

    #[cfg(any(test, feature = "test-util"))]
    pub fn with_timeouts(
        mut self,
        timeouts: ConnectorTimeouts,
    ) -> Result<Self, ConnectorBuildError> {
        self.timeouts = timeouts.validate()?;
        self.endpoint.set_connect_timeout(self.timeouts.connect);
        Ok(self)
    }

    #[cfg(any(test, feature = "test-util"))]
    pub fn with_response_limits(
        mut self,
        max_response_bytes: usize,
        max_event_bytes: usize,
    ) -> Result<Self, ConnectorBuildError> {
        if max_response_bytes == 0 {
            return Err(ConnectorBuildError::ZeroLimit("max_response_bytes"));
        }
        if max_event_bytes == 0 {
            return Err(ConnectorBuildError::ZeroLimit("max_event_bytes"));
        }
        self.max_response_bytes = max_response_bytes;
        self.max_event_bytes = max_event_bytes;
        Ok(self)
    }

    #[cfg(test)]
    fn for_local_test(base_url: &str, timeouts: ConnectorTimeouts) -> Self {
        let mut endpoint = Endpoint::for_local_test(base_url);
        endpoint.set_connect_timeout(timeouts.connect);
        Self {
            endpoint,
            timeouts,
            ..Self::default()
        }
    }
}

pub struct AnthropicApiKey(Zeroizing<String>);

impl AnthropicApiKey {
    pub fn new(value: impl Into<String>) -> Result<Self, ConnectorBuildError> {
        let value = value.into();
        if value.trim().is_empty() {
            return Err(ConnectorBuildError::EmptyApiKey);
        }
        if !value.bytes().all(|byte| byte.is_ascii_graphic()) {
            return Err(ConnectorBuildError::InvalidApiKey);
        }
        Ok(Self(Zeroizing::new(value)))
    }

    pub(crate) fn expose(&self) -> &str {
        self.0.as_str()
    }
}

impl fmt::Debug for AnthropicApiKey {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("AnthropicApiKey([REDACTED])")
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ConnectorBuildError {
    #[error(transparent)]
    Endpoint(#[from] EndpointError),
    #[error("Anthropic API key cannot be empty")]
    EmptyApiKey,
    #[error("Anthropic API key must contain visible ASCII characters only")]
    InvalidApiKey,
    #[error("Anthropic API version must contain visible ASCII characters only")]
    InvalidApiVersion,
    #[cfg(any(test, feature = "test-util"))]
    #[error("Anthropic connector {0} timeout must be greater than zero")]
    ZeroTimeout(&'static str),
    #[cfg(any(test, feature = "test-util"))]
    #[error("Anthropic connector {0} limit must be greater than zero")]
    ZeroLimit(&'static str),
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn secrets_are_debug_redacted_and_header_injection_is_rejected() {
        let key = AnthropicApiKey::new("sk-ant-secret").unwrap();
        let debug = format!("{key:?}");
        assert!(debug.contains("REDACTED"));
        assert!(!debug.contains("sk-ant-secret"));
        assert!(matches!(
            AnthropicApiKey::new("secret\nheader"),
            Err(ConnectorBuildError::InvalidApiKey)
        ));
        assert!(matches!(
            ConnectorConfig::default().with_api_version("bad\nversion"),
            Err(ConnectorBuildError::InvalidApiVersion)
        ));
    }

    #[test]
    fn rejects_zero_deadlines_and_limits() {
        assert!(matches!(
            ConnectorConfig::default().with_timeouts(ConnectorTimeouts {
                idle: Duration::ZERO,
                ..ConnectorTimeouts::default()
            }),
            Err(ConnectorBuildError::ZeroTimeout("idle"))
        ));
        assert!(matches!(
            ConnectorConfig::default().with_response_limits(1, 0),
            Err(ConnectorBuildError::ZeroLimit("max_event_bytes"))
        ));
    }
}

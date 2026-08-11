//! Direct OpenAI connector with a fail-closed custom-endpoint boundary.
//!
//! DNS is resolved, classified, and pinned into a redirect-free `reqwest`
//! client. A connector reuses that client's connection pool only while the
//! immutable origin and validated DNS identity remain unchanged; DNS is
//! periodically revalidated and idle sockets have a bounded lifetime.
//! Provider credentials are attached only after endpoint validation and
//! request translation have completed and are never stored in the client.

mod certification;
mod endpoint;
mod transport;

use std::fmt;

pub use certification::{
    CompatibleCapability, CompatibleCapabilityCertificationError, NativeOpenAiCertificationEvidence,
};
pub use endpoint::EndpointError;
pub use transport::OpenAiConnector;
use zeroize::Zeroizing;

pub use crate::ConnectorTimeouts;
use crate::openai::endpoint::Endpoint;

pub const DEFAULT_MAX_RESPONSE_BYTES: usize = 16 * 1024 * 1024;
pub const DEFAULT_MAX_EVENT_BYTES: usize = 1024 * 1024;

#[derive(Clone, Debug)]
pub struct ConnectorConfig {
    endpoint: Endpoint,
    timeouts: ConnectorTimeouts,
    max_response_bytes: usize,
    max_event_bytes: usize,
}

impl Default for ConnectorConfig {
    fn default() -> Self {
        Self {
            endpoint: Endpoint::default(),
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

    /// Accepts plain-HTTP and non-public targets. Exists only for test
    /// builds; release binaries never compile this constructor.
    #[cfg(any(test, feature = "test-util"))]
    pub fn with_base_url_unsafe_test_target(base_url: &str) -> Result<Self, ConnectorBuildError> {
        Ok(Self {
            endpoint: Endpoint::parse_with_unsafe_test_target(base_url)?,
            ..Self::default()
        })
    }

    pub fn with_timeouts(
        mut self,
        timeouts: ConnectorTimeouts,
    ) -> Result<Self, ConnectorBuildError> {
        self.timeouts = timeouts
            .validate()
            .map_err(ConnectorBuildError::ZeroTimeout)?;
        self.endpoint.set_connect_timeout(self.timeouts.connect);
        Ok(self)
    }

    pub fn with_response_limits(
        mut self,
        max_response_bytes: usize,
        max_event_bytes: usize,
    ) -> Result<Self, ConnectorBuildError> {
        crate::connector::validate_response_limits(max_response_bytes, max_event_bytes)
            .map_err(ConnectorBuildError::ZeroLimit)?;
        self.max_response_bytes = max_response_bytes;
        self.max_event_bytes = max_event_bytes;
        Ok(self)
    }

    /// Appends a validated `api-version` query parameter to every resource
    /// URL. This is purpose-specific for Azure OpenAI and cannot inject an
    /// arbitrary query name or additional authority.
    pub fn with_api_version(mut self, api_version: &str) -> Result<Self, ConnectorBuildError> {
        self.endpoint.set_api_version(api_version)?;
        Ok(self)
    }

    #[cfg(any(test, feature = "test-util"))]
    pub fn for_local_test(base_url: &str, timeouts: ConnectorTimeouts) -> Self {
        let mut endpoint = Endpoint::for_local_test(base_url);
        endpoint.set_connect_timeout(timeouts.connect);
        Self {
            endpoint,
            timeouts,
            max_response_bytes: DEFAULT_MAX_RESPONSE_BYTES,
            max_event_bytes: DEFAULT_MAX_EVENT_BYTES,
        }
    }
}

pub struct OpenAiApiKey(Zeroizing<String>);

impl OpenAiApiKey {
    pub fn new(value: impl Into<String>) -> Result<Self, ConnectorBuildError> {
        crate::connector::visible_secret(
            value,
            ConnectorBuildError::EmptyApiKey,
            ConnectorBuildError::InvalidApiKey,
        )
        .map(Self)
    }

    pub(crate) fn expose(&self) -> &str {
        self.0.as_str()
    }
}

impl fmt::Debug for OpenAiApiKey {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("OpenAiApiKey([REDACTED])")
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ConnectorBuildError {
    #[error(transparent)]
    Endpoint(#[from] EndpointError),
    #[error("OpenAI API key cannot be empty")]
    EmptyApiKey,
    #[error("OpenAI API key must contain visible ASCII characters only")]
    InvalidApiKey,
    #[error("OpenAI connector {0} timeout must be greater than zero")]
    ZeroTimeout(&'static str),
    #[error("OpenAI connector {0} limit must be greater than zero")]
    ZeroLimit(&'static str),
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use super::*;

    #[test]
    fn api_key_debug_is_redacted() {
        let key = OpenAiApiKey::new("sk-super-secret").unwrap();
        let debug = format!("{key:?}");
        assert!(debug.contains("REDACTED"));
        assert!(!debug.contains("super-secret"));
        assert!(matches!(
            OpenAiApiKey::new("sk-key\nheader-injection"),
            Err(ConnectorBuildError::InvalidApiKey)
        ));
    }

    #[test]
    fn rejects_zero_deadlines_and_limits() {
        assert!(matches!(
            ConnectorConfig::default().with_timeouts(ConnectorTimeouts {
                connect: Duration::ZERO,
                ..ConnectorTimeouts::default()
            }),
            Err(ConnectorBuildError::ZeroTimeout("connect"))
        ));
        assert!(matches!(
            ConnectorConfig::default().with_response_limits(0, 1),
            Err(ConnectorBuildError::ZeroLimit("max_response_bytes"))
        ));
    }
}

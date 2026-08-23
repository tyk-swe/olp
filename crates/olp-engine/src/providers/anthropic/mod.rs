//! Direct Anthropic connector with a fail-closed custom-endpoint boundary.
//!
//! DNS is resolved, classified, and pinned into a redirect-free client whose
//! connection pool is reused only for an unchanged, periodically revalidated
//! DNS identity; idle sockets have a bounded lifetime. Credentials are
//! attached only after request encoding and endpoint validation complete. The
//! connector performs no hidden retries.

mod endpoint;
pub mod transport;

use crate::providers::anthropic::endpoint::Endpoint;
use crate::providers::connector::Timeouts;
use crate::providers::endpoint::Error;

pub const DEFAULT_MAX_RESPONSE_BYTES: usize = 16 * 1024 * 1024;
pub const DEFAULT_MAX_EVENT_BYTES: usize = 1024 * 1024;
pub const DEFAULT_API_VERSION: &str = "2023-06-01";

#[derive(Clone, Debug)]
pub struct ConnectorConfig {
    endpoint: Endpoint,
    api_version: String,
    timeouts: Timeouts,
    max_response_bytes: usize,
    max_event_bytes: usize,
}

impl Default for ConnectorConfig {
    fn default() -> Self {
        Self {
            endpoint: Endpoint::default(),
            api_version: DEFAULT_API_VERSION.to_owned(),
            timeouts: Timeouts::default(),
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
    pub(in crate::providers) fn for_local_test(base_url: &str, timeouts: Timeouts) -> Self {
        let mut endpoint = Endpoint::for_local_test(base_url);
        endpoint.set_connect_timeout(timeouts.connect);
        Self {
            endpoint,
            timeouts,
            ..Self::default()
        }
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ConnectorBuildError {
    #[error(transparent)]
    Endpoint(#[from] Error),
    #[error("Anthropic API version must contain visible ASCII characters only")]
    InvalidApiVersion,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn header_injection_in_api_version_is_rejected() {
        assert!(matches!(
            ConnectorConfig::default().with_api_version("bad\nversion"),
            Err(ConnectorBuildError::InvalidApiVersion)
        ));
    }
}

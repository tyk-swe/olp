//! Direct OpenAI connector with a fail-closed custom-endpoint boundary.
//!
//! DNS is resolved, classified, and pinned into a redirect-free `reqwest`
//! client. A connector reuses that client's connection pool only while the
//! immutable origin and validated DNS identity remain unchanged; DNS is
//! periodically revalidated and idle sockets have a bounded lifetime.
//! Provider credentials are attached only after endpoint validation and
//! request translation have completed and are never stored in the client.

pub mod certification;
mod endpoint;
pub mod transport;

use crate::providers::connector::Timeouts;
use crate::providers::openai::endpoint::Endpoint;

pub const DEFAULT_MAX_RESPONSE_BYTES: usize = 16 * 1024 * 1024;
pub const DEFAULT_MAX_EVENT_BYTES: usize = 1024 * 1024;

#[derive(Clone, Debug)]
pub struct ConnectorConfig {
    endpoint: Endpoint,
    timeouts: Timeouts,
    max_response_bytes: usize,
    max_event_bytes: usize,
}

impl Default for ConnectorConfig {
    fn default() -> Self {
        Self {
            endpoint: Endpoint::default(),
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

    /// Accepts plain-HTTP and non-public targets. Exists only for test
    /// builds; release binaries never compile this constructor.
    #[cfg(any(test, feature = "test-util"))]
    pub fn with_base_url_unsafe_test_target(base_url: &str) -> Result<Self, ConnectorBuildError> {
        Ok(Self {
            endpoint: Endpoint::parse_with_unsafe_test_target(base_url)?,
            ..Self::default()
        })
    }

    /// Appends a validated `api-version` query parameter to every resource
    /// URL. This is purpose-specific for Azure OpenAI and cannot inject an
    /// arbitrary query name or additional authority.
    pub fn with_api_version(mut self, api_version: &str) -> Result<Self, ConnectorBuildError> {
        self.endpoint.set_api_version(api_version)?;
        Ok(self)
    }

    #[cfg(any(test, feature = "test-util"))]
    pub fn for_local_test(base_url: &str, timeouts: Timeouts) -> Self {
        let endpoint = Endpoint::for_local_test(base_url, timeouts.connect);
        Self {
            endpoint,
            timeouts,
            max_response_bytes: DEFAULT_MAX_RESPONSE_BYTES,
            max_event_bytes: DEFAULT_MAX_EVENT_BYTES,
        }
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ConnectorBuildError {
    #[error(transparent)]
    Endpoint(#[from] endpoint::Error),
}

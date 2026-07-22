//! Direct Gemini Developer API connector with fail-closed endpoint handling.
//!
//! DNS answers are validated and pinned before the API key is attached. The
//! redirect-free connection pool is reused only for an unchanged,
//! periodically revalidated DNS identity and has a bounded idle lifetime.
//! Ambient proxies and reqwest retries are disabled.

mod endpoint;
mod headers;
mod transport;

use std::{fmt, sync::Arc};

#[cfg(test)]
use std::time::Duration;

pub use endpoint::EndpointError;
use olp_domain::BoxFuture;
pub use transport::{GeminiConnector, validate_operation};

pub use crate::connector_config::{
    ConnectorTimeouts, DEFAULT_MAX_EVENT_BYTES, DEFAULT_MAX_RESPONSE_BYTES,
};
use crate::connector_config::{ResponseLimits, SecretString, SecretValidationError};
use crate::gemini::endpoint::Endpoint;

#[derive(Clone, Debug)]
pub struct ConnectorConfig {
    endpoint: Endpoint,
    timeouts: ConnectorTimeouts,
    response_limits: ResponseLimits,
}

impl Default for ConnectorConfig {
    fn default() -> Self {
        Self {
            endpoint: Endpoint::default(),
            timeouts: ConnectorTimeouts::default(),
            response_limits: ResponseLimits::default(),
        }
    }
}

impl ConnectorConfig {
    /// Overrides the Developer API root. The root normally ends in `/v1beta/`
    /// (or `/v1/` when the stable surface is desired).
    pub fn with_base_url(base_url: &str) -> Result<Self, ConnectorBuildError> {
        Ok(Self {
            endpoint: Endpoint::parse(base_url)?,
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
        self.response_limits = ResponseLimits::new(max_response_bytes, max_event_bytes)
            .map_err(ConnectorBuildError::ZeroLimit)?;
        Ok(self)
    }

    #[cfg(any(test, feature = "test-util"))]
    #[doc(hidden)]
    pub fn for_local_test(base_url: &str, timeouts: ConnectorTimeouts) -> Self {
        let mut endpoint = Endpoint::for_local_test(base_url);
        endpoint.set_connect_timeout(timeouts.connect);
        Self {
            endpoint,
            timeouts,
            ..Self::default()
        }
    }
}

pub struct GeminiApiKey(SecretString);

impl GeminiApiKey {
    pub fn new(value: impl Into<String>) -> Result<Self, ConnectorBuildError> {
        match SecretString::new(value) {
            Ok(value) => Ok(Self(value)),
            Err(SecretValidationError::Empty) => Err(ConnectorBuildError::EmptyApiKey),
            Err(SecretValidationError::Invalid) => Err(ConnectorBuildError::InvalidApiKey),
        }
    }

    pub(crate) fn expose(&self) -> &str {
        self.0.expose()
    }
}

/// A short-lived OAuth bearer token. The value is zeroized when the request
/// header has been constructed and is never included in `Debug` output.
pub struct SecretBearerToken(SecretString);

impl SecretBearerToken {
    pub fn new(value: impl Into<String>) -> Result<Self, BearerTokenError> {
        SecretString::new(value).map(Self).map_err(|_| BearerTokenError)
    }

    pub(crate) fn expose(&self) -> &str {
        self.0.expose()
    }
}

impl fmt::Debug for SecretBearerToken {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("SecretBearerToken([REDACTED])")
    }
}

/// Supplies short-lived bearer tokens to Google transports. Implementations
/// own refresh/caching policy; connectors never persist or log returned values.
pub trait BearerTokenProvider: Send + Sync + fmt::Debug {
    fn token<'a>(&'a self) -> BoxFuture<'a, Result<SecretBearerToken, BearerTokenError>>;
}

#[derive(Clone, Copy, Debug, thiserror::Error)]
#[error("Google OAuth bearer token acquisition failed")]
pub struct BearerTokenError;

pub(crate) enum ConnectorCredential {
    ApiKey(GeminiApiKey),
    Bearer(Arc<dyn BearerTokenProvider>),
}

impl fmt::Debug for ConnectorCredential {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::ApiKey(_) => formatter.write_str("ConnectorCredential::ApiKey([REDACTED])"),
            Self::Bearer(_) => formatter.write_str("ConnectorCredential::Bearer([REDACTED])"),
        }
    }
}

impl fmt::Debug for GeminiApiKey {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("GeminiApiKey([REDACTED])")
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ConnectorBuildError {
    #[error(transparent)]
    Endpoint(#[from] EndpointError),
    #[error("Gemini API key cannot be empty")]
    EmptyApiKey,
    #[error("Gemini API key must contain visible ASCII characters only")]
    InvalidApiKey,
    #[error("Gemini connector {0} timeout must be greater than zero")]
    ZeroTimeout(&'static str),
    #[error("Gemini connector {0} limit must be greater than zero")]
    ZeroLimit(&'static str),
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn key_debug_is_redacted_and_header_injection_is_rejected() {
        let key = GeminiApiKey::new("google-secret").unwrap();
        let debug = format!("{key:?}");
        assert!(debug.contains("REDACTED"));
        assert!(!debug.contains("google-secret"));
        assert!(matches!(
            GeminiApiKey::new("secret\nheader"),
            Err(ConnectorBuildError::InvalidApiKey)
        ));
    }

    #[test]
    fn rejects_zero_deadlines_and_limits() {
        assert!(matches!(
            ConnectorConfig::default().with_timeouts(ConnectorTimeouts {
                first_byte: Duration::ZERO,
                ..ConnectorTimeouts::default()
            }),
            Err(ConnectorBuildError::ZeroTimeout("first_byte"))
        ));
        assert!(matches!(
            ConnectorConfig::default().with_response_limits(0, 1),
            Err(ConnectorBuildError::ZeroLimit("max_response_bytes"))
        ));
    }
}

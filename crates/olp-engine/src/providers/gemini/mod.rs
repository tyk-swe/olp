//! Direct Gemini Developer API connector with fail-closed endpoint handling.
//!
//! DNS answers are validated and pinned before the API key is attached. The
//! redirect-free connection pool is reused only for an unchanged,
//! periodically revalidated DNS identity and has a bounded idle lifetime.
//! Ambient proxies and reqwest retries are disabled.

pub mod endpoint;
pub mod transport;

use std::{fmt, sync::Arc};

use crate::domain::ports::BoxFuture;
use zeroize::Zeroizing;

use crate::providers::EgressPolicy;
use crate::providers::connector::Timeouts;
use crate::providers::gemini::endpoint::Endpoint;

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
    /// Overrides the Developer API root. The root normally ends in `/v1beta/`
    /// (or `/v1/` when the stable surface is desired).
    pub fn with_base_url(base_url: &str) -> Result<Self, ConnectorBuildError> {
        Self::with_base_url_and_policy(base_url, &EgressPolicy::default())
    }

    pub fn with_base_url_and_policy(
        base_url: &str,
        policy: &EgressPolicy,
    ) -> Result<Self, ConnectorBuildError> {
        Ok(Self {
            endpoint: Endpoint::parse_with_policy(base_url, policy)?,
            ..Self::default()
        })
    }

    pub fn with_timeouts(mut self, timeouts: Timeouts) -> Result<Self, ConnectorBuildError> {
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
        crate::providers::connector::validate_response_limits(max_response_bytes, max_event_bytes)
            .map_err(ConnectorBuildError::ZeroLimit)?;
        self.max_response_bytes = max_response_bytes;
        self.max_event_bytes = max_event_bytes;
        Ok(self)
    }

    #[cfg(test)]
    pub(in crate::providers) fn response_limits(
        &self,
    ) -> crate::providers::connector::ResponseLimits {
        crate::providers::connector::ResponseLimits {
            max_response_bytes: self.max_response_bytes,
            max_event_bytes: self.max_event_bytes,
        }
    }

    #[cfg(any(test, feature = "test-util"))]
    #[doc(hidden)]
    pub fn for_local_test(base_url: &str, timeouts: Timeouts) -> Self {
        let mut endpoint = Endpoint::for_local_test(base_url);
        endpoint.set_connect_timeout(timeouts.connect);
        Self {
            endpoint,
            timeouts,
            ..Self::default()
        }
    }
}

pub struct ApiKey(Zeroizing<String>);

impl ApiKey {
    pub fn new(value: impl Into<String>) -> Result<Self, ConnectorBuildError> {
        crate::providers::connector::visible_secret(
            value,
            ConnectorBuildError::EmptyApiKey,
            ConnectorBuildError::InvalidApiKey,
        )
        .map(Self)
    }

    pub(in crate::providers) fn expose(&self) -> &str {
        self.0.as_str()
    }
}

/// A short-lived OAuth bearer token. The value is zeroized when the request
/// header has been constructed and is never included in `Debug` output.
pub struct SecretBearerToken(Zeroizing<String>);

impl SecretBearerToken {
    pub fn new(value: impl Into<String>) -> Result<Self, BearerTokenError> {
        crate::providers::connector::visible_secret(value, BearerTokenError, BearerTokenError)
            .map(Self)
    }

    pub(in crate::providers) fn expose(&self) -> &str {
        self.0.as_str()
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

pub(in crate::providers) enum ConnectorCredential {
    ApiKey(ApiKey),
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

impl fmt::Debug for ApiKey {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("ApiKey([REDACTED])")
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ConnectorBuildError {
    #[error(transparent)]
    Endpoint(#[from] endpoint::Error),
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
    use std::time::Duration;

    use super::*;

    #[test]
    fn key_debug_is_redacted_and_header_injection_is_rejected() {
        let key = ApiKey::new("google-secret").unwrap();
        let debug = format!("{key:?}");
        assert!(debug.contains("REDACTED"));
        assert!(!debug.contains("google-secret"));
        assert!(matches!(
            ApiKey::new("secret\nheader"),
            Err(ConnectorBuildError::InvalidApiKey)
        ));
    }

    #[test]
    fn rejects_zero_deadlines_and_limits() {
        assert!(matches!(
            ConnectorConfig::default().with_timeouts(Timeouts {
                first_byte: Duration::ZERO,
                ..Timeouts::default()
            }),
            Err(ConnectorBuildError::ZeroTimeout("first_byte"))
        ));
        assert!(matches!(
            ConnectorConfig::default().with_response_limits(0, 1),
            Err(ConnectorBuildError::ZeroLimit("max_response_bytes"))
        ));
    }
}

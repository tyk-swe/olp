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

use crate::providers::connector::{ApiKey, Timeouts};
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
        Ok(Self {
            endpoint: Endpoint::parse(base_url)?,
            ..Self::default()
        })
    }

    #[cfg(any(test, feature = "test-util"))]
    #[doc(hidden)]
    pub fn for_local_test(base_url: &str, timeouts: Timeouts) -> Self {
        let endpoint = Endpoint::for_local_test(base_url, timeouts.connect);
        Self {
            endpoint,
            timeouts,
            ..Self::default()
        }
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

#[derive(Debug, thiserror::Error)]
pub enum ConnectorBuildError {
    #[error(transparent)]
    Endpoint(#[from] endpoint::Error),
}

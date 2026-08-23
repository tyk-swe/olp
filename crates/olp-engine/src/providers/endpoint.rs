use std::{fmt, net::IpAddr, time::Duration};

use reqwest::{Client, Url};
use thiserror::Error;

use crate::providers::http_egress::{
    is_public_ip,
    pinned::{PinnedClientConfig, PinnedClientError, PinnedClientPool, literal_ip},
};

const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_secs(30);
const MAX_IDLE_CONNECTIONS_PER_HOST: usize = 256;

#[derive(Clone)]
pub(in crate::providers) struct EndpointCore {
    base_url: Url,
    provider: &'static str,
    client_connect_timeout: Duration,
    client_pool: PinnedClientPool,
    #[cfg(any(test, feature = "test-util"))]
    allow_unsafe_test_target: bool,
}

impl EndpointCore {
    pub(in crate::providers) fn parse(
        value: &str,
        provider: &'static str,
        allow_unsafe_target: bool,
    ) -> Result<Self, Error> {
        let mut base_url = Url::parse(value).map_err(|error| Error::InvalidUrl {
            provider,
            message: error.to_string(),
        })?;
        if base_url.scheme() != "https" && !allow_unsafe_target {
            return Err(Error::HttpsRequired { provider });
        }
        if !matches!(base_url.scheme(), "http" | "https") {
            return Err(Error::UnsupportedScheme { provider });
        }
        if !base_url.username().is_empty() || base_url.password().is_some() {
            return Err(Error::UserInfoForbidden { provider });
        }
        if base_url.host().is_none() {
            return Err(Error::MissingHost { provider });
        }
        if base_url.port() == Some(0) {
            return Err(Error::InvalidPort { provider });
        }
        if base_url.query().is_some() || base_url.fragment().is_some() {
            return Err(Error::QueryOrFragmentForbidden { provider });
        }
        if !base_url.path().ends_with('/') {
            base_url.set_path(&format!("{}/", base_url.path()));
        }
        if let Some(address) = literal_ip(&base_url)
            && !allow_unsafe_target
            && !is_public_ip(address)
        {
            return Err(Error::ForbiddenAddress { provider, address });
        }
        Ok(Self {
            base_url,
            provider,
            client_connect_timeout: DEFAULT_CONNECT_TIMEOUT,
            client_pool: PinnedClientPool::default(),
            #[cfg(any(test, feature = "test-util"))]
            allow_unsafe_test_target: allow_unsafe_target,
        })
    }

    pub(in crate::providers) fn url(&self) -> &Url {
        &self.base_url
    }

    pub(in crate::providers) fn join(&self, path: &str) -> Result<Url, Error> {
        self.base_url.join(path).map_err(|error| Error::InvalidUrl {
            provider: self.provider,
            message: error.to_string(),
        })
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(in crate::providers) fn set_connect_timeout(&mut self, value: Duration) {
        self.client_connect_timeout = value;
    }

    pub(in crate::providers) async fn pinned_client(
        &self,
        connect_timeout: Duration,
    ) -> Result<Client, Error> {
        #[cfg(any(test, feature = "test-util"))]
        let allow_unsafe_target = self.allow_unsafe_test_target;
        #[cfg(not(any(test, feature = "test-util")))]
        let allow_unsafe_target = false;
        self.client_pool
            .client(
                &self.base_url,
                connect_timeout,
                PinnedClientConfig {
                    connect_timeout: self.client_connect_timeout,
                    pool_idle_timeout: Some(POOL_IDLE_TIMEOUT),
                    pool_max_idle_per_host: Some(MAX_IDLE_CONNECTIONS_PER_HOST),
                    allow_unsafe_target,
                    user_agent: "openllmproxy",
                },
            )
            .await
            .map_err(|error| Error::from_pinned(self.provider, error))
    }
}

impl fmt::Debug for EndpointCore {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Endpoint")
            .field("scheme", &self.base_url.scheme())
            .field("host", &self.base_url.host_str())
            .field("port", &self.base_url.port())
            .field("path", &"[REDACTED]")
            .finish_non_exhaustive()
    }
}

/// Error shared by all HTTP provider endpoints. The provider label keeps the
/// existing diagnostics vendor-specific without duplicating endpoint policy.
#[derive(Debug, Error)]
pub enum Error {
    #[error("custom {provider} endpoints must use HTTPS")]
    HttpsRequired { provider: &'static str },
    #[error("custom {provider} endpoint scheme must be HTTP or HTTPS")]
    UnsupportedScheme { provider: &'static str },
    #[error("custom {provider} endpoints cannot contain user information")]
    UserInfoForbidden { provider: &'static str },
    #[error("custom {provider} endpoint must include a host")]
    MissingHost { provider: &'static str },
    #[error("custom {provider} endpoint must have a known or explicit port")]
    MissingPort { provider: &'static str },
    #[error("custom {provider} endpoint port must be greater than zero")]
    InvalidPort { provider: &'static str },
    #[error("custom {provider} endpoints cannot contain a query or fragment")]
    QueryOrFragmentForbidden { provider: &'static str },
    #[error("custom {provider} endpoint URL is invalid: {message}")]
    InvalidUrl {
        provider: &'static str,
        message: String,
    },
    #[error("custom {provider} endpoint resolves to forbidden address {address}")]
    ForbiddenAddress {
        provider: &'static str,
        address: IpAddr,
    },
    #[error("custom {provider} endpoint DNS resolution timed out")]
    DnsTimeout { provider: &'static str },
    #[error("custom {provider} endpoint DNS resolution failed")]
    DnsResolution {
        provider: &'static str,
        #[source]
        source: std::io::Error,
    },
    #[error("custom {provider} endpoint did not resolve to an address")]
    NoAddresses { provider: &'static str },
    #[error("failed to build the pinned {provider} HTTP client")]
    ClientBuild {
        provider: &'static str,
        #[source]
        source: reqwest::Error,
    },
}

impl Error {
    fn from_pinned(provider: &'static str, error: PinnedClientError) -> Self {
        match error {
            PinnedClientError::MissingHost => Self::MissingHost { provider },
            PinnedClientError::MissingPort => Self::MissingPort { provider },
            PinnedClientError::DnsTimeout => Self::DnsTimeout { provider },
            PinnedClientError::DnsResolution(source) => Self::DnsResolution { provider, source },
            PinnedClientError::NoAddresses => Self::NoAddresses { provider },
            PinnedClientError::ForbiddenAddress(address) => {
                Self::ForbiddenAddress { provider, address }
            }
            PinnedClientError::ClientBuild(source) => Self::ClientBuild { provider, source },
        }
    }
}

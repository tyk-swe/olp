//! Shared validation and client-pool state for configurable provider origins.

use std::{fmt, net::IpAddr, time::Duration};

use reqwest::{Client, Url};
use thiserror::Error;

use crate::connector_config::DEFAULT_CONNECT_TIMEOUT;
use crate::http_egress::{
    is_public_ip,
    pinned::{PinnedClientConfig, PinnedClientError, PinnedClientPool, literal_ip},
};

const POOL_IDLE_TIMEOUT: Duration = Duration::from_secs(30);
const MAX_IDLE_CONNECTIONS_PER_HOST: usize = 256;
const USER_AGENT: &str = "openllmproxy";

pub(crate) struct ProviderEndpoint {
    base_url: Url,
    client_connect_timeout: Duration,
    client_pool: PinnedClientPool,
    allow_unsafe_target: bool,
}

impl Clone for ProviderEndpoint {
    fn clone(&self) -> Self {
        Self {
            base_url: self.base_url.clone(),
            client_connect_timeout: self.client_connect_timeout,
            client_pool: self.client_pool.clone(),
            allow_unsafe_target: self.allow_unsafe_target,
        }
    }
}

impl fmt::Debug for ProviderEndpoint {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.fmt_redacted("ProviderEndpoint", formatter)
    }
}

impl ProviderEndpoint {
    pub(crate) fn parse(value: &str) -> Result<Self, ProviderEndpointError> {
        Self::parse_with_policy(value, false)
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(crate) fn for_local_test(value: &str) -> Result<Self, ProviderEndpointError> {
        Self::parse_with_policy(value, true)
    }

    fn parse_with_policy(
        value: &str,
        allow_unsafe_target: bool,
    ) -> Result<Self, ProviderEndpointError> {
        let mut base_url = Url::parse(value)
            .map_err(|error| ProviderEndpointError::InvalidUrl(error.to_string()))?;
        if base_url.scheme() != "https" && !allow_unsafe_target {
            return Err(ProviderEndpointError::HttpsRequired);
        }
        if !matches!(base_url.scheme(), "http" | "https") {
            return Err(ProviderEndpointError::UnsupportedScheme);
        }
        if !base_url.username().is_empty() || base_url.password().is_some() {
            return Err(ProviderEndpointError::UserInfoForbidden);
        }
        if base_url.host().is_none() {
            return Err(ProviderEndpointError::MissingHost);
        }
        if base_url.port() == Some(0) {
            return Err(ProviderEndpointError::InvalidPort);
        }
        if base_url.query().is_some() || base_url.fragment().is_some() {
            return Err(ProviderEndpointError::QueryOrFragmentForbidden);
        }
        if !base_url.path().ends_with('/') {
            let normalized = format!("{}/", base_url.path());
            base_url.set_path(&normalized);
        }
        if let Some(address) = literal_ip(&base_url)
            && !allow_unsafe_target
            && !is_public_ip(address)
        {
            return Err(ProviderEndpointError::ForbiddenAddress(address));
        }
        Ok(Self {
            base_url,
            client_connect_timeout: DEFAULT_CONNECT_TIMEOUT,
            client_pool: PinnedClientPool::default(),
            allow_unsafe_target,
        })
    }

    pub(crate) fn base_url(&self) -> &Url {
        &self.base_url
    }

    pub(crate) fn join(&self, path: &str) -> Result<Url, ProviderEndpointError> {
        self.base_url
            .join(path)
            .map_err(|error| ProviderEndpointError::InvalidUrl(error.to_string()))
    }

    pub(crate) fn set_connect_timeout(&mut self, value: Duration) {
        self.client_connect_timeout = value;
    }

    pub(crate) async fn pinned_client(
        &self,
        preparation_timeout: Duration,
    ) -> Result<Client, ProviderEndpointError> {
        self.client_pool
            .client(
                &self.base_url,
                preparation_timeout,
                PinnedClientConfig {
                    connect_timeout: self.client_connect_timeout,
                    pool_idle_timeout: Some(POOL_IDLE_TIMEOUT),
                    pool_max_idle_per_host: Some(MAX_IDLE_CONNECTIONS_PER_HOST),
                    allow_unsafe_target: self.allow_unsafe_target,
                    user_agent: USER_AGENT,
                },
            )
            .await
            .map_err(ProviderEndpointError::from)
    }

    pub(crate) fn fmt_redacted(
        &self,
        name: &str,
        formatter: &mut fmt::Formatter<'_>,
    ) -> fmt::Result {
        formatter
            .debug_struct(name)
            .field("scheme", &self.base_url.scheme())
            .field("host", &self.base_url.host_str())
            .field("port", &self.base_url.port())
            .field("path", &"[REDACTED]")
            .finish_non_exhaustive()
    }
}

impl From<PinnedClientError> for ProviderEndpointError {
    fn from(error: PinnedClientError) -> Self {
        match error {
            PinnedClientError::MissingHost => Self::MissingHost,
            PinnedClientError::MissingPort => Self::MissingPort,
            PinnedClientError::DnsTimeout => Self::DnsTimeout,
            PinnedClientError::DnsResolution(error) => Self::DnsResolution(error),
            PinnedClientError::NoAddresses => Self::NoAddresses,
            PinnedClientError::ForbiddenAddress(address) => Self::ForbiddenAddress(address),
            PinnedClientError::ClientBuild(error) => Self::ClientBuild(error),
        }
    }
}

#[derive(Debug, Error)]
pub(crate) enum ProviderEndpointError {
    #[error("custom provider endpoints must use HTTPS")]
    HttpsRequired,
    #[error("custom provider endpoint scheme must be HTTP or HTTPS")]
    UnsupportedScheme,
    #[error("custom provider endpoints cannot contain user information")]
    UserInfoForbidden,
    #[error("custom provider endpoint must include a host")]
    MissingHost,
    #[error("custom provider endpoint must have a known or explicit port")]
    MissingPort,
    #[error("custom provider endpoint port must be greater than zero")]
    InvalidPort,
    #[error("custom provider endpoints cannot contain a query or fragment")]
    QueryOrFragmentForbidden,
    #[error("custom provider endpoint URL is invalid: {0}")]
    InvalidUrl(String),
    #[error("custom provider endpoint resolves to forbidden address {0}")]
    ForbiddenAddress(IpAddr),
    #[error("custom provider endpoint DNS resolution timed out")]
    DnsTimeout,
    #[error("custom provider endpoint DNS resolution failed")]
    DnsResolution(#[source] std::io::Error),
    #[error("custom provider endpoint did not resolve to an address")]
    NoAddresses,
    #[error("failed to build the pinned provider HTTP client")]
    ClientBuild(#[source] reqwest::Error),
}

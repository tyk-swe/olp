use std::{fmt, net::IpAddr, time::Duration};

use crate::http_egress::{
    is_public_ip,
    pinned::{PinnedClientConfig, PinnedClientError, PinnedClientPool, literal_ip},
};
use reqwest::{Client, Url};
use thiserror::Error;

const DEFAULT_BASE_URL: &str = "https://api.anthropic.com/v1/";
const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_secs(30);
const MAX_IDLE_CONNECTIONS_PER_HOST: usize = 256;

pub(crate) struct Endpoint {
    base_url: Url,
    client_connect_timeout: Duration,
    client_pool: PinnedClientPool,
    #[cfg(test)]
    allow_unsafe_test_target: bool,
}

impl Clone for Endpoint {
    fn clone(&self) -> Self {
        Self {
            base_url: self.base_url.clone(),
            client_connect_timeout: self.client_connect_timeout,
            client_pool: self.client_pool.clone(),
            #[cfg(test)]
            allow_unsafe_test_target: self.allow_unsafe_test_target,
        }
    }
}

impl fmt::Debug for Endpoint {
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

impl Default for Endpoint {
    fn default() -> Self {
        Self::parse(DEFAULT_BASE_URL).expect("the built-in Anthropic endpoint is valid")
    }
}

impl Endpoint {
    pub(crate) fn parse(value: &str) -> Result<Self, EndpointError> {
        Self::parse_with_policy(value, false)
    }

    fn parse_with_policy(value: &str, allow_unsafe_target: bool) -> Result<Self, EndpointError> {
        let mut base_url =
            Url::parse(value).map_err(|error| EndpointError::InvalidUrl(error.to_string()))?;
        if base_url.scheme() != "https" && !allow_unsafe_target {
            return Err(EndpointError::HttpsRequired);
        }
        if !matches!(base_url.scheme(), "http" | "https") {
            return Err(EndpointError::UnsupportedScheme);
        }
        if !base_url.username().is_empty() || base_url.password().is_some() {
            return Err(EndpointError::UserInfoForbidden);
        }
        if base_url.host().is_none() {
            return Err(EndpointError::MissingHost);
        }
        if base_url.port() == Some(0) {
            return Err(EndpointError::InvalidPort);
        }
        if base_url.query().is_some() || base_url.fragment().is_some() {
            return Err(EndpointError::QueryOrFragmentForbidden);
        }
        if !base_url.path().ends_with('/') {
            let normalized = format!("{}/", base_url.path());
            base_url.set_path(&normalized);
        }
        if let Some(ip) = literal_ip(&base_url)
            && !allow_unsafe_target
            && !is_public_ip(ip)
        {
            return Err(EndpointError::ForbiddenAddress(ip));
        }
        Ok(Self {
            base_url,
            client_connect_timeout: DEFAULT_CONNECT_TIMEOUT,
            client_pool: PinnedClientPool::default(),
            #[cfg(test)]
            allow_unsafe_test_target: allow_unsafe_target,
        })
    }

    #[cfg(test)]
    pub(crate) fn for_local_test(value: &str) -> Self {
        Self::parse_with_policy(value, true).expect("local test endpoint must be valid")
    }

    pub(crate) fn messages_url(&self) -> Result<Url, EndpointError> {
        self.join("messages")
    }

    pub(crate) fn count_tokens_url(&self) -> Result<Url, EndpointError> {
        self.join("messages/count_tokens")
    }

    pub(crate) fn models_url(&self) -> Result<Url, EndpointError> {
        self.join("models")
    }

    fn join(&self, path: &str) -> Result<Url, EndpointError> {
        self.base_url
            .join(path)
            .map_err(|error| EndpointError::InvalidUrl(error.to_string()))
    }

    pub(crate) fn set_connect_timeout(&mut self, value: Duration) {
        self.client_connect_timeout = value;
    }

    pub(crate) async fn pinned_client(
        &self,
        connect_timeout: Duration,
    ) -> Result<Client, EndpointError> {
        #[cfg(test)]
        let allow_unsafe_target = self.allow_unsafe_test_target;
        #[cfg(not(test))]
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
            .map_err(EndpointError::from)
    }
}

impl From<PinnedClientError> for EndpointError {
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
pub enum EndpointError {
    #[error("custom Anthropic endpoints must use HTTPS")]
    HttpsRequired,
    #[error("custom Anthropic endpoint scheme must be HTTP or HTTPS")]
    UnsupportedScheme,
    #[error("custom Anthropic endpoints cannot contain user information")]
    UserInfoForbidden,
    #[error("custom Anthropic endpoint must include a host")]
    MissingHost,
    #[error("custom Anthropic endpoint must have a known or explicit port")]
    MissingPort,
    #[error("custom Anthropic endpoint port must be greater than zero")]
    InvalidPort,
    #[error("custom Anthropic endpoints cannot contain a query or fragment")]
    QueryOrFragmentForbidden,
    #[error("custom Anthropic endpoint URL is invalid: {0}")]
    InvalidUrl(String),
    #[error("custom Anthropic endpoint resolves to forbidden address {0}")]
    ForbiddenAddress(IpAddr),
    #[error("custom Anthropic endpoint DNS resolution timed out")]
    DnsTimeout,
    #[error("custom Anthropic endpoint DNS resolution failed")]
    DnsResolution(#[source] std::io::Error),
    #[error("custom Anthropic endpoint did not resolve to an address")]
    NoAddresses,
    #[error("failed to build the pinned Anthropic HTTP client")]
    ClientBuild(#[source] reqwest::Error),
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn endpoint_policy_and_path_join_are_fail_closed() {
        assert!(matches!(
            Endpoint::parse("http://api.anthropic.com/v1"),
            Err(EndpointError::HttpsRequired)
        ));
        assert!(matches!(
            Endpoint::parse("https://user:secret@api.anthropic.com/v1"),
            Err(EndpointError::UserInfoForbidden)
        ));
        assert!(matches!(
            Endpoint::parse("https://api.anthropic.com/v1?next=x"),
            Err(EndpointError::QueryOrFragmentForbidden)
        ));
        let endpoint = Endpoint::parse("https://example.com/proxy/v1").unwrap();
        assert_eq!(
            endpoint.count_tokens_url().unwrap().as_str(),
            "https://example.com/proxy/v1/messages/count_tokens"
        );
    }

    #[test]
    fn endpoint_debug_redacts_sensitive_path() {
        let endpoint = Endpoint::parse("https://example.com/private-token/v1").unwrap();
        let debug = format!("{endpoint:?}");
        assert!(!debug.contains("private-token"));
        assert!(debug.contains("REDACTED"));
    }

    #[test]
    fn literal_private_target_preserves_anthropic_error_mapping() {
        let address: IpAddr = "169.254.169.254".parse().unwrap();
        assert!(matches!(
            Endpoint::parse("https://169.254.169.254/v1"),
            Err(EndpointError::ForbiddenAddress(blocked)) if blocked == address
        ));
    }
}

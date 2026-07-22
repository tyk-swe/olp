use std::{fmt, net::IpAddr, time::Duration};

use reqwest::{Client, Url};
use thiserror::Error;

use crate::provider_endpoint::{ProviderEndpoint, ProviderEndpointError};

const DEFAULT_BASE_URL: &str = "https://api.anthropic.com/v1/";

#[derive(Clone)]
pub(crate) struct Endpoint {
    inner: ProviderEndpoint,
}

impl fmt::Debug for Endpoint {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.inner.fmt_redacted("Endpoint", formatter)
    }
}

impl Default for Endpoint {
    fn default() -> Self {
        Self::parse(DEFAULT_BASE_URL).expect("the built-in Anthropic endpoint is valid")
    }
}

impl Endpoint {
    pub(crate) fn parse(value: &str) -> Result<Self, EndpointError> {
        Ok(Self {
            inner: ProviderEndpoint::parse(value)?,
        })
    }

    #[cfg(test)]
    pub(crate) fn for_local_test(value: &str) -> Self {
        Self {
            inner: ProviderEndpoint::for_local_test(value)
                .expect("local test endpoint must be valid"),
        }
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
        self.inner.join(path).map_err(EndpointError::from)
    }

    pub(crate) fn set_connect_timeout(&mut self, value: Duration) {
        self.inner.set_connect_timeout(value);
    }

    pub(crate) async fn pinned_client(
        &self,
        connect_timeout: Duration,
    ) -> Result<Client, EndpointError> {
        self.inner
            .pinned_client(connect_timeout)
            .await
            .map_err(EndpointError::from)
    }
}

impl From<ProviderEndpointError> for EndpointError {
    fn from(error: ProviderEndpointError) -> Self {
        match error {
            ProviderEndpointError::HttpsRequired => Self::HttpsRequired,
            ProviderEndpointError::UnsupportedScheme => Self::UnsupportedScheme,
            ProviderEndpointError::UserInfoForbidden => Self::UserInfoForbidden,
            ProviderEndpointError::MissingHost => Self::MissingHost,
            ProviderEndpointError::MissingPort => Self::MissingPort,
            ProviderEndpointError::InvalidPort => Self::InvalidPort,
            ProviderEndpointError::QueryOrFragmentForbidden => Self::QueryOrFragmentForbidden,
            ProviderEndpointError::InvalidUrl(error) => Self::InvalidUrl(error),
            ProviderEndpointError::ForbiddenAddress(address) => Self::ForbiddenAddress(address),
            ProviderEndpointError::DnsTimeout => Self::DnsTimeout,
            ProviderEndpointError::DnsResolution(error) => Self::DnsResolution(error),
            ProviderEndpointError::NoAddresses => Self::NoAddresses,
            ProviderEndpointError::ClientBuild(error) => Self::ClientBuild(error),
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
            Endpoint::parse("file:///tmp/provider"),
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

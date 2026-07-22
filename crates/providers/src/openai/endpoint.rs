use std::{fmt, net::IpAddr, time::Duration};

use reqwest::{Client, Url};
use thiserror::Error;

use crate::provider_endpoint::{ProviderEndpoint, ProviderEndpointError};

const DEFAULT_OPENAI_BASE_URL: &str = "https://api.openai.com/v1/";

#[derive(Clone)]
pub(crate) struct Endpoint {
    inner: ProviderEndpoint,
    fixed_query: Option<(String, String)>,
}

impl fmt::Debug for Endpoint {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.inner.fmt_redacted("Endpoint", formatter)
    }
}

impl Default for Endpoint {
    fn default() -> Self {
        Self::parse(DEFAULT_OPENAI_BASE_URL).expect("the built-in OpenAI endpoint is valid")
    }
}

impl Endpoint {
    pub(crate) fn parse(value: &str) -> Result<Self, EndpointError> {
        Ok(Self {
            inner: ProviderEndpoint::parse(value)?,
            fixed_query: None,
        })
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(crate) fn for_local_test(value: &str) -> Self {
        Self {
            inner: ProviderEndpoint::for_local_test(value)
                .expect("local test endpoint must be a valid HTTP URL"),
            fixed_query: None,
        }
    }

    pub(crate) fn resource_url(&self, path: &str) -> Result<Url, EndpointError> {
        if path.is_empty()
            || path.starts_with('/')
            || path.contains(['\\', '?', '#'])
            || path.chars().any(char::is_control)
        {
            return Err(EndpointError::InvalidResourcePath);
        }
        let base_url = self.inner.base_url();
        let mut url = self.inner.join(path).map_err(EndpointError::from)?;
        if url.origin() != base_url.origin() || !url.path().starts_with(base_url.path()) {
            return Err(EndpointError::InvalidResourcePath);
        }
        if let Some((name, value)) = &self.fixed_query {
            url.query_pairs_mut().append_pair(name, value);
        }
        Ok(url)
    }

    pub(crate) fn set_api_version(&mut self, value: &str) -> Result<(), EndpointError> {
        if value.is_empty()
            || value.len() > 128
            || !value
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
        {
            return Err(EndpointError::InvalidApiVersion);
        }
        self.fixed_query = Some(("api-version".into(), value.into()));
        Ok(())
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
    #[error("custom OpenAI endpoints must use HTTPS")]
    HttpsRequired,
    #[error("custom OpenAI endpoint scheme must be HTTP or HTTPS")]
    UnsupportedScheme,
    #[error("custom OpenAI endpoints cannot contain user information")]
    UserInfoForbidden,
    #[error("custom OpenAI endpoint must include a host")]
    MissingHost,
    #[error("custom OpenAI endpoint must have a known or explicit port")]
    MissingPort,
    #[error("custom OpenAI endpoint port must be greater than zero")]
    InvalidPort,
    #[error("custom OpenAI endpoints cannot contain a query or fragment")]
    QueryOrFragmentForbidden,
    #[error("custom OpenAI endpoint URL is invalid: {0}")]
    InvalidUrl(String),
    #[error("OpenAI resource path is invalid")]
    InvalidResourcePath,
    #[error("OpenAI API version is invalid")]
    InvalidApiVersion,
    #[error("custom OpenAI endpoint resolves to forbidden address {0}")]
    ForbiddenAddress(IpAddr),
    #[error("custom OpenAI endpoint DNS resolution timed out")]
    DnsTimeout,
    #[error("custom OpenAI endpoint DNS resolution failed")]
    DnsResolution(#[source] std::io::Error),
    #[error("custom OpenAI endpoint did not resolve to an address")]
    NoAddresses,
    #[error("failed to build the pinned OpenAI HTTP client")]
    ClientBuild(#[source] reqwest::Error),
}

#[cfg(test)]
mod tests {
    use std::net::{Ipv4Addr, Ipv6Addr};

    use super::*;

    #[test]
    fn endpoint_requires_https_and_forbids_ambient_authority() {
        assert!(matches!(
            Endpoint::parse("http://api.openai.com/v1"),
            Err(EndpointError::HttpsRequired)
        ));
        assert!(matches!(
            Endpoint::parse("file:///tmp/openai"),
            Err(EndpointError::HttpsRequired)
        ));
        assert!(matches!(
            Endpoint::parse("https://user:secret@api.openai.com/v1"),
            Err(EndpointError::UserInfoForbidden)
        ));
        assert!(matches!(
            Endpoint::parse("https://api.openai.com/v1?redirect=1"),
            Err(EndpointError::QueryOrFragmentForbidden)
        ));
    }

    #[test]
    fn endpoint_join_preserves_the_configured_base_path() {
        let endpoint = Endpoint::parse("https://example.com/proxy/v1").unwrap();
        assert_eq!(
            endpoint.resource_url("chat/completions").unwrap().as_str(),
            "https://example.com/proxy/v1/chat/completions"
        );
    }

    #[test]
    fn resource_paths_cannot_escape_the_configured_origin() {
        let endpoint = Endpoint::parse("https://example.com/proxy/v1").unwrap();

        for path in [
            r"\\attacker.example/v1",
            r"videos\job-id",
            "%2e%2e/credentials",
            "../credentials",
            "/absolute",
            "models?key=ambient",
        ] {
            assert!(matches!(
                endpoint.resource_url(path),
                Err(EndpointError::InvalidResourcePath)
            ));
        }
    }

    #[test]
    fn endpoint_debug_redacts_path_embedded_credentials() {
        let endpoint = Endpoint::parse("https://example.com/private-token/v1").unwrap();
        let debug = format!("{endpoint:?}");

        assert!(!debug.contains("private-token"));
        assert!(debug.contains("[REDACTED]"));
    }

    #[test]
    fn literal_private_targets_are_rejected_before_dns() {
        assert!(matches!(
            Endpoint::parse("https://169.254.169.254/latest/meta-data"),
            Err(EndpointError::ForbiddenAddress(address))
                if address == IpAddr::V4(Ipv4Addr::new(169, 254, 169, 254))
        ));
        assert!(matches!(
            Endpoint::parse("https://[::1]/v1"),
            Err(EndpointError::ForbiddenAddress(address))
                if address == IpAddr::V6(Ipv6Addr::LOCALHOST)
        ));
    }
}

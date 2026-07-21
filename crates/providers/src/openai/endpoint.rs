use std::{fmt, net::IpAddr, time::Duration};

use crate::http_egress::{
    is_public_ip,
    pinned::{PinnedClientConfig, PinnedClientError, PinnedClientPool, literal_ip},
};
use reqwest::{Client, Url};
use thiserror::Error;

const DEFAULT_OPENAI_BASE_URL: &str = "https://api.openai.com/v1/";
const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_secs(30);
const MAX_IDLE_CONNECTIONS_PER_HOST: usize = 256;

pub(crate) struct Endpoint {
    base_url: Url,
    fixed_query: Option<(String, String)>,
    client_connect_timeout: Duration,
    client_pool: PinnedClientPool,
    #[cfg(any(test, feature = "test-util"))]
    allow_unsafe_test_target: bool,
}

// Cloning configuration must not accidentally make independently configured
// providers share a transport pool. A connector reuses its own Endpoint, while
// a cloned ConnectorConfig receives a new, credential-neutral pool.
impl Clone for Endpoint {
    fn clone(&self) -> Self {
        Self {
            base_url: self.base_url.clone(),
            fixed_query: self.fixed_query.clone(),
            client_connect_timeout: self.client_connect_timeout,
            client_pool: self.client_pool.clone(),
            #[cfg(any(test, feature = "test-util"))]
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
        Self::parse(DEFAULT_OPENAI_BASE_URL).expect("the built-in OpenAI endpoint is valid")
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
            let normalized_path = format!("{}/", base_url.path());
            base_url.set_path(&normalized_path);
        }

        if let Some(ip) = literal_ip(&base_url)
            && !allow_unsafe_target
            && !is_public_ip(ip)
        {
            return Err(EndpointError::ForbiddenAddress(ip));
        }

        Ok(Self {
            base_url,
            fixed_query: None,
            client_connect_timeout: DEFAULT_CONNECT_TIMEOUT,
            client_pool: PinnedClientPool::default(),
            #[cfg(any(test, feature = "test-util"))]
            allow_unsafe_test_target: allow_unsafe_target,
        })
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(crate) fn for_local_test(value: &str) -> Self {
        Self::parse_with_policy(value, true).expect("local test endpoint must be a valid HTTP URL")
    }

    pub(crate) fn resource_url(&self, path: &str) -> Result<Url, EndpointError> {
        if path.starts_with('/') || path.contains("..") || path.contains(['\\', '?', '#']) {
            return Err(EndpointError::InvalidResourcePath);
        }
        let mut url = self
            .base_url
            .join(path)
            .map_err(|error| EndpointError::InvalidUrl(error.to_string()))?;
        if url.origin() != self.base_url.origin() || !url.path().starts_with(self.base_url.path()) {
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
        self.client_connect_timeout = value;
    }

    pub(crate) async fn pinned_client(
        &self,
        connect_timeout: Duration,
    ) -> Result<Client, EndpointError> {
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
    fn resource_paths_cannot_escape_the_configured_origin_with_backslashes() {
        let endpoint = Endpoint::parse("https://example.com/proxy/v1").unwrap();

        assert!(matches!(
            endpoint.resource_url(r"\\attacker.example/v1"),
            Err(EndpointError::InvalidResourcePath)
        ));
        assert!(matches!(
            endpoint.resource_url(r"videos\job-id"),
            Err(EndpointError::InvalidResourcePath)
        ));
        assert!(matches!(
            endpoint.resource_url("%2e%2e/credentials"),
            Err(EndpointError::InvalidResourcePath)
        ));
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
            Err(EndpointError::ForbiddenAddress(address)) if address == IpAddr::V6(Ipv6Addr::LOCALHOST)
        ));
    }
}

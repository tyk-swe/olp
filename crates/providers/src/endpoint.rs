use std::{fmt, net::IpAddr, time::Duration};

use reqwest::{Client, Url};

use crate::http_egress::{
    is_public_ip,
    pinned::{PinnedClientConfig, PinnedClientError, PinnedClientPool, literal_ip},
};

const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_secs(30);
const MAX_IDLE_CONNECTIONS_PER_HOST: usize = 256;

#[derive(Clone)]
pub(crate) struct EndpointCore {
    base_url: Url,
    client_connect_timeout: Duration,
    client_pool: PinnedClientPool,
    #[cfg(any(test, feature = "test-util"))]
    allow_unsafe_test_target: bool,
}

impl EndpointCore {
    pub(crate) fn parse(value: &str, allow_unsafe_target: bool) -> Result<Self, EndpointCoreError> {
        let mut base_url =
            Url::parse(value).map_err(|error| EndpointCoreError::InvalidUrl(error.to_string()))?;
        if base_url.scheme() != "https" && !allow_unsafe_target {
            return Err(EndpointCoreError::HttpsRequired);
        }
        if !matches!(base_url.scheme(), "http" | "https") {
            return Err(EndpointCoreError::UnsupportedScheme);
        }
        if !base_url.username().is_empty() || base_url.password().is_some() {
            return Err(EndpointCoreError::UserInfoForbidden);
        }
        if base_url.host().is_none() {
            return Err(EndpointCoreError::MissingHost);
        }
        if base_url.port() == Some(0) {
            return Err(EndpointCoreError::InvalidPort);
        }
        if base_url.query().is_some() || base_url.fragment().is_some() {
            return Err(EndpointCoreError::QueryOrFragmentForbidden);
        }
        if !base_url.path().ends_with('/') {
            base_url.set_path(&format!("{}/", base_url.path()));
        }
        if let Some(address) = literal_ip(&base_url)
            && !allow_unsafe_target
            && !is_public_ip(address)
        {
            return Err(EndpointCoreError::ForbiddenAddress(address));
        }
        Ok(Self {
            base_url,
            client_connect_timeout: DEFAULT_CONNECT_TIMEOUT,
            client_pool: PinnedClientPool::default(),
            #[cfg(any(test, feature = "test-util"))]
            allow_unsafe_test_target: allow_unsafe_target,
        })
    }

    pub(crate) fn url(&self) -> &Url {
        &self.base_url
    }

    pub(crate) fn join(&self, path: &str) -> Result<Url, EndpointCoreError> {
        self.base_url
            .join(path)
            .map_err(|error| EndpointCoreError::InvalidUrl(error.to_string()))
    }

    pub(crate) fn set_connect_timeout(&mut self, value: Duration) {
        self.client_connect_timeout = value;
    }

    pub(crate) async fn pinned_client(
        &self,
        connect_timeout: Duration,
    ) -> Result<Client, EndpointCoreError> {
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
            .map_err(EndpointCoreError::from)
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

#[derive(Debug)]
pub(crate) enum EndpointCoreError {
    HttpsRequired,
    UnsupportedScheme,
    UserInfoForbidden,
    MissingHost,
    MissingPort,
    InvalidPort,
    QueryOrFragmentForbidden,
    InvalidUrl(String),
    ForbiddenAddress(IpAddr),
    DnsTimeout,
    DnsResolution(std::io::Error),
    NoAddresses,
    ClientBuild(reqwest::Error),
}

impl From<PinnedClientError> for EndpointCoreError {
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

macro_rules! impl_endpoint_core_error {
    ($target:ident) => {
        impl From<$crate::endpoint::EndpointCoreError> for $target {
            fn from(error: $crate::endpoint::EndpointCoreError) -> Self {
                use $crate::endpoint::EndpointCoreError;

                match error {
                    EndpointCoreError::HttpsRequired => Self::HttpsRequired,
                    EndpointCoreError::UnsupportedScheme => Self::UnsupportedScheme,
                    EndpointCoreError::UserInfoForbidden => Self::UserInfoForbidden,
                    EndpointCoreError::MissingHost => Self::MissingHost,
                    EndpointCoreError::MissingPort => Self::MissingPort,
                    EndpointCoreError::InvalidPort => Self::InvalidPort,
                    EndpointCoreError::QueryOrFragmentForbidden => Self::QueryOrFragmentForbidden,
                    EndpointCoreError::InvalidUrl(error) => Self::InvalidUrl(error),
                    EndpointCoreError::ForbiddenAddress(address) => Self::ForbiddenAddress(address),
                    EndpointCoreError::DnsTimeout => Self::DnsTimeout,
                    EndpointCoreError::DnsResolution(error) => Self::DnsResolution(error),
                    EndpointCoreError::NoAddresses => Self::NoAddresses,
                    EndpointCoreError::ClientBuild(error) => Self::ClientBuild(error),
                }
            }
        }
    };
}

pub(crate) use impl_endpoint_core_error;

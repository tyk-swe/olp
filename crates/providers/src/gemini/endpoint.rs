use std::{fmt, net::IpAddr, time::Duration};

use reqwest::{Client, Url};
use thiserror::Error;

use crate::provider_endpoint::{ProviderEndpoint, ProviderEndpointError};

const DEFAULT_BASE_URL: &str = "https://generativelanguage.googleapis.com/v1beta/";

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
        Self::parse(DEFAULT_BASE_URL).expect("the built-in Gemini endpoint is valid")
    }
}

impl Endpoint {
    pub(crate) fn parse(value: &str) -> Result<Self, EndpointError> {
        Ok(Self {
            inner: ProviderEndpoint::parse(value)?,
        })
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(crate) fn for_local_test(value: &str) -> Self {
        Self {
            inner: ProviderEndpoint::for_local_test(value)
                .expect("local test endpoint must be valid"),
        }
    }

    pub(crate) fn generate_url(
        &self,
        upstream_model: &str,
        streaming: bool,
    ) -> Result<Url, EndpointError> {
        let action = if streaming {
            "streamGenerateContent"
        } else {
            "generateContent"
        };
        let mut url = self.model_action_url(upstream_model, action)?;
        if streaming {
            url.query_pairs_mut().append_pair("alt", "sse");
        }
        Ok(url)
    }

    pub(crate) fn count_tokens_url(&self, upstream_model: &str) -> Result<Url, EndpointError> {
        self.model_action_url(upstream_model, "countTokens")
    }

    pub(crate) fn models_url(&self) -> Result<Url, EndpointError> {
        self.inner.join("models").map_err(EndpointError::from)
    }

    pub(crate) fn set_connect_timeout(&mut self, value: Duration) {
        self.inner.set_connect_timeout(value);
    }

    fn model_action_url(&self, upstream_model: &str, action: &str) -> Result<Url, EndpointError> {
        let model = upstream_model
            .strip_prefix("models/")
            .unwrap_or(upstream_model);
        let segments = model.split('/').collect::<Vec<_>>();
        if segments.is_empty()
            || segments
                .iter()
                .any(|segment| segment.is_empty() || matches!(*segment, "." | ".."))
            || upstream_model.chars().any(char::is_control)
        {
            return Err(EndpointError::InvalidModelName);
        }
        let mut url = self.inner.base_url().clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|()| EndpointError::CannotBeBase)?;
            path.pop_if_empty().push("models");
            for (index, segment) in segments.iter().enumerate() {
                if index + 1 == segments.len() {
                    path.push(&format!("{segment}:{action}"));
                } else {
                    path.push(segment);
                }
            }
        }
        Ok(url)
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
    #[error("custom Gemini endpoints must use HTTPS")]
    HttpsRequired,
    #[error("custom Gemini endpoint scheme must be HTTP or HTTPS")]
    UnsupportedScheme,
    #[error("custom Gemini endpoints cannot contain user information")]
    UserInfoForbidden,
    #[error("custom Gemini endpoint must include a host")]
    MissingHost,
    #[error("custom Gemini endpoint must have a known or explicit port")]
    MissingPort,
    #[error("custom Gemini endpoint port must be greater than zero")]
    InvalidPort,
    #[error("custom Gemini endpoints cannot contain a query or fragment")]
    QueryOrFragmentForbidden,
    #[error("custom Gemini endpoint URL is invalid: {0}")]
    InvalidUrl(String),
    #[error("Gemini provider model name is invalid")]
    InvalidModelName,
    #[error("custom Gemini endpoint cannot be used as a URL base")]
    CannotBeBase,
    #[error("custom Gemini endpoint resolves to forbidden address {0}")]
    ForbiddenAddress(IpAddr),
    #[error("custom Gemini endpoint DNS resolution timed out")]
    DnsTimeout,
    #[error("custom Gemini endpoint DNS resolution failed")]
    DnsResolution(#[source] std::io::Error),
    #[error("custom Gemini endpoint did not resolve to an address")]
    NoAddresses,
    #[error("failed to build the pinned Gemini HTTP client")]
    ClientBuild(#[source] reqwest::Error),
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn endpoint_policy_and_action_paths_are_safe() {
        assert!(matches!(
            Endpoint::parse("http://generativelanguage.googleapis.com/v1beta"),
            Err(EndpointError::HttpsRequired)
        ));
        assert!(matches!(
            Endpoint::parse("ftp://googleapis.com/v1beta"),
            Err(EndpointError::HttpsRequired)
        ));
        assert!(matches!(
            Endpoint::parse("https://key@googleapis.com/v1beta"),
            Err(EndpointError::UserInfoForbidden)
        ));
        assert!(matches!(
            Endpoint::parse("https://googleapis.com/v1beta?key=ambient"),
            Err(EndpointError::QueryOrFragmentForbidden)
        ));
        let endpoint = Endpoint::parse("https://example.com/proxy/v1beta").unwrap();
        assert_eq!(
            endpoint
                .generate_url("models/gemini-2.5-flash", true)
                .unwrap()
                .as_str(),
            "https://example.com/proxy/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse"
        );
        assert_eq!(
            endpoint
                .count_tokens_url("publishers/google/gemini-pro")
                .unwrap()
                .as_str(),
            "https://example.com/proxy/v1beta/models/publishers/google/gemini-pro:countTokens"
        );
        assert!(matches!(
            endpoint.generate_url("../metadata", false),
            Err(EndpointError::InvalidModelName)
        ));
    }

    #[test]
    fn endpoint_debug_redacts_path_and_preserves_private_target_error_mapping() {
        let endpoint = Endpoint::parse("https://example.com/private-token/v1beta").unwrap();
        let debug = format!("{endpoint:?}");
        assert!(!debug.contains("private-token"));
        assert!(debug.contains("REDACTED"));
        let address: IpAddr = "127.0.0.1".parse().unwrap();
        assert!(matches!(
            Endpoint::parse("https://127.0.0.1/v1beta"),
            Err(EndpointError::ForbiddenAddress(blocked)) if blocked == address
        ));
    }
}

use std::{fmt, time::Duration};

use reqwest::{Client, Url};
use thiserror::Error;

use crate::providers::{
    endpoint::{EndpointCore, Error as CommonEndpointError},
    http_egress::EgressPolicy,
};

const DEFAULT_OPENAI_BASE_URL: &str = "https://api.openai.com/v1/";
const PROVIDER: &str = "OpenAI";

#[derive(Debug, Error)]
pub enum Error {
    #[error(transparent)]
    Common(#[from] CommonEndpointError),
    #[error("OpenAI resource path is invalid")]
    InvalidResourcePath,
    #[error("OpenAI API version is invalid")]
    InvalidApiVersion,
}

#[derive(Clone)]
pub(in crate::providers) struct Endpoint {
    core: EndpointCore,
    fixed_query: Option<(String, String)>,
}

impl fmt::Debug for Endpoint {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.core.fmt(formatter)
    }
}

impl Default for Endpoint {
    fn default() -> Self {
        Self::parse(DEFAULT_OPENAI_BASE_URL).expect("the built-in OpenAI endpoint is valid")
    }
}

impl Endpoint {
    pub(in crate::providers) fn parse(value: &str) -> Result<Self, Error> {
        Self::parse_with_policy(value, &EgressPolicy::default())
    }

    pub(in crate::providers) fn parse_with_policy(
        value: &str,
        policy: &EgressPolicy,
    ) -> Result<Self, Error> {
        Ok(Self {
            core: EndpointCore::parse(value, PROVIDER, policy)?,
            fixed_query: None,
        })
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(in crate::providers) fn for_local_test(value: &str) -> Self {
        Self::parse_with_policy(value, &EgressPolicy::unsafe_test_targets())
            .expect("local test endpoint must be a valid HTTP URL")
    }

    pub(in crate::providers) fn resource_url(&self, path: &str) -> Result<Url, Error> {
        if path.starts_with('/') || path.contains("..") || path.contains(['\\', '?', '#']) {
            return Err(Error::InvalidResourcePath);
        }
        let mut url = self.core.join(path)?;
        if url.origin() != self.core.url().origin()
            || !url.path().starts_with(self.core.url().path())
        {
            return Err(Error::InvalidResourcePath);
        }
        if let Some((name, value)) = &self.fixed_query {
            url.query_pairs_mut().append_pair(name, value);
        }
        Ok(url)
    }

    pub(in crate::providers) fn set_api_version(&mut self, value: &str) -> Result<(), Error> {
        if value.is_empty()
            || value.len() > 128
            || !value
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
        {
            return Err(Error::InvalidApiVersion);
        }
        self.fixed_query = Some(("api-version".into(), value.into()));
        Ok(())
    }

    pub(in crate::providers) fn set_connect_timeout(&mut self, value: Duration) {
        self.core.set_connect_timeout(value);
    }

    pub(in crate::providers) async fn pinned_client(
        &self,
        connect_timeout: Duration,
    ) -> Result<Client, Error> {
        self.core
            .pinned_client(connect_timeout)
            .await
            .map_err(Into::into)
    }
}

#[cfg(test)]
mod tests {
    use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};

    use super::*;

    #[test]
    fn endpoint_requires_https_and_forbids_ambient_authority() {
        assert!(matches!(
            Endpoint::parse("http://api.openai.com/v1"),
            Err(Error::Common(CommonEndpointError::HttpsRequired { .. }))
        ));
        assert!(matches!(
            Endpoint::parse("https://user:secret@api.openai.com/v1"),
            Err(Error::Common(CommonEndpointError::UserInfoForbidden { .. }))
        ));
        assert!(matches!(
            Endpoint::parse("https://api.openai.com/v1?redirect=1"),
            Err(Error::Common(
                CommonEndpointError::QueryOrFragmentForbidden { .. }
            ))
        ));
    }

    #[test]
    fn plain_http_is_accepted_only_for_allowlisted_hosts() {
        let policy = EgressPolicy::new(vec![], vec!["vllm.internal".to_owned()]);
        Endpoint::parse_with_policy("http://vllm.internal:8000/v1", &policy).unwrap();
        assert!(matches!(
            Endpoint::parse_with_policy("http://other.internal:8000/v1", &policy),
            Err(Error::Common(CommonEndpointError::HttpsRequired { .. }))
        ));
        assert!(matches!(
            Endpoint::parse_with_policy("ftp://vllm.internal/v1", &policy),
            Err(Error::Common(CommonEndpointError::UnsupportedScheme { .. }))
        ));
    }

    #[test]
    fn plain_http_private_literal_needs_both_allowlists() {
        let host_only = EgressPolicy::new(vec![], vec!["10.0.0.5".to_owned()]);
        assert!(matches!(
            Endpoint::parse_with_policy("http://10.0.0.5/v1", &host_only),
            Err(Error::Common(CommonEndpointError::ForbiddenAddress { .. }))
        ));
        let cidr_only = EgressPolicy::new(vec!["10.0.0.0/8".parse().unwrap()], vec![]);
        assert!(matches!(
            Endpoint::parse_with_policy("http://10.0.0.5/v1", &cidr_only),
            Err(Error::Common(CommonEndpointError::HttpsRequired { .. }))
        ));
        Endpoint::parse_with_policy("https://10.0.0.5/v1", &cidr_only).unwrap();
        let both = EgressPolicy::new(
            vec!["10.0.0.0/8".parse().unwrap()],
            vec!["10.0.0.5".to_owned()],
        );
        Endpoint::parse_with_policy("http://10.0.0.5/v1", &both).unwrap();
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
            Err(Error::InvalidResourcePath)
        ));
        assert!(matches!(
            endpoint.resource_url(r"videos\job-id"),
            Err(Error::InvalidResourcePath)
        ));
        assert!(matches!(
            endpoint.resource_url("%2e%2e/credentials"),
            Err(Error::InvalidResourcePath)
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
            Err(Error::Common(CommonEndpointError::ForbiddenAddress { address, .. }))
                if address == IpAddr::V4(Ipv4Addr::new(169, 254, 169, 254))
        ));
        assert!(matches!(
            Endpoint::parse("https://[::1]/v1"),
            Err(Error::Common(CommonEndpointError::ForbiddenAddress { address, .. }))
                if address == IpAddr::V6(Ipv6Addr::LOCALHOST)
        ));
    }
}

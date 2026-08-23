use std::ops::{Deref, DerefMut};

use reqwest::Url;
use thiserror::Error;

use crate::providers::endpoint::{EndpointCore, Error as CommonEndpointError};
use crate::providers::transport_common::EndpointFailure;

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

impl EndpointFailure for Error {
    fn is_dns_timeout(&self) -> bool {
        matches!(self, Self::Common(error) if error.is_dns_timeout())
    }
}

#[derive(Clone, Debug)]
pub(in crate::providers) struct Endpoint {
    core: EndpointCore,
    fixed_query: Option<(String, String)>,
}

impl Deref for Endpoint {
    type Target = EndpointCore;

    fn deref(&self) -> &EndpointCore {
        &self.core
    }
}

impl DerefMut for Endpoint {
    fn deref_mut(&mut self) -> &mut EndpointCore {
        &mut self.core
    }
}

impl Default for Endpoint {
    fn default() -> Self {
        Self::parse(DEFAULT_OPENAI_BASE_URL).expect("the built-in OpenAI endpoint is valid")
    }
}

impl Endpoint {
    pub(in crate::providers) fn parse(value: &str) -> Result<Self, Error> {
        Self::parse_with_policy(value, false)
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(in crate::providers) fn parse_with_unsafe_test_target(value: &str) -> Result<Self, Error> {
        Self::parse_with_policy(value, true)
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(in crate::providers) fn for_local_test(value: &str) -> Self {
        Self::parse_with_policy(value, true).expect("local test endpoint must be a valid HTTP URL")
    }

    fn parse_with_policy(value: &str, allow_unsafe_target: bool) -> Result<Self, Error> {
        Ok(Self {
            core: EndpointCore::parse(value, PROVIDER, allow_unsafe_target)?,
            fixed_query: None,
        })
    }

    pub(in crate::providers) fn resource_url(&self, path: &str) -> Result<Url, Error> {
        if path.starts_with('/') || path.contains("..") || path.contains(['\\', '?', '#']) {
            return Err(Error::InvalidResourcePath);
        }
        let mut url = self.join(path)?;
        if url.origin() != self.url().origin() || !url.path().starts_with(self.url().path()) {
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
}

#[cfg(test)]
mod tests {
    use super::*;

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
}

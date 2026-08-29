use std::{fmt, time::Duration};

use reqwest::{Client, Url};

use crate::providers::{
    EgressPolicy,
    endpoint::{EndpointCore, Error},
};

const DEFAULT_BASE_URL: &str = "https://api.anthropic.com/v1/";
const PROVIDER: &str = "Anthropic";

#[derive(Clone)]
pub(in crate::providers) struct Endpoint {
    core: EndpointCore,
}

impl fmt::Debug for Endpoint {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.core.fmt(formatter)
    }
}

impl Default for Endpoint {
    fn default() -> Self {
        Self::parse(DEFAULT_BASE_URL).expect("the built-in Anthropic endpoint is valid")
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
        })
    }

    #[cfg(test)]
    pub(in crate::providers) fn for_local_test(value: &str) -> Self {
        Self::parse_with_policy(value, &EgressPolicy::unsafe_test_targets())
            .expect("local test endpoint must be valid")
    }

    pub(in crate::providers) fn messages_url(&self) -> Result<Url, Error> {
        self.join("messages")
    }

    pub(in crate::providers) fn count_tokens_url(&self) -> Result<Url, Error> {
        self.join("messages/count_tokens")
    }

    pub(in crate::providers) fn models_url(&self) -> Result<Url, Error> {
        self.join("models")
    }

    fn join(&self, path: &str) -> Result<Url, Error> {
        self.core.join(path)
    }

    pub(in crate::providers) fn set_connect_timeout(&mut self, value: Duration) {
        self.core.set_connect_timeout(value);
    }

    pub(in crate::providers) async fn pinned_client(
        &self,
        connect_timeout: Duration,
    ) -> Result<Client, Error> {
        self.core.pinned_client(connect_timeout).await
    }
}

#[cfg(test)]
mod tests {
    use std::net::IpAddr;

    use super::*;

    #[test]
    fn endpoint_policy_and_path_join_are_fail_closed() {
        assert!(matches!(
            Endpoint::parse("http://api.anthropic.com/v1"),
            Err(Error::HttpsRequired { .. })
        ));
        assert!(matches!(
            Endpoint::parse("https://user:secret@api.anthropic.com/v1"),
            Err(Error::UserInfoForbidden { .. })
        ));
        assert!(matches!(
            Endpoint::parse("https://api.anthropic.com/v1?next=x"),
            Err(Error::QueryOrFragmentForbidden { .. })
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
            Err(Error::ForbiddenAddress { address: blocked, .. }) if blocked == address
        ));
    }
}

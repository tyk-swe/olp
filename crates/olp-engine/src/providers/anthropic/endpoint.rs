use std::ops::Deref;

use reqwest::Url;

use crate::providers::endpoint::{EndpointCore, Error};

const DEFAULT_BASE_URL: &str = "https://api.anthropic.com/v1/";
const PROVIDER: &str = "Anthropic";

#[derive(Clone, Debug)]
pub(in crate::providers) struct Endpoint(EndpointCore);

impl Deref for Endpoint {
    type Target = EndpointCore;

    fn deref(&self) -> &EndpointCore {
        &self.0
    }
}

impl Default for Endpoint {
    fn default() -> Self {
        Self::parse(DEFAULT_BASE_URL).expect("the built-in Anthropic endpoint is valid")
    }
}

impl Endpoint {
    pub(in crate::providers) fn parse(value: &str) -> Result<Self, Error> {
        EndpointCore::parse(value, PROVIDER, false).map(Self)
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(in crate::providers) fn for_local_test(
        value: &str,
        connect_timeout: std::time::Duration,
    ) -> Self {
        let mut core =
            EndpointCore::parse(value, PROVIDER, true).expect("local test endpoint must be valid");
        core.set_connect_timeout(connect_timeout);
        Self(core)
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

use std::ops::{Deref, DerefMut};

use reqwest::Url;
use thiserror::Error;

use crate::providers::endpoint::{EndpointCore, Error as CommonEndpointError};
use crate::providers::transport_common::EndpointFailure;

const DEFAULT_BASE_URL: &str = "https://generativelanguage.googleapis.com/v1beta/";
const PROVIDER: &str = "Gemini";

#[derive(Debug, Error)]
pub enum Error {
    #[error(transparent)]
    Common(#[from] CommonEndpointError),
    #[error("Gemini provider model name is invalid")]
    InvalidModelName,
    #[error("custom Gemini endpoint cannot be used as a URL base")]
    CannotBeBase,
}

impl EndpointFailure for Error {
    fn is_dns_timeout(&self) -> bool {
        matches!(self, Self::Common(error) if error.is_dns_timeout())
    }
}

#[derive(Clone, Debug)]
pub(in crate::providers) struct Endpoint(EndpointCore);

impl Deref for Endpoint {
    type Target = EndpointCore;

    fn deref(&self) -> &EndpointCore {
        &self.0
    }
}

impl DerefMut for Endpoint {
    fn deref_mut(&mut self) -> &mut EndpointCore {
        &mut self.0
    }
}

impl Default for Endpoint {
    fn default() -> Self {
        Self::parse(DEFAULT_BASE_URL).expect("the built-in Gemini endpoint is valid")
    }
}

impl Endpoint {
    pub(in crate::providers) fn parse(value: &str) -> Result<Self, Error> {
        Ok(Self(EndpointCore::parse(value, PROVIDER, false)?))
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(in crate::providers) fn for_local_test(value: &str) -> Self {
        EndpointCore::parse(value, PROVIDER, true)
            .map(Self)
            .expect("local test endpoint must be valid")
    }

    pub(in crate::providers) fn generate_url(
        &self,
        upstream_model: &str,
        streaming: bool,
    ) -> Result<Url, Error> {
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

    pub(in crate::providers) fn count_tokens_url(
        &self,
        upstream_model: &str,
    ) -> Result<Url, Error> {
        self.model_action_url(upstream_model, "countTokens")
    }

    pub(in crate::providers) fn models_url(&self) -> Result<Url, Error> {
        self.join("models").map_err(Into::into)
    }

    fn model_action_url(&self, upstream_model: &str, action: &str) -> Result<Url, Error> {
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
            return Err(Error::InvalidModelName);
        }
        let mut url = self.url().clone();
        {
            let mut path = url.path_segments_mut().map_err(|()| Error::CannotBeBase)?;
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
}

#[cfg(test)]
mod tests {
    use std::net::IpAddr;

    use super::*;

    #[test]
    fn endpoint_policy_and_action_paths_are_safe() {
        assert!(matches!(
            Endpoint::parse("http://generativelanguage.googleapis.com/v1beta"),
            Err(Error::Common(CommonEndpointError::HttpsRequired { .. }))
        ));
        assert!(matches!(
            Endpoint::parse("https://key@googleapis.com/v1beta"),
            Err(Error::Common(CommonEndpointError::UserInfoForbidden { .. }))
        ));
        assert!(matches!(
            Endpoint::parse("https://googleapis.com/v1beta?key=ambient"),
            Err(Error::Common(
                CommonEndpointError::QueryOrFragmentForbidden { .. }
            ))
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
            Err(Error::InvalidModelName)
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
            Err(Error::Common(CommonEndpointError::ForbiddenAddress {
                address: blocked,
                ..
            })) if blocked == address
        ));
    }
}

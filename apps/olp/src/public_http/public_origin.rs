use std::{fmt, str::FromStr, sync::Arc};

use thiserror::Error;
use url::{Host, Url};

/// The canonical externally visible origin of this installation.
///
/// The stored serialization is suitable for browser `Origin` comparison and
/// never contains a trailing slash, path, query, fragment, or userinfo.
#[derive(Clone, Eq, Hash, PartialEq)]
pub struct PublicOrigin {
    serialized: Arc<str>,
    url: Url,
}

#[derive(Clone, Debug, Error, Eq, PartialEq)]
pub enum PublicOriginError {
    #[error("public origin must not be empty")]
    Empty,
    #[error("public origin must not contain whitespace or control characters")]
    WhitespaceOrControl,
    #[error("public origin must not contain backslashes")]
    Backslash,
    #[error("public origin is not a valid absolute URL")]
    InvalidUrl,
    #[error("public origin scheme must be https, or http for a loopback host")]
    InvalidScheme,
    #[error("public origin must include a valid host")]
    MissingHost,
    #[error("public origin port must be a non-empty decimal number between 0 and 65535")]
    InvalidPort,
    #[error("public origin must not include a username or password")]
    Userinfo,
    #[error("public origin must not include a path other than /")]
    Path,
    #[error("public origin must not include a query string")]
    Query,
    #[error("public origin must not include a fragment")]
    Fragment,
    #[error("an http public origin is permitted only for localhost or a loopback IP address")]
    NonLoopbackHttp,
}

impl PublicOrigin {
    pub fn parse(value: &str) -> Result<Self, PublicOriginError> {
        if value.is_empty() {
            return Err(PublicOriginError::Empty);
        }
        if value
            .chars()
            .any(|character| character.is_whitespace() || character.is_control())
        {
            return Err(PublicOriginError::WhitespaceOrControl);
        }
        if value.contains('\\') {
            return Err(PublicOriginError::Backslash);
        }
        let mut url = Url::parse(value).map_err(|_| PublicOriginError::InvalidUrl)?;
        if !matches!(url.scheme(), "http" | "https") || url.cannot_be_a_base() {
            return Err(PublicOriginError::InvalidScheme);
        }
        let host = url.host().ok_or(PublicOriginError::MissingHost)?;

        // Checking the raw authority catches the syntactically present but
        // empty userinfo form (`https://@host`) in addition to ordinary names.
        let remainder = value
            .split_once("://")
            .map(|(_, remainder)| remainder)
            .ok_or(PublicOriginError::InvalidUrl)?;
        let authority = remainder.split(['/', '?', '#']).next().unwrap_or_default();
        if authority.is_empty() {
            return Err(PublicOriginError::MissingHost);
        }
        if authority.contains('@') || !url.username().is_empty() || url.password().is_some() {
            return Err(PublicOriginError::Userinfo);
        }
        validate_authority_port(authority)?;
        let raw_path = remainder[authority.len()..]
            .split(['?', '#'])
            .next()
            .unwrap_or_default();
        if !matches!(raw_path, "" | "/") || url.path() != "/" {
            return Err(PublicOriginError::Path);
        }
        if url.query().is_some() {
            return Err(PublicOriginError::Query);
        }
        if url.fragment().is_some() {
            return Err(PublicOriginError::Fragment);
        }
        if url.scheme() == "http" && !loopback_host(&host) {
            return Err(PublicOriginError::NonLoopbackHttp);
        }

        // `url` canonicalizes host casing, IDNA, IPv6 brackets, and default
        // ports. Origin serialization deliberately omits the path slash.
        let serialized = url.origin().ascii_serialization();
        url = Url::parse(&serialized).expect("a serialized URL origin is a valid URL");
        Ok(Self {
            serialized: Arc::from(serialized),
            url,
        })
    }

    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.serialized
    }

    #[must_use]
    pub fn is_https(&self) -> bool {
        self.url.scheme() == "https"
    }

    #[must_use]
    pub fn with_path(&self, path: &str) -> Url {
        let mut url = self.url.clone();
        url.set_path(path);
        url.set_query(None);
        url.set_fragment(None);
        url
    }

    #[must_use]
    pub fn matches_header(&self, candidate: &str) -> bool {
        Self::parse(candidate).is_ok_and(|candidate| candidate == *self)
    }
}

fn validate_authority_port(authority: &str) -> Result<(), PublicOriginError> {
    let port = if authority.starts_with('[') {
        let closing_bracket = authority.rfind(']').ok_or(PublicOriginError::InvalidUrl)?;
        let suffix = &authority[closing_bracket + 1..];
        if suffix.is_empty() {
            return Ok(());
        }
        suffix
            .strip_prefix(':')
            .ok_or(PublicOriginError::InvalidUrl)?
    } else if let Some((host, port)) = authority.rsplit_once(':') {
        if host.contains(':') {
            return Err(PublicOriginError::InvalidUrl);
        }
        port
    } else {
        return Ok(());
    };

    if port.is_empty()
        || !port.bytes().all(|byte| byte.is_ascii_digit())
        || port.parse::<u16>().is_err()
    {
        return Err(PublicOriginError::InvalidPort);
    }
    Ok(())
}

fn loopback_host(host: &Host<&str>) -> bool {
    match host {
        Host::Ipv4(address) => address.is_loopback(),
        Host::Ipv6(address) => address.is_loopback(),
        Host::Domain(domain) => domain.eq_ignore_ascii_case("localhost"),
    }
}

impl FromStr for PublicOrigin {
    type Err = PublicOriginError;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        Self::parse(value)
    }
}

impl AsRef<str> for PublicOrigin {
    fn as_ref(&self) -> &str {
        self.as_str()
    }
}

impl fmt::Display for PublicOrigin {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

impl fmt::Debug for PublicOrigin {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_tuple("PublicOrigin")
            .field(&self.as_str())
            .finish()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn canonicalizes_equivalent_origins() {
        assert_eq!(
            PublicOrigin::parse("https://EXAMPLE.COM:443/")
                .unwrap()
                .as_str(),
            "https://example.com"
        );
        assert_eq!(
            PublicOrigin::parse("http://LOCALHOST:80").unwrap().as_str(),
            "http://localhost"
        );
        assert_eq!(
            PublicOrigin::parse("https://192.0.2.10:443/")
                .unwrap()
                .as_str(),
            "https://192.0.2.10"
        );
        assert_eq!(
            PublicOrigin::parse("https://[2001:db8::1]:443")
                .unwrap()
                .as_str(),
            "https://[2001:db8::1]"
        );
        assert_eq!(
            PublicOrigin::parse("https://BÜCHER.example")
                .unwrap()
                .as_str(),
            "https://xn--bcher-kva.example"
        );
    }

    #[test]
    fn permits_only_loopback_http() {
        for value in [
            "http://127.0.0.1:8080",
            "http://[::1]:8080",
            "http://localhost:8080",
        ] {
            assert!(PublicOrigin::parse(value).is_ok(), "{value}");
        }
        assert_eq!(
            PublicOrigin::parse("http://example.test").unwrap_err(),
            PublicOriginError::NonLoopbackHttp
        );
    }

    #[test]
    fn rejects_non_origin_components_and_ambiguous_input() {
        for value in [
            " https://example.test",
            "https://example.test ",
            "https://example.test\n",
            "https:\\example.test",
            "https://user@example.test",
            "https://:password@example.test",
            "https://example.test/path",
            "https://example.test/a/..",
            "https://example.test:",
            "https://[2001:db8::1]:",
            "https://example.test:65536",
            "https:////example.test",
            "https://example.test?query",
            "https://example.test#fragment",
        ] {
            assert!(PublicOrigin::parse(value).is_err(), "{value:?}");
        }
    }

    #[test]
    fn compares_origin_headers_canonically() {
        let origin = PublicOrigin::parse("https://example.test").unwrap();
        assert!(origin.matches_header("https://EXAMPLE.TEST:443"));
        assert!(!origin.matches_header("https://example.test/path"));
        assert!(!origin.matches_header("https://other.test"));
    }
}

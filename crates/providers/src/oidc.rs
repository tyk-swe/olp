use std::time::Duration;

use crate::http_egress::{
    is_public_ip,
    pinned::{PinnedClientConfig, PinnedClientError, literal_ip, one_shot_client},
};
use bytes::Bytes;
use futures::{Stream, StreamExt as _};
use reqwest::{Client, Response, Url};
use serde::de::DeserializeOwned;
use thiserror::Error;
use tokio::time::timeout;
use zeroize::Zeroizing;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(10);
const BODY_TOTAL_TIMEOUT: Duration = Duration::from_secs(10);
const BODY_IDLE_TIMEOUT: Duration = Duration::from_secs(2);

#[derive(Clone, Copy, Debug)]
pub struct OidcNetworkPolicy {
    pub allow_insecure_test_endpoints: bool,
}

#[derive(Debug, Error)]
pub enum OidcNetworkError {
    #[error("the OIDC endpoint URL is invalid")]
    InvalidUrl,
    #[error("OIDC endpoints must use HTTPS")]
    HttpsRequired,
    #[error("OIDC endpoint credentials and fragments are forbidden")]
    UnsafeUrl,
    #[error("the OIDC endpoint resolves to a non-public network address")]
    ForbiddenAddress,
    #[error("the OIDC endpoint could not be resolved safely")]
    Resolution,
    #[error("the OIDC endpoint request failed")]
    Request,
    #[error("the OIDC endpoint returned an unsuccessful status")]
    Status,
    #[error("the OIDC endpoint response is too large")]
    ResponseTooLarge,
    #[error("the OIDC endpoint response body timed out")]
    ResponseTimeout,
    #[error("the OIDC endpoint response is not JSON")]
    ContentType,
    #[error("the OIDC endpoint returned malformed JSON")]
    InvalidDocument,
}

impl OidcNetworkPolicy {
    pub async fn validate_url(&self, value: &str) -> Result<Url, OidcNetworkError> {
        let url = parse_url(value, self.allow_insecure_test_endpoints)?;
        let _ = self.client_for(&url).await?;
        Ok(url)
    }

    pub async fn get_json<T: DeserializeOwned>(
        &self,
        value: &str,
        maximum_bytes: usize,
    ) -> Result<T, OidcNetworkError> {
        let url = parse_url(value, self.allow_insecure_test_endpoints)?;
        let client = self.client_for(&url).await?;
        let response = timeout(REQUEST_TIMEOUT, client.get(url).send())
            .await
            .map_err(|_| OidcNetworkError::Request)?
            .map_err(|_| OidcNetworkError::Request)?;
        decode_json(response, maximum_bytes).await
    }

    pub async fn post_form_json<T: DeserializeOwned>(
        &self,
        value: &str,
        form: &[(String, String)],
        basic_auth: Option<(&str, &str)>,
        maximum_bytes: usize,
    ) -> Result<T, OidcNetworkError> {
        let url = parse_url(value, self.allow_insecure_test_endpoints)?;
        let client = self.client_for(&url).await?;
        let mut request = client.post(url).form(form);
        if let Some((username, password)) = basic_auth {
            request = request.basic_auth(username, Some(password));
        }
        let response = timeout(REQUEST_TIMEOUT, request.send())
            .await
            .map_err(|_| OidcNetworkError::Request)?
            .map_err(|_| OidcNetworkError::Request)?;
        decode_json(response, maximum_bytes).await
    }

    async fn client_for(&self, url: &Url) -> Result<Client, OidcNetworkError> {
        one_shot_client(
            url,
            CONNECT_TIMEOUT,
            PinnedClientConfig {
                connect_timeout: CONNECT_TIMEOUT,
                pool_idle_timeout: None,
                pool_max_idle_per_host: None,
                allow_unsafe_target: self.allow_insecure_test_endpoints,
                user_agent: "openllmproxy-oidc",
            },
        )
        .await
        .map_err(map_pinned_client_error)
    }
}

fn map_pinned_client_error(error: PinnedClientError) -> OidcNetworkError {
    match error {
        PinnedClientError::MissingHost | PinnedClientError::MissingPort => {
            OidcNetworkError::InvalidUrl
        }
        PinnedClientError::DnsTimeout
        | PinnedClientError::DnsResolution(_)
        | PinnedClientError::NoAddresses => OidcNetworkError::Resolution,
        PinnedClientError::ForbiddenAddress(_) => OidcNetworkError::ForbiddenAddress,
        PinnedClientError::ClientBuild(_) => OidcNetworkError::Request,
    }
}

fn parse_url(value: &str, allow_insecure: bool) -> Result<Url, OidcNetworkError> {
    let url = Url::parse(value).map_err(|_| OidcNetworkError::InvalidUrl)?;
    if !matches!(url.scheme(), "http" | "https") {
        return Err(OidcNetworkError::InvalidUrl);
    }
    if !allow_insecure && url.scheme() != "https" {
        return Err(OidcNetworkError::HttpsRequired);
    }
    if !url.username().is_empty() || url.password().is_some() || url.fragment().is_some() {
        return Err(OidcNetworkError::UnsafeUrl);
    }
    let host = url.host_str().ok_or(OidcNetworkError::InvalidUrl)?;
    if !allow_insecure
        && (host.eq_ignore_ascii_case("localhost")
            || host.ends_with(".localhost")
            || url.port() == Some(0))
    {
        return Err(OidcNetworkError::ForbiddenAddress);
    }
    if let Some(ip) = literal_ip(&url)
        && !allow_insecure
        && !is_public_ip(ip)
    {
        return Err(OidcNetworkError::ForbiddenAddress);
    }
    Ok(url)
}

async fn decode_json<T: DeserializeOwned>(
    response: Response,
    maximum_bytes: usize,
) -> Result<T, OidcNetworkError> {
    if !response.status().is_success() {
        return Err(OidcNetworkError::Status);
    }
    let content_type = response
        .headers()
        .get(reqwest::header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default()
        .to_ascii_lowercase();
    if !content_type.starts_with("application/json")
        && !content_type.starts_with("application/jwk-set+json")
    {
        return Err(OidcNetworkError::ContentType);
    }
    if response
        .content_length()
        .is_some_and(|length| length > maximum_bytes as u64)
    {
        return Err(OidcNetworkError::ResponseTooLarge);
    }
    let body = read_bounded_body(
        response.bytes_stream(),
        maximum_bytes,
        BODY_TOTAL_TIMEOUT,
        BODY_IDLE_TIMEOUT,
    )
    .await?;
    serde_json::from_slice(&body).map_err(|_| OidcNetworkError::InvalidDocument)
}

async fn read_bounded_body<S, E>(
    stream: S,
    maximum_bytes: usize,
    total_deadline: Duration,
    idle_deadline: Duration,
) -> Result<Zeroizing<Vec<u8>>, OidcNetworkError>
where
    S: Stream<Item = Result<Bytes, E>>,
{
    timeout(total_deadline, async move {
        let mut body = Zeroizing::new(Vec::new());
        let mut stream = Box::pin(stream);
        loop {
            let next = timeout(idle_deadline, stream.next())
                .await
                .map_err(|_| OidcNetworkError::ResponseTimeout)?;
            let Some(chunk) = next else {
                break;
            };
            let chunk = chunk.map_err(|_| OidcNetworkError::Request)?;
            if body.len().saturating_add(chunk.len()) > maximum_bytes {
                return Err(OidcNetworkError::ResponseTooLarge);
            }
            body.extend_from_slice(&chunk);
        }
        Ok(body)
    })
    .await
    .map_err(|_| OidcNetworkError::ResponseTimeout)?
}

#[cfg(test)]
mod tests {
    use super::*;
    use futures::stream;

    #[test]
    fn production_policy_rejects_credentials_fragments_and_private_ranges() {
        for url in [
            "https://user:password@example.test/discovery",
            "https://example.test/discovery#fragment",
            "https://127.0.0.1/discovery",
            "https://169.254.169.254/latest/meta-data",
            "https://[::1]/discovery",
            "https://[::ffff:127.0.0.1]/discovery",
            "https://localhost/discovery",
        ] {
            assert!(parse_url(url, false).is_err(), "{url} must be rejected");
        }
        assert!(matches!(
            parse_url("http://idp.example.test/discovery", false),
            Err(OidcNetworkError::HttpsRequired)
        ));
    }

    #[test]
    fn test_policy_allows_loopback_http_only_when_explicitly_enabled() {
        assert!(parse_url("http://127.0.0.1:8080/discovery", true).is_ok());
    }

    #[tokio::test]
    async fn body_reader_enforces_idle_deadline() {
        let stalled = stream::pending::<Result<Bytes, ()>>();
        let result = read_bounded_body(
            stalled,
            1024,
            Duration::from_millis(100),
            Duration::from_millis(5),
        )
        .await;
        assert!(matches!(result, Err(OidcNetworkError::ResponseTimeout)));
    }

    #[tokio::test]
    async fn body_reader_enforces_total_deadline_while_data_arrives() {
        let trickle = stream::unfold((), |()| async {
            tokio::time::sleep(Duration::from_millis(2)).await;
            Some((Ok::<_, ()>(Bytes::from_static(b" ")), ()))
        });
        let result = read_bounded_body(
            trickle,
            usize::MAX,
            Duration::from_millis(15),
            Duration::from_millis(10),
        )
        .await;
        assert!(matches!(result, Err(OidcNetworkError::ResponseTimeout)));
    }
}

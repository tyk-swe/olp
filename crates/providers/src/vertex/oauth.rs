use std::{
    fmt,
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use arc_swap::ArcSwapOption;

use crate::gemini::{BearerTokenError, BearerTokenProvider, SecretBearerToken};
use crate::http_egress::pinned::{PinnedClientConfig, PinnedClientError, PinnedClientPool};
use futures::StreamExt;
use google_cloud_auth::credentials::AccessTokenCredentials;
use http::{HeaderValue, StatusCode, header};
use jsonwebtoken::{Algorithm, EncodingKey, Header, encode};
use reqwest::{Client, Url};
use serde::{Deserialize, Serialize};
use tokio::{sync::Mutex, time::Instant, time::timeout};
use zeroize::Zeroizing;

const GOOGLE_TOKEN_ENDPOINT: &str = "https://oauth2.googleapis.com/token";
const MAX_TOKEN_RESPONSE_BYTES: usize = 64 * 1024;
const TOKEN_RESPONSE_TIMEOUT: Duration = Duration::from_secs(10);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_secs(30);
const MAX_IDLE_CONNECTIONS_PER_HOST: usize = 32;

pub(crate) struct ApplicationDefaultTokenProvider {
    credentials: AccessTokenCredentials,
}

impl ApplicationDefaultTokenProvider {
    pub(crate) const fn new(credentials: AccessTokenCredentials) -> Self {
        Self { credentials }
    }
}

impl fmt::Debug for ApplicationDefaultTokenProvider {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("ApplicationDefaultTokenProvider([REDACTED])")
    }
}

impl BearerTokenProvider for ApplicationDefaultTokenProvider {
    fn token<'a>(
        &'a self,
    ) -> olp_domain::BoxFuture<'a, Result<SecretBearerToken, BearerTokenError>> {
        Box::pin(async move {
            let token = self
                .credentials
                .access_token()
                .await
                .map_err(|_| BearerTokenError)?;
            SecretBearerToken::new(token.token)
        })
    }
}

#[derive(Deserialize)]
struct ServiceAccountCredential {
    #[serde(rename = "type")]
    credential_type: String,
    private_key_id: String,
    #[serde(deserialize_with = "deserialize_zeroizing_string")]
    private_key: Zeroizing<String>,
    client_email: String,
    token_uri: String,
}

impl fmt::Debug for ServiceAccountCredential {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("ServiceAccountCredential([REDACTED])")
    }
}

struct CachedToken {
    value: Zeroizing<String>,
    refresh_at: Instant,
    expires_at: Instant,
}

pub(crate) struct ServiceAccountTokenProvider {
    credential: ServiceAccountCredential,
    endpoint: OAuthEndpoint,
    cache: ArcSwapOption<CachedToken>,
    refresh_lock: Mutex<()>,
}

impl ServiceAccountTokenProvider {
    pub(crate) fn from_json(value: &str) -> Result<Self, ServiceAccountError> {
        Self::from_json_with_policy(value, false)
    }

    fn from_json_with_policy(
        value: &str,
        allow_unsafe_test_endpoint: bool,
    ) -> Result<Self, ServiceAccountError> {
        if value.len() > 64 * 1024 {
            return Err(ServiceAccountError::CredentialTooLarge);
        }
        let credential: ServiceAccountCredential =
            serde_json::from_str(value).map_err(|_| ServiceAccountError::InvalidCredential)?;
        validate_credential(&credential, allow_unsafe_test_endpoint)?;
        let endpoint = OAuthEndpoint::parse(&credential.token_uri, allow_unsafe_test_endpoint)?;
        // Parse the key once at generation construction, but never retain the
        // derived encoding key outside the encrypted credential's lifetime.
        EncodingKey::from_rsa_pem(credential.private_key.as_bytes())
            .map_err(|_| ServiceAccountError::InvalidPrivateKey)?;
        Ok(Self {
            credential,
            endpoint,
            cache: ArcSwapOption::empty(),
            refresh_lock: Mutex::new(()),
        })
    }

    #[cfg(test)]
    pub(crate) fn from_json_for_test(value: &str) -> Result<Self, ServiceAccountError> {
        Self::from_json_with_policy(value, true)
    }

    async fn refresh(&self) -> Result<CachedToken, BearerTokenError> {
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_err(|_| BearerTokenError)?
            .as_secs();
        let claims = ServiceAccountClaims {
            iss: &self.credential.client_email,
            scope: super::DEFAULT_SCOPE,
            aud: self.endpoint.url.as_str(),
            iat: now,
            exp: now.saturating_add(3_600),
        };
        let mut header = Header::new(Algorithm::RS256);
        header.kid = Some(self.credential.private_key_id.clone());
        let key = EncodingKey::from_rsa_pem(self.credential.private_key.as_bytes())
            .map_err(|_| BearerTokenError)?;
        let assertion =
            Zeroizing::new(encode(&header, &claims, &key).map_err(|_| BearerTokenError)?);
        let client = self
            .endpoint
            .pinned_client(Duration::from_secs(5))
            .await
            .map_err(|_| BearerTokenError)?;
        let response = timeout(
            Duration::from_secs(10),
            client
                .post(self.endpoint.url.clone())
                .header(header::ACCEPT, HeaderValue::from_static("application/json"))
                .form(&[
                    ("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer"),
                    ("assertion", assertion.as_str()),
                ])
                .send(),
        )
        .await
        .map_err(|_| BearerTokenError)?
        .map_err(|_| BearerTokenError)?;
        if response.status() != StatusCode::OK {
            return Err(BearerTokenError);
        }
        if !crate::transport_common::has_content_type(response.headers(), "application/json") {
            return Err(BearerTokenError);
        }
        let bytes = read_bounded_token_response(response, TOKEN_RESPONSE_TIMEOUT).await?;
        let response: TokenResponse =
            serde_json::from_slice(&bytes).map_err(|_| BearerTokenError)?;
        if !response.token_type.eq_ignore_ascii_case("Bearer")
            || response.expires_in < 30
            || response.access_token.trim().is_empty()
        {
            return Err(BearerTokenError);
        }
        let (refresh_at, expires_at) = token_deadlines(response.expires_in)?;
        Ok(CachedToken {
            value: Zeroizing::new(response.access_token),
            refresh_at,
            expires_at,
        })
    }

    fn cached_token(
        &self,
        allow_refresh_due: bool,
    ) -> Result<Option<SecretBearerToken>, BearerTokenError> {
        let now = Instant::now();
        self.cache
            .load()
            .as_ref()
            .filter(|token| {
                if allow_refresh_due {
                    token.expires_at > now
                } else {
                    token.refresh_at > now
                }
            })
            .map(|token| SecretBearerToken::new(token.value.as_str().to_owned()))
            .transpose()
    }
}

fn token_deadlines(expires_in: u64) -> Result<(Instant, Instant), BearerTokenError> {
    let refresh_margin = 300_u64.min(expires_in / 2);
    let now = Instant::now();
    let expires_at = now
        .checked_add(Duration::from_secs(expires_in))
        .ok_or(BearerTokenError)?;
    let refresh_at = now
        .checked_add(Duration::from_secs(
            expires_in.saturating_sub(refresh_margin),
        ))
        .ok_or(BearerTokenError)?;
    Ok((refresh_at, expires_at))
}

impl fmt::Debug for ServiceAccountTokenProvider {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("ServiceAccountTokenProvider([REDACTED])")
    }
}

impl BearerTokenProvider for ServiceAccountTokenProvider {
    fn token<'a>(
        &'a self,
    ) -> olp_domain::BoxFuture<'a, Result<SecretBearerToken, BearerTokenError>> {
        Box::pin(async move {
            if let Some(token) = self.cached_token(false)? {
                return Ok(token);
            }
            let _refresh = self.refresh_lock.lock().await;
            if let Some(token) = self.cached_token(false)? {
                return Ok(token);
            }
            let refreshed = match self.refresh().await {
                Ok(token) => Arc::new(token),
                Err(error) => return self.cached_token(true)?.ok_or(error),
            };
            let value = SecretBearerToken::new(refreshed.value.as_str().to_owned())?;
            self.cache.store(Some(refreshed));
            Ok(value)
        })
    }
}

#[derive(Serialize)]
struct ServiceAccountClaims<'a> {
    iss: &'a str,
    scope: &'a str,
    aud: &'a str,
    iat: u64,
    exp: u64,
}

#[derive(Deserialize)]
struct TokenResponse {
    access_token: String,
    token_type: String,
    expires_in: u64,
}

fn deserialize_zeroizing_string<'de, D>(deserializer: D) -> Result<Zeroizing<String>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    String::deserialize(deserializer).map(Zeroizing::new)
}

struct OAuthEndpoint {
    url: Url,
    allow_unsafe_test_endpoint: bool,
    client_pool: PinnedClientPool,
}

impl Clone for OAuthEndpoint {
    fn clone(&self) -> Self {
        Self {
            url: self.url.clone(),
            allow_unsafe_test_endpoint: self.allow_unsafe_test_endpoint,
            client_pool: self.client_pool.clone(),
        }
    }
}

impl OAuthEndpoint {
    fn parse(value: &str, allow_unsafe_test_endpoint: bool) -> Result<Self, ServiceAccountError> {
        let url = Url::parse(value).map_err(|_| ServiceAccountError::InvalidTokenEndpoint)?;
        if !allow_unsafe_test_endpoint && url.as_str() != GOOGLE_TOKEN_ENDPOINT {
            return Err(ServiceAccountError::InvalidTokenEndpoint);
        }
        if allow_unsafe_test_endpoint && !matches!(url.scheme(), "http" | "https") {
            return Err(ServiceAccountError::InvalidTokenEndpoint);
        }
        if !url.username().is_empty()
            || url.password().is_some()
            || url.query().is_some()
            || url.fragment().is_some()
            || url.host().is_none()
        {
            return Err(ServiceAccountError::InvalidTokenEndpoint);
        }
        Ok(Self {
            url,
            allow_unsafe_test_endpoint,
            client_pool: PinnedClientPool::default(),
        })
    }

    async fn pinned_client(
        &self,
        connect_timeout: Duration,
    ) -> Result<Client, ServiceAccountError> {
        self.client_pool
            .client(
                &self.url,
                connect_timeout,
                PinnedClientConfig {
                    connect_timeout,
                    pool_idle_timeout: Some(POOL_IDLE_TIMEOUT),
                    pool_max_idle_per_host: Some(MAX_IDLE_CONNECTIONS_PER_HOST),
                    allow_unsafe_target: self.allow_unsafe_test_endpoint,
                    user_agent: "openllmproxy",
                },
            )
            .await
            .map_err(map_pinned_client_error)
    }
}

fn map_pinned_client_error(error: PinnedClientError) -> ServiceAccountError {
    match error {
        PinnedClientError::MissingHost | PinnedClientError::MissingPort => {
            ServiceAccountError::InvalidTokenEndpoint
        }
        PinnedClientError::DnsTimeout
        | PinnedClientError::DnsResolution(_)
        | PinnedClientError::NoAddresses
        | PinnedClientError::ForbiddenAddress(_)
        | PinnedClientError::ClientBuild(_) => ServiceAccountError::TokenEndpointUnavailable,
    }
}

async fn read_bounded_token_response(
    response: reqwest::Response,
    total_timeout: Duration,
) -> Result<Vec<u8>, BearerTokenError> {
    timeout(total_timeout, async move {
        let mut source = response.bytes_stream();
        let mut output = Vec::new();
        while let Some(chunk) = source.next().await {
            let chunk = chunk.map_err(|_| BearerTokenError)?;
            if output.len().saturating_add(chunk.len()) > MAX_TOKEN_RESPONSE_BYTES {
                return Err(BearerTokenError);
            }
            output.extend_from_slice(&chunk);
        }
        if output.is_empty() {
            return Err(BearerTokenError);
        }
        Ok(output)
    })
    .await
    .map_err(|_| BearerTokenError)?
}

fn validate_credential(
    credential: &ServiceAccountCredential,
    allow_unsafe_test_endpoint: bool,
) -> Result<(), ServiceAccountError> {
    if credential.credential_type != "service_account"
        || credential.private_key_id.trim().is_empty()
        || credential.client_email.trim().is_empty()
        || credential.client_email.contains(char::is_whitespace)
        || credential.private_key.len() > 32 * 1024
        || credential.private_key.is_empty()
    {
        return Err(ServiceAccountError::InvalidCredential);
    }
    OAuthEndpoint::parse(&credential.token_uri, allow_unsafe_test_endpoint)?;
    Ok(())
}

#[derive(Debug, thiserror::Error)]
pub(crate) enum ServiceAccountError {
    #[error("credential JSON exceeds 64 KiB")]
    CredentialTooLarge,
    #[error("credential JSON is malformed or missing required service-account fields")]
    InvalidCredential,
    #[error("credential private key is not a valid RSA PKCS#8 key")]
    InvalidPrivateKey,
    #[error("credential token endpoint must be the official Google OAuth endpoint")]
    InvalidTokenEndpoint,
    #[error("Google OAuth token endpoint is unavailable")]
    TokenEndpointUnavailable,
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::{io::AsyncWriteExt, net::TcpListener};

    #[test]
    fn token_lifetime_cannot_overflow_the_runtime_clock() {
        let (refresh_at, expires_at) = token_deadlines(3_600).unwrap();
        assert!(refresh_at < expires_at);
        assert!(token_deadlines(u64::MAX).is_err());
    }

    fn test_provider(token_uri: &str) -> ServiceAccountTokenProvider {
        let credential = serde_json::json!({
            "type": "service_account",
            "private_key_id": "test-key",
            "private_key": include_str!("../../testdata/vertex/test_only_private_key.pem"),
            "client_email": "test@test-project.iam.gserviceaccount.com",
            "token_uri": token_uri,
        });
        ServiceAccountTokenProvider::from_json_for_test(&credential.to_string()).unwrap()
    }

    #[tokio::test]
    async fn refresh_failure_uses_only_an_unexpired_cached_token() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let endpoint = format!("http://{}/token", listener.local_addr().unwrap());
        let server = tokio::spawn(async move {
            for _ in 0..2 {
                let (socket, _) = listener.accept().await.unwrap();
                drop(socket);
            }
        });
        let provider = test_provider(&endpoint);
        let now = Instant::now();
        provider.cache.store(Some(Arc::new(CachedToken {
            value: Zeroizing::new("cached-token".to_owned()),
            refresh_at: now - Duration::from_secs(1),
            expires_at: now + Duration::from_secs(30),
        })));
        assert!(provider.token().await.is_ok());

        provider.cache.store(Some(Arc::new(CachedToken {
            value: Zeroizing::new("expired-token".to_owned()),
            refresh_at: now - Duration::from_secs(2),
            expires_at: now - Duration::from_secs(1),
        })));
        assert!(provider.token().await.is_err());
        server.await.unwrap();
    }

    #[tokio::test]
    async fn token_response_timeout_is_total_not_per_chunk() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await.unwrap();
            socket
                .write_all(
                    b"HTTP/1.1 200 OK\r\nContent-Length: 100000\r\nConnection: close\r\n\r\n",
                )
                .await
                .unwrap();
            loop {
                if socket.write_all(b"x").await.is_err() {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        });
        let response = reqwest::Client::new()
            .get(format!("http://{address}"))
            .send()
            .await
            .unwrap();
        assert!(
            read_bounded_token_response(response, Duration::from_millis(25))
                .await
                .is_err()
        );
    }

    #[test]
    fn production_oauth_endpoint_remains_exactly_allowlisted() {
        assert!(matches!(
            OAuthEndpoint::parse("https://oauth2.googleapis.com/other", false),
            Err(ServiceAccountError::InvalidTokenEndpoint)
        ));
        OAuthEndpoint::parse(GOOGLE_TOKEN_ENDPOINT, false).unwrap();
    }
}

use std::{
    fmt,
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use crate::gemini::{BearerTokenError, BearerTokenProvider, SecretBearerToken};
use crate::http_egress::pinned::{PinnedClientConfig, PinnedClientError, PinnedClientPool};
use futures::StreamExt;
use google_cloud_auth::credentials::AccessTokenCredentials;
use http::{HeaderValue, StatusCode, header};
use jsonwebtoken::{Algorithm, EncodingKey, Header, encode};
use reqwest::{Client, Url};
use serde::{Deserialize, Serialize};
use tokio::{sync::Mutex, time::Instant, time::timeout};
use zeroize::{Zeroize, Zeroizing};

const GOOGLE_TOKEN_ENDPOINT: &str = "https://oauth2.googleapis.com/token";
const CLOUD_PLATFORM_SCOPE: &str = "https://www.googleapis.com/auth/cloud-platform";
const MAX_TOKEN_RESPONSE_BYTES: usize = 64 * 1024;
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
pub(crate) struct ServiceAccountCredential {
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

impl Drop for ServiceAccountCredential {
    fn drop(&mut self) {
        self.private_key_id.zeroize();
        self.client_email.zeroize();
        self.token_uri.zeroize();
    }
}

struct CachedToken {
    value: Zeroizing<String>,
    refresh_at: Instant,
}

pub(crate) struct ServiceAccountTokenProvider {
    credential: Arc<ServiceAccountCredential>,
    endpoint: OAuthEndpoint,
    cache: Mutex<Option<CachedToken>>,
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
            credential: Arc::new(credential),
            endpoint,
            cache: Mutex::new(None),
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
            scope: CLOUD_PLATFORM_SCOPE,
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
        let content_type_ok = response
            .headers()
            .get(header::CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.split(';').next())
            .is_some_and(|value| value.trim().eq_ignore_ascii_case("application/json"));
        if !content_type_ok {
            return Err(BearerTokenError);
        }
        let bytes = read_bounded_token_response(response).await?;
        let response: TokenResponse =
            serde_json::from_slice(&bytes).map_err(|_| BearerTokenError)?;
        if response.token_type != "Bearer"
            || response.expires_in < 30
            || response.access_token.trim().is_empty()
        {
            return Err(BearerTokenError);
        }
        Ok(CachedToken {
            value: Zeroizing::new(response.access_token),
            refresh_at: token_refresh_deadline(response.expires_in)?,
        })
    }
}

fn token_refresh_deadline(expires_in: u64) -> Result<Instant, BearerTokenError> {
    let refresh_margin = 300_u64.min(expires_in / 2);
    Instant::now()
        .checked_add(Duration::from_secs(
            expires_in.saturating_sub(refresh_margin),
        ))
        .ok_or(BearerTokenError)
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
            let mut cache = self.cache.lock().await;
            if let Some(token) = cache
                .as_ref()
                .filter(|token| token.refresh_at > Instant::now())
            {
                return SecretBearerToken::new(token.value.as_str().to_owned());
            }
            let refreshed = self.refresh().await?;
            let value = SecretBearerToken::new(refreshed.value.as_str().to_owned())?;
            *cache = Some(refreshed);
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
) -> Result<Vec<u8>, BearerTokenError> {
    let mut source = response.bytes_stream();
    let mut output = Vec::new();
    while let Some(chunk) = timeout(Duration::from_secs(10), source.next())
        .await
        .map_err(|_| BearerTokenError)?
    {
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
pub enum ServiceAccountError {
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

    #[test]
    fn token_lifetime_cannot_overflow_the_runtime_clock() {
        assert!(token_refresh_deadline(3_600).is_ok());
        assert!(token_refresh_deadline(u64::MAX).is_err());
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

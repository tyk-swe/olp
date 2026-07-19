use std::{
    collections::BTreeSet,
    fmt,
    future::Future,
    net::{IpAddr, SocketAddr},
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use crate::gemini::{BearerTokenError, BearerTokenProvider, SecretBearerToken};
use crate::http_egress::is_public_ip;
use futures::StreamExt;
use google_cloud_auth::credentials::AccessTokenCredentials;
use http::{HeaderValue, StatusCode, header};
use jsonwebtoken::{Algorithm, EncodingKey, Header, encode};
use reqwest::{Client, Url, redirect::Policy};
use serde::{Deserialize, Serialize};
use tokio::{net::lookup_host, sync::Mutex, time::Instant, time::timeout};
use zeroize::{Zeroize, Zeroizing};

const GOOGLE_TOKEN_ENDPOINT: &str = "https://oauth2.googleapis.com/token";
const CLOUD_PLATFORM_SCOPE: &str = "https://www.googleapis.com/auth/cloud-platform";
const MAX_TOKEN_RESPONSE_BYTES: usize = 64 * 1024;
const DNS_REVALIDATION_INTERVAL: Duration = Duration::from_secs(30);
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
    client_pool: Arc<OAuthClientPool>,
}

impl Clone for OAuthEndpoint {
    fn clone(&self) -> Self {
        Self {
            url: self.url.clone(),
            allow_unsafe_test_endpoint: self.allow_unsafe_test_endpoint,
            client_pool: Arc::new(OAuthClientPool::default()),
        }
    }
}

#[derive(Debug, Eq, PartialEq)]
struct OAuthClientIdentity {
    scheme: String,
    host: String,
    port: u16,
    addresses: Vec<SocketAddr>,
    connect_timeout: Duration,
    allow_unsafe_test_endpoint: bool,
}

struct CachedOAuthClient {
    identity: OAuthClientIdentity,
    client: Client,
    validated_until: Instant,
}

#[derive(Default)]
struct OAuthClientPool {
    state: Mutex<Option<CachedOAuthClient>>,
    #[cfg(test)]
    builds: std::sync::atomic::AtomicUsize,
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
            client_pool: Arc::new(OAuthClientPool::default()),
        })
    }

    async fn pinned_client(
        &self,
        connect_timeout: Duration,
    ) -> Result<Client, ServiceAccountError> {
        timeout(
            connect_timeout,
            self.pinned_client_with_resolver(connect_timeout, |host, port| async move {
                let resolved = lookup_host((host.as_str(), port)).await?;
                Ok(resolved.collect::<BTreeSet<_>>().into_iter().collect())
            }),
        )
        .await
        .map_err(|_| ServiceAccountError::TokenEndpointUnavailable)?
    }

    async fn pinned_client_with_resolver<Resolve, ResolveFuture>(
        &self,
        connect_timeout: Duration,
        resolve: Resolve,
    ) -> Result<Client, ServiceAccountError>
    where
        Resolve: FnOnce(String, u16) -> ResolveFuture,
        ResolveFuture: Future<Output = Result<Vec<SocketAddr>, std::io::Error>>,
    {
        let mut cached = self.client_pool.state.lock().await;
        let now = Instant::now();
        if let Some(entry) = cached.as_ref()
            && entry.validated_until > now
            && entry.identity.connect_timeout == connect_timeout
        {
            return Ok(entry.client.clone());
        }

        let host = self
            .url
            .host_str()
            .ok_or(ServiceAccountError::InvalidTokenEndpoint)?
            .to_owned();
        let port = self
            .url
            .port_or_known_default()
            .ok_or(ServiceAccountError::InvalidTokenEndpoint)?;
        let literal_ip = host.trim_matches(['[', ']']).parse::<IpAddr>().ok();
        let addresses = if let Some(ip) = literal_ip {
            vec![SocketAddr::new(ip, port)]
        } else {
            resolve(host.clone(), port)
                .await
                .map_err(|_| ServiceAccountError::TokenEndpointUnavailable)?
                .into_iter()
                .collect::<BTreeSet<_>>()
                .into_iter()
                .collect::<Vec<_>>()
        };
        if addresses.is_empty() {
            return Err(ServiceAccountError::TokenEndpointUnavailable);
        }
        if !self.allow_unsafe_test_endpoint {
            for address in &addresses {
                if !is_public_ip(address.ip()) {
                    return Err(ServiceAccountError::TokenEndpointUnavailable);
                }
            }
        }
        let identity = OAuthClientIdentity {
            scheme: self.url.scheme().to_owned(),
            host: host.clone(),
            port,
            addresses: addresses.clone(),
            connect_timeout,
            allow_unsafe_test_endpoint: self.allow_unsafe_test_endpoint,
        };
        let validated_until = Instant::now() + DNS_REVALIDATION_INTERVAL;
        if let Some(entry) = cached.as_mut()
            && entry.identity == identity
        {
            entry.validated_until = validated_until;
            return Ok(entry.client.clone());
        }
        let mut builder = Client::builder()
            .redirect(Policy::none())
            .retry(reqwest::retry::never())
            .no_proxy()
            .connect_timeout(connect_timeout)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(MAX_IDLE_CONNECTIONS_PER_HOST)
            .tcp_nodelay(true)
            .referer(false)
            .user_agent("openllmproxy");
        if !self.allow_unsafe_test_endpoint {
            builder = builder.https_only(true);
        }
        if literal_ip.is_none() {
            builder = builder.resolve_to_addrs(&host, &addresses);
        }
        let client = builder
            .build()
            .map_err(|_| ServiceAccountError::TokenEndpointUnavailable)?;
        #[cfg(test)]
        self.client_pool
            .builds
            .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        *cached = Some(CachedOAuthClient {
            identity,
            client: client.clone(),
            validated_until,
        });
        Ok(client)
    }

    #[cfg(test)]
    async fn expire_cached_dns_for_test(&self) {
        if let Some(cached) = self.client_pool.state.lock().await.as_mut() {
            cached.validated_until = Instant::now();
        }
    }

    #[cfg(test)]
    fn client_builds_for_test(&self) -> usize {
        self.client_pool
            .builds
            .load(std::sync::atomic::Ordering::Relaxed)
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
mod endpoint_pool_tests {
    use super::*;

    #[test]
    fn token_lifetime_cannot_overflow_the_runtime_clock() {
        assert!(token_refresh_deadline(3_600).is_ok());
        assert!(token_refresh_deadline(u64::MAX).is_err());
    }

    #[tokio::test]
    async fn oauth_pool_reuses_dns_but_isolates_clones_and_rejects_rebinding() {
        let endpoint = OAuthEndpoint::parse(GOOGLE_TOKEN_ENDPOINT, false).unwrap();
        let public = SocketAddr::new("8.8.8.8".parse().unwrap(), 443);
        for _ in 0..2 {
            endpoint
                .pinned_client_with_resolver(Duration::from_secs(1), move |_, _| async move {
                    Ok(vec![public])
                })
                .await
                .unwrap();
        }
        assert_eq!(endpoint.client_builds_for_test(), 1);

        let other_provider = endpoint.clone();
        other_provider
            .pinned_client_with_resolver(Duration::from_secs(1), move |_, _| async move {
                Ok(vec![public])
            })
            .await
            .unwrap();
        assert_eq!(other_provider.client_builds_for_test(), 1);
        assert!(!Arc::ptr_eq(
            &endpoint.client_pool,
            &other_provider.client_pool
        ));

        endpoint.expire_cached_dns_for_test().await;
        let rebound = endpoint
            .pinned_client_with_resolver(Duration::from_secs(1), |_, _| async move {
                Ok(vec![SocketAddr::new("127.0.0.1".parse().unwrap(), 443)])
            })
            .await;
        assert!(matches!(
            rebound,
            Err(ServiceAccountError::TokenEndpointUnavailable)
        ));
        assert_eq!(endpoint.client_builds_for_test(), 1);
    }
}

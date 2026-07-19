use std::{
    collections::BTreeSet,
    fmt,
    future::Future,
    net::{IpAddr, SocketAddr},
    sync::Arc,
    time::Duration,
};

use crate::http_egress::is_public_ip;
use reqwest::{Client, Url, redirect::Policy};
use thiserror::Error;
use tokio::{net::lookup_host, sync::Mutex, time::Instant, time::timeout};

const DEFAULT_BASE_URL: &str = "https://generativelanguage.googleapis.com/v1beta/";
const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const DNS_REVALIDATION_INTERVAL: Duration = Duration::from_secs(30);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_secs(30);
const MAX_IDLE_CONNECTIONS_PER_HOST: usize = 256;

pub(crate) struct Endpoint {
    base_url: Url,
    client_connect_timeout: Duration,
    client_pool: Arc<ClientPool>,
    #[cfg(any(test, feature = "test-util"))]
    allow_unsafe_test_target: bool,
}

impl Clone for Endpoint {
    fn clone(&self) -> Self {
        Self {
            base_url: self.base_url.clone(),
            client_connect_timeout: self.client_connect_timeout,
            client_pool: Arc::new(ClientPool::default()),
            #[cfg(any(test, feature = "test-util"))]
            allow_unsafe_test_target: self.allow_unsafe_test_target,
        }
    }
}

#[derive(Debug, Eq, PartialEq)]
struct ClientIdentity {
    scheme: String,
    host: String,
    port: u16,
    addresses: Vec<SocketAddr>,
    connect_timeout: Duration,
    allow_unsafe_target: bool,
}

struct CachedClient {
    identity: ClientIdentity,
    client: Client,
    validated_until: Instant,
}

#[derive(Default)]
struct ClientPool {
    state: Mutex<Option<CachedClient>>,
    #[cfg(test)]
    builds: std::sync::atomic::AtomicUsize,
}

impl fmt::Debug for Endpoint {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Endpoint")
            .field("scheme", &self.base_url.scheme())
            .field("host", &self.base_url.host_str())
            .field("port", &self.base_url.port())
            .field("path", &"[REDACTED]")
            .finish_non_exhaustive()
    }
}

impl Default for Endpoint {
    fn default() -> Self {
        Self::parse(DEFAULT_BASE_URL).expect("the built-in Gemini endpoint is valid")
    }
}

impl Endpoint {
    pub(crate) fn parse(value: &str) -> Result<Self, EndpointError> {
        Self::parse_with_policy(value, false)
    }

    fn parse_with_policy(value: &str, allow_unsafe_target: bool) -> Result<Self, EndpointError> {
        let mut base_url =
            Url::parse(value).map_err(|error| EndpointError::InvalidUrl(error.to_string()))?;
        if base_url.scheme() != "https" && !allow_unsafe_target {
            return Err(EndpointError::HttpsRequired);
        }
        if !matches!(base_url.scheme(), "http" | "https") {
            return Err(EndpointError::UnsupportedScheme);
        }
        if !base_url.username().is_empty() || base_url.password().is_some() {
            return Err(EndpointError::UserInfoForbidden);
        }
        if base_url.host().is_none() {
            return Err(EndpointError::MissingHost);
        }
        if base_url.port() == Some(0) {
            return Err(EndpointError::InvalidPort);
        }
        if base_url.query().is_some() || base_url.fragment().is_some() {
            return Err(EndpointError::QueryOrFragmentForbidden);
        }
        if !base_url.path().ends_with('/') {
            let normalized = format!("{}/", base_url.path());
            base_url.set_path(&normalized);
        }
        if let Some(ip) = literal_ip(&base_url)
            && !allow_unsafe_target
        {
            validate_public_ip(ip)?;
        }
        Ok(Self {
            base_url,
            client_connect_timeout: DEFAULT_CONNECT_TIMEOUT,
            client_pool: Arc::new(ClientPool::default()),
            #[cfg(any(test, feature = "test-util"))]
            allow_unsafe_test_target: allow_unsafe_target,
        })
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(crate) fn for_local_test(value: &str) -> Self {
        Self::parse_with_policy(value, true).expect("local test endpoint must be valid")
    }

    pub(crate) fn generate_url(
        &self,
        provider_model: &str,
        streaming: bool,
    ) -> Result<Url, EndpointError> {
        let action = if streaming {
            "streamGenerateContent"
        } else {
            "generateContent"
        };
        let mut url = self.model_action_url(provider_model, action)?;
        if streaming {
            url.query_pairs_mut().append_pair("alt", "sse");
        }
        Ok(url)
    }

    pub(crate) fn count_tokens_url(&self, provider_model: &str) -> Result<Url, EndpointError> {
        self.model_action_url(provider_model, "countTokens")
    }

    pub(crate) fn models_url(&self) -> Result<Url, EndpointError> {
        self.base_url
            .join("models")
            .map_err(|error| EndpointError::InvalidUrl(error.to_string()))
    }

    pub(crate) fn set_connect_timeout(&mut self, value: Duration) {
        self.client_connect_timeout = value;
    }

    fn model_action_url(&self, provider_model: &str, action: &str) -> Result<Url, EndpointError> {
        let model = provider_model
            .strip_prefix("models/")
            .unwrap_or(provider_model);
        let segments = model.split('/').collect::<Vec<_>>();
        if segments.is_empty()
            || segments
                .iter()
                .any(|segment| segment.is_empty() || matches!(*segment, "." | ".."))
            || provider_model.chars().any(char::is_control)
        {
            return Err(EndpointError::InvalidModelName);
        }
        let mut url = self.base_url.clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|()| EndpointError::CannotBeBase)?;
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

    pub(crate) async fn pinned_client(
        &self,
        connect_timeout: Duration,
    ) -> Result<Client, EndpointError> {
        self.pinned_client_with_resolver(connect_timeout, |host, port| async move {
            let resolved = lookup_host((host.as_str(), port)).await?;
            Ok(resolved.collect::<BTreeSet<_>>().into_iter().collect())
        })
        .await
    }

    async fn pinned_client_with_resolver<Resolve, ResolveFuture>(
        &self,
        connect_timeout: Duration,
        resolve: Resolve,
    ) -> Result<Client, EndpointError>
    where
        Resolve: FnOnce(String, u16) -> ResolveFuture,
        ResolveFuture: Future<Output = Result<Vec<SocketAddr>, std::io::Error>>,
    {
        timeout(connect_timeout, self.pinned_client_inner(resolve))
            .await
            .map_err(|_| EndpointError::DnsTimeout)?
    }

    async fn pinned_client_inner<Resolve, ResolveFuture>(
        &self,
        resolve: Resolve,
    ) -> Result<Client, EndpointError>
    where
        Resolve: FnOnce(String, u16) -> ResolveFuture,
        ResolveFuture: Future<Output = Result<Vec<SocketAddr>, std::io::Error>>,
    {
        let mut cached = self.client_pool.state.lock().await;
        let now = Instant::now();
        if let Some(entry) = cached.as_ref()
            && entry.validated_until > now
            && entry.identity.connect_timeout == self.client_connect_timeout
        {
            return Ok(entry.client.clone());
        }

        let host = self
            .base_url
            .host_str()
            .ok_or(EndpointError::MissingHost)?
            .to_owned();
        let port = self
            .base_url
            .port_or_known_default()
            .ok_or(EndpointError::MissingPort)?;
        let addresses = if let Some(ip) = literal_ip(&self.base_url) {
            vec![SocketAddr::new(ip, port)]
        } else {
            resolve(host.clone(), port)
                .await
                .map_err(EndpointError::DnsResolution)?
                .into_iter()
                .collect::<BTreeSet<_>>()
                .into_iter()
                .collect::<Vec<_>>()
        };
        if addresses.is_empty() {
            return Err(EndpointError::NoAddresses);
        }
        #[cfg(any(test, feature = "test-util"))]
        let allow_unsafe_target = self.allow_unsafe_test_target;
        #[cfg(not(any(test, feature = "test-util")))]
        let allow_unsafe_target = false;
        if !allow_unsafe_target {
            for address in &addresses {
                validate_public_ip(address.ip())?;
            }
        }
        let identity = ClientIdentity {
            scheme: self.base_url.scheme().to_owned(),
            host: host.clone(),
            port,
            addresses: addresses.clone(),
            connect_timeout: self.client_connect_timeout,
            allow_unsafe_target,
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
            .connect_timeout(self.client_connect_timeout)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(MAX_IDLE_CONNECTIONS_PER_HOST)
            .tcp_nodelay(true)
            .referer(false)
            .user_agent("openllmproxy");
        if !allow_unsafe_target {
            builder = builder.https_only(true);
        }
        if literal_ip(&self.base_url).is_none() {
            builder = builder.resolve_to_addrs(&host, &addresses);
        }
        let client = builder.build().map_err(EndpointError::ClientBuild)?;
        #[cfg(test)]
        self.client_pool
            .builds
            .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        *cached = Some(CachedClient {
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

fn literal_ip(url: &Url) -> Option<IpAddr> {
    url.host_str()?
        .trim_start_matches('[')
        .trim_end_matches(']')
        .parse()
        .ok()
}

pub(crate) fn validate_public_ip(address: IpAddr) -> Result<(), EndpointError> {
    if !is_public_ip(address) {
        return Err(EndpointError::ForbiddenAddress(address));
    }
    Ok(())
}

#[derive(Debug, Error)]
pub enum EndpointError {
    #[error("custom Gemini endpoints must use HTTPS")]
    HttpsRequired,
    #[error("custom Gemini endpoint scheme must be HTTP or HTTPS")]
    UnsupportedScheme,
    #[error("custom Gemini endpoints cannot contain user information")]
    UserInfoForbidden,
    #[error("custom Gemini endpoint must include a host")]
    MissingHost,
    #[error("custom Gemini endpoint must have a known or explicit port")]
    MissingPort,
    #[error("custom Gemini endpoint port must be greater than zero")]
    InvalidPort,
    #[error("custom Gemini endpoints cannot contain a query or fragment")]
    QueryOrFragmentForbidden,
    #[error("custom Gemini endpoint URL is invalid: {0}")]
    InvalidUrl(String),
    #[error("Gemini provider model name is invalid")]
    InvalidModelName,
    #[error("custom Gemini endpoint cannot be used as a URL base")]
    CannotBeBase,
    #[error("custom Gemini endpoint resolves to forbidden address {0}")]
    ForbiddenAddress(IpAddr),
    #[error("custom Gemini endpoint DNS resolution timed out")]
    DnsTimeout,
    #[error("custom Gemini endpoint DNS resolution failed")]
    DnsResolution(#[source] std::io::Error),
    #[error("custom Gemini endpoint did not resolve to an address")]
    NoAddresses,
    #[error("failed to build the pinned Gemini HTTP client")]
    ClientBuild(#[source] reqwest::Error),
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn endpoint_policy_and_action_paths_are_safe() {
        assert!(matches!(
            Endpoint::parse("http://generativelanguage.googleapis.com/v1beta"),
            Err(EndpointError::HttpsRequired)
        ));
        assert!(matches!(
            Endpoint::parse("https://key@googleapis.com/v1beta"),
            Err(EndpointError::UserInfoForbidden)
        ));
        assert!(matches!(
            Endpoint::parse("https://googleapis.com/v1beta?key=ambient"),
            Err(EndpointError::QueryOrFragmentForbidden)
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
            Err(EndpointError::InvalidModelName)
        ));
    }

    #[test]
    fn endpoint_debug_redacts_path_and_private_targets_are_blocked() {
        let endpoint = Endpoint::parse("https://example.com/private-token/v1beta").unwrap();
        let debug = format!("{endpoint:?}");
        assert!(!debug.contains("private-token"));
        assert!(debug.contains("REDACTED"));
        for address in [
            "10.0.0.1",
            "127.0.0.1",
            "169.254.169.254",
            "::1",
            "::ffff:127.0.0.1",
            "fc00::1",
            "64:ff9b::7f00:1",
            "2001:db8::1",
        ] {
            let address = address.parse().unwrap();
            assert!(matches!(
                validate_public_ip(address),
                Err(EndpointError::ForbiddenAddress(blocked)) if blocked == address
            ));
        }
        validate_public_ip("8.8.8.8".parse().unwrap()).unwrap();
    }

    #[tokio::test]
    async fn pool_reuses_unchanged_dns_but_isolates_clones_and_rejects_rebinding() {
        let endpoint = Endpoint::parse("https://pool.example/v1beta").unwrap();
        let public = SocketAddr::new("8.8.8.8".parse().unwrap(), 443);
        for preparation_budget in [Duration::from_secs(1), Duration::from_millis(500)] {
            endpoint
                .pinned_client_with_resolver(preparation_budget, move |_, _| async move {
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
            Err(EndpointError::ForbiddenAddress(address)) if address.is_loopback()
        ));
        assert_eq!(endpoint.client_builds_for_test(), 1);
    }
}

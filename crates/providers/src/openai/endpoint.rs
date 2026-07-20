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

const DEFAULT_OPENAI_BASE_URL: &str = "https://api.openai.com/v1/";
const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
// A DNS answer is trusted only for this bounded interval. Connections are
// pinned to the validated answer set, so a re-resolution can never retarget an
// already-built client. This intentionally does not use the system resolver's
// potentially unbounded cache lifetime.
const DNS_REVALIDATION_INTERVAL: Duration = Duration::from_secs(30);
// Keep enough warm HTTP/1.1 connections for high-throughput compatible
// endpoints while ensuring an unused pool cannot retain sockets indefinitely.
const POOL_IDLE_TIMEOUT: Duration = Duration::from_secs(30);
const MAX_IDLE_CONNECTIONS_PER_HOST: usize = 256;

pub(crate) struct Endpoint {
    base_url: Url,
    fixed_query: Option<(String, String)>,
    client_connect_timeout: Duration,
    client_pool: Arc<ClientPool>,
    #[cfg(any(test, feature = "test-util"))]
    allow_unsafe_test_target: bool,
}

// Cloning configuration must not accidentally make independently configured
// providers share a transport pool. A connector reuses its own Endpoint, while
// a cloned ConnectorConfig receives a new, credential-neutral pool.
impl Clone for Endpoint {
    fn clone(&self) -> Self {
        Self {
            base_url: self.base_url.clone(),
            fixed_query: self.fixed_query.clone(),
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
        Self::parse(DEFAULT_OPENAI_BASE_URL).expect("the built-in OpenAI endpoint is valid")
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
            let normalized_path = format!("{}/", base_url.path());
            base_url.set_path(&normalized_path);
        }

        if let Some(ip) = literal_ip(&base_url)
            && !allow_unsafe_target
        {
            validate_public_ip(ip)?;
        }

        Ok(Self {
            base_url,
            fixed_query: None,
            client_connect_timeout: DEFAULT_CONNECT_TIMEOUT,
            client_pool: Arc::new(ClientPool::default()),
            #[cfg(any(test, feature = "test-util"))]
            allow_unsafe_test_target: allow_unsafe_target,
        })
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(crate) fn for_local_test(value: &str) -> Self {
        Self::parse_with_policy(value, true).expect("local test endpoint must be a valid HTTP URL")
    }

    pub(crate) fn resource_url(&self, path: &str) -> Result<Url, EndpointError> {
        if path.starts_with('/') || path.contains("..") || path.contains(['\\', '?', '#']) {
            return Err(EndpointError::InvalidResourcePath);
        }
        let mut url = self
            .base_url
            .join(path)
            .map_err(|error| EndpointError::InvalidUrl(error.to_string()))?;
        if url.origin() != self.base_url.origin() || !url.path().starts_with(self.base_url.path()) {
            return Err(EndpointError::InvalidResourcePath);
        }
        if let Some((name, value)) = &self.fixed_query {
            url.query_pairs_mut().append_pair(name, value);
        }
        Ok(url)
    }

    pub(crate) fn set_api_version(&mut self, value: &str) -> Result<(), EndpointError> {
        if value.is_empty()
            || value.len() > 128
            || !value
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
        {
            return Err(EndpointError::InvalidApiVersion);
        }
        self.fixed_query = Some(("api-version".into(), value.into()));
        Ok(())
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(crate) fn set_connect_timeout(&mut self, value: Duration) {
        self.client_connect_timeout = value;
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
        // Serialize cache refreshes. The public caller bounds this entire
        // critical section by the connect deadline, including waiting behind
        // another refresh, so contention cannot escape the attempt budget.
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
                .collect()
        };

        if addresses.is_empty() {
            return Err(EndpointError::NoAddresses);
        }

        #[cfg(any(test, feature = "test-util"))]
        let allow_unsafe_target = self.allow_unsafe_test_target;
        #[cfg(not(any(test, feature = "test-util")))]
        let allow_unsafe_target = false;

        if !allow_unsafe_target {
            validate_resolved_addresses(&addresses)?;
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
            // The immutable origin and complete DNS answer set still match.
            // Refresh validation without discarding its live connection pool.
            entry.validated_until = validated_until;
            return Ok(entry.client.clone());
        }

        let mut builder = Client::builder()
            .redirect(Policy::none())
            // Routing owns the attempt budget. Reqwest 0.13 otherwise retries
            // selected protocol NACKs implicitly.
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

fn validate_resolved_addresses(addresses: &[SocketAddr]) -> Result<(), EndpointError> {
    for address in addresses {
        validate_public_ip(address.ip())?;
    }
    Ok(())
}

#[derive(Debug, Error)]
pub enum EndpointError {
    #[error("custom OpenAI endpoints must use HTTPS")]
    HttpsRequired,
    #[error("custom OpenAI endpoint scheme must be HTTP or HTTPS")]
    UnsupportedScheme,
    #[error("custom OpenAI endpoints cannot contain user information")]
    UserInfoForbidden,
    #[error("custom OpenAI endpoint must include a host")]
    MissingHost,
    #[error("custom OpenAI endpoint must have a known or explicit port")]
    MissingPort,
    #[error("custom OpenAI endpoint port must be greater than zero")]
    InvalidPort,
    #[error("custom OpenAI endpoints cannot contain a query or fragment")]
    QueryOrFragmentForbidden,
    #[error("custom OpenAI endpoint URL is invalid: {0}")]
    InvalidUrl(String),
    #[error("OpenAI resource path is invalid")]
    InvalidResourcePath,
    #[error("OpenAI API version is invalid")]
    InvalidApiVersion,
    #[error("custom OpenAI endpoint resolves to forbidden address {0}")]
    ForbiddenAddress(IpAddr),
    #[error("custom OpenAI endpoint DNS resolution timed out")]
    DnsTimeout,
    #[error("custom OpenAI endpoint DNS resolution failed")]
    DnsResolution(#[source] std::io::Error),
    #[error("custom OpenAI endpoint did not resolve to an address")]
    NoAddresses,
    #[error("failed to build the pinned OpenAI HTTP client")]
    ClientBuild(#[source] reqwest::Error),
}

#[cfg(test)]
mod tests {
    use std::net::{Ipv4Addr, Ipv6Addr};
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };

    use super::*;

    #[test]
    fn endpoint_requires_https_and_forbids_ambient_authority() {
        assert!(matches!(
            Endpoint::parse("http://api.openai.com/v1"),
            Err(EndpointError::HttpsRequired)
        ));
        assert!(matches!(
            Endpoint::parse("https://user:secret@api.openai.com/v1"),
            Err(EndpointError::UserInfoForbidden)
        ));
        assert!(matches!(
            Endpoint::parse("https://api.openai.com/v1?redirect=1"),
            Err(EndpointError::QueryOrFragmentForbidden)
        ));
    }

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
            Err(EndpointError::InvalidResourcePath)
        ));
        assert!(matches!(
            endpoint.resource_url(r"videos\job-id"),
            Err(EndpointError::InvalidResourcePath)
        ));
        assert!(matches!(
            endpoint.resource_url("%2e%2e/credentials"),
            Err(EndpointError::InvalidResourcePath)
        ));
    }

    #[test]
    fn endpoint_debug_redacts_path_embedded_credentials() {
        let endpoint = Endpoint::parse("https://example.com/private-token/v1").unwrap();
        let debug = format!("{endpoint:?}");

        assert!(!debug.contains("private-token"));
        assert!(debug.contains("[REDACTED]"));
    }

    #[test]
    fn blocks_internal_metadata_and_non_routable_addresses() {
        for address in [
            "0.0.0.0",
            "10.0.0.1",
            "100.64.0.1",
            "127.0.0.1",
            "169.254.169.254",
            "172.16.0.1",
            "192.88.99.1",
            "192.168.0.1",
            "224.0.0.1",
            "255.255.255.255",
            "::",
            "::1",
            "::ffff:127.0.0.1",
            "fc00::1",
            "fe80::1",
            "fec0::1",
            "ff02::1",
            "64:ff9b::7f00:1",
            "64:ff9b:1::1",
            "100::1",
            "2001::1",
            "2001:db8::1",
            "2002:7f00:1::1",
            "3fff::1",
            "5f00::1",
        ] {
            let address: IpAddr = address.parse().unwrap();
            assert!(
                matches!(
                    validate_public_ip(address),
                    Err(EndpointError::ForbiddenAddress(blocked)) if blocked == address
                ),
                "address {address} was not blocked"
            );
        }
    }

    #[test]
    fn accepts_public_addresses() {
        for address in ["8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"] {
            validate_public_ip(address.parse().unwrap()).unwrap();
        }
    }

    #[test]
    fn rejects_a_dns_answer_set_if_any_address_is_private() {
        let addresses = [
            SocketAddr::new("8.8.8.8".parse().unwrap(), 443),
            SocketAddr::new("127.0.0.1".parse().unwrap(), 443),
        ];
        assert!(matches!(
            validate_resolved_addresses(&addresses),
            Err(EndpointError::ForbiddenAddress(address)) if address.is_loopback()
        ));
    }

    #[test]
    fn literal_private_targets_are_rejected_before_dns() {
        assert!(matches!(
            Endpoint::parse("https://169.254.169.254/latest/meta-data"),
            Err(EndpointError::ForbiddenAddress(address))
                if address == IpAddr::V4(Ipv4Addr::new(169, 254, 169, 254))
        ));
        assert!(matches!(
            Endpoint::parse("https://[::1]/v1"),
            Err(EndpointError::ForbiddenAddress(address)) if address == IpAddr::V6(Ipv6Addr::LOCALHOST)
        ));
    }

    #[tokio::test]
    async fn reuses_one_client_until_dns_revalidation_and_across_unchanged_answers() {
        let endpoint = Endpoint::parse("https://pool.example/v1").unwrap();
        let resolutions = Arc::new(AtomicUsize::new(0));
        let public = SocketAddr::new("8.8.8.8".parse().unwrap(), 443);

        for preparation_budget in [Duration::from_secs(1), Duration::from_millis(500)] {
            let resolutions = Arc::clone(&resolutions);
            endpoint
                .pinned_client_with_resolver(preparation_budget, move |_, _| async move {
                    resolutions.fetch_add(1, Ordering::Relaxed);
                    Ok(vec![public])
                })
                .await
                .unwrap();
        }
        assert_eq!(resolutions.load(Ordering::Relaxed), 1);
        assert_eq!(endpoint.client_builds_for_test(), 1);

        endpoint.expire_cached_dns_for_test().await;
        let resolutions_after_expiry = Arc::clone(&resolutions);
        endpoint
            .pinned_client_with_resolver(Duration::from_secs(1), move |_, _| async move {
                resolutions_after_expiry.fetch_add(1, Ordering::Relaxed);
                Ok(vec![public])
            })
            .await
            .unwrap();
        assert_eq!(resolutions.load(Ordering::Relaxed), 2);
        assert_eq!(endpoint.client_builds_for_test(), 1);
    }

    #[tokio::test]
    async fn cloned_provider_configurations_have_isolated_client_pools() {
        let endpoint = Endpoint::parse("https://pool.example/v1").unwrap();
        let other_provider = endpoint.clone();
        let public = SocketAddr::new("8.8.8.8".parse().unwrap(), 443);

        endpoint
            .pinned_client_with_resolver(Duration::from_secs(1), move |_, _| async move {
                Ok(vec![public])
            })
            .await
            .unwrap();
        other_provider
            .pinned_client_with_resolver(Duration::from_secs(1), move |_, _| async move {
                Ok(vec![public])
            })
            .await
            .unwrap();

        assert_eq!(endpoint.client_builds_for_test(), 1);
        assert_eq!(other_provider.client_builds_for_test(), 1);
        assert!(!Arc::ptr_eq(
            &endpoint.client_pool,
            &other_provider.client_pool
        ));
    }

    #[tokio::test]
    async fn dns_rebinding_to_a_forbidden_address_fails_closed() {
        let endpoint = Endpoint::parse("https://pool.example/v1").unwrap();
        endpoint
            .pinned_client_with_resolver(Duration::from_secs(1), |_, _| async move {
                Ok(vec![SocketAddr::new("8.8.8.8".parse().unwrap(), 443)])
            })
            .await
            .unwrap();
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
        // A forbidden refresh never replaces the old validated identity and,
        // crucially, the expired old client is not returned as a fallback.
        assert_eq!(endpoint.client_builds_for_test(), 1);

        endpoint
            .pinned_client_with_resolver(Duration::from_secs(1), |_, _| async move {
                Ok(vec![SocketAddr::new("1.1.1.1".parse().unwrap(), 443)])
            })
            .await
            .unwrap();
        assert_eq!(endpoint.client_builds_for_test(), 2);
    }
}

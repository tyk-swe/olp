use std::{
    collections::BTreeSet,
    future::Future,
    net::{IpAddr, SocketAddr},
    time::Duration,
};

use reqwest::{Client, Url, redirect::Policy};
use thiserror::Error;
use tokio::{net::lookup_host, sync::Mutex, time::Instant, time::timeout};

use super::is_public_ip;

const DNS_REVALIDATION_INTERVAL: Duration = Duration::from_secs(30);

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct PinnedClientConfig {
    pub(crate) connect_timeout: Duration,
    pub(crate) pool_idle_timeout: Option<Duration>,
    pub(crate) pool_max_idle_per_host: Option<usize>,
    pub(crate) allow_unsafe_target: bool,
    pub(crate) user_agent: &'static str,
}

#[derive(Debug, Error)]
pub(crate) enum PinnedClientError {
    #[error("endpoint URL does not contain a host")]
    MissingHost,
    #[error("endpoint URL does not have a known or explicit port")]
    MissingPort,
    #[error("endpoint DNS resolution timed out")]
    DnsTimeout,
    #[error("endpoint DNS resolution failed")]
    DnsResolution(#[source] std::io::Error),
    #[error("endpoint did not resolve to an address")]
    NoAddresses,
    #[error("endpoint resolves to forbidden address {0}")]
    ForbiddenAddress(IpAddr),
    #[error("failed to build pinned HTTP client")]
    ClientBuild(#[source] reqwest::Error),
}

#[derive(Debug, Eq, PartialEq)]
struct ClientIdentity {
    scheme: String,
    host: String,
    port: u16,
    addresses: Vec<SocketAddr>,
    config: PinnedClientConfig,
}

struct CachedClient {
    identity: ClientIdentity,
    client: Client,
    validated_until: Instant,
}

#[derive(Default)]
pub(crate) struct PinnedClientPool {
    state: Mutex<Option<CachedClient>>,
    #[cfg(test)]
    builds: std::sync::atomic::AtomicUsize,
}

// Provider configuration clones must never share a client cache or transport
// pool. Reqwest Client clones returned by `client` still share within one
// provider instance, as intended.
impl Clone for PinnedClientPool {
    fn clone(&self) -> Self {
        Self::default()
    }
}

impl PinnedClientPool {
    pub(crate) async fn client(
        &self,
        url: &Url,
        preparation_timeout: Duration,
        config: PinnedClientConfig,
    ) -> Result<Client, PinnedClientError> {
        timeout(
            preparation_timeout,
            self.client_inner(url, config, |host, port| async move {
                let resolved = lookup_host((host.as_str(), port)).await?;
                Ok(resolved.collect())
            }),
        )
        .await
        .map_err(|_| PinnedClientError::DnsTimeout)?
    }

    async fn client_inner<Resolve, ResolveFuture>(
        &self,
        url: &Url,
        config: PinnedClientConfig,
        resolve: Resolve,
    ) -> Result<Client, PinnedClientError>
    where
        Resolve: FnOnce(String, u16) -> ResolveFuture,
        ResolveFuture: Future<Output = Result<Vec<SocketAddr>, std::io::Error>>,
    {
        // Serialize refreshes. The caller bounds this entire critical section,
        // including contention, by its connection-preparation budget.
        let mut cached = self.state.lock().await;
        let target = Target::from_url(url)?;
        let now = Instant::now();
        if let Some(entry) = cached.as_ref()
            && entry.validated_until > now
            && entry.identity.matches(&target, config)
        {
            return Ok(entry.client.clone());
        }

        let addresses = target.resolve(resolve).await?;
        validate_addresses(&addresses, config.allow_unsafe_target)?;

        let identity = ClientIdentity {
            scheme: target.scheme.clone(),
            host: target.host.clone(),
            port: target.port,
            addresses: addresses.clone(),
            config,
        };
        let validated_until = Instant::now() + DNS_REVALIDATION_INTERVAL;
        if let Some(entry) = cached.as_mut()
            && entry.identity == identity
        {
            // The immutable origin and complete DNS answer set still match, so
            // retain live connections while renewing the validation deadline.
            entry.validated_until = validated_until;
            return Ok(entry.client.clone());
        }

        let client = build_client(&target, &addresses, config)?;
        #[cfg(test)]
        self.builds
            .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        *cached = Some(CachedClient {
            identity,
            client: client.clone(),
            validated_until,
        });
        Ok(client)
    }

    #[cfg(test)]
    async fn client_with_resolver<Resolve, ResolveFuture>(
        &self,
        url: &Url,
        preparation_timeout: Duration,
        config: PinnedClientConfig,
        resolve: Resolve,
    ) -> Result<Client, PinnedClientError>
    where
        Resolve: FnOnce(String, u16) -> ResolveFuture,
        ResolveFuture: Future<Output = Result<Vec<SocketAddr>, std::io::Error>>,
    {
        timeout(preparation_timeout, self.client_inner(url, config, resolve))
            .await
            .map_err(|_| PinnedClientError::DnsTimeout)?
    }

    #[cfg(test)]
    async fn expire_cached_dns_for_test(&self) {
        if let Some(cached) = self.state.lock().await.as_mut() {
            cached.validated_until = Instant::now();
        }
    }

    #[cfg(test)]
    fn client_builds_for_test(&self) -> usize {
        self.builds.load(std::sync::atomic::Ordering::Relaxed)
    }
}

impl ClientIdentity {
    fn matches(&self, target: &Target, config: PinnedClientConfig) -> bool {
        self.scheme == target.scheme
            && self.host == target.host
            && self.port == target.port
            && self.config == config
    }
}

struct Target {
    scheme: String,
    host: String,
    port: u16,
    literal_ip: Option<IpAddr>,
}

impl Target {
    fn from_url(url: &Url) -> Result<Self, PinnedClientError> {
        let host = url
            .host_str()
            .ok_or(PinnedClientError::MissingHost)?
            .to_owned();
        let port = url
            .port_or_known_default()
            .ok_or(PinnedClientError::MissingPort)?;
        Ok(Self {
            scheme: url.scheme().to_owned(),
            literal_ip: literal_ip(url),
            host,
            port,
        })
    }

    async fn resolve<Resolve, ResolveFuture>(
        &self,
        resolve: Resolve,
    ) -> Result<Vec<SocketAddr>, PinnedClientError>
    where
        Resolve: FnOnce(String, u16) -> ResolveFuture,
        ResolveFuture: Future<Output = Result<Vec<SocketAddr>, std::io::Error>>,
    {
        let addresses = if let Some(ip) = self.literal_ip {
            vec![SocketAddr::new(ip, self.port)]
        } else {
            resolve(self.host.clone(), self.port)
                .await
                .map_err(PinnedClientError::DnsResolution)?
        };
        normalize_addresses(addresses)
    }
}

/// Builds a fresh client around one validated DNS answer set. Unlike the
/// pooled path, only resolution is bounded by `resolution_timeout`; this keeps
/// OIDC's existing one-request timeout semantics intact.
pub(crate) async fn one_shot_client(
    url: &Url,
    resolution_timeout: Duration,
    config: PinnedClientConfig,
) -> Result<Client, PinnedClientError> {
    let target = Target::from_url(url)?;
    let addresses = if let Some(ip) = target.literal_ip {
        vec![SocketAddr::new(ip, target.port)]
    } else {
        timeout(
            resolution_timeout,
            lookup_host((target.host.as_str(), target.port)),
        )
        .await
        .map_err(|_| PinnedClientError::DnsTimeout)?
        .map_err(PinnedClientError::DnsResolution)?
        .collect()
    };
    let addresses = normalize_addresses(addresses)?;
    validate_addresses(&addresses, config.allow_unsafe_target)?;
    build_client(&target, &addresses, config)
}

fn normalize_addresses(addresses: Vec<SocketAddr>) -> Result<Vec<SocketAddr>, PinnedClientError> {
    let addresses = addresses
        .into_iter()
        .collect::<BTreeSet<_>>()
        .into_iter()
        .collect::<Vec<_>>();
    if addresses.is_empty() {
        return Err(PinnedClientError::NoAddresses);
    }
    Ok(addresses)
}

fn validate_addresses(
    addresses: &[SocketAddr],
    allow_unsafe_target: bool,
) -> Result<(), PinnedClientError> {
    if !allow_unsafe_target {
        for address in addresses {
            if !is_public_ip(address.ip()) {
                return Err(PinnedClientError::ForbiddenAddress(address.ip()));
            }
        }
    }
    Ok(())
}

fn build_client(
    target: &Target,
    addresses: &[SocketAddr],
    config: PinnedClientConfig,
) -> Result<Client, PinnedClientError> {
    let mut builder = Client::builder()
        .redirect(Policy::none())
        .retry(reqwest::retry::never())
        .no_proxy()
        .connect_timeout(config.connect_timeout)
        .tcp_nodelay(true)
        .referer(false)
        .user_agent(config.user_agent);
    if let Some(pool_idle_timeout) = config.pool_idle_timeout {
        builder = builder.pool_idle_timeout(pool_idle_timeout);
    }
    if let Some(maximum) = config.pool_max_idle_per_host {
        builder = builder.pool_max_idle_per_host(maximum);
    }
    if !config.allow_unsafe_target {
        builder = builder.https_only(true);
    }
    if target.literal_ip.is_none() {
        builder = builder.resolve_to_addrs(&target.host, addresses);
    }
    builder.build().map_err(PinnedClientError::ClientBuild)
}

pub(crate) fn literal_ip(url: &Url) -> Option<IpAddr> {
    url.host_str()?
        .trim_start_matches('[')
        .trim_end_matches(']')
        .parse()
        .ok()
}

#[cfg(test)]
mod tests {
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };

    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    use super::*;

    fn data_plane_config() -> PinnedClientConfig {
        PinnedClientConfig {
            connect_timeout: Duration::from_secs(1),
            pool_idle_timeout: Some(Duration::from_secs(30)),
            pool_max_idle_per_host: Some(256),
            allow_unsafe_target: false,
            user_agent: "openllmproxy",
        }
    }

    #[tokio::test]
    async fn cache_revalidates_dns_retains_unchanged_pools_and_isolates_clones() {
        let url = Url::parse("https://pool.example/v1").unwrap();
        let pool = PinnedClientPool::default();
        let resolutions = Arc::new(AtomicUsize::new(0));
        let public = SocketAddr::new("8.8.8.8".parse().unwrap(), 443);

        for preparation_timeout in [Duration::from_secs(1), Duration::from_millis(500)] {
            let resolutions = Arc::clone(&resolutions);
            pool.client_with_resolver(
                &url,
                preparation_timeout,
                data_plane_config(),
                move |_, _| async move {
                    resolutions.fetch_add(1, Ordering::Relaxed);
                    Ok(vec![public])
                },
            )
            .await
            .unwrap();
        }
        assert_eq!(resolutions.load(Ordering::Relaxed), 1);
        assert_eq!(pool.client_builds_for_test(), 1);

        let other_pool = pool.clone();
        let other_resolutions = Arc::new(AtomicUsize::new(0));
        let counted_resolutions = Arc::clone(&other_resolutions);
        other_pool
            .client_with_resolver(
                &url,
                Duration::from_secs(1),
                data_plane_config(),
                move |_, _| async move {
                    counted_resolutions.fetch_add(1, Ordering::Relaxed);
                    Ok(vec![public])
                },
            )
            .await
            .unwrap();
        assert_eq!(other_resolutions.load(Ordering::Relaxed), 1);
        assert_eq!(other_pool.client_builds_for_test(), 1);

        pool.expire_cached_dns_for_test().await;
        pool.client_with_resolver(
            &url,
            Duration::from_secs(1),
            data_plane_config(),
            |_, _| async move { Ok(vec![public]) },
        )
        .await
        .unwrap();
        assert_eq!(pool.client_builds_for_test(), 1);

        let mut changed_connect_timeout = data_plane_config();
        changed_connect_timeout.connect_timeout = Duration::from_secs(2);
        pool.client_with_resolver(
            &url,
            Duration::from_secs(1),
            changed_connect_timeout,
            |_, _| async move { Ok(vec![public]) },
        )
        .await
        .unwrap();
        assert_eq!(pool.client_builds_for_test(), 2);
    }

    #[tokio::test]
    async fn rebinding_or_one_unsafe_address_fails_closed_without_reviving_expired_client() {
        let url = Url::parse("https://pool.example/v1").unwrap();
        let pool = PinnedClientPool::default();
        let public = SocketAddr::new("8.8.8.8".parse().unwrap(), 443);
        pool.client_with_resolver(
            &url,
            Duration::from_secs(1),
            data_plane_config(),
            |_, _| async move { Ok(vec![public]) },
        )
        .await
        .unwrap();
        pool.expire_cached_dns_for_test().await;

        let rebound = pool
            .client_with_resolver(
                &url,
                Duration::from_secs(1),
                data_plane_config(),
                |_, _| async move {
                    Ok(vec![
                        public,
                        SocketAddr::new("127.0.0.1".parse().unwrap(), 443),
                    ])
                },
            )
            .await;
        assert!(matches!(
            rebound,
            Err(PinnedClientError::ForbiddenAddress(address)) if address.is_loopback()
        ));
        assert_eq!(pool.client_builds_for_test(), 1);

        pool.client_with_resolver(
            &url,
            Duration::from_secs(1),
            data_plane_config(),
            |_, _| async move { Ok(vec![SocketAddr::new("1.1.1.1".parse().unwrap(), 443)]) },
        )
        .await
        .unwrap();
        assert_eq!(pool.client_builds_for_test(), 2);
    }

    #[tokio::test]
    async fn preparation_budget_includes_waiting_for_a_cache_refresh() {
        let url = Url::parse("https://pool.example/v1").unwrap();
        let pool = Arc::new(PinnedClientPool::default());
        let refreshing = Arc::clone(&pool);
        let refresh = tokio::spawn(async move {
            refreshing
                .client_with_resolver(
                    &url,
                    Duration::from_secs(1),
                    data_plane_config(),
                    |_, _| async move {
                        tokio::time::sleep(Duration::from_millis(100)).await;
                        Ok(vec![SocketAddr::new("8.8.8.8".parse().unwrap(), 443)])
                    },
                )
                .await
        });
        tokio::time::sleep(Duration::from_millis(10)).await;

        let waiting = pool
            .client_with_resolver(
                &Url::parse("https://pool.example/v1").unwrap(),
                Duration::from_millis(5),
                data_plane_config(),
                |_, _| async move { unreachable!("the first refresh owns resolution") },
            )
            .await;
        assert!(matches!(waiting, Err(PinnedClientError::DnsTimeout)));
        refresh.await.unwrap().unwrap();
    }

    #[tokio::test]
    async fn hardened_client_does_not_follow_redirects_and_sets_its_user_agent() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let server = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.unwrap();
            let mut request = vec![0; 4096];
            let read = stream.read(&mut request).await.unwrap();
            request.truncate(read);
            stream
                .write_all(
                    b"HTTP/1.1 302 Found\r\nLocation: /redirected\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
                )
                .await
                .unwrap();
            request
        });
        let url = Url::parse(&format!("http://{address}/first")).unwrap();
        let client = one_shot_client(
            &url,
            Duration::from_secs(1),
            PinnedClientConfig {
                allow_unsafe_target: true,
                user_agent: "pinned-client-test",
                pool_idle_timeout: None,
                pool_max_idle_per_host: None,
                ..data_plane_config()
            },
        )
        .await
        .unwrap();
        let response = client.get(url).send().await.unwrap();
        assert_eq!(response.status(), reqwest::StatusCode::FOUND);

        let request = String::from_utf8(server.await.unwrap()).unwrap();
        assert!(request.contains("GET /first HTTP/1.1"));
        assert!(
            request
                .to_ascii_lowercase()
                .contains("user-agent: pinned-client-test")
        );
    }
}

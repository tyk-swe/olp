//! Trusted-proxy parsing and public-auth source attribution.

use std::{
    error::Error as StdError,
    net::{IpAddr, SocketAddr},
    str::FromStr,
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
    },
    time::{SystemTime, UNIX_EPOCH},
};

use axum::http::{HeaderMap, HeaderName};
use olp_storage::security::AuthHmacKey;

use crate::{Problem, RequestBoundaryState};

const UNCONFIGURED_PROXY_WARNING_INTERVAL_SECONDS: u64 = 60;
static LAST_UNCONFIGURED_PROXY_WARNING: AtomicU64 = AtomicU64::new(0);

/// A CIDR range whose peer addresses are allowed to provide a forwarding
/// chain for public-auth source attribution. Direct clients never control the
/// resolved source through `X-Forwarded-For`.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TrustedProxyCidr {
    network: ipnet::IpNet,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TrustedProxyCidrParseError {
    detail: &'static str,
}

impl std::fmt::Display for TrustedProxyCidrParseError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(self.detail)
    }
}

impl StdError for TrustedProxyCidrParseError {}

impl FromStr for TrustedProxyCidr {
    type Err = TrustedProxyCidrParseError;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        let value = value.trim();
        if !value.contains('/') {
            return Err(TrustedProxyCidrParseError {
                detail: "trusted proxy CIDRs must use address/prefix notation",
            });
        }
        let network = value.parse().map_err(|_| TrustedProxyCidrParseError {
            detail: "trusted proxy CIDR is invalid",
        })?;
        Ok(Self { network })
    }
}

impl TrustedProxyCidr {
    #[must_use]
    pub fn contains(&self, address: IpAddr) -> bool {
        self.network.contains(&address)
    }
}

fn forwarded_for_invalid() -> Problem {
    Problem::bad_request(
        "forwarded_for_invalid",
        "The trusted proxy supplied a malformed forwarding chain.",
    )
}

/// Resolves the source identity used exclusively for unauthenticated public
/// authentication admission. Production listeners attach the TCP peer via
/// `ConnectInfo`. Embeddings that omit it fail closed rather than silently
/// sharing a single global admission bucket.
pub(crate) fn public_auth_source(
    state: &RequestBoundaryState,
    headers: &HeaderMap,
    peer: Option<SocketAddr>,
) -> Result<String, Problem> {
    let peer = peer
        .map(|address| address.ip())
        .ok_or_else(|| Problem::service_unavailable("client_address_unavailable"))?;
    if !state.peer_is_trusted_proxy(peer) {
        if !state.trusted_proxies_configured() && headers.contains_key("x-forwarded-for") {
            warn_unconfigured_forwarded_for(peer);
        }
        // A direct client cannot influence admission by spoofing a forwarding
        // header; only its connected peer address is authoritative.
        return Ok(peer.to_string());
    }

    let forwarded_for = HeaderName::from_static("x-forwarded-for");
    let mut chain = Vec::new();
    for value in headers.get_all(forwarded_for).iter() {
        let value = value.to_str().map_err(|_| forwarded_for_invalid())?;
        for candidate in value.split(',') {
            let candidate = candidate.trim();
            if candidate.is_empty() {
                return Err(forwarded_for_invalid());
            }
            let address = candidate
                .parse::<IpAddr>()
                .map_err(|_| forwarded_for_invalid())?;
            chain.push(address);
        }
    }
    if chain.is_empty() {
        return Err(Problem::bad_request(
            "forwarded_for_required",
            "A trusted proxy must provide a forwarding chain for public authentication.",
        ));
    }
    chain
        .into_iter()
        .rev()
        .find(|address| !state.peer_is_trusted_proxy(*address))
        .map(|address| address.to_string())
        .ok_or_else(|| {
            Problem::bad_request(
                "forwarded_for_invalid",
                "The trusted proxy supplied a forwarding chain without a client address.",
            )
        })
}

fn warn_unconfigured_forwarded_for(peer: IpAddr) {
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
        .max(1);
    if claim_warning_slot(&LAST_UNCONFIGURED_PROXY_WARNING, now) {
        tracing::warn!(
            %peer,
            "ignored X-Forwarded-For because OLP_TRUSTED_PROXY_CIDRS is empty"
        );
    }
}

fn claim_warning_slot(last_warning: &AtomicU64, now: u64) -> bool {
    let mut previous = last_warning.load(Ordering::Relaxed);
    loop {
        if previous != 0
            && now.saturating_sub(previous) < UNCONFIGURED_PROXY_WARNING_INTERVAL_SECONDS
        {
            return false;
        }
        match last_warning.compare_exchange_weak(
            previous,
            now,
            Ordering::Relaxed,
            Ordering::Relaxed,
        ) {
            Ok(_) => return true,
            Err(observed) => previous = observed,
        }
    }
}

fn resolve_auth_source<'a>(
    state: &'a RequestBoundaryState,
    headers: &HeaderMap,
    peer: Option<SocketAddr>,
) -> Result<(String, &'a Arc<AuthHmacKey>), Problem> {
    let source = public_auth_source(state, headers, peer)?;
    let auth_hmac_key = &state.auth_hmac_key;
    Ok((source, auth_hmac_key))
}

pub(crate) fn public_auth_source_digest(
    state: &RequestBoundaryState,
    headers: &HeaderMap,
    peer: Option<SocketAddr>,
) -> Result<[u8; 32], Problem> {
    let (source, auth_hmac_key) = resolve_auth_source(state, headers, peer)?;
    Ok(auth_hmac_key.public_auth_source_digest(&source))
}

pub(crate) fn public_auth_source_target_digests(
    state: &RequestBoundaryState,
    headers: &HeaderMap,
    peer: Option<SocketAddr>,
    target: &str,
) -> Result<([u8; 32], [u8; 32]), Problem> {
    let (source, auth_hmac_key) = resolve_auth_source(state, headers, peer)?;
    Ok((
        auth_hmac_key.public_auth_source_digest(&source),
        auth_hmac_key.public_auth_source_target_digest(&source, target),
    ))
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::AtomicU64;

    use super::claim_warning_slot;

    #[test]
    fn unconfigured_proxy_warning_is_rate_limited() {
        let last_warning = AtomicU64::new(0);
        assert!(claim_warning_slot(&last_warning, 100));
        assert!(!claim_warning_slot(&last_warning, 159));
        assert!(claim_warning_slot(&last_warning, 160));
    }
}

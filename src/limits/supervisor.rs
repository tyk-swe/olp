use crate::limits::admission::{LimitOutagePolicy, ReloadableLimiter};
use crate::limits::distributed::DistributedLimiter;
use crate::settings::repository::LimitsValkeyUnavailablePolicy;
use sqlx::PgPool;
use std::time::Duration;
use tokio::sync::watch;
use tracing::{info, warn};
pub(crate) async fn limiter_supervisor(
    reloadable_limiter: ReloadableLimiter,
    valkey_url: String,
    limits_namespace: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    loop {
        if *shutdown.borrow() {
            return;
        }
        if let Some(limiter) = reloadable_limiter.current() {
            let healthy = matches!(
                tokio::time::timeout(Duration::from_secs(1), limiter.ping()).await,
                Ok(Ok(()))
            );
            if healthy {
                tokio::select! {
                    changed = shutdown.changed() => {
                        if changed.is_err() || *shutdown.borrow() {
                            return;
                        }
                    }
                    () = tokio::time::sleep(Duration::from_secs(5)) => {}
                }
                continue;
            }
            reloadable_limiter.clear();
            warn!("Valkey limiter health check failed; hard limits remain fail-closed");
        }

        match tokio::time::timeout(
            Duration::from_secs(3),
            DistributedLimiter::connect(&valkey_url, &limits_namespace),
        )
        .await
        {
            Ok(Ok(limiter)) => {
                reloadable_limiter.install(limiter);
                backoff = Duration::from_millis(100);
                info!("Valkey limiter connection is available");
            }
            Ok(Err(error)) => warn!(%error, "Valkey limiter connection failed"),
            Err(_) => warn!("Valkey limiter connection timed out"),
        }
        tokio::select! {
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return;
                }
            }
            () = tokio::time::sleep(backoff) => {}
        }
        backoff = (backoff * 2).min(Duration::from_secs(5));
    }
}

pub(crate) async fn load_limits_outage_policy(pool: &PgPool, limiter: &ReloadableLimiter) {
    match crate::settings::repository::limits_valkey_unavailable_policy(pool).await {
        Ok(policy) => {
            let policy = match policy {
                LimitsValkeyUnavailablePolicy::FailClosed => LimitOutagePolicy::FailClosed,
                LimitsValkeyUnavailablePolicy::FailOpen => LimitOutagePolicy::FailOpen,
            };
            if limiter.outage_policy() != policy {
                info!(?policy, "limits.valkey_unavailable policy applied");
                limiter.set_outage_policy(policy);
            }
        }
        Err(error) => {
            warn!(%error, "limits.valkey_unavailable policy load failed; keeping current")
        }
    }
}

pub(crate) async fn limits_policy_supervisor(
    pool: PgPool,
    limiter: ReloadableLimiter,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut interval = tokio::time::interval(Duration::from_secs(15));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => load_limits_outage_policy(&pool, &limiter).await,
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return;
                }
            }
        }
    }
}

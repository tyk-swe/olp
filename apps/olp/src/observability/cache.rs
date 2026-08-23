//! Background refresh and process-local observability snapshots.

use std::{
    sync::{Arc, RwLock},
    time::{Duration, Instant},
};

use axum::{http::HeaderName, response::Response};

use super::{
    metrics::collect_metrics,
    readiness::{HealthResponse, collect_readiness},
};
use crate::{bootstrap::mode_dependencies::ObservabilityState, public_http::problem::Problem};

const OBSERVABILITY_REFRESH_TIMEOUT: Duration = Duration::from_secs(4);
const OBSERVABILITY_READINESS_REFRESH_INTERVAL: Duration = Duration::from_secs(5);
const OBSERVABILITY_METRICS_REFRESH_INTERVAL: Duration = Duration::from_secs(15);
// Metrics are refreshed every fifteen seconds. Give a successful snapshot
// enough headroom for normal scheduler jitter and a single refresh timeout;
// otherwise a healthy metrics endpoint would mark itself stale for the last
// third of every refresh interval.
pub(crate) const OBSERVABILITY_SNAPSHOT_STALE_AFTER: Duration = Duration::from_secs(30);

/// Process-local snapshots used by the private observability listener. The
/// request path only reads these locks; all dependency I/O happens in the
/// background refresh task below.
#[derive(Clone, Default)]
pub(crate) struct ObservabilityCache {
    pub(crate) readiness: Arc<RwLock<Cached<HealthResponse>>>,
    pub(crate) metrics: Arc<RwLock<Cached<Arc<str>>>>,
}

#[derive(Clone)]
pub(crate) struct Cached<T> {
    pub(crate) value: Option<T>,
    pub(crate) last_attempt_at: Option<Instant>,
    pub(crate) last_success_at: Option<Instant>,
}

impl<T> Default for Cached<T> {
    fn default() -> Self {
        Self {
            value: None,
            last_attempt_at: None,
            last_success_at: None,
        }
    }
}

impl<T> Cached<T> {
    fn record(&mut self, value: Option<T>) {
        let now = Instant::now();
        self.last_attempt_at = Some(now);
        if let Some(value) = value {
            self.last_success_at = Some(now);
            self.value = Some(value);
        }
    }
}

pub(crate) type CachedReadiness = Cached<HealthResponse>;
pub(crate) type CachedMetrics = Cached<Arc<str>>;

impl ObservabilityCache {
    pub(crate) fn readiness(&self) -> CachedReadiness {
        self.readiness
            .read()
            .expect("observability readiness cache lock poisoned")
            .clone()
    }

    pub(super) fn metrics(&self) -> CachedMetrics {
        self.metrics
            .read()
            .expect("observability metrics cache lock poisoned")
            .clone()
    }

    fn record_readiness(&self, result: Result<HealthResponse, Problem>) {
        self.readiness
            .write()
            .expect("observability readiness cache lock poisoned")
            .record(result.ok());
    }

    pub(crate) fn record_metrics(&self, body: Option<String>) {
        self.metrics
            .write()
            .expect("observability metrics cache lock poisoned")
            .record(body.map(Arc::from));
    }
}

/// Refresh both snapshots immediately. Integration tests use this to prime the
/// cache before opening an observability listener; production servers should
/// use [`spawn_observability_cache`].
pub async fn refresh_observability_cache(state: &ObservabilityState) {
    tokio::join!(refresh_readiness_cache(state), refresh_metrics_cache(state));
}

/// Starts the background cache supervisor used by the private observability
/// listener. Readiness is refreshed every five seconds, while the more
/// expensive metrics rollups are refreshed every fifteen seconds.
pub fn spawn_observability_cache(
    state: ObservabilityState,
    mut shutdown: tokio::sync::watch::Receiver<bool>,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        refresh_observability_cache(&state).await;

        let mut readiness_interval =
            tokio::time::interval(OBSERVABILITY_READINESS_REFRESH_INTERVAL);
        let mut metrics_interval = tokio::time::interval(OBSERVABILITY_METRICS_REFRESH_INTERVAL);
        readiness_interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        metrics_interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        // `interval` ticks immediately. The initial synchronous refresh above
        // already populated both snapshots, so consume those first ticks.
        readiness_interval.tick().await;
        metrics_interval.tick().await;

        let readiness_state = state.clone();
        let mut readiness_shutdown = shutdown.clone();
        let readiness_refresh = async move {
            loop {
                tokio::select! {
                    _ = readiness_interval.tick() => refresh_readiness_cache(&readiness_state).await,
                    changed = readiness_shutdown.changed() => {
                        if changed.is_err() || *readiness_shutdown.borrow() {
                            return;
                        }
                    }
                }
            }
        };
        let metrics_refresh = async move {
            loop {
                tokio::select! {
                    _ = metrics_interval.tick() => refresh_metrics_cache(&state).await,
                    changed = shutdown.changed() => {
                        if changed.is_err() || *shutdown.borrow() {
                            return;
                        }
                    }
                }
            }
        };
        tokio::join!(readiness_refresh, metrics_refresh);
    })
}

async fn refresh_readiness_cache(state: &ObservabilityState) {
    let result =
        match tokio::time::timeout(OBSERVABILITY_REFRESH_TIMEOUT, collect_readiness(state)).await {
            Ok(result) => result,
            Err(_) => {
                tracing::warn!("observability readiness refresh timed out");
                Err(Problem::service_unavailable(
                    "observability_snapshot_timeout",
                ))
            }
        };
    state.observability.record_readiness(result);
}

async fn refresh_metrics_cache(state: &ObservabilityState) {
    let body = tokio::time::timeout(OBSERVABILITY_REFRESH_TIMEOUT, collect_metrics(state))
        .await
        .inspect_err(|_| tracing::warn!("observability metrics refresh timed out"))
        .ok();
    state.observability.record_metrics(body);
}

pub(super) fn snapshot_age_seconds(at: Option<Instant>, now: Instant) -> Option<u64> {
    at.map(|at| now.saturating_duration_since(at).as_secs())
}

pub(super) fn snapshot_is_current(
    last_success_at: Option<Instant>,
    last_attempt_at: Option<Instant>,
    now: Instant,
) -> bool {
    last_success_at
        .is_some_and(|at| now.saturating_duration_since(at) <= OBSERVABILITY_SNAPSHOT_STALE_AFTER)
        && last_success_at == last_attempt_at
}

pub(super) fn attach_snapshot_freshness(response: &mut Response, age: Option<u64>, fresh: bool) {
    let age = age
        .map(|age| age.to_string())
        .and_then(|age| axum::http::HeaderValue::from_str(&age).ok())
        .unwrap_or_else(|| axum::http::HeaderValue::from_static("unknown"));
    response.headers_mut().insert(
        HeaderName::from_static("x-olp-observability-snapshot-age-seconds"),
        age,
    );
    response.headers_mut().insert(
        HeaderName::from_static("x-olp-observability-snapshot-fresh"),
        axum::http::HeaderValue::from_static(if fresh { "1" } else { "0" }),
    );
}

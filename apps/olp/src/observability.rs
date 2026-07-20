//! Private health and metrics routing backed by asynchronously refreshed snapshots.

use std::{
    fmt::Write as _,
    sync::{Arc, RwLock},
    time::{Duration, Instant},
};

use axum::{
    BoxError, Router,
    http::HeaderName,
    response::{IntoResponse, Response},
    routing::get,
};
use olp_storage::{
    RequestMetadataConsumerStatus, RequestMetadataEmitter, RequestMetadataEpochHealth,
};
use serde::Serialize;
use tower::ServiceBuilder;
use utoipa::ToSchema;

use crate::{ApiState, Problem};

const OBSERVABILITY_CONCURRENCY_LIMIT: usize = 8;
const OBSERVABILITY_REQUEST_TIMEOUT: Duration = Duration::from_secs(2);
const OBSERVABILITY_REFRESH_TIMEOUT: Duration = Duration::from_secs(4);
const OBSERVABILITY_READINESS_REFRESH_INTERVAL: Duration = Duration::from_secs(5);
const OBSERVABILITY_METRICS_REFRESH_INTERVAL: Duration = Duration::from_secs(15);
// Metrics are refreshed every fifteen seconds. Give a successful snapshot
// enough headroom for normal scheduler jitter and a single refresh timeout;
// otherwise a healthy metrics endpoint would mark itself stale for the last
// third of every refresh interval.
pub(super) const OBSERVABILITY_SNAPSHOT_STALE_AFTER: Duration = Duration::from_secs(30);

/// Builds the private observability router. It exposes no console,
/// management, or inference routes.
pub fn observability_router(state: ApiState) -> Router {
    Router::new()
        .route("/health/live", get(live))
        .route("/health/ready", get(ready))
        .route("/metrics", get(metrics))
        .with_state(state)
        .layer(
            ServiceBuilder::new()
                .layer(axum::error_handling::HandleErrorLayer::new(
                    observability_service_error,
                ))
                .layer(tower::load_shed::LoadShedLayer::new())
                .layer(tower::limit::ConcurrencyLimitLayer::new(
                    OBSERVABILITY_CONCURRENCY_LIMIT,
                ))
                .layer(tower::timeout::TimeoutLayer::new(
                    OBSERVABILITY_REQUEST_TIMEOUT,
                )),
        )
}

async fn observability_service_error(error: BoxError) -> Problem {
    if error.is::<tower::timeout::error::Elapsed>() {
        Problem::new(
            axum::http::StatusCode::SERVICE_UNAVAILABLE,
            "observability_timeout",
            "Observability unavailable",
            "The observability request exceeded its deadline.",
        )
    } else {
        Problem::new(
            axum::http::StatusCode::SERVICE_UNAVAILABLE,
            "observability_overloaded",
            "Observability unavailable",
            "The observability listener is temporarily overloaded.",
        )
    }
}

#[derive(Clone, Serialize, ToSchema)]
pub(crate) struct HealthResponse {
    status: &'static str,
    generation: Option<u64>,
    database: &'static str,
    limits: &'static str,
    request_metadata_complete: bool,
    request_metadata_consumer: &'static str,
    request_metadata_consumer_pending_events: u64,
    request_metadata_consumer_lag_events: u64,
    request_metadata_consumer_oldest_pending_at: Option<chrono::DateTime<chrono::Utc>>,
    request_metadata_consumer_checked_at: Option<chrono::DateTime<chrono::Utc>>,
    request_metadata_consumer_heartbeat_age_seconds: Option<u64>,
    request_metadata_gateway_open_epochs: u64,
    request_metadata_gateway_unresolved_epochs: u64,
    request_metadata_historical_uncertain_gaps: u64,
    request_metadata_gateway_unresolved_event_lower_bound: u64,
    media_reconciliation: &'static str,
    media_reconciliation_pending: u64,
    media_reconciliation_stale: u64,
    media_reconciliation_failed: u64,
    media_reconciliation_unbound: u64,
    media_reconciliation_gaps_total: u64,
}

/// Process-local snapshots used by the private observability listener. The
/// request path only reads this lock; all dependency I/O happens in the
/// background refresh task below.
#[derive(Clone, Default)]
pub(super) struct ObservabilityCache {
    pub(super) readiness: Arc<RwLock<CachedReadiness>>,
    pub(super) metrics: Arc<RwLock<CachedMetrics>>,
}

#[derive(Clone, Default)]
pub(super) struct CachedReadiness {
    pub(super) result: Option<HealthResponse>,
    pub(super) last_attempt_at: Option<Instant>,
    pub(super) last_success_at: Option<Instant>,
}

#[derive(Clone, Default)]
pub(super) struct CachedMetrics {
    pub(super) body: Option<Arc<str>>,
    pub(super) last_attempt_at: Option<Instant>,
    pub(super) last_success_at: Option<Instant>,
}

impl ObservabilityCache {
    pub(super) fn readiness(&self) -> CachedReadiness {
        self.readiness
            .read()
            .expect("observability readiness cache lock poisoned")
            .clone()
    }

    fn metrics(&self) -> CachedMetrics {
        self.metrics
            .read()
            .expect("observability metrics cache lock poisoned")
            .clone()
    }

    fn record_readiness(&self, result: Result<HealthResponse, Problem>) {
        let now = Instant::now();
        let mut readiness = self
            .readiness
            .write()
            .expect("observability readiness cache lock poisoned");
        readiness.last_attempt_at = Some(now);
        if let Ok(result) = result {
            readiness.last_success_at = Some(now);
            readiness.result = Some(result);
        }
    }

    fn record_metrics(&self, body: String) {
        let now = Instant::now();
        let mut metrics = self
            .metrics
            .write()
            .expect("observability metrics cache lock poisoned");
        metrics.last_attempt_at = Some(now);
        metrics.last_success_at = Some(now);
        metrics.body = Some(Arc::from(body));
    }

    pub(super) fn record_metrics_failure(&self) {
        self.metrics
            .write()
            .expect("observability metrics cache lock poisoned")
            .last_attempt_at = Some(Instant::now());
    }
}

/// Refresh both snapshots immediately. This is public so embeddings and
/// integration tests can prime the cache before opening an observability
/// listener; production servers should use [`spawn_observability_cache`].
pub async fn refresh_observability_cache(state: &ApiState) {
    tokio::join!(refresh_readiness_cache(state), refresh_metrics_cache(state));
}

/// Starts the background cache supervisor used by the private observability
/// listener. Readiness is refreshed every five seconds, while the more
/// expensive metrics rollups are refreshed every fifteen seconds.
pub fn spawn_observability_cache(
    state: ApiState,
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

async fn refresh_readiness_cache(state: &ApiState) {
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

async fn refresh_metrics_cache(state: &ApiState) {
    match tokio::time::timeout(OBSERVABILITY_REFRESH_TIMEOUT, collect_metrics(state)).await {
        Ok(body) => state.observability.record_metrics(body),
        Err(_) => {
            tracing::warn!("observability metrics refresh timed out");
            state.observability.record_metrics_failure();
        }
    }
}

fn snapshot_age_seconds(at: Option<Instant>, now: Instant) -> Option<u64> {
    at.map(|at| now.saturating_duration_since(at).as_secs())
}

fn snapshot_is_fresh(at: Option<Instant>, now: Instant) -> bool {
    at.is_some_and(|at| now.saturating_duration_since(at) <= OBSERVABILITY_SNAPSHOT_STALE_AFTER)
}

fn snapshot_is_current(
    last_success_at: Option<Instant>,
    last_attempt_at: Option<Instant>,
    now: Instant,
) -> bool {
    snapshot_is_fresh(last_success_at, now) && last_success_at == last_attempt_at
}

fn cached_readiness_is_fresh(snapshot: &CachedReadiness, now: Instant) -> bool {
    snapshot_is_current(snapshot.last_success_at, snapshot.last_attempt_at, now)
}

pub(super) fn cached_readiness_from_snapshot(
    snapshot: &CachedReadiness,
    now: Instant,
) -> Result<HealthResponse, Problem> {
    if !cached_readiness_is_fresh(snapshot, now) {
        return Err(Problem::service_unavailable("observability_snapshot_stale"));
    }
    snapshot
        .result
        .clone()
        .ok_or_else(|| Problem::service_unavailable("observability_snapshot_unavailable"))
}

fn cached_metrics_is_fresh(snapshot: &CachedMetrics, now: Instant) -> bool {
    snapshot_is_current(snapshot.last_success_at, snapshot.last_attempt_at, now)
}

fn attach_snapshot_freshness(response: &mut Response, age: Option<u64>, fresh: bool) {
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

async fn live() -> axum::Json<HealthResponse> {
    axum::Json(HealthResponse {
        status: "ok",
        generation: None,
        database: "not_checked",
        limits: "not_checked",
        request_metadata_complete: true,
        request_metadata_consumer: "not_checked",
        request_metadata_consumer_pending_events: 0,
        request_metadata_consumer_lag_events: 0,
        request_metadata_consumer_oldest_pending_at: None,
        request_metadata_consumer_checked_at: None,
        request_metadata_consumer_heartbeat_age_seconds: None,
        request_metadata_gateway_open_epochs: 0,
        request_metadata_gateway_unresolved_epochs: 0,
        request_metadata_historical_uncertain_gaps: 0,
        request_metadata_gateway_unresolved_event_lower_bound: 0,
        media_reconciliation: "not_checked",
        media_reconciliation_pending: 0,
        media_reconciliation_stale: 0,
        media_reconciliation_failed: 0,
        media_reconciliation_unbound: 0,
        media_reconciliation_gaps_total: 0,
    })
}

async fn ready(axum::extract::State(state): axum::extract::State<ApiState>) -> Response {
    let now = Instant::now();
    let snapshot = state.observability.readiness();
    let fresh = cached_readiness_is_fresh(&snapshot, now);
    let mut response = match cached_readiness_from_snapshot(&snapshot, now) {
        Ok(health) => axum::Json(health).into_response(),
        Err(problem) => problem.into_response(),
    };
    attach_snapshot_freshness(
        &mut response,
        snapshot_age_seconds(snapshot.last_success_at, now),
        fresh,
    );
    response
}

async fn collect_readiness(state: &ApiState) -> Result<HealthResponse, Problem> {
    let generation = state.runtime.ordinal();
    let now = chrono::Utc::now();
    let unknown_consumer = RequestMetadataConsumerStatus::from_health(None, now);
    let (database, media_reconciliation, request_metadata_consumer, request_metadata_epochs) =
        if let Some(store) = &state.store {
            match store.ping().await {
                Ok(()) => {
                    let (media, consumer, epochs) = tokio::join!(
                        store.media_reconciliation_summary(now),
                        store.request_metadata_consumer_status(now),
                        store.request_metadata_gateway_epoch_health(),
                    );
                    let media =
                        media.map_err(|_| Problem::service_unavailable("database_unavailable"))?;
                    let consumer = consumer
                        .map_err(|_| Problem::service_unavailable("database_unavailable"))?;
                    let epochs =
                        epochs.map_err(|_| Problem::service_unavailable("database_unavailable"))?;
                    ("ok", Some(media), consumer, epochs)
                }
                Err(_) if state.mode.serves_gateway() && generation.is_some() => (
                    "unavailable_lkg",
                    None,
                    unknown_consumer,
                    RequestMetadataEpochHealth::default(),
                ),
                Err(_) => return Err(Problem::service_unavailable("database_unavailable")),
            }
        } else if state.mode.serves_control() {
            return Err(Problem::service_unavailable("database_not_configured"));
        } else {
            (
                "not_configured",
                None,
                unknown_consumer,
                RequestMetadataEpochHealth::default(),
            )
        };

    if state.mode.serves_gateway() {
        let snapshot = state.runtime.pin();
        if generation.is_none() {
            return Err(Problem::service_unavailable(
                "runtime_generation_unavailable",
            ));
        }
        if state.auth_hmac_key.is_none() {
            return Err(Problem::service_unavailable(
                "api_key_authentication_unavailable",
            ));
        }
        if !snapshot.has_all_transports() {
            return Err(Problem::service_unavailable(
                "provider_transport_unavailable",
            ));
        }
    }
    let limiter = state.limiter.get();
    let limits_healthy = if let Some(limiter) = &limiter {
        matches!(
            tokio::time::timeout(Duration::from_millis(500), limiter.ping()).await,
            Ok(Ok(()))
        )
    } else {
        false
    };
    let hard_limits_present = state
        .runtime
        .pin()
        .api_keys
        .values()
        .any(|key| key.limits.has_hard_limits());
    // Valkey loss degrades only requests whose keys declare hard limits. The
    // request path fails those keys closed, while unlimited keys remain safe to
    // serve from the immutable snapshot. Returning 503 here would remove the
    // whole gateway from a Kubernetes Service and incorrectly fail unlimited
    // traffic too.
    let degraded_limits = state.mode.serves_gateway() && hard_limits_present && !limits_healthy;
    let media_reconciliation_gaps = state.media_reconciliation_gap_count();
    let degraded_media = media_reconciliation
        .as_ref()
        .is_some_and(|summary| summary.stale > 0 || summary.failed > 0 || summary.unbound > 0)
        || media_reconciliation_gaps > 0;
    let local_request_metadata_complete = state
        .request_metadata
        .as_ref()
        .map_or(!state.mode.serves_gateway(), |request_metadata| {
            request_metadata.snapshot().complete()
        });
    let request_metadata_complete = local_request_metadata_complete
        && request_metadata_consumer.complete()
        && request_metadata_epochs.unresolved_epochs == 0;
    Ok(HealthResponse {
        status: if degraded_limits || degraded_media || !request_metadata_complete {
            "degraded"
        } else {
            "ok"
        },
        generation,
        database,
        limits: if limits_healthy {
            "ok"
        } else if state.limiter.is_configured() {
            "unavailable"
        } else {
            "not_configured"
        },
        request_metadata_complete,
        request_metadata_consumer: request_metadata_consumer.state.as_str(),
        request_metadata_consumer_pending_events: request_metadata_consumer.pending_events,
        request_metadata_consumer_lag_events: request_metadata_consumer.lag_events,
        request_metadata_consumer_oldest_pending_at: request_metadata_consumer.oldest_pending_at,
        request_metadata_consumer_checked_at: request_metadata_consumer.checked_at,
        request_metadata_consumer_heartbeat_age_seconds: request_metadata_consumer
            .heartbeat_age_seconds,
        request_metadata_gateway_open_epochs: request_metadata_epochs.open_epochs,
        request_metadata_gateway_unresolved_epochs: request_metadata_epochs.unresolved_epochs,
        request_metadata_historical_uncertain_gaps: request_metadata_epochs
            .historical_uncertain_gap_count,
        request_metadata_gateway_unresolved_event_lower_bound: request_metadata_epochs
            .unresolved_event_lower_bound,
        media_reconciliation: if media_reconciliation.is_some() {
            "ok"
        } else {
            "unknown"
        },
        media_reconciliation_pending: media_reconciliation
            .as_ref()
            .map_or(0, |summary| summary.pending),
        media_reconciliation_stale: media_reconciliation
            .as_ref()
            .map_or(0, |summary| summary.stale),
        media_reconciliation_failed: media_reconciliation
            .as_ref()
            .map_or(0, |summary| summary.failed),
        media_reconciliation_unbound: media_reconciliation
            .as_ref()
            .map_or(0, |summary| summary.unbound),
        media_reconciliation_gaps_total: media_reconciliation_gaps,
    })
}

async fn metrics(axum::extract::State(state): axum::extract::State<ApiState>) -> Response {
    let now = Instant::now();
    let readiness = state.observability.readiness();
    let metrics = state.observability.metrics();
    let readiness_fresh = cached_readiness_is_fresh(&readiness, now);
    let metrics_fresh = cached_metrics_is_fresh(&metrics, now);
    let readiness_available = readiness_fresh && readiness.result.is_some();
    let readiness_age = snapshot_age_seconds(readiness.last_success_at, now);
    let metrics_age = snapshot_age_seconds(metrics.last_success_at, now);
    let mut body = format!(
        concat!(
            "# HELP olp_ready Whether the process currently satisfies the HTTP readiness contract.\n",
            "# TYPE olp_ready gauge\n",
            "olp_ready {}\n",
            "# HELP olp_observability_readiness_snapshot_age_seconds Age of the last successful readiness snapshot.\n",
            "# TYPE olp_observability_readiness_snapshot_age_seconds gauge\n",
            "olp_observability_readiness_snapshot_age_seconds {}\n",
            "# HELP olp_observability_metrics_snapshot_age_seconds Age of the last successful metrics snapshot.\n",
            "# TYPE olp_observability_metrics_snapshot_age_seconds gauge\n",
            "olp_observability_metrics_snapshot_age_seconds {}\n",
            "# HELP olp_observability_readiness_snapshot_fresh Whether the readiness snapshot is fresh.\n",
            "# TYPE olp_observability_readiness_snapshot_fresh gauge\n",
            "olp_observability_readiness_snapshot_fresh {}\n",
            "# HELP olp_observability_metrics_snapshot_fresh Whether the metrics snapshot is fresh.\n",
            "# TYPE olp_observability_metrics_snapshot_fresh gauge\n",
            "olp_observability_metrics_snapshot_fresh {}\n",
        ),
        u8::from(readiness_available),
        readiness_age.unwrap_or(u64::MAX),
        metrics_age.unwrap_or(u64::MAX),
        u8::from(readiness_fresh),
        u8::from(metrics_fresh),
    );
    if let Some(metrics) = metrics.body {
        body.push_str(&metrics);
    }
    let mut response = (
        [(
            axum::http::header::CONTENT_TYPE,
            "text/plain; version=0.0.4",
        )],
        body,
    )
        .into_response();
    attach_snapshot_freshness(&mut response, metrics_age, metrics_fresh);
    response
}

async fn collect_metrics(state: &ApiState) -> String {
    let request_metadata = state
        .request_metadata
        .as_ref()
        .map(RequestMetadataEmitter::snapshot);
    let limiter_available = state.limiter.get().is_some();
    let now = chrono::Utc::now();
    let mut request_metadata_consumer = RequestMetadataConsumerStatus::from_health(None, now);
    let mut request_metadata_epochs = RequestMetadataEpochHealth::default();
    let mut operations_summary = None;
    let mut provider_health = Vec::new();
    let media_reconciliation = if let Some(store) = &state.store {
        let (consumer, epochs, operations, providers, media) = tokio::join!(
            store.request_metadata_consumer_status(now),
            store.request_metadata_gateway_epoch_health(),
            store.prometheus_operations_summary(5),
            store.provider_health(15, None, 100),
            store.media_reconciliation_summary(now),
        );
        if let Ok(status) = consumer {
            request_metadata_consumer = status;
        }
        if let Ok(health) = epochs {
            request_metadata_epochs = health;
        }
        operations_summary = operations.ok();
        if let Ok(page) = providers {
            provider_health = page.items;
        }
        media.ok()
    } else {
        None
    };
    let mut body = format!(
        "# HELP olp_runtime_generation Current immutable runtime generation.\n\
         # TYPE olp_runtime_generation gauge\n\
         olp_runtime_generation {}\n\
         # HELP olp_request_metadata_events_dropped_total Metadata events dropped from the bounded buffer.\n\
         # TYPE olp_request_metadata_events_dropped_total counter\n\
         olp_request_metadata_events_dropped_total {}\n\
         # HELP olp_request_metadata_events_abandoned_total Accepted metadata events abandoned during shutdown or worker failure.\n\
         # TYPE olp_request_metadata_events_abandoned_total counter\n\
         olp_request_metadata_events_abandoned_total {}\n\
         # HELP olp_request_metadata_events_pending Accepted metadata events not yet written to the stream.\n\
         # TYPE olp_request_metadata_events_pending gauge\n\
         olp_request_metadata_events_pending {}\n\
         # HELP olp_request_metadata_stream_retrying Whether the local writer is retrying Valkey.\n\
         # TYPE olp_request_metadata_stream_retrying gauge\n\
         olp_request_metadata_stream_retrying {}\n\
         # HELP olp_request_metadata_persistence_available Whether a request metadata sink is active.\n\
         # TYPE olp_request_metadata_persistence_available gauge\n\
         olp_request_metadata_persistence_available {}\n\
         # HELP olp_request_metadata_consumer_pending_events Delivered request metadata events awaiting consumer acknowledgement.\n\
         # TYPE olp_request_metadata_consumer_pending_events gauge\n\
         olp_request_metadata_consumer_pending_events {}\n\
         # HELP olp_request_metadata_consumer_lag_events Request metadata stream events not yet delivered to the persistence consumer.\n\
         # TYPE olp_request_metadata_consumer_lag_events gauge\n\
         olp_request_metadata_consumer_lag_events {}\n\
         # HELP olp_request_metadata_consumer_heartbeat_age_seconds Age of the last durable worker checkpoint.\n\
         # TYPE olp_request_metadata_consumer_heartbeat_age_seconds gauge\n\
         olp_request_metadata_consumer_heartbeat_age_seconds {}\n\
         # HELP olp_request_metadata_consumer_healthy Whether the durable consumer is current and fully drained.\n\
         # TYPE olp_request_metadata_consumer_healthy gauge\n\
         olp_request_metadata_consumer_healthy {}\n\
         # HELP olp_request_metadata_consumer_stale Whether the durable consumer missed its heartbeat threshold.\n\
         # TYPE olp_request_metadata_consumer_stale gauge\n\
         olp_request_metadata_consumer_stale {}\n\
         # HELP olp_request_metadata_gateway_open_epochs Gateway process epochs still emitting checkpoints.\n\
         # TYPE olp_request_metadata_gateway_open_epochs gauge\n\
         olp_request_metadata_gateway_open_epochs {}\n\
         # HELP olp_request_metadata_gateway_unresolved_epochs Unclean gateway epochs awaiting operator acknowledgement.\n\
         # TYPE olp_request_metadata_gateway_unresolved_epochs gauge\n\
         olp_request_metadata_gateway_unresolved_epochs {}\n\
         # HELP olp_request_metadata_historical_uncertain_gaps Retained exactness gaps across raw and hourly evidence.\n\
         # TYPE olp_request_metadata_historical_uncertain_gaps gauge\n\
         olp_request_metadata_historical_uncertain_gaps {}\n\
         # HELP olp_request_metadata_gateway_unresolved_event_lower_bound Last durable in-flight event lower bound across unresolved epochs.\n\
         # TYPE olp_request_metadata_gateway_unresolved_event_lower_bound gauge\n\
         olp_request_metadata_gateway_unresolved_event_lower_bound {}\n\
         # HELP olp_distributed_limiter_available Whether a Valkey limiter connection is installed.\n\
         # TYPE olp_distributed_limiter_available gauge\n\
         olp_distributed_limiter_available {}\n\
         # HELP olp_open_target_circuits Number of target circuits currently open or half-open.\n\
         # TYPE olp_open_target_circuits gauge\n\
         olp_open_target_circuits {}\n\
         # HELP olp_media_reconciliation_pending Metadata-only media jobs awaiting reconciliation.\n\
         # TYPE olp_media_reconciliation_pending gauge\n\
         olp_media_reconciliation_pending {}\n\
         # HELP olp_media_reconciliation_stale Media reconciliation jobs past their grace period.\n\
         # TYPE olp_media_reconciliation_stale gauge\n\
         olp_media_reconciliation_stale {}\n\
         # HELP olp_media_reconciliation_failed Media jobs whose latest autonomous reconciliation attempt failed.\n\
         # TYPE olp_media_reconciliation_failed gauge\n\
         olp_media_reconciliation_failed {}\n\
         # HELP olp_media_reconciliation_unbound Live media jobs without immutable runtime authority.\n\
         # TYPE olp_media_reconciliation_unbound gauge\n\
         olp_media_reconciliation_unbound {}\n\
         # HELP olp_media_reconciliation_gaps_total Upstream media side effects that could not be durably recorded.\n\
         # TYPE olp_media_reconciliation_gaps_total counter\n\
         olp_media_reconciliation_gaps_total {}\n",
        state.runtime.ordinal().unwrap_or(0),
        request_metadata.map_or(0, |snapshot| snapshot.dropped),
        request_metadata.map_or(0, |snapshot| snapshot.abandoned),
        request_metadata.map_or(0, |snapshot| snapshot.pending()),
        request_metadata.map_or(0, |snapshot| u8::from(snapshot.retrying)),
        request_metadata.map_or(0, |snapshot| u8::from(!snapshot.closed)),
        request_metadata_consumer.pending_events,
        request_metadata_consumer.lag_events,
        request_metadata_consumer.heartbeat_age_seconds.unwrap_or(0),
        u8::from(request_metadata_consumer.complete()),
        u8::from(matches!(
            request_metadata_consumer.state,
            olp_storage::RequestMetadataConsumerState::Stale
        )),
        request_metadata_epochs.open_epochs,
        request_metadata_epochs.unresolved_epochs,
        request_metadata_epochs.historical_uncertain_gap_count,
        request_metadata_epochs.unresolved_event_lower_bound,
        u8::from(limiter_available),
        state.circuits.open_count(),
        media_reconciliation
            .as_ref()
            .map_or(0, |value| value.pending),
        media_reconciliation.as_ref().map_or(0, |value| value.stale),
        media_reconciliation
            .as_ref()
            .map_or(0, |value| value.failed),
        media_reconciliation
            .as_ref()
            .map_or(0, |value| value.unbound),
        state.media_reconciliation_gap_count(),
    );
    body.push_str(
        "# HELP olp_operational_metrics_available Whether the PostgreSQL operational rollup was available.\n\
         # TYPE olp_operational_metrics_available gauge\n",
    );
    let _ = writeln!(
        body,
        "olp_operational_metrics_available {}",
        u8::from(operations_summary.is_some())
    );
    if let Some(summary) = operations_summary {
        let success_ratio = if summary.request_count == 0 {
            1.0
        } else {
            summary.success_count as f64 / summary.request_count as f64
        };
        body.push_str(
            "# HELP olp_requests_5m Metadata requests observed during the trailing five minutes.\n\
             # TYPE olp_requests_5m gauge\n\
             # HELP olp_request_success_ratio_5m Successful request ratio during the trailing five minutes.\n\
             # TYPE olp_request_success_ratio_5m gauge\n\
             # HELP olp_request_latency_seconds Request latency quantiles during the trailing five minutes.\n\
             # TYPE olp_request_latency_seconds gauge\n\
             # HELP olp_upstream_cancellations_5m Cancelled upstream attempts during the trailing five minutes.\n\
             # TYPE olp_upstream_cancellations_5m gauge\n",
        );
        let _ = writeln!(body, "olp_requests_5m {}", summary.request_count);
        let _ = writeln!(body, "olp_request_success_ratio_5m {success_ratio:.6}");
        let _ = writeln!(
            body,
            "olp_request_latency_seconds{{quantile=\"0.95\"}} {:.6}",
            summary.p95_latency_ms.unwrap_or(0.0) / 1_000.0
        );
        let _ = writeln!(
            body,
            "olp_request_latency_seconds{{quantile=\"0.99\"}} {:.6}",
            summary.p99_latency_ms.unwrap_or(0.0) / 1_000.0
        );
        let _ = writeln!(
            body,
            "olp_upstream_cancellations_5m {}",
            summary.cancelled_attempt_count
        );
    }
    if !provider_health.is_empty() {
        body.push_str(
            "# HELP olp_provider_health Provider health classification over the trailing fifteen minutes.\n\
             # TYPE olp_provider_health gauge\n\
             # HELP olp_provider_success_ratio_15m Provider attempt success ratio over the trailing fifteen minutes.\n\
             # TYPE olp_provider_success_ratio_15m gauge\n\
             # HELP olp_provider_latency_seconds_15m Provider average attempt latency over the trailing fifteen minutes.\n\
             # TYPE olp_provider_latency_seconds_15m gauge\n",
        );
        for provider in provider_health {
            let provider_id = provider.provider_id;
            let name = prometheus_label(&provider.provider_name);
            let kind = prometheus_label(provider.provider_kind.as_str());
            let status = prometheus_label(&provider.status);
            let success_ratio = if provider.attempt_count == 0 {
                1.0
            } else {
                provider.success_count as f64 / provider.attempt_count as f64
            };
            let labels = format!(
                "provider_id=\"{provider_id}\",provider_name=\"{name}\",provider_kind=\"{kind}\",status=\"{status}\""
            );
            let _ = writeln!(body, "olp_provider_health{{{labels}}} 1");
            let _ = writeln!(
                body,
                "olp_provider_success_ratio_15m{{{labels}}} {success_ratio:.6}"
            );
            let _ = writeln!(
                body,
                "olp_provider_latency_seconds_15m{{{labels}}} {:.6}",
                provider.average_latency_ms.unwrap_or(0.0) / 1_000.0
            );
        }
    }
    body
}

pub(super) fn prometheus_label(value: &str) -> String {
    value
        .replace('\\', "\\\\")
        .replace('\n', "\\n")
        .replace('"', "\\\"")
}

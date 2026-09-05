//! Liveness and readiness snapshot collection.

use std::time::{Duration, Instant};

use axum::response::{IntoResponse, Response};
use olp_db::{
    media_jobs::MediaReconciliationSummary,
    request_metadata::delivery_health::ConsumerStatus,
    request_metadata::reconciliation::EpochHealth,
    runtime::outbox::{RuntimeOutboxState, RuntimeOutboxStatus},
    worker_health::{WorkerRecoveryCounters, WorkerTask, WorkerTaskHealthSummary},
};
use serde::Serialize;
use utoipa::ToSchema;

use super::cache::{
    CachedReadiness, attach_snapshot_freshness, snapshot_age_seconds, snapshot_is_current,
};
use crate::{observability::state::ObservabilityState, public_http::problem::Problem};

const NON_VALKEY_WORKER_TASKS: [WorkerTask; 2] = [
    WorkerTask::Maintenance,
    WorkerTask::RequestMetadataGatewayEpochDetection,
];

#[derive(Clone, Serialize, ToSchema)]
pub(crate) struct HealthResponse {
    status: &'static str,
    asynchronous_plane: &'static str,
    asynchronous_plane_current: bool,
    asynchronous_plane_drained: bool,
    asynchronous_plane_last_progress_at: Option<chrono::DateTime<chrono::Utc>>,
    worker_tasks_stale: u64,
    worker_tasks_unknown: u64,
    generation: Option<u64>,
    database: &'static str,
    limits: &'static str,
    request_metadata_complete: bool,
    request_metadata_consumer: &'static str,
    request_metadata_consumer_pending_events: u64,
    request_metadata_consumer_lag_events: u64,
    request_metadata_consumer_oldest_pending_at: Option<chrono::DateTime<chrono::Utc>>,
    request_metadata_consumer_oldest_pending_age_seconds: Option<u64>,
    request_metadata_consumer_checked_at: Option<chrono::DateTime<chrono::Utc>>,
    request_metadata_consumer_heartbeat_age_seconds: Option<u64>,
    request_metadata_reclaimed_events_total: Option<u64>,
    request_metadata_recovered_events_total: Option<u64>,
    request_metadata_duplicate_persistence_total: Option<u64>,
    request_metadata_gateway_open_epochs: u64,
    request_metadata_gateway_unresolved_epochs: u64,
    request_metadata_historical_uncertain_gaps: u64,
    request_metadata_gateway_unresolved_event_lower_bound: u64,
    runtime_outbox: &'static str,
    runtime_outbox_pending_rows: u64,
    runtime_outbox_oldest_pending_at: Option<chrono::DateTime<chrono::Utc>>,
    runtime_outbox_oldest_pending_age_seconds: Option<u64>,
    runtime_outbox_owner_active: bool,
    runtime_outbox_claimed_rows: u64,
    runtime_outbox_owner_abandoned: bool,
    runtime_outbox_heartbeat_age_seconds: Option<u64>,
    runtime_outbox_publication_attempts_total: Option<u64>,
    runtime_outbox_publication_retries_total: Option<u64>,
    runtime_outbox_repeated_publication_attempts_total: Option<u64>,
    runtime_outbox_abandoned_ownership_total: Option<u64>,
    runtime_outbox_failed_takeovers_total: Option<u64>,
    media_reconciliation: &'static str,
    media_reconciliation_pending: u64,
    media_reconciliation_stale: u64,
    media_reconciliation_failed: u64,
    media_reconciliation_unbound: u64,
    media_reconciliation_gaps_total: u64,
    media_spool_used_bytes: Option<u64>,
    media_spool_capacity_bytes: Option<u64>,
}

pub(super) async fn live() -> axum::Json<HealthResponse> {
    axum::Json(HealthResponse {
        status: "ok",
        asynchronous_plane: "not_checked",
        asynchronous_plane_current: false,
        asynchronous_plane_drained: false,
        asynchronous_plane_last_progress_at: None,
        worker_tasks_stale: 0,
        worker_tasks_unknown: 0,
        generation: None,
        database: "not_checked",
        limits: "not_checked",
        request_metadata_complete: true,
        request_metadata_consumer: "not_checked",
        request_metadata_consumer_pending_events: 0,
        request_metadata_consumer_lag_events: 0,
        request_metadata_consumer_oldest_pending_at: None,
        request_metadata_consumer_oldest_pending_age_seconds: None,
        request_metadata_consumer_checked_at: None,
        request_metadata_consumer_heartbeat_age_seconds: None,
        request_metadata_reclaimed_events_total: None,
        request_metadata_recovered_events_total: None,
        request_metadata_duplicate_persistence_total: None,
        request_metadata_gateway_open_epochs: 0,
        request_metadata_gateway_unresolved_epochs: 0,
        request_metadata_historical_uncertain_gaps: 0,
        request_metadata_gateway_unresolved_event_lower_bound: 0,
        runtime_outbox: "not_checked",
        runtime_outbox_pending_rows: 0,
        runtime_outbox_oldest_pending_at: None,
        runtime_outbox_oldest_pending_age_seconds: None,
        runtime_outbox_owner_active: false,
        runtime_outbox_claimed_rows: 0,
        runtime_outbox_owner_abandoned: false,
        runtime_outbox_heartbeat_age_seconds: None,
        runtime_outbox_publication_attempts_total: None,
        runtime_outbox_publication_retries_total: None,
        runtime_outbox_repeated_publication_attempts_total: None,
        runtime_outbox_abandoned_ownership_total: None,
        runtime_outbox_failed_takeovers_total: None,
        media_reconciliation: "not_checked",
        media_reconciliation_pending: 0,
        media_reconciliation_stale: 0,
        media_reconciliation_failed: 0,
        media_reconciliation_unbound: 0,
        media_reconciliation_gaps_total: 0,
        media_spool_used_bytes: None,
        media_spool_capacity_bytes: None,
    })
}

pub(super) async fn ready(
    axum::extract::State(state): axum::extract::State<ObservabilityState>,
) -> Response {
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

pub(crate) fn cached_readiness_from_snapshot(
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

fn cached_readiness_is_fresh(snapshot: &CachedReadiness, now: Instant) -> bool {
    snapshot_is_current(snapshot.last_success_at, snapshot.last_attempt_at, now)
}

struct StoreProbe {
    database: &'static str,
    media_reconciliation: Option<MediaReconciliationSummary>,
    request_metadata_consumer: ConsumerStatus,
    request_metadata_epochs: EpochHealth,
    runtime_outbox: RuntimeOutboxStatus,
    worker_tasks: WorkerTaskHealthSummary,
    recovery_counters: Option<WorkerRecoveryCounters>,
}

struct ReadinessFlags {
    status: &'static str,
    limits: &'static str,
    request_metadata_complete: bool,
    asynchronous_plane_current: bool,
    asynchronous_plane_drained: bool,
}

pub(super) async fn collect_readiness(
    state: &ObservabilityState,
) -> Result<HealthResponse, Problem> {
    let generation = state.runtime().active_generation_ordinal();
    let now = chrono::Utc::now();
    let probe = probe_store(state, now, generation).await?;
    let expected_worker_tasks = expected_worker_tasks(state);
    check_gateway_runtime(state, generation)?;
    let limits_healthy = limiter_is_healthy(state).await;
    let hard_limits_present = state
        .runtime()
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
    let degraded_media = probe
        .media_reconciliation
        .as_ref()
        .is_some_and(|summary| summary.stale > 0 || summary.failed > 0 || summary.unbound > 0)
        || media_reconciliation_gaps > 0;
    let local_request_metadata_complete = state
        .request_metadata()
        .map_or(!state.mode.serves_gateway(), |request_metadata| {
            request_metadata.snapshot().complete()
        });
    let expects_request_metadata_consumer =
        expected_worker_tasks.contains(&WorkerTask::RequestMetadataConsumer);
    let request_metadata_complete = local_request_metadata_complete
        && (!expects_request_metadata_consumer || probe.request_metadata_consumer.complete())
        && probe.request_metadata_epochs.unresolved_epochs == 0;
    let (asynchronous_plane_current, asynchronous_plane_drained) = asynchronous_plane_flags(
        &probe.worker_tasks,
        expected_worker_tasks,
        probe.request_metadata_consumer,
        probe.runtime_outbox,
    );
    let flags = ReadinessFlags {
        status: if degraded_limits
            || degraded_media
            || !request_metadata_complete
            || !(asynchronous_plane_current && asynchronous_plane_drained)
        {
            "degraded"
        } else {
            "ok"
        },
        limits: if limits_healthy {
            "ok"
        } else if state.limiter().is_configured() {
            "unavailable"
        } else {
            "not_configured"
        },
        request_metadata_complete,
        asynchronous_plane_current,
        asynchronous_plane_drained,
    };
    Ok(readiness_response(
        state,
        now,
        generation,
        &probe,
        expected_worker_tasks,
        &flags,
    ))
}

async fn probe_store(
    state: &ObservabilityState,
    now: chrono::DateTime<chrono::Utc>,
    generation: Option<u64>,
) -> Result<StoreProbe, Problem> {
    match state.store().ping().await {
        Ok(()) => {
            let (media, consumer, epochs, outbox, tasks, counters) = tokio::join!(
                state.store().media_reconciliation_summary(now),
                state.store().request_metadata_consumer_status(now),
                state.store().request_metadata_gateway_epoch_health(),
                state.store().runtime_outbox_status(),
                state.store().worker_task_health(),
                state.store().worker_recovery_counters(),
            );
            fn unavailable<E>(_error: E) -> Problem {
                Problem::service_unavailable("database_unavailable")
            }
            Ok(StoreProbe {
                database: "ok",
                media_reconciliation: Some(media.map_err(unavailable)?),
                request_metadata_consumer: consumer.map_err(unavailable)?,
                request_metadata_epochs: epochs.map_err(unavailable)?,
                runtime_outbox: outbox.map_err(unavailable)?,
                worker_tasks: tasks.map_err(unavailable)?,
                recovery_counters: Some(counters.map_err(unavailable)?),
            })
        }
        Err(_) if state.mode.serves_gateway() && generation.is_some() => Ok(StoreProbe {
            database: "unavailable_lkg",
            media_reconciliation: None,
            request_metadata_consumer: ConsumerStatus::from_health(None, now),
            request_metadata_epochs: EpochHealth::default(),
            runtime_outbox: RuntimeOutboxStatus::unknown(),
            worker_tasks: WorkerTaskHealthSummary::unknown(),
            recovery_counters: None,
        }),
        Err(_) => Err(Problem::service_unavailable("database_unavailable")),
    }
}

fn check_gateway_runtime(
    state: &ObservabilityState,
    generation: Option<u64>,
) -> Result<(), Problem> {
    if !state.mode.serves_gateway() {
        return Ok(());
    }
    let snapshot = state.runtime().pin();
    if generation.is_none() {
        return Err(Problem::service_unavailable(
            "runtime_generation_unavailable",
        ));
    }
    if !snapshot.has_all_transports() {
        return Err(Problem::service_unavailable(
            "provider_transport_unavailable",
        ));
    }
    Ok(())
}

async fn limiter_is_healthy(state: &ObservabilityState) -> bool {
    let limiter = state.limiter().current();
    if let Some(limiter) = &limiter {
        matches!(
            tokio::time::timeout(Duration::from_millis(500), limiter.ping()).await,
            Ok(Ok(()))
        )
    } else {
        false
    }
}

fn readiness_response(
    state: &ObservabilityState,
    now: chrono::DateTime<chrono::Utc>,
    generation: Option<u64>,
    probe: &StoreProbe,
    expected_worker_tasks: &'static [WorkerTask],
    flags: &ReadinessFlags,
) -> HealthResponse {
    let consumer = probe.request_metadata_consumer;
    let epochs = &probe.request_metadata_epochs;
    let outbox = probe.runtime_outbox;
    let counters = probe.recovery_counters;
    let media = probe.media_reconciliation.as_ref();
    let tasks = &probe.worker_tasks;
    HealthResponse {
        status: flags.status,
        asynchronous_plane: asynchronous_plane_state(
            flags.asynchronous_plane_current,
            flags.asynchronous_plane_drained,
            tasks,
            expected_worker_tasks,
            consumer,
            outbox,
        ),
        asynchronous_plane_current: flags.asynchronous_plane_current,
        asynchronous_plane_drained: flags.asynchronous_plane_drained,
        asynchronous_plane_last_progress_at: tasks.last_progress_at_for(expected_worker_tasks),
        worker_tasks_stale: tasks.stale_tasks_for(expected_worker_tasks),
        worker_tasks_unknown: tasks.unknown_tasks_for(expected_worker_tasks),
        generation,
        database: probe.database,
        limits: flags.limits,
        request_metadata_complete: flags.request_metadata_complete,
        request_metadata_consumer: consumer.state.as_str(),
        request_metadata_consumer_pending_events: consumer.pending_events,
        request_metadata_consumer_lag_events: consumer.lag_events,
        request_metadata_consumer_oldest_pending_at: consumer.oldest_pending_at,
        request_metadata_consumer_oldest_pending_age_seconds: datetime_age_seconds(
            now,
            consumer.oldest_pending_at,
        ),
        request_metadata_consumer_checked_at: consumer.checked_at,
        request_metadata_consumer_heartbeat_age_seconds: consumer.heartbeat_age_seconds,
        request_metadata_reclaimed_events_total: counters.map(|c| c.request_metadata_reclaimed),
        request_metadata_recovered_events_total: counters.map(|c| c.request_metadata_recovered),
        request_metadata_duplicate_persistence_total: counters
            .map(|c| c.request_metadata_duplicates),
        request_metadata_gateway_open_epochs: epochs.open_epochs,
        request_metadata_gateway_unresolved_epochs: epochs.unresolved_epochs,
        request_metadata_historical_uncertain_gaps: epochs.historical_uncertain_gap_count,
        request_metadata_gateway_unresolved_event_lower_bound: epochs.unresolved_event_lower_bound,
        runtime_outbox: outbox.state.as_str(),
        runtime_outbox_pending_rows: outbox.pending_rows,
        runtime_outbox_oldest_pending_at: outbox.oldest_pending_at,
        runtime_outbox_oldest_pending_age_seconds: datetime_age_seconds(
            now,
            outbox.oldest_pending_at,
        ),
        runtime_outbox_owner_active: outbox.owner_active,
        runtime_outbox_claimed_rows: outbox.claimed_rows,
        runtime_outbox_owner_abandoned: outbox.ownership_abandoned(),
        runtime_outbox_heartbeat_age_seconds: outbox.heartbeat_age_seconds,
        runtime_outbox_publication_attempts_total: counters.map(|c| c.runtime_outbox_attempts),
        runtime_outbox_publication_retries_total: counters
            .map(|c| c.runtime_outbox_retry_scheduled),
        runtime_outbox_repeated_publication_attempts_total: counters
            .map(|c| c.runtime_outbox_repeated_attempts),
        runtime_outbox_abandoned_ownership_total: counters
            .map(|c| c.runtime_outbox_abandoned_ownership),
        runtime_outbox_failed_takeovers_total: counters.map(|c| c.runtime_outbox_failed_takeovers),
        media_reconciliation: if media.is_some() { "ok" } else { "unknown" },
        media_reconciliation_pending: media.map_or(0, |summary| summary.pending),
        media_reconciliation_stale: media.map_or(0, |summary| summary.stale),
        media_reconciliation_failed: media.map_or(0, |summary| summary.failed),
        media_reconciliation_unbound: media.map_or(0, |summary| summary.unbound),
        media_reconciliation_gaps_total: state.media_reconciliation_gap_count(),
        media_spool_used_bytes: state.media_spool().used_bytes(),
        media_spool_capacity_bytes: state.media_spool().capacity_bytes(),
    }
}

fn request_metadata_consumer_is_current(status: ConsumerStatus) -> bool {
    matches!(
        status.state,
        olp_db::request_metadata::delivery_health::ConsumerState::Healthy
            | olp_db::request_metadata::delivery_health::ConsumerState::Backlogged
    )
}

fn runtime_outbox_is_current(status: RuntimeOutboxStatus) -> bool {
    matches!(
        status.state,
        RuntimeOutboxState::Healthy | RuntimeOutboxState::Backlogged
    )
}

fn expected_worker_tasks(state: &ObservabilityState) -> &'static [WorkerTask] {
    if state.limiter().is_configured() {
        &WorkerTask::ALL
    } else {
        &NON_VALKEY_WORKER_TASKS
    }
}

pub(super) fn asynchronous_plane_flags(
    tasks: &WorkerTaskHealthSummary,
    expected_tasks: &[WorkerTask],
    consumer: ConsumerStatus,
    outbox: RuntimeOutboxStatus,
) -> (bool, bool) {
    let expects_consumer = expected_tasks.contains(&WorkerTask::RequestMetadataConsumer);
    let expects_outbox = expected_tasks.contains(&WorkerTask::RuntimeOutbox);
    let current = tasks.current_for(expected_tasks)
        && (!expects_consumer || request_metadata_consumer_is_current(consumer))
        && (!expects_outbox || runtime_outbox_is_current(outbox));
    let drained = (!expects_consumer || (consumer.pending_events == 0 && consumer.lag_events == 0))
        && (!expects_outbox || (outbox.pending_rows == 0 && outbox.claimed_rows == 0));
    (current, drained)
}

fn asynchronous_plane_state(
    current: bool,
    drained: bool,
    tasks: &WorkerTaskHealthSummary,
    expected_tasks: &[WorkerTask],
    consumer: ConsumerStatus,
    outbox: RuntimeOutboxStatus,
) -> &'static str {
    if current && drained {
        "healthy"
    } else if current {
        "backlogged"
    } else if tasks.unknown_tasks_for(expected_tasks) > 0
        || matches!(
            consumer.state,
            olp_db::request_metadata::delivery_health::ConsumerState::Unknown
        )
        || outbox.state == RuntimeOutboxState::Unknown
    {
        "unknown"
    } else {
        "stale"
    }
}

pub(super) fn datetime_age_seconds(
    now: chrono::DateTime<chrono::Utc>,
    at: Option<chrono::DateTime<chrono::Utc>>,
) -> Option<u64> {
    at.map(|at| {
        u64::try_from(now.signed_duration_since(at).num_seconds().max(0)).unwrap_or(u64::MAX)
    })
}

#[cfg(test)]
mod tests;

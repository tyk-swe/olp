//! Liveness and readiness snapshot collection.

use std::time::{Duration, Instant};

use axum::response::{IntoResponse, Response};
use olp_db::{
    request_metadata::delivery_health::ConsumerStatus,
    request_metadata::reconciliation::EpochHealth,
    runtime::outbox::{RuntimeOutboxState, RuntimeOutboxStatus},
    worker_health::{WorkerTask, WorkerTaskHealthSummary},
};
use serde::Serialize;
use utoipa::ToSchema;

use super::cache::{
    CachedReadiness, attach_snapshot_freshness, snapshot_age_seconds, snapshot_is_current,
};
use crate::{bootstrap::mode_dependencies::ObservabilityState, public_http::problem::Problem};

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

pub(super) async fn collect_readiness(
    state: &ObservabilityState,
) -> Result<HealthResponse, Problem> {
    let generation = state.runtime().active_generation_ordinal();
    let now = chrono::Utc::now();
    let unknown_consumer = ConsumerStatus::from_health(None, now);
    let (
        database,
        media_reconciliation,
        request_metadata_consumer,
        request_metadata_epochs,
        runtime_outbox,
        worker_tasks,
        recovery_counters,
    ) = match state.store().ping().await {
        Ok(()) => {
            let (media, consumer, epochs, outbox, tasks, counters) = tokio::join!(
                state.store().media_reconciliation_summary(now),
                state.store().request_metadata_consumer_status(now),
                state.store().request_metadata_gateway_epoch_health(),
                state.store().runtime_outbox_status(),
                state.store().worker_task_health(),
                state.store().worker_recovery_counters(),
            );
            (
                "ok",
                Some(media.map_err(|_| Problem::service_unavailable("database_unavailable"))?),
                consumer.map_err(|_| Problem::service_unavailable("database_unavailable"))?,
                epochs.map_err(|_| Problem::service_unavailable("database_unavailable"))?,
                outbox.map_err(|_| Problem::service_unavailable("database_unavailable"))?,
                tasks.map_err(|_| Problem::service_unavailable("database_unavailable"))?,
                Some(counters.map_err(|_| Problem::service_unavailable("database_unavailable"))?),
            )
        }
        Err(_) if state.mode.serves_gateway() && generation.is_some() => (
            "unavailable_lkg",
            None,
            unknown_consumer,
            EpochHealth::default(),
            RuntimeOutboxStatus::unknown(),
            WorkerTaskHealthSummary::unknown(),
            None,
        ),
        Err(_) => return Err(Problem::service_unavailable("database_unavailable")),
    };
    let expected_worker_tasks = expected_worker_tasks(state);

    if state.mode.serves_gateway() {
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
    }
    let limiter = state.limiter().current();
    let limits_healthy = if let Some(limiter) = &limiter {
        matches!(
            tokio::time::timeout(Duration::from_millis(500), limiter.ping()).await,
            Ok(Ok(()))
        )
    } else {
        false
    };
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
    let degraded_media = media_reconciliation
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
        && (!expects_request_metadata_consumer || request_metadata_consumer.complete())
        && request_metadata_epochs.unresolved_epochs == 0;
    let (asynchronous_plane_current, asynchronous_plane_drained) = asynchronous_plane_flags(
        &worker_tasks,
        expected_worker_tasks,
        request_metadata_consumer,
        runtime_outbox,
    );
    let asynchronous_plane_healthy = asynchronous_plane_current && asynchronous_plane_drained;
    Ok(HealthResponse {
        status: if degraded_limits
            || degraded_media
            || !request_metadata_complete
            || !asynchronous_plane_healthy
        {
            "degraded"
        } else {
            "ok"
        },
        asynchronous_plane: asynchronous_plane_state(
            asynchronous_plane_current,
            asynchronous_plane_drained,
            &worker_tasks,
            expected_worker_tasks,
            request_metadata_consumer,
            runtime_outbox,
        ),
        asynchronous_plane_current,
        asynchronous_plane_drained,
        asynchronous_plane_last_progress_at: worker_tasks
            .last_progress_at_for(expected_worker_tasks),
        worker_tasks_stale: worker_tasks.stale_tasks_for(expected_worker_tasks),
        worker_tasks_unknown: worker_tasks.unknown_tasks_for(expected_worker_tasks),
        generation,
        database,
        limits: if limits_healthy {
            "ok"
        } else if state.limiter().is_configured() {
            "unavailable"
        } else {
            "not_configured"
        },
        request_metadata_complete,
        request_metadata_consumer: request_metadata_consumer.state.as_str(),
        request_metadata_consumer_pending_events: request_metadata_consumer.pending_events,
        request_metadata_consumer_lag_events: request_metadata_consumer.lag_events,
        request_metadata_consumer_oldest_pending_at: request_metadata_consumer.oldest_pending_at,
        request_metadata_consumer_oldest_pending_age_seconds: datetime_age_seconds(
            now,
            request_metadata_consumer.oldest_pending_at,
        ),
        request_metadata_consumer_checked_at: request_metadata_consumer.checked_at,
        request_metadata_consumer_heartbeat_age_seconds: request_metadata_consumer
            .heartbeat_age_seconds,
        request_metadata_reclaimed_events_total: recovery_counters
            .map(|counters| counters.request_metadata_reclaimed),
        request_metadata_recovered_events_total: recovery_counters
            .map(|counters| counters.request_metadata_recovered),
        request_metadata_duplicate_persistence_total: recovery_counters
            .map(|counters| counters.request_metadata_duplicates),
        request_metadata_gateway_open_epochs: request_metadata_epochs.open_epochs,
        request_metadata_gateway_unresolved_epochs: request_metadata_epochs.unresolved_epochs,
        request_metadata_historical_uncertain_gaps: request_metadata_epochs
            .historical_uncertain_gap_count,
        request_metadata_gateway_unresolved_event_lower_bound: request_metadata_epochs
            .unresolved_event_lower_bound,
        runtime_outbox: runtime_outbox.state.as_str(),
        runtime_outbox_pending_rows: runtime_outbox.pending_rows,
        runtime_outbox_oldest_pending_at: runtime_outbox.oldest_pending_at,
        runtime_outbox_oldest_pending_age_seconds: datetime_age_seconds(
            now,
            runtime_outbox.oldest_pending_at,
        ),
        runtime_outbox_owner_active: runtime_outbox.owner_active,
        runtime_outbox_claimed_rows: runtime_outbox.claimed_rows,
        runtime_outbox_owner_abandoned: runtime_outbox.ownership_abandoned(),
        runtime_outbox_heartbeat_age_seconds: runtime_outbox.heartbeat_age_seconds,
        runtime_outbox_publication_attempts_total: recovery_counters
            .map(|counters| counters.runtime_outbox_attempts),
        runtime_outbox_publication_retries_total: recovery_counters
            .map(|counters| counters.runtime_outbox_retry_scheduled),
        runtime_outbox_repeated_publication_attempts_total: recovery_counters
            .map(|counters| counters.runtime_outbox_repeated_attempts),
        runtime_outbox_abandoned_ownership_total: recovery_counters
            .map(|counters| counters.runtime_outbox_abandoned_ownership),
        runtime_outbox_failed_takeovers_total: recovery_counters
            .map(|counters| counters.runtime_outbox_failed_takeovers),
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
        media_spool_used_bytes: state.media_spool().used_bytes(),
        media_spool_capacity_bytes: state.media_spool().capacity_bytes(),
    })
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
mod tests {
    use std::{path::PathBuf, sync::Arc};

    use olp_db::{
        request_metadata::delivery_health::{ConsumerHealth, ConsumerStatus},
        runtime::outbox::{RuntimeOutboxState, RuntimeOutboxStatus},
        worker_health::{WorkerTaskState, WorkerTaskStatus},
    };
    use olp_engine::inference::runtime::Manager;

    use super::*;
    use crate::{bootstrap::state::ApiMode, bootstrap::state::ProcessComposition};

    #[test]
    fn http_only_processes_still_check_fleet_worker_summaries() {
        let now = chrono::DateTime::parse_from_rfc3339("2026-08-08T12:00:00Z")
            .unwrap()
            .with_timezone(&chrono::Utc);
        let stale_consumer = ConsumerStatus::from_health(
            Some(ConsumerHealth {
                pending_events: 0,
                lag_events: 0,
                oldest_pending_at: None,
                checked_at: now - chrono::Duration::seconds(21),
            }),
            now,
        );
        let healthy_outbox = RuntimeOutboxStatus {
            state: RuntimeOutboxState::Healthy,
            pending_rows: 0,
            oldest_pending_at: None,
            owner_active: true,
            claimed_rows: 0,
            checked_at: Some(now),
            heartbeat_age_seconds: Some(0),
            last_progress_at: Some(now),
            last_progress_age_seconds: Some(0),
        };
        let healthy_tasks = WorkerTaskHealthSummary {
            tasks: WorkerTask::ALL
                .into_iter()
                .map(|task| WorkerTaskStatus {
                    task,
                    state: WorkerTaskState::Healthy,
                    checked_at: Some(now),
                    last_success_at: Some(now),
                    last_progress_at: Some(now),
                    heartbeat_age_seconds: Some(0),
                    last_success_age_seconds: Some(0),
                    successes_total: 1,
                    failures_total: 0,
                    skipped_total: 0,
                })
                .collect(),
        };

        for mode in [ApiMode::Gateway, ApiMode::Control] {
            let state = ProcessComposition::new(
                mode,
                None,
                Arc::new(Manager::empty()),
                "https://olp.example.test",
                PathBuf::from("missing-console"),
            )
            .observability_state_for_test();
            state.limiter().mark_configured();

            let expected_tasks = expected_worker_tasks(&state);
            assert_eq!(expected_tasks, WorkerTask::ALL.as_slice());
            let (current, drained) = asynchronous_plane_flags(
                &healthy_tasks,
                expected_tasks,
                stale_consumer,
                healthy_outbox,
            );

            assert!(!current, "{mode} must not skip the stale durable consumer");
            assert!(drained, "{mode} should separate staleness from backlog");
            assert_eq!(
                asynchronous_plane_state(
                    current,
                    drained,
                    &healthy_tasks,
                    expected_tasks,
                    stale_consumer,
                    healthy_outbox,
                ),
                "stale"
            );
        }
    }
}

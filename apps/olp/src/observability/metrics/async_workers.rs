use std::fmt::Write as _;

use olp_db::{
    request_metadata::delivery_health::ConsumerStatus,
    runtime::outbox::RuntimeOutboxStatus,
    worker_health::{WorkerRecoveryCounters, WorkerTask, WorkerTaskHealthSummary, WorkerTaskState},
};

use crate::observability::readiness::{asynchronous_plane_flags, datetime_age_seconds};

pub(crate) fn append_async_worker_metrics(
    body: &mut String,
    now: chrono::DateTime<chrono::Utc>,
    consumer_available: bool,
    consumer: ConsumerStatus,
    outbox: Option<RuntimeOutboxStatus>,
    tasks: Option<&WorkerTaskHealthSummary>,
    counters: Option<WorkerRecoveryCounters>,
) {
    let available = consumer_available && outbox.is_some() && tasks.is_some() && counters.is_some();
    body.push_str(
        "# HELP olp_async_worker_observability_available Whether all PostgreSQL-backed asynchronous worker summaries were available.\n\
         # TYPE olp_async_worker_observability_available gauge\n",
    );
    let _ = writeln!(
        body,
        "olp_async_worker_observability_available {}",
        u8::from(available)
    );
    let (Some(outbox), Some(tasks)) = (outbox, tasks) else {
        return;
    };

    let consumer = if consumer_available {
        consumer
    } else {
        ConsumerStatus::from_health(None, now)
    };
    let (current, drained) = asynchronous_plane_flags(tasks, &WorkerTask::ALL, consumer, outbox);
    append_async_plane_help(body);
    append_async_plane_values(body, now, current, drained, consumer, outbox, tasks);
    append_worker_task_values(body, tasks);
    let Some(counters) = counters else {
        // Omitting recovery counter series during a database read failure keeps
        // a transient dependency outage from looking like a counter reset.
        return;
    };
    append_worker_recovery_counters(body, &counters);
}

fn append_async_plane_help(body: &mut String) {
    body.push_str(
        "# HELP olp_async_plane_current Whether every replicated worker responsibility has a current successful checkpoint.\n\
         # TYPE olp_async_plane_current gauge\n\
         # HELP olp_async_plane_drained Whether request-metadata and runtime-outbox backlogs are drained.\n\
         # TYPE olp_async_plane_drained gauge\n\
         # HELP olp_async_plane_healthy Whether the asynchronous plane is both current and drained.\n\
         # TYPE olp_async_plane_healthy gauge\n\
         # HELP olp_async_plane_last_progress_timestamp_seconds Unix time of the latest durable worker progress.\n\
         # TYPE olp_async_plane_last_progress_timestamp_seconds gauge\n\
         # HELP olp_request_metadata_consumer_oldest_pending_age_seconds Age of the oldest delivered metadata entry awaiting acknowledgement.\n\
         # TYPE olp_request_metadata_consumer_oldest_pending_age_seconds gauge\n\
         # HELP olp_runtime_outbox_pending_rows Unpublished runtime-hint outbox rows.\n\
         # TYPE olp_runtime_outbox_pending_rows gauge\n\
         # HELP olp_runtime_outbox_oldest_pending_age_seconds Age of the oldest unpublished runtime-hint outbox row.\n\
         # TYPE olp_runtime_outbox_oldest_pending_age_seconds gauge\n\
         # HELP olp_runtime_outbox_owner_active Whether the durable summary records an active advisory-lock owner.\n\
         # TYPE olp_runtime_outbox_owner_active gauge\n\
         # HELP olp_runtime_outbox_claimed_rows Outbox rows currently inside a publication attempt.\n\
         # TYPE olp_runtime_outbox_claimed_rows gauge\n\
         # HELP olp_runtime_outbox_owner_stale Whether recorded outbox ownership has exceeded its heartbeat window.\n\
         # TYPE olp_runtime_outbox_owner_stale gauge\n\
         # HELP olp_runtime_outbox_heartbeat_age_seconds Age of the last durable outbox-owner checkpoint.\n\
         # TYPE olp_runtime_outbox_heartbeat_age_seconds gauge\n\
         # HELP olp_worker_task_healthy Whether a fixed worker responsibility has a current successful checkpoint.\n\
         # TYPE olp_worker_task_healthy gauge\n\
         # HELP olp_worker_task_heartbeat_age_seconds Age of the latest checkpoint for a fixed worker responsibility.\n\
         # TYPE olp_worker_task_heartbeat_age_seconds gauge\n\
         # HELP olp_worker_task_last_success_age_seconds Age of the latest successful checkpoint for a fixed worker responsibility.\n\
         # TYPE olp_worker_task_last_success_age_seconds gauge\n\
         # HELP olp_worker_task_runs_total Durable checkpoint outcomes across all worker replicas.\n\
         # TYPE olp_worker_task_runs_total counter\n",
    );
}

fn append_async_plane_values(
    body: &mut String,
    now: chrono::DateTime<chrono::Utc>,
    current: bool,
    drained: bool,
    consumer: ConsumerStatus,
    outbox: RuntimeOutboxStatus,
    tasks: &WorkerTaskHealthSummary,
) {
    let last_progress = tasks
        .last_progress_at()
        .map_or(0, |at| at.timestamp().max(0));
    let _ = writeln!(body, "olp_async_plane_current {}", u8::from(current));
    let _ = writeln!(body, "olp_async_plane_drained {}", u8::from(drained));
    let _ = writeln!(
        body,
        "olp_async_plane_healthy {}",
        u8::from(current && drained)
    );
    let _ = writeln!(
        body,
        "olp_async_plane_last_progress_timestamp_seconds {last_progress}"
    );
    let _ = writeln!(
        body,
        "olp_request_metadata_consumer_oldest_pending_age_seconds {}",
        datetime_age_seconds(now, consumer.oldest_pending_at).unwrap_or(0)
    );
    let _ = writeln!(
        body,
        "olp_runtime_outbox_pending_rows {}",
        outbox.pending_rows
    );
    let _ = writeln!(
        body,
        "olp_runtime_outbox_oldest_pending_age_seconds {}",
        datetime_age_seconds(now, outbox.oldest_pending_at).unwrap_or(0)
    );
    let _ = writeln!(
        body,
        "olp_runtime_outbox_owner_active {}",
        u8::from(outbox.owner_active)
    );
    let _ = writeln!(
        body,
        "olp_runtime_outbox_claimed_rows {}",
        outbox.claimed_rows
    );
    let _ = writeln!(
        body,
        "olp_runtime_outbox_owner_stale {}",
        u8::from(outbox.ownership_abandoned())
    );
    let _ = writeln!(
        body,
        "olp_runtime_outbox_heartbeat_age_seconds {}",
        outbox.heartbeat_age_seconds.unwrap_or(u64::MAX)
    );
}

fn append_worker_task_values(body: &mut String, tasks: &WorkerTaskHealthSummary) {
    for task in &tasks.tasks {
        let task_label = task.task.as_str();
        let _ = writeln!(
            body,
            "olp_worker_task_healthy{{task=\"{task_label}\"}} {}",
            u8::from(task.state == WorkerTaskState::Healthy)
        );
        let _ = writeln!(
            body,
            "olp_worker_task_heartbeat_age_seconds{{task=\"{task_label}\"}} {}",
            task.heartbeat_age_seconds.unwrap_or(u64::MAX)
        );
        let _ = writeln!(
            body,
            "olp_worker_task_last_success_age_seconds{{task=\"{task_label}\"}} {}",
            task.last_success_age_seconds.unwrap_or(u64::MAX)
        );
        for (outcome, value) in [
            ("success", task.successes_total),
            ("failure", task.failures_total),
            ("skipped", task.skipped_total),
        ] {
            let _ = writeln!(
                body,
                "olp_worker_task_runs_total{{task=\"{task_label}\",outcome=\"{outcome}\"}} {value}"
            );
        }
    }
}

fn append_worker_recovery_counters(body: &mut String, counters: &WorkerRecoveryCounters) {
    append_worker_recovery_counter_help(body);
    let _ = writeln!(
        body,
        "olp_request_metadata_events_reclaimed_total {}",
        counters.request_metadata_reclaimed
    );
    let _ = writeln!(
        body,
        "olp_request_metadata_events_recovered_total {}",
        counters.request_metadata_recovered
    );
    let _ = writeln!(
        body,
        "olp_request_metadata_persistence_duplicates_total {}",
        counters.request_metadata_duplicates
    );
    let _ = writeln!(
        body,
        "olp_request_metadata_events_processed_total {}",
        counters.request_metadata_processed
    );
    let _ = writeln!(
        body,
        "olp_runtime_outbox_publication_attempts_total {}",
        counters.runtime_outbox_attempts
    );
    let _ = writeln!(
        body,
        "olp_runtime_outbox_publication_retries_total {}",
        counters.runtime_outbox_retry_scheduled
    );
    let _ = writeln!(
        body,
        "olp_runtime_outbox_repeated_publication_attempts_total {}",
        counters.runtime_outbox_repeated_attempts
    );
    let _ = writeln!(
        body,
        "olp_runtime_outbox_published_total {}",
        counters.runtime_outbox_published
    );
    let _ = writeln!(
        body,
        "olp_runtime_outbox_duplicate_publications_total {}",
        counters.runtime_outbox_duplicate_publications
    );
    let _ = writeln!(
        body,
        "olp_runtime_outbox_abandoned_ownership_total {}",
        counters.runtime_outbox_abandoned_ownership
    );
    let _ = writeln!(
        body,
        "olp_runtime_outbox_abandoned_claims_total {}",
        counters.runtime_outbox_abandoned_claims
    );
    let _ = writeln!(
        body,
        "olp_runtime_outbox_failed_takeovers_total {}",
        counters.runtime_outbox_failed_takeovers
    );
}

fn append_worker_recovery_counter_help(body: &mut String) {
    body.push_str(
        "# HELP olp_request_metadata_events_reclaimed_total Metadata entries transferred from stale consumer ownership.\n\
         # TYPE olp_request_metadata_events_reclaimed_total counter\n\
         # HELP olp_request_metadata_events_recovered_total Pending metadata entries durably resolved by a recovery pass.\n\
         # TYPE olp_request_metadata_events_recovered_total counter\n\
         # HELP olp_request_metadata_persistence_duplicates_total Duplicate metadata persistence outcomes accepted idempotently.\n\
         # TYPE olp_request_metadata_persistence_duplicates_total counter\n\
         # HELP olp_request_metadata_events_processed_total Stream metadata entries durably resolved by the replicated consumer group.\n\
         # TYPE olp_request_metadata_events_processed_total counter\n\
         # HELP olp_runtime_outbox_publication_attempts_total Runtime-hint publication attempts begun durably.\n\
         # TYPE olp_runtime_outbox_publication_attempts_total counter\n\
         # HELP olp_runtime_outbox_publication_retries_total Ambiguous or failed publications left pending for retry.\n\
         # TYPE olp_runtime_outbox_publication_retries_total counter\n\
         # HELP olp_runtime_outbox_repeated_publication_attempts_total Publication attempts for rows that had already been attempted.\n\
         # TYPE olp_runtime_outbox_repeated_publication_attempts_total counter\n\
         # HELP olp_runtime_outbox_published_total Runtime-hint outbox rows durably completed after publication.\n\
         # TYPE olp_runtime_outbox_published_total counter\n\
         # HELP olp_runtime_outbox_duplicate_publications_total Accepted publications whose outbox row was already complete.\n\
         # TYPE olp_runtime_outbox_duplicate_publications_total counter\n\
         # HELP olp_runtime_outbox_abandoned_ownership_total Advisory-lock acquisitions that recovered uncleared ownership.\n\
         # TYPE olp_runtime_outbox_abandoned_ownership_total counter\n\
         # HELP olp_runtime_outbox_abandoned_claims_total Claimed rows recovered from owners that disappeared.\n\
         # TYPE olp_runtime_outbox_abandoned_claims_total counter\n\
         # HELP olp_runtime_outbox_failed_takeovers_total Attempts that could not take over an owner past its heartbeat window.\n\
         # TYPE olp_runtime_outbox_failed_takeovers_total counter\n",
    );
}

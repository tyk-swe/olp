//! Prometheus snapshot collection and rendering.

use std::{fmt::Write as _, time::Instant};

use axum::response::{IntoResponse, Response};
use olp_db::{
    request_metadata::delivery_health::ConsumerStatus,
    request_metadata::reconciliation::EpochHealth,
    runtime::outbox::RuntimeOutboxStatus,
    worker_health::{WorkerRecoveryCounters, WorkerTask, WorkerTaskHealthSummary, WorkerTaskState},
};
use olp_engine::inference::request_metadata::Emitter;

use super::{
    cache::{CachedMetrics, attach_snapshot_freshness, snapshot_age_seconds, snapshot_is_current},
    readiness::{asynchronous_plane_flags, datetime_age_seconds},
};
use crate::bootstrap::mode_dependencies::ObservabilityState;

fn cached_metrics_is_fresh(snapshot: &CachedMetrics, now: Instant) -> bool {
    snapshot_is_current(snapshot.last_success_at, snapshot.last_attempt_at, now)
}

pub(super) async fn metrics(
    axum::extract::State(state): axum::extract::State<ObservabilityState>,
) -> Response {
    let now = Instant::now();
    let readiness = state.observability.readiness();
    let metrics = state.observability.metrics();
    let readiness_fresh =
        snapshot_is_current(readiness.last_success_at, readiness.last_attempt_at, now);
    let metrics_fresh = cached_metrics_is_fresh(&metrics, now);
    let readiness_available = readiness_fresh && readiness.value.is_some();
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
    if let Some(metrics) = metrics.value {
        body.push_str(&metrics);
    }
    body.push_str(&state.public_admission.metrics());
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

pub(super) async fn collect_metrics(state: &ObservabilityState) -> String {
    let request_metadata = state.request_metadata().map(Emitter::snapshot);
    let limiter_available = state.limiter().current().is_some();
    let now = chrono::Utc::now();
    let mut request_metadata_consumer = ConsumerStatus::from_health(None, now);
    let mut request_metadata_epochs = EpochHealth::default();
    let mut provider_health = Vec::new();
    let (consumer, epochs, operations, providers, media, outbox, tasks, counters) = tokio::join!(
        state.store().request_metadata_consumer_status(now),
        state.store().request_metadata_gateway_epoch_health(),
        state.store().prometheus_operations_summary(5),
        state.store().provider_health(15, None, 100),
        state.store().media_reconciliation_summary(now),
        state.store().runtime_outbox_status(),
        state.store().worker_task_health(now),
        state.store().worker_recovery_counters(),
    );
    let consumer_available = consumer.is_ok();
    if let Ok(status) = consumer {
        request_metadata_consumer = status;
    }
    if let Ok(health) = epochs {
        request_metadata_epochs = health;
    }
    let operations_summary = operations.ok();
    if let Ok(page) = providers {
        provider_health = page.items;
    }
    let media_reconciliation = media.ok();
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
        state.runtime().active_generation_ordinal().unwrap_or(0),
        request_metadata.map_or(0, |snapshot| snapshot.dropped),
        request_metadata.map_or(0, |snapshot| snapshot.abandoned),
        request_metadata.map_or(0, |snapshot| snapshot.pending()),
        request_metadata.map_or(0, |snapshot| u8::from(snapshot.retrying)),
        request_metadata.map_or(0, |snapshot| u8::from(!snapshot.closed)),
        request_metadata_consumer.pending_events,
        request_metadata_consumer.lag_events,
        request_metadata_consumer
            .heartbeat_age_seconds
            .unwrap_or(u64::MAX),
        u8::from(request_metadata_consumer.complete()),
        u8::from(matches!(
            request_metadata_consumer.state,
            olp_db::request_metadata::delivery_health::ConsumerState::Stale
        )),
        request_metadata_epochs.open_epochs,
        request_metadata_epochs.unresolved_epochs,
        request_metadata_epochs.historical_uncertain_gap_count,
        request_metadata_epochs.unresolved_event_lower_bound,
        u8::from(limiter_available),
        state.circuits().open_count(),
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
    append_async_worker_metrics(
        &mut body,
        now,
        consumer_available,
        request_metadata_consumer,
        outbox.ok(),
        tasks.ok().as_ref(),
        counters.ok(),
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

    let Some(counters) = counters else {
        // Omitting recovery counter series during a database read failure keeps
        // a transient dependency outage from looking like a counter reset.
        return;
    };
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

pub(crate) fn prometheus_label(value: &str) -> String {
    value
        .replace('\\', "\\\\")
        .replace('\n', "\\n")
        .replace('"', "\\\"")
}

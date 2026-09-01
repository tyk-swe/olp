//! Prometheus snapshot collection and rendering.

use std::{
    fmt::Write as _,
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
    },
    time::Instant,
};

use axum::response::{IntoResponse, Response};
use olp_db::{
    media_jobs::MediaReconciliationSummary,
    operations::health::{PrometheusOperationsSummary, ProviderHealthRecord},
    request_metadata::delivery_health::ConsumerStatus,
    request_metadata::reconciliation::{EpochHealth, LossReport},
};
use olp_engine::inference::request_metadata::{Emitter, Snapshot};

mod async_workers;

pub(crate) use async_workers::append_async_worker_metrics;

use super::cache::{
    CachedMetrics, attach_snapshot_freshness, snapshot_age_seconds, snapshot_is_current,
};
use crate::bootstrap::mode_dependencies::ObservabilityState;

fn cached_metrics_is_fresh(snapshot: &CachedMetrics, now: Instant) -> bool {
    snapshot_is_current(snapshot.last_success_at, snapshot.last_attempt_at, now)
}

#[derive(Debug, Default)]
struct RequestMetadataLossTotals {
    events: AtomicU64,
    dropped: AtomicU64,
    abandoned: AtomicU64,
}

/// Process-wide totals for request metadata loss that the reporter has
/// durably recorded in PostgreSQL. The loss reporter owns the increments and
/// the metrics endpoint renders them.
#[derive(Clone, Debug, Default)]
pub(crate) struct RequestMetadataLossCounters(Arc<RequestMetadataLossTotals>);

impl RequestMetadataLossCounters {
    pub(crate) fn record(&self, report: LossReport) {
        self.0
            .events
            .fetch_add(report.reported_events, Ordering::Relaxed);
        self.0
            .dropped
            .fetch_add(report.reported_dropped, Ordering::Relaxed);
        self.0
            .abandoned
            .fetch_add(report.reported_abandoned, Ordering::Relaxed);
    }

    pub(crate) fn totals(&self) -> (u64, u64, u64) {
        (
            self.0.events.load(Ordering::Relaxed),
            self.0.dropped.load(Ordering::Relaxed),
            self.0.abandoned.load(Ordering::Relaxed),
        )
    }
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
    body.push_str(&state.public_admission.metrics());
    append_trace_export_metrics(&mut body);
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

fn append_trace_export_metrics(body: &mut String) {
    body.push_str(
        "# HELP olp_trace_export_dropped_total Spans dropped before successful OTLP export.\n\
         # TYPE olp_trace_export_dropped_total counter\n",
    );
    let _ = writeln!(
        body,
        "olp_trace_export_dropped_total {}",
        crate::observability::tracing::export_dropped_total()
    );
}

/// Renders the durably reported request metadata loss totals. All three series
/// are always emitted, including at zero, so an absent line means the exporter
/// itself is broken rather than that nothing was lost.
fn append_request_metadata_loss_totals(
    body: &mut String,
    events: u64,
    dropped: u64,
    abandoned: u64,
) {
    body.push_str(
        "# HELP olp_request_metadata_loss_reported_total Local buffer loss durably reported by the gateway checkpoint.\n\
         # TYPE olp_request_metadata_loss_reported_total counter\n",
    );
    let _ = writeln!(
        body,
        "olp_request_metadata_loss_reported_total{{kind=\"events\"}} {events}\n\
         olp_request_metadata_loss_reported_total{{kind=\"dropped\"}} {dropped}\n\
         olp_request_metadata_loss_reported_total{{kind=\"abandoned\"}} {abandoned}"
    );
}

/// Renders the local media spool gauges. A deployment without a spool has no
/// capacity to report, and a missing series is deliberately different from a
/// zero one: it says the spool is not configured at all.
fn append_media_spool_metrics(
    body: &mut String,
    capacity_bytes: Option<u64>,
    used_bytes: Option<u64>,
) {
    if let Some(capacity_bytes) = capacity_bytes {
        body.push_str(
            "# HELP olp_media_spool_capacity_bytes Configured capacity of the private local media spool.\n\
             # TYPE olp_media_spool_capacity_bytes gauge\n",
        );
        let _ = writeln!(body, "olp_media_spool_capacity_bytes {capacity_bytes}");
    }
    if let Some(used_bytes) = used_bytes {
        body.push_str(
            "# HELP olp_media_spool_used_bytes Bytes currently reserved in the private local media spool.\n\
             # TYPE olp_media_spool_used_bytes gauge\n",
        );
        let _ = writeln!(body, "olp_media_spool_used_bytes {used_bytes}");
    }
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
        state.store().worker_task_health(),
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
    let (reported_loss_events, reported_loss_dropped, reported_loss_abandoned) =
        state.request_metadata_loss_counters().totals();
    let mut body = String::with_capacity(8192);
    append_runtime_and_metadata_gauges(
        &mut body,
        state,
        request_metadata,
        request_metadata_consumer,
        &request_metadata_epochs,
    );
    append_limiter_circuit_and_media_gauges(
        &mut body,
        state,
        limiter_available,
        media_reconciliation.as_ref(),
    );
    append_request_metadata_loss_totals(
        &mut body,
        reported_loss_events,
        reported_loss_dropped,
        reported_loss_abandoned,
    );
    append_media_spool_metrics(
        &mut body,
        state.media_spool().capacity_bytes(),
        state.media_spool().used_bytes(),
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
    append_operations_metrics(&mut body, operations_summary);
    append_provider_health_metrics(&mut body, provider_health);
    body
}

fn append_runtime_and_metadata_gauges(
    body: &mut String,
    state: &ObservabilityState,
    request_metadata: Option<Snapshot>,
    request_metadata_consumer: ConsumerStatus,
    request_metadata_epochs: &EpochHealth,
) {
    let _ = write!(
        body,
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
         olp_request_metadata_gateway_unresolved_event_lower_bound {}\n",
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
    );
}

fn append_limiter_circuit_and_media_gauges(
    body: &mut String,
    state: &ObservabilityState,
    limiter_available: bool,
    media_reconciliation: Option<&MediaReconciliationSummary>,
) {
    let _ = write!(
        body,
        "# HELP olp_distributed_limiter_available Whether a Valkey limiter connection is installed.\n\
         # TYPE olp_distributed_limiter_available gauge\n\
         olp_distributed_limiter_available {}\n\
         # HELP olp_limits_fail_open_total Hard-limited requests admitted without a lease under the fail-open outage policy.\n\
         # TYPE olp_limits_fail_open_total counter\n\
         olp_limits_fail_open_total {}\n\
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
        u8::from(limiter_available),
        state.limiter().fail_open_total(),
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
}

/// Successful fraction of `total`, treating "nothing observed" as fully healthy
/// so an idle window does not read as an outage.
fn success_ratio(success: u64, total: u64) -> f64 {
    if total == 0 {
        1.0
    } else {
        success as f64 / total as f64
    }
}

fn append_operations_metrics(
    body: &mut String,
    operations_summary: Option<PrometheusOperationsSummary>,
) {
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
        let success_ratio = success_ratio(summary.success_count, summary.request_count);
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
}

fn append_provider_health_metrics(body: &mut String, provider_health: Vec<ProviderHealthRecord>) {
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
            let success_ratio = success_ratio(provider.success_count, provider.attempt_count);
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
}

pub(crate) fn prometheus_label(value: &str) -> String {
    value
        .replace('\\', "\\\\")
        .replace('\n', "\\n")
        .replace('"', "\\\"")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn loss_counters_accumulate_every_reported_checkpoint() {
        let counters = RequestMetadataLossCounters::default();
        assert_eq!(counters.totals(), (0, 0, 0));

        counters.record(LossReport {
            reported_events: 3,
            reported_dropped: 2,
            reported_abandoned: 1,
            process_epoch_changed: true,
        });
        counters.record(LossReport {
            reported_events: 4,
            reported_dropped: 4,
            reported_abandoned: 0,
            process_epoch_changed: false,
        });

        assert_eq!(counters.totals(), (7, 6, 1));
    }

    #[test]
    fn every_loss_kind_is_rendered_even_at_zero() {
        let mut body = String::new();
        append_request_metadata_loss_totals(&mut body, 7, 6, 0);
        assert!(body.contains("# TYPE olp_request_metadata_loss_reported_total counter\n"));
        assert!(body.contains("olp_request_metadata_loss_reported_total{kind=\"events\"} 7\n"));
        assert!(body.contains("olp_request_metadata_loss_reported_total{kind=\"dropped\"} 6\n"));
        assert!(body.ends_with("olp_request_metadata_loss_reported_total{kind=\"abandoned\"} 0\n"));
    }

    #[test]
    fn media_spool_gauges_appear_only_when_a_spool_is_configured() {
        let mut configured = String::new();
        append_media_spool_metrics(&mut configured, Some(4096), Some(1024));
        assert!(configured.contains("# TYPE olp_media_spool_capacity_bytes gauge\n"));
        assert!(configured.contains("olp_media_spool_capacity_bytes 4096\n"));
        assert!(configured.contains("# TYPE olp_media_spool_used_bytes gauge\n"));
        assert!(configured.contains("olp_media_spool_used_bytes 1024\n"));

        let mut absent = String::new();
        append_media_spool_metrics(&mut absent, None, None);
        assert!(absent.is_empty());

        // A spool that reports capacity but not usage must not fabricate a zero.
        let mut partial = String::new();
        append_media_spool_metrics(&mut partial, Some(4096), None);
        assert!(partial.contains("olp_media_spool_capacity_bytes 4096\n"));
        assert!(!partial.contains("olp_media_spool_used_bytes"));
    }

    #[test]
    fn trace_export_drop_counter_is_always_rendered() {
        let mut body = String::new();
        append_trace_export_metrics(&mut body);

        assert!(body.contains("# TYPE olp_trace_export_dropped_total counter\n"));
        assert!(body.contains("olp_trace_export_dropped_total "));
    }
}

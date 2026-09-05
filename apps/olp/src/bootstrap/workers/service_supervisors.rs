use std::time::Duration;

use olp_db::{
    limits::DistributedLimiter, operations::settings::LimitsValkeyUnavailablePolicy,
    request_metadata::reconciliation::LossReport, store::Store,
};
use olp_engine::inference::{
    limits::{LimitOutagePolicy, ReloadableLimiter},
    request_metadata::Emitter,
};
use tokio::sync::watch;
use tracing::{error, info, warn};

use crate::{
    application::media_jobs::reconcile_media_jobs_once,
    observability::metrics::RequestMetadataLossCounters,
};

pub(crate) async fn media_reconciliation_supervisor(
    state: crate::application::media_jobs::MediaJobs,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut interval = tokio::time::interval(Duration::from_secs(5));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                match reconcile_media_jobs_once(&state, 16).await {
                    Ok(report) if report.claimed > 0 => {
                        info!(
                            claimed = report.claimed,
                            completed = report.completed,
                            failed = report.failed,
                            "autonomous media reconciliation pass completed"
                        );
                    }
                    Ok(_) => {}
                    Err(error) => warn!(%error, "autonomous media reconciliation pass failed"),
                }
            }
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return;
                }
            }
        }
    }
}

/// What a durable loss checkpoint is worth saying out loud. Reports are the
/// only evidence that buffered metadata was lost, so a checkpoint that reports
/// nothing must still leave a process epoch change visible in the log; a
/// checkpoint that reports nothing and changes nothing stays silent so the
/// once-a-second cadence does not drown the log.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum LossCheckpointOutcome {
    Reported,
    EpochChanged,
    Silent,
}

const fn classify_loss_checkpoint(report: LossReport) -> LossCheckpointOutcome {
    if report.reported_events > 0 {
        LossCheckpointOutcome::Reported
    } else if report.process_epoch_changed {
        LossCheckpointOutcome::EpochChanged
    } else {
        LossCheckpointOutcome::Silent
    }
}

/// Records one durable loss checkpoint.
fn record_loss_checkpoint(
    counters: &RequestMetadataLossCounters,
    gateway_instance: &str,
    report: LossReport,
) {
    counters.record(report);
    match classify_loss_checkpoint(report) {
        LossCheckpointOutcome::Reported => warn!(
            %gateway_instance,
            reported_events = report.reported_events,
            reported_dropped = report.reported_dropped,
            reported_abandoned = report.reported_abandoned,
            process_epoch_changed = report.process_epoch_changed,
            "request metadata loss durably reported"
        ),
        LossCheckpointOutcome::EpochChanged => info!(
            %gateway_instance,
            "request metadata reporter checkpointed a new gateway process epoch"
        ),
        LossCheckpointOutcome::Silent => {}
    }
}

pub(crate) async fn request_metadata_loss_reporter(
    store: Store,
    emitter: Emitter,
    gateway_instance: String,
    loss_counters: RequestMetadataLossCounters,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut interval = tokio::time::interval(Duration::from_secs(1));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                let snapshot = emitter.snapshot();
                match store.report_request_metadata_buffer_loss(&gateway_instance, &snapshot).await {
                    Ok(report) => record_loss_checkpoint(&loss_counters, &gateway_instance, report),
                    Err(error) => warn!(%error, %gateway_instance, "request metadata loss checkpoint failed; retrying"),
                }
            }
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    // Let the stream writer close its receiver and account for
                    // accepted-but-abandoned entries, then durably checkpoint
                    // the final counters before graceful process exit.
                    tokio::time::sleep(Duration::from_millis(100)).await;
                    let deadline = tokio::time::Instant::now() + Duration::from_secs(4);
                    loop {
                        let snapshot = emitter.snapshot();
                        match store.close_request_metadata_buffer_epoch(&gateway_instance, &snapshot).await {
                            Ok(report) => {
                                record_loss_checkpoint(&loss_counters, &gateway_instance, report);
                                return;
                            }
                            Err(error) if tokio::time::Instant::now() < deadline => {
                                warn!(%error, %gateway_instance, "final request metadata loss checkpoint failed; retrying");
                                tokio::time::sleep(Duration::from_millis(200)).await;
                            }
                            Err(error) => {
                                error!(%error, %gateway_instance, lost = snapshot.lost(), "final request metadata loss checkpoint could not be persisted");
                                return;
                            }
                        }
                    }
                }
            }
        }
    }
}

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

pub(crate) async fn load_limits_outage_policy(store: &Store, limiter: &ReloadableLimiter) {
    match store.limits_valkey_unavailable_policy().await {
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
    store: Store,
    limiter: ReloadableLimiter,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut interval = tokio::time::interval(Duration::from_secs(15));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => load_limits_outage_policy(&store, &limiter).await,
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return;
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::{
        LossCheckpointOutcome, LossReport, RequestMetadataLossCounters, classify_loss_checkpoint,
        record_loss_checkpoint,
    };

    const fn silent() -> LossReport {
        LossReport {
            reported_events: 0,
            reported_dropped: 0,
            reported_abandoned: 0,
            process_epoch_changed: false,
        }
    }

    #[test]
    fn a_checkpoint_with_reported_events_is_warned_about() {
        let report = LossReport {
            reported_events: 2,
            reported_dropped: 1,
            reported_abandoned: 1,
            process_epoch_changed: false,
        };
        assert_eq!(
            classify_loss_checkpoint(report),
            LossCheckpointOutcome::Reported
        );
    }

    #[test]
    fn reported_events_outrank_an_unchanged_epoch_flag() {
        // A checkpoint that both reports loss and rolls the epoch must still
        // surface the loss, not the quieter epoch line.
        let report = LossReport {
            reported_events: 5,
            reported_dropped: 5,
            reported_abandoned: 0,
            process_epoch_changed: true,
        };
        assert_eq!(
            classify_loss_checkpoint(report),
            LossCheckpointOutcome::Reported
        );
    }

    #[test]
    fn a_new_epoch_without_loss_is_still_visible() {
        let report = LossReport {
            reported_events: 0,
            reported_dropped: 0,
            reported_abandoned: 0,
            process_epoch_changed: true,
        };
        assert_eq!(
            classify_loss_checkpoint(report),
            LossCheckpointOutcome::EpochChanged
        );
    }

    #[test]
    fn an_empty_unchanged_checkpoint_says_nothing() {
        assert_eq!(
            classify_loss_checkpoint(silent()),
            LossCheckpointOutcome::Silent
        );
    }

    #[test]
    fn every_checkpoint_accumulates_into_the_process_counters() {
        let counters = RequestMetadataLossCounters::default();
        for report in [
            LossReport {
                reported_events: 3,
                reported_dropped: 2,
                reported_abandoned: 1,
                process_epoch_changed: false,
            },
            LossReport {
                reported_events: 0,
                reported_dropped: 0,
                reported_abandoned: 0,
                process_epoch_changed: true,
            },
            silent(),
        ] {
            record_loss_checkpoint(&counters, "gateway-under-test", report);
        }
        assert_eq!(counters.totals(), (3, 2, 1));
    }
}

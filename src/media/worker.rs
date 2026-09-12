use crate::media::service::reconcile_media_jobs_once;
use std::time::Duration;
use tokio::sync::watch;
use tracing::{info, warn};

/// Per-tick claim budget. Jobs are claimed in chunks of
/// `RECONCILIATION_CONCURRENCY`, so a claim never waits behind a running one.
const RECONCILIATION_BATCH: u16 = 16;

pub(crate) async fn media_reconciliation_supervisor(
    state: crate::media::service::MediaJobs,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut interval = tokio::time::interval(Duration::from_secs(5));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                match reconcile_media_jobs_once(&state, RECONCILIATION_BATCH).await {
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

use olp_db::{
    store::Store,
    worker_health::{WorkerTask, WorkerTaskCheckpointOutcome},
};
use std::time::Duration;
use tokio::sync::watch;
use tracing::{error, info, warn};

pub(crate) async fn maintenance_supervisor(store: Store, mut shutdown: watch::Receiver<bool>) {
    // Frequent bounded passes keep receipt expiry from becoming one large
    // hourly DELETE/WAL spike at qualified request rates.
    let mut interval = tokio::time::interval(Duration::from_secs(60));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                match store.run_maintenance(chrono::Utc::now()).await {
                    Ok(report) => {
                        let outcome = if report.lock_acquired {
                            WorkerTaskCheckpointOutcome::Success
                        } else {
                            WorkerTaskCheckpointOutcome::Skipped
                        };
                        if let Err(error) = store
                            .report_worker_task_checkpoint(
                                WorkerTask::Maintenance,
                                outcome,
                                report.lock_acquired,
                            )
                            .await
                        {
                            warn!(%error, "maintenance health checkpoint failed");
                        }
                        if report.lock_acquired {
                            info!(?report, "maintenance pass completed");
                        }
                    }
                    Err(error) => {
                        if let Err(checkpoint_error) = store
                            .report_worker_task_checkpoint(
                                WorkerTask::Maintenance,
                                WorkerTaskCheckpointOutcome::Failure,
                                false,
                            )
                            .await
                        {
                            warn!(%checkpoint_error, "maintenance failure checkpoint failed");
                        }
                        error!(%error, "maintenance pass failed; retrying next interval");
                    }
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

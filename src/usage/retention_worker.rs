use crate::observability::workers::WorkerTask;
use crate::observability::workers::WorkerTaskCheckpointOutcome;
use sqlx::PgPool;
use std::time::Duration;
use tokio::sync::watch;
use tracing::error;
use tracing::info;
use tracing::warn;

pub(crate) async fn maintenance_supervisor(pool: PgPool, mut shutdown: watch::Receiver<bool>) {
    // Frequent bounded passes keep receipt expiry from becoming one large
    // hourly DELETE/WAL spike at qualified request rates.
    let mut interval = tokio::time::interval(Duration::from_secs(60));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                match crate::usage::retention::run_maintenance(&pool, chrono::Utc::now()).await {
                    Ok(report) => {
                        let outcome = if report.lock_acquired {
                            WorkerTaskCheckpointOutcome::Success
                        } else {
                            WorkerTaskCheckpointOutcome::Skipped
                        };
                        if let Err(error) = crate::observability::workers::report_worker_task_checkpoint(&pool,
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
                        if let Err(checkpoint_error) = crate::observability::workers::report_worker_task_checkpoint(&pool,
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

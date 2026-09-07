use crate::limits::distributed::DistributedLimiter;
use crate::limits::distributed::cost_reconciliation::CostReconciliationLeader;
use crate::limits::distributed::costs::CostReconciliationError;
use crate::limits::distributed::costs::CostReconciliationReport;
use crate::observability::workers::WorkerTask;
use crate::observability::workers::WorkerTaskCheckpointOutcome;
use sqlx::PgPool;
use std::time::Duration;
use tokio::sync::watch;
use tracing::info;
use tracing::warn;
pub(crate) async fn cost_reconciliation_supervisor(
    pool: PgPool,
    valkey_url: String,
    limits_namespace: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut interval = tokio::time::interval(Duration::from_secs(60));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let mut leader = None;
    loop {
        if *shutdown.borrow() {
            return;
        }
        tokio::select! {
            biased;
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return;
                }
            }
            _ = interval.tick() => {
                let result = tokio::select! {
                    biased;
                    _ = shutdown.changed() => return,
                    result = tokio::time::timeout(
                        Duration::from_secs(120),
                        reconcile_costs_as_leader(
                            &pool, &valkey_url, &limits_namespace, &mut leader,
                        ),
                    ) => result
                        .map_err(crate::limits::admission::LimitError::service)
                        .and_then(|result| result.map_err(
                            crate::limits::admission::LimitError::service,
                        )),
                };
                let (outcome, progress) = match result {
                    Ok(report) if report.lock_acquired => {
                        if report.keys_reconciled > 0 {
                            info!(?report, "cost reconciliation pass completed");
                        }
                        (WorkerTaskCheckpointOutcome::Success, report.keys_reconciled > 0)
                    }
                    Ok(_) => (WorkerTaskCheckpointOutcome::Skipped, false),
                    Err(error) => {
                        leader = None;
                        warn!(%error, "cost reconciliation failed; releasing leadership and retrying");
                        (WorkerTaskCheckpointOutcome::Failure, false)
                    }
                };
                if let Err(error) = crate::observability::workers::report_worker_task_checkpoint(&pool,
                    WorkerTask::CostReconciliation, outcome, progress,
                ).await {
                    warn!(%error, "cost reconciliation health checkpoint failed");
                }
            }
        }
    }
}

async fn reconcile_costs_as_leader(
    pool: &PgPool,
    valkey_url: &str,
    limits_namespace: &str,
    leader: &mut Option<CostReconciliationLeader>,
) -> Result<CostReconciliationReport, CostReconciliationError> {
    if leader.is_none() {
        *leader = crate::limits::distributed::cost_reconciliation::try_acquire_cost_reconciliation_leader(pool).await?;
    }
    let Some(leader) = leader.as_mut() else {
        return Ok(CostReconciliationReport::default());
    };
    let limiter = DistributedLimiter::connect(valkey_url, limits_namespace).await?;
    leader.reconcile(&limiter, chrono::Utc::now()).await
}

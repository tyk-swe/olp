use olp_db::limits::{
    DistributedLimiter,
    cost_reconciliation::CostReconciliationLeader,
    costs::{CostReconciliationError, CostReconciliationReport},
};
use olp_db::{
    store::Store,
    worker_health::{WorkerTask, WorkerTaskCheckpointOutcome},
};
use std::time::Duration;
use tokio::sync::watch;
use tracing::{info, warn};
pub(crate) async fn cost_reconciliation_supervisor(
    store: Store,
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
                            &store, &valkey_url, &limits_namespace, &mut leader,
                        ),
                    ) => result
                        .map_err(olp_engine::inference::limits::LimitError::service)
                        .and_then(|result| result.map_err(
                            olp_engine::inference::limits::LimitError::service,
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
                if let Err(error) = store.report_worker_task_checkpoint(
                    WorkerTask::CostReconciliation, outcome, progress,
                ).await {
                    warn!(%error, "cost reconciliation health checkpoint failed");
                }
            }
        }
    }
}

async fn reconcile_costs_as_leader(
    store: &Store,
    valkey_url: &str,
    limits_namespace: &str,
    leader: &mut Option<CostReconciliationLeader>,
) -> Result<CostReconciliationReport, CostReconciliationError> {
    if leader.is_none() {
        *leader = store.try_acquire_cost_reconciliation_leader().await?;
    }
    let Some(leader) = leader.as_mut() else {
        return Ok(CostReconciliationReport::default());
    };
    let limiter = DistributedLimiter::connect(valkey_url, limits_namespace).await?;
    leader.reconcile(&limiter, chrono::Utc::now()).await
}
